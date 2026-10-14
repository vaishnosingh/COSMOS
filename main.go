package main

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"io"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"math/big"
	"os"
	"os/exec"
	"time"

	"github.com/gocql/gocql"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

//go:embed templates/*
var templatesFS embed.FS

var tmpl *template.Template
var scyllaSession *gocql.Session

func initDB() {
	cluster := gocql.NewCluster("127.0.0.1")
	cluster.Keyspace = "system"
	cluster.Timeout = 10 * time.Second

	var sys *gocql.Session
	var err error
	for i := 0; i < 60; i++ {
		sys, err = cluster.CreateSession()
		if err == nil {
			break
		}
		log.Printf("[DB] Waiting for ScyllaDB (%d/60): %v", i+1, err)
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		log.Fatalf("[DB] Failed to connect: %v", err)
	}

	err = sys.Query("CREATE KEYSPACE IF NOT EXISTS unity WITH replication = {'class': 'NetworkTopologyStrategy', 'datacenter1': 1}").Exec()
	if err != nil {
		log.Fatalf("[DB] Failed to create keyspace: %v", err)
	}
	sys.Close()

	kCluster := gocql.NewCluster("127.0.0.1")
	kCluster.Keyspace = "cosmos"
	kCluster.Timeout = 10 * time.Second
	scyllaSession, err = kCluster.CreateSession()
	if err != nil {
		log.Fatalf("[DB] Failed to connect to unity keyspace: %v", err)
	}

	scyllaSession.Query(`CREATE TABLE IF NOT EXISTS users (
		service       text,
		email         text,
		name          text,
		password_hash text,
		access_key    text,
		secret_key    text,
		bucket        text,
		session_id    text,
		created_at    bigint,
		PRIMARY KEY ((service, email))
	)`).Exec()

	scyllaSession.Query(`CREATE INDEX IF NOT EXISTS users_session ON cosmos.users (session_id)`).Exec()
	// Migrate existing tables: add created_at if missing (ignore errors on existing columns)
	scyllaSession.Query(`ALTER TABLE cosmos.users ADD created_at bigint`).Exec()

	scyllaSession.Query(`CREATE TABLE IF NOT EXISTS pending (
		service       text,
		reg_token     text,
		session_id    text,
		name          text,
		email         text,
		password_hash text,
		PRIMARY KEY ((service, reg_token))
	)`).Exec()
	// raw_password column no longer used — stored in memory only; ignore error if already dropped
	scyllaSession.Query(`ALTER TABLE cosmos.pending DROP raw_password`).Exec()

	// Forgot-password reset tokens — 'token' is a CQL reserved word, use reset_token
	// Drop the old broken table (if it exists with wrong schema) and recreate
	scyllaSession.Query(`DROP TABLE IF EXISTS cosmos.password_reset_tokens`).Exec()
	scyllaSession.Query(`CREATE TABLE IF NOT EXISTS cosmos.password_reset_tokens (
		reset_token text PRIMARY KEY,
		service     text,
		email       text,
		expires_at  bigint
	)`).Exec()

	log.Println("[DB] ScyllaDB ready")
}

var pendingPasswords sync.Map // map[service+":"+regToken]rawPassword — in-memory only, never written to DB

func init() {
	tmpl = template.Must(template.ParseFS(templatesFS, "templates/*.html"))
}

func generateOTP() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	return fmt.Sprintf("%06d", n.Int64())
}

func sendVerificationEmail(email, token, service string) {
	msg := fmt.Sprintf("From: noreply@cosmos.linuxguy.space\r\nTo: %s\r\nSubject: Verify your Cosmos %s Account\r\n\r\nYour verification code is: %s\r\n\r\nEnter this 6-digit code in the portal to verify your account.\r\n",
		email, strings.ToUpper(service), token)
	cmd := exec.Command("msmtp", "-t")
	cmd.Stdin = bytes.NewBufferString(msg)
	if err := cmd.Run(); err != nil {
		log.Printf("[MAIL] Failed: %v", err)
	}
}

// sessionMaxAge is the maximum age of a valid session (30 days).
const sessionMaxAge = 30 * 24 * time.Hour

func lookupBySession(service, sid string) (name, ak, sk, bucket string, err error) {
	var createdAt int64
	err = scyllaSession.Query(
		`SELECT name, access_key, secret_key, bucket, created_at FROM cosmos.users WHERE service = ? AND session_id = ? ALLOW FILTERING`,
		service, sid).Scan(&name, &ak, &sk, &bucket, &createdAt)
	if err != nil {
		return
	}
	// Enforce session expiry — createdAt=0 means old session (pre-expiry feature), allow it
	if createdAt > 0 && time.Since(time.Unix(createdAt, 0)) > sessionMaxAge {
		err = fmt.Errorf("session expired")
		name, ak, sk, bucket = "", "", "", ""
	}
	return
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.ExecuteTemplate(w, "index.html", nil)
}

func handleServicePage(w http.ResponseWriter, r *http.Request, service string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.ExecuteTemplate(w, service+".html", nil)
}


func handlePoll(w http.ResponseWriter, r *http.Request, service string) {
	sid := r.URL.Query().Get("sid")
	w.Header().Set("Content-Type", "application/json")
	_, _, _, _, err := lookupBySession(service, sid)
	if err == nil {
		w.Write([]byte(`{"ok": true}`))
	} else {
		w.Write([]byte(`{"ok": false}`))
	}
}
// Replacing handleSignup
func handleSignup(w http.ResponseWriter, r *http.Request, service string) {
	if r.Method == "GET" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		tmpl.ExecuteTemplate(w, "signup.html", map[string]string{"Service": service})
		return
	}
	r.ParseMultipartForm(10 << 20)
	name := strings.TrimSpace(r.FormValue("name"))
	username := strings.ToLower(strings.TrimSpace(r.FormValue("username")))
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	password := r.FormValue("password")
	confirm := r.FormValue("confirm_password")
	
	if email == "" || password == "" || name == "" || username == "" {
		http.Error(w, "All fields required", http.StatusBadRequest)
		return
	}
	if password != confirm {
		http.Error(w, "Passwords do not match", http.StatusBadRequest)
		return
	}
	
	// Read profile image if present
	var profileImageBase64 string
	file, _, err := r.FormFile("profile_image")
	if err == nil {
		defer file.Close()
		bytes, err := io.ReadAll(file)
		if err == nil && len(bytes) > 0 {
			profileImageBase64 = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(bytes)
		}
	}

	var ex string
	if scyllaSession.Query(`SELECT email FROM cosmos.users WHERE service = ? AND email = ?`, service, email).Scan(&ex) == nil {
		http.Error(w, "Email already registered", http.StatusConflict)
		return
	}
	// Also check username uniqueness
	if scyllaSession.Query(`SELECT email FROM cosmos.users WHERE service = ? AND username = ? ALLOW FILTERING`, service, username).Scan(&ex) == nil {
		http.Error(w, "Username already taken", http.StatusConflict)
		return
	}

	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	regToken := generateOTP()
	sessionID := uuid.New().String()
	
	// Store session+OTP in pending table — raw_password NOT stored in DB (security)
	if err := scyllaSession.Query(`INSERT INTO unity.pending (service, reg_token, session_id, name, email, password_hash, username, profile_image) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		service, regToken, sessionID, name, email, string(hash), username, profileImageBase64).Exec(); err != nil {
		log.Printf("[SIGNUP] DB: %v", err)
		http.Error(w, "Database error", 500)
		return
	}
	// Store plain password in memory (not DB) for Gitea provisioning — auto-expire after 15 min
	pendingPasswords.Store(service+":"+regToken, password)
	go func(key string) {
		time.Sleep(15 * time.Minute)
		pendingPasswords.Delete(key)
	}(service + ":" + regToken)
	go sendVerificationEmail(email, regToken, service)
	
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>Verify Email – Cosmos</title><style>*{box-sizing:border-box;margin:0;padding:0}body{background:#000;color:#fff;font-family:-apple-system,sans-serif;text-align:center;padding:120px 20px}.sp{border:3px solid rgba(255,255,255,0.1);width:40px;height:40px;border-radius:50%;border-left-color:#2997ff;animation:spin 1s linear infinite;margin:0 auto 32px;display:none}@keyframes spin{to{transform:rotate(360deg)}}h2{font-size:32px;margin-bottom:12px}p{color:#86868b;font-size:17px;line-height:1.5;margin-bottom:30px}input{background:#111;border:1px solid rgba(255,255,255,0.15);color:#fff;font-size:24px;padding:16px;border-radius:12px;width:200px;text-align:center;letter-spacing:4px;outline:none}input:focus{border-color:#2997ff}button{background:#fff;color:#000;border:none;padding:16px 32px;font-size:17px;font-weight:600;border-radius:12px;cursor:pointer;margin-top:20px;transition:.2s}button:hover{background:#f5f5f7}button:active{transform:scale(0.98)}</style></head><body><div id="sp" class="sp"></div><h2 id="hd">Check your email</h2><p id="sb">We sent a 6-digit verification code to ` + email + `.<br>It may take a moment to arrive.</p><form id="fm" onsubmit="event.preventDefault(); verify()"><input type="text" id="code" maxlength="6" pattern="[0-9]{6}" autocomplete="off" autofocus required placeholder="000000"><br><button type="submit">Verify Email</button></form><script>async function verify(){document.getElementById('sp').style.display='block';document.getElementById('fm').style.display='none';document.getElementById('hd').innerText='Verifying...';document.getElementById('sb').innerText='Please wait while we set up your account.';const r=await fetch('/`+service+`/verify',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:'code='+document.getElementById('code').value});if(r.ok){const data=await r.json();location.href=data.redirect;}else{document.getElementById('sp').style.display='none';document.getElementById('fm').style.display='block';document.getElementById('hd').innerText='Verification Failed';document.getElementById('sb').innerText='The code was incorrect. Please try again.';}}</script></body></html>`))
}

// Replacing handleLogin
func handleLogin(w http.ResponseWriter, r *http.Request, service string) {
	if r.Method == "GET" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data := map[string]string{"Service": service}
		if r.URL.Query().Get("error") != "" {
			data["Error"] = "Invalid credentials."
		}
		tmpl.ExecuteTemplate(w, "login.html", data)
		return
	}
	r.ParseForm()
	ident := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	password := r.FormValue("password")
	
	var sessionID, passwordHash string
	
	// Check if ident is email or username
	query := `SELECT session_id, password_hash FROM cosmos.users WHERE service = ? AND email = ?`
	if !strings.Contains(ident, "@") {
		query = `SELECT session_id, password_hash FROM cosmos.users WHERE service = ? AND username = ? ALLOW FILTERING`
	}
	
	if err := scyllaSession.Query(query, service, ident).Scan(&sessionID, &passwordHash); err != nil {
		http.Redirect(w, r, "/"+service+"/login?error=1", http.StatusFound)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)) != nil {
		http.Redirect(w, r, "/"+service+"/login?error=1", http.StatusFound)
		return
	}

	if service == "git" {
		var giteaUser string
		scyllaSession.Query(`SELECT username FROM cosmos.users WHERE service = ? AND email = ?`, service, ident).Scan(&giteaUser)
		if giteaUser == "" {
			scyllaSession.Query(`SELECT username FROM cosmos.users WHERE service = ? AND username = ? ALLOW FILTERING`, service, ident).Scan(&giteaUser)
		}
		if giteaUser == "" {
			giteaUser = ident
		}
		cookies, err := loginGitea(giteaUser, password)
		if err == nil {
			for _, c := range cookies {
				if c.Name == "i_like_gitea" && c.Value != "" {
					http.SetCookie(w, &http.Cookie{
						Name:     c.Name,
						Value:    c.Value,
						Path:     "/source",
						MaxAge:   86400 * 7,
						HttpOnly: true,
						Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
						SameSite: http.SameSiteLaxMode,
					})
				}
			}
			http.SetCookie(w, &http.Cookie{
				Name:     "cosmos_sid",
				Value:    sessionID,
				Path:     "/",
				MaxAge:   86400 * 30,
				HttpOnly: true,
				Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
				SameSite: http.SameSiteLaxMode,
			})
			http.Redirect(w, r, "/source/", http.StatusFound)
			return
		}
		log.Printf("[LOGIN] Gitea login attempt failed for %s: %v", giteaUser, err)
	}

	http.Redirect(w, r, "/"+service+"/dashboard?sid="+sessionID, http.StatusFound)
}

func handleVerify(w http.ResponseWriter, r *http.Request, service string) {
	if r.Method == "POST" {
		r.ParseForm()
	}
	token := r.FormValue("code")
	if token == "" {
		token = r.FormValue("token")
	}
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if token == "" {
		http.Error(w, "Missing token", 400)
		return
	}
	var sessionID, name, email, passwordHash, username, profileImage string
	// raw_password is never stored in DB — read from in-memory map only
	err := scyllaSession.Query(`SELECT session_id, name, email, password_hash, username, profile_image FROM unity.pending WHERE service = ? AND reg_token = ?`,
		service, token).Scan(&sessionID, &name, &email, &passwordHash, &username, &profileImage)
	if err != nil {
		if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
			http.Error(w, "Invalid token", 400)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body style="background:#0a0a0b;color:#fafafa;font-family:sans-serif;text-align:center;padding:100px"><h2>Invalid or expired link.</h2></body></html>`)
		return
	}

	// Retrieve raw password from in-memory map (for Gitea provisioning only) — never stored in DB
	var rawPassword string
	if val, ok := pendingPasswords.Load(service + ":" + token); ok {
		rawPassword = val.(string)
	}

	accessKey := email
	secretKey := strings.ReplaceAll(uuid.New().String(), "-", "") + strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	bucket := "unity-" + strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	// Store created_at for future session expiry checks
	createdAt := time.Now().Unix()
	if err = scyllaSession.Query(`INSERT INTO cosmos.users (service, email, name, password_hash, access_key, secret_key, bucket, bucket_display_name, session_id, username, profile_image, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'mybucket', ?, ?, ?, ?)`,
		service, email, name, passwordHash, accessKey, secretKey, bucket, sessionID, username, profileImage, createdAt).Exec(); err != nil {
		// created_at column may not exist yet — fall back without it
		if err2 := scyllaSession.Query(`INSERT INTO cosmos.users (service, email, name, password_hash, access_key, secret_key, bucket, bucket_display_name, session_id, username, profile_image) VALUES (?, ?, ?, ?, ?, ?, ?, 'mybucket', ?, ?, ?)`,
			service, email, name, passwordHash, accessKey, secretKey, bucket, sessionID, username, profileImage).Exec(); err2 != nil {
			log.Printf("[VERIFY] Insert failed: %v", err2)
			http.Error(w, "Failed to create account", 500)
			return
		}
	}
	scyllaSession.Query(`DELETE FROM unity.pending WHERE service = ? AND reg_token = ?`, service, token).Exec()

	// Clear raw password from memory immediately after use
	pendingPasswords.Delete(service + ":" + token)

	go func() {
		switch service {
		case "oort":
			provisionMinIO(accessKey, secretKey, bucket)
		case "git":
			if rawPassword != "" {
				provisionGitea(username, email, rawPassword)
			}
		}
	}()
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"redirect": "/%s/login?verified=1"}`, service)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Verified – Cosmos</title><style>
body{background:#0a0a0b;color:#fafafa;font-family:-apple-system,sans-serif;text-align:center;padding:120px 20px}
.ck{width:64px;height:64px;border-radius:16px;background:rgba(34,197,94,0.1);display:inline-flex;align-items:center;justify-content:center;margin-bottom:24px}
h2{font-size:32px;margin-bottom:12px}p{color:#a1a1aa;font-size:15px;line-height:1.6;margin-bottom:40px}
a{background:#fafafa;color:#000;padding:14px 28px;border-radius:8px;text-decoration:none;font-size:15px;font-weight:600}
</style></head><body>
<div class="ck"><svg width="32" height="32" fill="none" viewBox="0 0 24 24" stroke="#22c55e" stroke-width="2.5"><path stroke-linecap="round" stroke-linejoin="round" d="M5 13l4 4L19 7"/></svg></div>
<h2>Email Verified!</h2>
<p>Your Cosmos %s account is provisioned.<br>You can now sign in.</p>
<a href="/%s/login">Continue to Login</a>
</body></html>`, strings.ToUpper(service), service)
}


func handleLogout(w http.ResponseWriter, r *http.Request, service string) {
	http.SetCookie(w, &http.Cookie{Name: "cosmos_sid", Value: "", Path: "/", MaxAge: -1})
	if service == "git" {
		http.SetCookie(w, &http.Cookie{Name: "i_like_gitea", Value: "", Path: "/source", MaxAge: -1})
	}
	http.Redirect(w, r, "/" + service, http.StatusFound)
}

func handleDashboard(w http.ResponseWriter, r *http.Request, service string) {
	sid := r.URL.Query().Get("sid")
	if sid == "" {
		http.Redirect(w, r, "/"+service+"/login", http.StatusFound)
		return
	}
	name, ak, sk, bucket, err := lookupBySession(service, sid)
	if err != nil {
		log.Printf("[DASHBOARD] Lookup failed service=%s sid=%s: %v", service, sid, err)
		http.Redirect(w, r, "/"+service+"/login", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	switch service {
	case "oort":
		fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>S3 – Cosmos</title><style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#000;color:#fff;font-family:-apple-system,sans-serif;min-height:100vh}
nav{position:fixed;top:0;width:100%%;height:44px;background:rgba(0,0,0,0.85);backdrop-filter:blur(20px);border-bottom:1px solid rgba(255,255,255,0.1);display:flex;align-items:center;justify-content:center;z-index:999}
.ni{max-width:980px;width:100%%;display:flex;justify-content:space-between;padding:0 22px;font-size:12px}
nav a{color:rgba(255,255,255,0.8);text-decoration:none}
.main{max-width:680px;margin:0 auto;padding:100px 22px 60px}
h1{font-size:48px;font-weight:700;margin-bottom:8px}
.sub{color:#86868b;font-size:19px;margin-bottom:48px}
.card{background:#111;border:1px solid rgba(255,255,255,0.1);border-radius:20px;padding:28px;margin-bottom:16px}
.lbl{font-size:11px;text-transform:uppercase;letter-spacing:0.08em;color:#86868b;font-weight:600;margin-bottom:6px}
.val{font-family:monospace;font-size:15px;color:#2997ff;word-break:break-all}
.btn{display:inline-block;margin-top:40px;background:#fff;color:#000;padding:16px 36px;border-radius:980px;text-decoration:none;font-size:17px;font-weight:500}
</style></head><body>
<nav><div class="ni"><a href="/">Cosmos</a><a href="/oort" style="color:#fff;font-weight:600">Storage</a><a href="/oort/logout" style="color:#ff3b30;margin-left:24px;font-weight:600">Sign Out</a></div></nav>
<div class="main">
<h1>Welcome, %s.</h1><p class="sub">Your S3 workspace is provisioned.</p>
<div class="card"><div class="lbl">Access Key ID</div><div class="val">%s</div></div>
<div class="card"><div class="lbl">Secret Access Key</div><div class="val">%s</div></div>
<div class="card"><div class="lbl">Private Bucket</div><div class="val">%s</div></div>
<div class="card"><div class="lbl">S3 Endpoint</div><div class="val">https://cosmos.linuxguy.space</div></div>
<a href="/oort/console?sid=%s" class="btn">Open Storage Console →</a>
</div></body></html>`, name, ak, sk, bucket, sid)

	case "git":
		fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Git – Cosmos</title><style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#000;color:#fff;font-family:-apple-system,sans-serif;min-height:100vh}
nav{position:fixed;top:0;width:100%%;height:44px;background:rgba(0,0,0,0.85);backdrop-filter:blur(20px);border-bottom:1px solid rgba(255,255,255,0.1);display:flex;align-items:center;justify-content:center;z-index:999}
.ni{max-width:980px;width:100%%;display:flex;justify-content:space-between;padding:0 22px;font-size:12px}
nav a{color:rgba(255,255,255,0.8);text-decoration:none}
.main{max-width:680px;margin:0 auto;padding:100px 22px 60px;text-align:center}
h1{font-size:48px;font-weight:700;margin-bottom:8px}
.sub{color:#86868b;font-size:19px;margin-bottom:48px}
.btn{display:inline-block;background:#fff;color:#000;padding:18px 40px;border-radius:980px;text-decoration:none;font-size:19px;font-weight:500}
</style></head><body>
<nav><div class="ni"><a href="/">Cosmos</a><a href="/git" style="color:#fff;font-weight:600">Source</a><a href="/git/logout" style="color:#ff3b30;margin-left:24px;font-weight:600">Sign Out</a></div></nav>
<div class="main">
<h1>Welcome, %s.</h1><p class="sub">Your Forge workspace is connected.</p>
<a href="/source/" class="btn">Open Git Server →</a>
</div></body></html>`, name)
	}
}

func provisionMinIO(ak, sk, bucket string) {
	log.Printf("[MinIO] Provisioning %s", ak)
	exec.Command("mc", "alias", "set", "cosmos", "http://127.0.0.1:9000", "unityadmin", "UnityAdmin2026!").Run()
	exec.Command("mc", "admin", "user", "add", "cosmos", ak, sk).Run()
	exec.Command("mc", "mb", "--ignore-existing", "cosmos/"+bucket).Run()
	exec.Command("mc", "version", "enable", "cosmos/"+bucket).Run()
	
	// Create user-specific policy restricting them to their own bucket
	policyStr := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::%s","arn:aws:s3:::%s/*"]}]}`, bucket, bucket)
	policyFile := "/tmp/policy_" + bucket + ".json"
	os.WriteFile(policyFile, []byte(policyStr), 0644)
	exec.Command("mc", "admin", "policy", "create", "cosmos", "policy_"+bucket, policyFile).Run()
	exec.Command("mc", "admin", "policy", "attach", "cosmos", "policy_"+bucket, "--user", ak).Run()
	
	// Set 10GB quota
	exec.Command("mc", "quota", "set", "cosmos/"+bucket, "--size", "10G").Run()
	
	log.Printf("[MinIO] Done: %s, bucket %s (10GB Quota)", ak, bucket)
}



func provisionGitea(username, email, password string) {
	log.Printf("[Gitea] Provisioning %s as %s", email, username)
	time.Sleep(3 * time.Second)
	cmd := exec.Command("sudo", "docker", "exec", "-u", "git", "-i", "gitea-gitea-1",
		"gitea", "admin", "user", "create",
		"--username", username,
		"--email", email,
		"--password", password,
		"--must-change-password=false")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[Gitea] Failed: %v | %s", err, string(out))
	}
}

func syncGiteaPassword(username, newPassword string) error {
	log.Printf("[Gitea] Synchronizing password for user: %s", username)
	cmd := exec.Command("sudo", "docker", "exec", "-u", "git", "-i", "gitea-gitea-1",
		"gitea", "admin", "user", "change-password",
		"--username", username,
		"--password", newPassword,
		"--must-change-password=false")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[Gitea] Password sync failed for %s: %v | %s", username, err, string(out))
		return err
	}
	log.Printf("[Gitea] Password synced successfully for %s", username)
	return nil
}

func loginGitea(username, password string) ([]*http.Cookie, error) {
	form := url.Values{
		"user_name": {username},
		"password":  {password},
	}
	req, err := http.NewRequest("POST", "http://127.0.0.1:3000/user/login", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 5 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusSeeOther || resp.StatusCode == http.StatusFound {
		return resp.Cookies(), nil
	}

	// If login returned 200 (auth failed), Gitea password might be out of sync.
	// Auto-heal by syncing password from Cosmos and retrying!
	log.Printf("[Gitea] Login returned status %d for %s — syncing password and retrying...", resp.StatusCode, username)
	if syncErr := syncGiteaPassword(username, password); syncErr == nil {
		req2, _ := http.NewRequest("POST", "http://127.0.0.1:3000/user/login", strings.NewReader(form.Encode()))
		req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp2, err2 := client.Do(req2)
		if err2 == nil {
			defer resp2.Body.Close()
			if resp2.StatusCode == http.StatusSeeOther || resp2.StatusCode == http.StatusFound {
				log.Printf("[Gitea] Auto-heal login succeeded for %s", username)
				return resp2.Cookies(), nil
			}
		}
	}

	return nil, fmt.Errorf("gitea login failed with status %d", resp.StatusCode)
}

// ─── FORGOT PASSWORD ──────────────────────────────────────────────────────────

const resetTokenMaxAge = time.Hour // Reset links expire after 1 hour

// sendPasswordResetEmail sends the reset link to the user's email address.
func sendPasswordResetEmail(toEmail, token, service string) {
	link := fmt.Sprintf("https://cosmos.linuxguy.space/%s/reset-password?token=%s", service, token)
	msg := fmt.Sprintf(
		"From: noreply@cosmos.linuxguy.space\r\nTo: %s\r\nSubject: Reset your Cosmos %s password\r\n\r\n"+
			"Hi,\r\n\r\n"+
			"We received a request to reset the password for your Cosmos %s account (%s).\r\n\r\n"+
			"Click the link below to set a new password. This link is valid for 1 hour.\r\n\r\n"+
			"  %s\r\n\r\n"+
			"If you did not request this, you can safely ignore this email — your account remains secure.\r\n\r\n"+
			"— Cosmos Team\r\n",
		toEmail, strings.ToUpper(service), strings.ToUpper(service), toEmail, link,
	)
	cmd := exec.Command("msmtp", "-t")
	cmd.Stdin = bytes.NewBufferString(msg)
	if err := cmd.Run(); err != nil {
		log.Printf("[MAIL] Reset email failed for %s: %v", toEmail, err)
	}
}

// handleForgotPassword handles GET (show form) and POST (send reset email).
func handleForgotPassword(w http.ResponseWriter, r *http.Request, service string) {
	if r.Method == "GET" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data := map[string]any{"Service": service, "Step": "request"}
		if r.URL.Query().Get("sent") == "1" {
			data["Sent"] = true
		}
		tmpl.ExecuteTemplate(w, "forgot-password.html", data)
		return
	}

	// POST — look up user, generate token, send email
	r.ParseForm()
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	if email == "" {
		http.Redirect(w, r, "/"+service+"/forgot-password", http.StatusFound)
		return
	}

	// Always redirect to "sent" page regardless of whether the email exists
	// (prevents user enumeration — attacker can't tell if an email is registered)
	go func() {
		var name string
		err := scyllaSession.Query(
			`SELECT name FROM cosmos.users WHERE service = ? AND email = ?`, service, email,
		).Scan(&name)
		if err != nil {
			// User not found — do nothing, but we still showed "email sent" to the requester
			log.Printf("[RESET] Email not found: %s (service=%s)", email, service)
			return
		}
		// Generate a cryptographically secure token (UUID v4)
		token := uuid.New().String()
		expiresAt := time.Now().Add(resetTokenMaxAge).Unix()

		// Delete any previous unused tokens for this email to prevent token accumulation
		scyllaSession.Query(
			`DELETE FROM cosmos.password_reset_tokens WHERE reset_token = ?`, token,
		).Exec() // no-op if not exists; real cleanup via secondary scan would need an index

		// Store token
		if err := scyllaSession.Query(
			`INSERT INTO cosmos.password_reset_tokens (reset_token, service, email, expires_at) VALUES (?, ?, ?, ?)`,
			token, service, email, expiresAt,
		).Exec(); err != nil {
			log.Printf("[RESET] DB insert failed: %v", err)
			return
		}
		sendPasswordResetEmail(email, token, service)
	}()

	// Always show "check your email" — prevents user enumeration
	http.Redirect(w, r, "/"+service+"/forgot-password?sent=1", http.StatusFound)
}

// handleResetPassword handles GET (show new-password form) and POST (apply new password).
func handleResetPassword(w http.ResponseWriter, r *http.Request, service string) {
	token := r.URL.Query().Get("token")
	if r.Method == "POST" {
		r.ParseForm()
		token = r.FormValue("token")
	}

	renderError := func(msg string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		tmpl.ExecuteTemplate(w, "forgot-password.html", map[string]any{
			"Service": service,
			"Step":    "reset",
			"Token":   token,
			"Error":   msg,
		})
	}

	if token == "" {
		http.Redirect(w, r, "/"+service+"/forgot-password", http.StatusFound)
		return
	}

	// Look up token
	var storedService, email string
	var expiresAt int64
	err := scyllaSession.Query(
		`SELECT service, email, expires_at FROM cosmos.password_reset_tokens WHERE reset_token = ?`, token,
	).Scan(&storedService, &email, &expiresAt)
	if err != nil || storedService != service {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		tmpl.ExecuteTemplate(w, "forgot-password.html", map[string]any{
			"Service": service,
			"Step":    "invalid",
		})
		return
	}

	// Check expiry
	if time.Now().Unix() > expiresAt {
		scyllaSession.Query(`DELETE FROM cosmos.password_reset_tokens WHERE reset_token = ?`, token).Exec()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		tmpl.ExecuteTemplate(w, "forgot-password.html", map[string]any{
			"Service": service,
			"Step":    "expired",
		})
		return
	}

	if r.Method == "GET" {
		// Show the new-password form
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		tmpl.ExecuteTemplate(w, "forgot-password.html", map[string]any{
			"Service": service,
			"Step":    "reset",
			"Token":   token,
			"Email":   email,
		})
		return
	}

	// POST — apply new password
	newPassword := r.FormValue("password")
	confirmPassword := r.FormValue("confirm_password")

	if len(newPassword) < 8 {
		renderError("Password must be at least 8 characters.")
		return
	}
	if newPassword != confirmPassword {
		renderError("Passwords do not match.")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		renderError("Internal error. Please try again.")
		return
	}

	// Update password in database
	if err := scyllaSession.Query(
		`UPDATE cosmos.users SET password_hash = ? WHERE service = ? AND email = ?`,
		string(hash), service, email,
	).Exec(); err != nil {
		log.Printf("[RESET] Password update failed for %s: %v", email, err)
		renderError("Failed to update password. Please try again.")
		return
	}

	// If resetting git/forge password, synchronize with Gitea internal database
	if service == "git" {
		var giteaUser string
		scyllaSession.Query(`SELECT username FROM cosmos.users WHERE service = ? AND email = ?`, service, email).Scan(&giteaUser)
		if giteaUser != "" {
			if err := syncGiteaPassword(giteaUser, newPassword); err != nil {
				log.Printf("[RESET] Gitea password sync warning for %s: %v", giteaUser, err)
			}
		}
	}

	// Invalidate the reset token immediately
	scyllaSession.Query(`DELETE FROM cosmos.password_reset_tokens WHERE reset_token = ?`, token).Exec()
	log.Printf("[RESET] Password successfully reset for %s (service=%s)", email, service)

	// Show success page
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.ExecuteTemplate(w, "forgot-password.html", map[string]any{
		"Service": service,
		"Step":    "done",
	})
}

func serviceRouter(service string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		prefix := "/" + service + "/"
		action := strings.TrimPrefix(r.URL.Path, prefix)
		sub := ""
		if idx := strings.Index(action, "/"); idx != -1 {
			sub = action[idx+1:]
			action = action[:idx]
			if action == "api" && sub == "poll" {
				handlePoll(w, r, service)
				return
			}
		}
		switch action {
		case "signup":
			handleSignup(w, r, service)
		case "login":
			handleLogin(w, r, service)
		case "verify":
			handleVerify(w, r, service)
		case "logout":
			handleLogout(w, r, service)
		case "dashboard":
			handleDashboard(w, r, service)
		case "forgot-password":
			handleForgotPassword(w, r, service)
		case "reset-password":
			handleResetPassword(w, r, service)
		case "":
			handleServicePage(w, r, service)
		default:
			if service == "oort" {
				handleS3PublicProxy(w, r, action, sub)
				return
			}
			http.NotFound(w, r)
		}
	}
}

func main() {
	initDB()
	mux := http.NewServeMux()
	mux.HandleFunc("/tailwind.css", func(w http.ResponseWriter, r *http.Request) {
		data, _ := templatesFS.ReadFile("templates/tailwind.css")
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(data)
	})
	RegisterS3ConsoleRoutes(mux)
	RegisterOrbitRoutes(mux)
	mux.HandleFunc("/oort", func(w http.ResponseWriter, r *http.Request) { handleServicePage(w, r, "oort") })
	mux.HandleFunc("/git", func(w http.ResponseWriter, r *http.Request) { handleServicePage(w, r, "git") })
	mux.HandleFunc("/oort/", serviceRouter("oort"))
	mux.HandleFunc("/git/", serviceRouter("git"))
	mux.HandleFunc("/", handleIndex)
	log.Println("[SERVER] Starting Cosmos Portals on 127.0.0.1:8000")
	log.Fatal(http.ListenAndServe("127.0.0.1:8000", mux))
}
