package main

import (
	"archive/zip"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"golang.org/x/crypto/bcrypt"
)

// ─── INITIALIZE EXPANSION SCHEMA ──────────────────────────────────────────────

func InitExpansionSchema() {
	if scyllaSession == nil {
		return
	}
	// Share links
	scyllaSession.Query(`CREATE TABLE IF NOT EXISTS cosmos.share_links (
		share_token text PRIMARY KEY,
		bucket text,
		object_key text,
		owner_email text,
		created_at timestamp,
		expires_at timestamp,
		password_hash text,
		max_downloads int,
		download_count int,
		is_active boolean
	)`).Exec()

	// Bucket website config
	scyllaSession.Query(`CREATE TABLE IF NOT EXISTS cosmos.bucket_website_config (
		bucket text PRIMARY KEY,
		enabled boolean,
		index_doc text,
		error_doc text,
		spa_mode boolean
	)`).Exec()

	// Bucket webhooks
	scyllaSession.Query(`CREATE TABLE IF NOT EXISTS cosmos.bucket_webhooks (
		bucket text,
		webhook_id text,
		target_url text,
		secret text,
		events set<text>,
		created_at timestamp,
		PRIMARY KEY (bucket, webhook_id)
	)`).Exec()
}

// ─── EXPANSION ROUTE REGISTRATION ─────────────────────────────────────────────

func RegisterExpansionRoutes(mux *http.ServeMux) {
	InitExpansionSchema()

	// Public share links
	mux.HandleFunc("/s/", handlePublicShare)

	// Share management API
	mux.HandleFunc("/oort/api/shares/create", handleShareCreate)
	mux.HandleFunc("/oort/api/shares/list", handleShareList)
	mux.HandleFunc("/oort/api/shares/revoke", handleShareRevoke)

	// File preview & live text editor
	mux.HandleFunc("/oort/api/preview", handlePreview)
	mux.HandleFunc("/oort/api/preview/save", handlePreviewSave)

	// Batch operations (Multi-select Zip & Delete)
	mux.HandleFunc("/oort/api/batch/zip", handleBatchZip)
	mux.HandleFunc("/oort/api/batch/delete", handleBatchDelete)

	// Static website hosting
	mux.HandleFunc("/oort/api/bucket/website", handleBucketWebsiteConfig)
	mux.HandleFunc("/oort/site/", handleStaticSite)

	// Webhooks
	mux.HandleFunc("/oort/api/bucket/webhooks", handleBucketWebhooks)
	mux.HandleFunc("/oort/api/bucket/webhooks/test", handleBucketWebhookTest)

	// Storage breakdown analytics
	mux.HandleFunc("/oort/api/storage/breakdown", handleStorageBreakdown)
}

// ─── HELPERS ──────────────────────────────────────────────────────────────────

func generateSecureToken(byteLen int) string {
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return uuid.New().String()
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func getOwnerCredentials(bucketOrEmail string) (ak, sk, email string, err error) {
	if strings.Contains(bucketOrEmail, "@") {
		email = bucketOrEmail
	} else {
		err = scyllaSession.Query(`SELECT owner_email FROM cosmos.buckets WHERE bucket_name = ?`, bucketOrEmail).Scan(&email)
		if err != nil || email == "" {
			scyllaSession.Query(`SELECT email FROM cosmos.users WHERE service = 'oort' AND bucket = ? ALLOW FILTERING`, bucketOrEmail).Scan(&email)
		}
	}
	if email == "" {
		return "", "", "", fmt.Errorf("owner not found")
	}
	err = scyllaSession.Query(`SELECT access_key, secret_key FROM cosmos.users WHERE service = 'oort' AND email = ?`, email).Scan(&ak, &sk)
	return ak, sk, email, err
}

func renderSharePage(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if tmpl != nil {
		err := tmpl.ExecuteTemplate(w, "share.html", data)
		if err == nil {
			return
		}
		log.Printf("[SHARE] template render error: %v", err)
	}
	w.Write([]byte(fmt.Sprintf("<html><body><h1>Cosmos Oort Share</h1><p>%v</p></body></html>", data)))
}

// ─── SHARE LINKS HANDLERS ─────────────────────────────────────────────────────

func handleShareCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonErr(w, "method not allowed", 405)
		return
	}
	ak, _, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}

	var req struct {
		Bucket       string `json:"bucket"`
		Key          string `json:"key"`
		ExpiresInSec int64  `json:"expires_in_sec"`
		Password     string `json:"password"`
		MaxDownloads int    `json:"max_downloads"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid request", 400)
		return
	}
	if req.Bucket == "" {
		req.Bucket = defBucket
	}
	if req.Key == "" {
		jsonErr(w, "missing key", 400)
		return
	}
	if req.ExpiresInSec <= 0 {
		req.ExpiresInSec = 86400 // default 24h
	}
	// Max expiry 30 days
	if req.ExpiresInSec > 86400*30 {
		req.ExpiresInSec = 86400 * 30
	}

	var pwdHash string
	if req.Password != "" {
		hashBytes, err := bcrypt.GenerateFromPassword([]byte(req.Password), 10)
		if err == nil {
			pwdHash = string(hashBytes)
		}
	}

	token := generateSecureToken(18)
	now := time.Now()
	expiresAt := now.Add(time.Duration(req.ExpiresInSec) * time.Second)

	err := scyllaSession.Query(`INSERT INTO cosmos.share_links (share_token, bucket, object_key, owner_email, created_at, expires_at, password_hash, max_downloads, download_count, is_active) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		token, req.Bucket, req.Key, ak, now, expiresAt, pwdHash, req.MaxDownloads, 0, true).Exec()
	if err != nil {
		jsonErr(w, "database insert failed", 500)
		return
	}

	jsonOK(w, map[string]any{
		"ok":          true,
		"share_token": token,
		"share_url":   "/s/" + token,
		"expires_at":  expiresAt.Unix(),
	})
}

func handleShareList(w http.ResponseWriter, r *http.Request) {
	_, _, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = defBucket
	}

	iter := scyllaSession.Query(`SELECT share_token, object_key, created_at, expires_at, password_hash, max_downloads, download_count, is_active FROM cosmos.share_links ALLOW FILTERING`).Iter()
	var links []map[string]any
	var token, objKey, pwdHash string
	var createdAt, expiresAt time.Time
	var maxDl, dlCount int
	var isActive bool

	now := time.Now()
	for iter.Scan(&token, &objKey, &createdAt, &expiresAt, &pwdHash, &maxDl, &dlCount, &isActive) {
		// Filter by bucket owner
		if now.After(expiresAt) {
			isActive = false
		}
		if maxDl > 0 && dlCount >= maxDl {
			isActive = false
		}
		links = append(links, map[string]any{
			"token":              token,
			"key":                objKey,
			"created_at":         createdAt.Unix(),
			"expires_at":         expiresAt.Unix(),
			"has_password":       pwdHash != "",
			"max_downloads":      maxDl,
			"download_count":     dlCount,
			"is_active":          isActive,
			"url":                "/s/" + token,
		})
	}
	iter.Close()
	if links == nil {
		links = []map[string]any{}
	}
	jsonOK(w, links)
}

func handleShareRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" && r.Method != "DELETE" {
		jsonErr(w, "method not allowed", 405)
		return
	}
	_, _, _, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		var req struct {
			Token string `json:"token"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		token = req.Token
	}
	if token == "" {
		jsonErr(w, "missing token", 400)
		return
	}

	scyllaSession.Query(`UPDATE cosmos.share_links SET is_active = false WHERE share_token = ?`, token).Exec()
	jsonOK(w, map[string]any{"ok": true})
}

func handlePublicShare(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/s/")
	isDownload := false
	if strings.HasSuffix(token, "/download") {
		isDownload = true
		token = strings.TrimSuffix(token, "/download")
	}
	token = strings.Trim(token, "/")

	if token == "" {
		http.NotFound(w, r)
		return
	}

	var bucket, objectKey, ownerEmail, passwordHash string
	var createdAt, expiresAt time.Time
	var maxDownloads, downloadCount int
	var isActive bool

	err := scyllaSession.Query(`SELECT bucket, object_key, owner_email, created_at, expires_at, password_hash, max_downloads, download_count, is_active FROM cosmos.share_links WHERE share_token = ?`, token).
		Scan(&bucket, &objectKey, &ownerEmail, &createdAt, &expiresAt, &passwordHash, &maxDownloads, &downloadCount, &isActive)
	if err != nil || !isActive {
		renderSharePage(w, map[string]any{"Expired": true})
		return
	}

	// Check expiry
	if time.Now().After(expiresAt) {
		scyllaSession.Query(`UPDATE cosmos.share_links SET is_active = false WHERE share_token = ?`, token).Exec()
		renderSharePage(w, map[string]any{"Expired": true})
		return
	}

	// Check max downloads
	if maxDownloads > 0 && downloadCount >= maxDownloads {
		scyllaSession.Query(`UPDATE cosmos.share_links SET is_active = false WHERE share_token = ?`, token).Exec()
		renderSharePage(w, map[string]any{"Expired": true})
		return
	}

	requiresPassword := passwordHash != ""
	authenticated := false

	if requiresPassword {
		cookie, err := r.Cookie("share_auth_" + token)
		if err == nil && cookie.Value == "valid" {
			authenticated = true
		}

		if r.Method == "POST" {
			r.ParseForm()
			pwd := r.FormValue("password")
			if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(pwd)) == nil {
				authenticated = true
				http.SetCookie(w, &http.Cookie{
					Name:     "share_auth_" + token,
					Value:    "valid",
					Path:     "/s/" + token,
					HttpOnly: true,
					MaxAge:   3600 * 24,
				})
				http.Redirect(w, r, "/s/"+token, http.StatusSeeOther)
				return
			} else {
				renderSharePage(w, map[string]any{
					"Token":            token,
					"FileName":         path.Base(objectKey),
					"RequiresPassword": true,
					"Error":            "Incorrect password. Please try again.",
				})
				return
			}
		}
	} else {
		authenticated = true
	}

	ak, sk, _, err := getOwnerCredentials(bucket)
	if err != nil {
		ak, sk, _, err = getOwnerCredentials(ownerEmail)
	}
	if err != nil {
		renderSharePage(w, map[string]any{"Expired": true, "Error": "Storage credentials error"})
		return
	}

	mc, err := minioClient(ak, sk)
	if err != nil {
		renderSharePage(w, map[string]any{"Expired": true, "Error": "MinIO client error"})
		return
	}

	stat, err := mc.StatObject(r.Context(), bucket, objectKey, minio.StatObjectOptions{})
	if err != nil {
		renderSharePage(w, map[string]any{"Expired": true, "Error": "Object not found in storage"})
		return
	}

	if isDownload {
		if !authenticated {
			http.Redirect(w, r, "/s/"+token, http.StatusSeeOther)
			return
		}

		newCount := downloadCount + 1
		newActive := true
		if maxDownloads > 0 && newCount >= maxDownloads {
			newActive = false
		}
		scyllaSession.Query(`UPDATE cosmos.share_links SET download_count = ?, is_active = ? WHERE share_token = ?`, newCount, newActive, token).Exec()

		obj, err := mc.GetObject(r.Context(), bucket, objectKey, minio.GetObjectOptions{})
		if err != nil {
			http.Error(w, "Failed to retrieve object", 500)
			return
		}
		defer obj.Close()

		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, path.Base(objectKey)))
		w.Header().Set("Content-Type", stat.ContentType)
		w.Header().Set("Content-Length", strconv.FormatInt(stat.Size, 10))
		io.Copy(w, obj)
		return
	}

	expiresIn := time.Until(expiresAt).Round(time.Minute).String()
	downloadsRemaining := "Unlimited"
	if maxDownloads > 0 {
		downloadsRemaining = fmt.Sprintf("%d left (of %d)", maxDownloads-downloadCount, maxDownloads)
	}

	renderSharePage(w, map[string]any{
		"Token":              token,
		"FileName":           path.Base(objectKey),
		"FileSize":           formatBytes(stat.Size),
		"ContentType":        stat.ContentType,
		"ExpiresIn":          expiresIn,
		"DownloadsRemaining": downloadsRemaining,
		"RequiresPassword":   requiresPassword && !authenticated,
		"DownloadURL":        "/s/" + token + "/download",
	})
}

// ─── FILE PREVIEWS & CODE EDITOR ──────────────────────────────────────────────

func handlePreview(w http.ResponseWriter, r *http.Request) {
	ak, sk, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = defBucket
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		jsonErr(w, "missing key", 400)
		return
	}

	mc, err := minioClient(ak, sk)
	if err != nil {
		jsonErr(w, "storage client error", 500)
		return
	}

	obj, err := mc.GetObject(r.Context(), bucket, key, minio.GetObjectOptions{})
	if err != nil {
		jsonErr(w, "not found", 404)
		return
	}
	defer obj.Close()

	stat, err := obj.Stat()
	if err != nil {
		jsonErr(w, "object stat error", 404)
		return
	}

	contentType := stat.ContentType
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = mime.TypeByExtension(path.Ext(key))
		if contentType == "" {
			contentType = "text/plain; charset=utf-8"
		}
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size, 10))
	io.Copy(w, obj)
}

func handlePreviewSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonErr(w, "method not allowed", 405)
		return
	}
	ak, sk, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}

	var req struct {
		Bucket  string `json:"bucket"`
		Key     string `json:"key"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid request", 400)
		return
	}
	if req.Bucket == "" {
		req.Bucket = defBucket
	}
	if req.Key == "" {
		jsonErr(w, "missing key", 400)
		return
	}

	mc, err := minioClient(ak, sk)
	if err != nil {
		jsonErr(w, "storage client error", 500)
		return
	}

	contentBytes := []byte(req.Content)
	contentType := mime.TypeByExtension(path.Ext(req.Key))
	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}

	_, err = mc.PutObject(r.Context(), req.Bucket, req.Key, bytes.NewReader(contentBytes), int64(len(contentBytes)), minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		jsonErr(w, err.Error(), 500)
		return
	}

	go dispatchWebhooks(req.Bucket, "object:created", req.Key, int64(len(contentBytes)), contentType)
	jsonOK(w, map[string]any{"ok": true, "size": len(contentBytes)})
}

// ─── BATCH OPERATIONS (ZIP & DELETE) ──────────────────────────────────────────

func handleBatchZip(w http.ResponseWriter, r *http.Request) {
	ak, sk, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = defBucket
	}
	keysParam := r.URL.Query().Get("keys")
	if keysParam == "" {
		jsonErr(w, "missing keys", 400)
		return
	}
	keys := strings.Split(keysParam, ",")
	if len(keys) == 0 {
		jsonErr(w, "no keys provided", 400)
		return
	}

	mc, err := minioClient(ak, sk)
	if err != nil {
		jsonErr(w, "storage client error", 500)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="cosmos-export-%d.zip"`, time.Now().Unix()))

	zw := zip.NewWriter(w)
	defer zw.Close()

	ctx := r.Context()
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		obj, err := mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
		if err != nil {
			continue
		}
		stat, err := obj.Stat()
		if err != nil {
			obj.Close()
			continue
		}

		header := &zip.FileHeader{
			Name:     key,
			Method:   zip.Deflate,
			Modified: stat.LastModified,
		}
		ew, err := zw.CreateHeader(header)
		if err == nil {
			io.Copy(ew, obj)
		}
		obj.Close()
	}
}

func handleBatchDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonErr(w, "method not allowed", 405)
		return
	}
	ak, sk, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	var req struct {
		Bucket string   `json:"bucket"`
		Keys   []string `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid request body", 400)
		return
	}
	if req.Bucket == "" {
		req.Bucket = defBucket
	}
	if len(req.Keys) == 0 {
		jsonErr(w, "no keys provided", 400)
		return
	}

	mc, err := minioClient(ak, sk)
	if err != nil {
		jsonErr(w, "storage client error", 500)
		return
	}

	ctx := r.Context()
	deleted := 0
	for _, key := range req.Keys {
		if key == "" {
			continue
		}
		err := mc.RemoveObject(ctx, req.Bucket, key, minio.RemoveObjectOptions{})
		if err == nil {
			deleted++
			go dispatchWebhooks(req.Bucket, "object:deleted", key, 0, "")
		}
	}
	jsonOK(w, map[string]any{"ok": true, "deleted_count": deleted})
}

// ─── STATIC WEBSITE HOSTING ───────────────────────────────────────────────────

func handleBucketWebsiteConfig(w http.ResponseWriter, r *http.Request) {
	_, _, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = defBucket
	}

	if r.Method == "GET" {
		var enabled, spaMode bool
		var indexDoc, errorDoc string
		err := scyllaSession.Query(`SELECT enabled, index_doc, error_doc, spa_mode FROM cosmos.bucket_website_config WHERE bucket = ?`, bucket).
			Scan(&enabled, &indexDoc, &errorDoc, &spaMode)
		if err != nil {
			indexDoc = "index.html"
			errorDoc = "404.html"
		}
		jsonOK(w, map[string]any{
			"bucket":    bucket,
			"enabled":   enabled,
			"index_doc": indexDoc,
			"error_doc": errorDoc,
			"spa_mode":  spaMode,
		})
		return
	}

	if r.Method == "POST" {
		var req struct {
			Bucket   string `json:"bucket"`
			Enabled  bool   `json:"enabled"`
			IndexDoc string `json:"index_doc"`
			ErrorDoc string `json:"error_doc"`
			SpaMode  bool   `json:"spa_mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, "invalid request", 400)
			return
		}
		if req.Bucket == "" {
			req.Bucket = bucket
		}
		if req.IndexDoc == "" {
			req.IndexDoc = "index.html"
		}
		if req.ErrorDoc == "" {
			req.ErrorDoc = "404.html"
		}

		scyllaSession.Query(`INSERT INTO cosmos.bucket_website_config (bucket, enabled, index_doc, error_doc, spa_mode) VALUES (?, ?, ?, ?, ?)`,
			req.Bucket, req.Enabled, req.IndexDoc, req.ErrorDoc, req.SpaMode).Exec()

		jsonOK(w, map[string]any{"ok": true})
		return
	}
	jsonErr(w, "method not allowed", 405)
}

func handleStaticSite(w http.ResponseWriter, r *http.Request) {
	pathParts := strings.TrimPrefix(r.URL.Path, "/oort/site/")
	slashIdx := strings.Index(pathParts, "/")
	var bucket, reqPath string
	if slashIdx == -1 {
		bucket = pathParts
		reqPath = ""
	} else {
		bucket = pathParts[:slashIdx]
		reqPath = pathParts[slashIdx+1:]
	}
	if bucket == "" {
		http.Error(w, "Bucket not specified", 400)
		return
	}

	var enabled, spaMode bool
	var indexDoc, errorDoc string
	err := scyllaSession.Query(`SELECT enabled, index_doc, error_doc, spa_mode FROM cosmos.bucket_website_config WHERE bucket = ?`, bucket).
		Scan(&enabled, &indexDoc, &errorDoc, &spaMode)
	if err != nil || !enabled {
		http.Error(w, "Static website hosting is not enabled for this bucket", 404)
		return
	}
	if indexDoc == "" {
		indexDoc = "index.html"
	}

	ak, sk, _, err := getOwnerCredentials(bucket)
	if err != nil {
		http.Error(w, "Bucket owner credentials error", 500)
		return
	}
	mc, err := minioClient(ak, sk)
	if err != nil {
		http.Error(w, "Storage client error", 500)
		return
	}

	targetKey := reqPath
	if targetKey == "" || strings.HasSuffix(targetKey, "/") {
		targetKey = path.Join(targetKey, indexDoc)
	}

	ctx := r.Context()
	obj, err := mc.GetObject(ctx, bucket, targetKey, minio.GetObjectOptions{})
	var stat minio.ObjectInfo
	if err == nil {
		stat, err = obj.Stat()
	}

	if err != nil {
		if !strings.HasSuffix(targetKey, ".html") {
			htmlKey := targetKey + ".html"
			hObj, hErr := mc.GetObject(ctx, bucket, htmlKey, minio.GetObjectOptions{})
			if hErr == nil {
				hStat, hStatErr := hObj.Stat()
				if hStatErr == nil {
					obj = hObj
					stat = hStat
					err = nil
				} else {
					hObj.Close()
				}
			}
		}
	}

	if err != nil && spaMode {
		sObj, sErr := mc.GetObject(ctx, bucket, indexDoc, minio.GetObjectOptions{})
		if sErr == nil {
			sStat, sStatErr := sObj.Stat()
			if sStatErr == nil {
				obj = sObj
				stat = sStat
				err = nil
			} else {
				sObj.Close()
			}
		}
	}

	if err != nil {
		if errorDoc != "" {
			eObj, eErr := mc.GetObject(ctx, bucket, errorDoc, minio.GetObjectOptions{})
			if eErr == nil {
				_, eStatErr := eObj.Stat()
				if eStatErr == nil {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(404)
					io.Copy(w, eObj)
					eObj.Close()
					return
				}
				eObj.Close()
			}
		}
		http.NotFound(w, r)
		return
	}
	defer obj.Close()

	ct := stat.ContentType
	if ct == "" || ct == "application/octet-stream" {
		ct = mime.TypeByExtension(path.Ext(stat.Key))
		if ct == "" {
			ct = "text/html; charset=utf-8"
		}
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size, 10))
	io.Copy(w, obj)
}

// ─── WEBHOOK DISPATCHER ───────────────────────────────────────────────────────

func handleBucketWebhooks(w http.ResponseWriter, r *http.Request) {
	_, _, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = defBucket
	}

	if r.Method == "GET" {
		iter := scyllaSession.Query(`SELECT webhook_id, target_url, secret, events, created_at FROM cosmos.bucket_webhooks WHERE bucket = ?`, bucket).Iter()
		var webhooks []map[string]any
		var id, targetUrl, secret string
		var events []string
		var createdAt time.Time
		for iter.Scan(&id, &targetUrl, &secret, &events, &createdAt) {
			hasSecret := secret != ""
			webhooks = append(webhooks, map[string]any{
				"id":         id,
				"target_url": targetUrl,
				"has_secret": hasSecret,
				"events":     events,
				"created_at": createdAt,
			})
		}
		iter.Close()
		if webhooks == nil {
			webhooks = []map[string]any{}
		}
		jsonOK(w, webhooks)
		return
	}

	if r.Method == "POST" {
		var req struct {
			Bucket    string   `json:"bucket"`
			TargetURL string   `json:"target_url"`
			Secret    string   `json:"secret"`
			Events    []string `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, "invalid request", 400)
			return
		}
		if req.Bucket == "" {
			req.Bucket = bucket
		}
		if req.TargetURL == "" {
			jsonErr(w, "missing target_url", 400)
			return
		}
		if len(req.Events) == 0 {
			req.Events = []string{"object:created", "object:deleted"}
		}
		id := uuid.New().String()
		scyllaSession.Query(`INSERT INTO cosmos.bucket_webhooks (bucket, webhook_id, target_url, secret, events, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			req.Bucket, id, req.TargetURL, req.Secret, req.Events, time.Now()).Exec()

		jsonOK(w, map[string]any{"ok": true, "id": id})
		return
	}

	if r.Method == "DELETE" {
		id := r.URL.Query().Get("id")
		if id == "" {
			jsonErr(w, "missing webhook id", 400)
			return
		}
		scyllaSession.Query(`DELETE FROM cosmos.bucket_webhooks WHERE bucket = ? AND webhook_id = ?`, bucket, id).Exec()
		jsonOK(w, map[string]any{"ok": true})
		return
	}
	jsonErr(w, "method not allowed", 405)
}

func handleBucketWebhookTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonErr(w, "method not allowed", 405)
		return
	}
	_, _, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = defBucket
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		jsonErr(w, "missing id", 400)
		return
	}

	var targetUrl, secret string
	err := scyllaSession.Query(`SELECT target_url, secret FROM cosmos.bucket_webhooks WHERE bucket = ? AND webhook_id = ?`, bucket, id).Scan(&targetUrl, &secret)
	if err != nil {
		jsonErr(w, "webhook not found", 404)
		return
	}

	payload, _ := json.Marshal(map[string]any{
		"event":     "ping",
		"bucket":    bucket,
		"message":   "Cosmos Oort Webhook Test",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})

	req, _ := http.NewRequest("POST", targetUrl, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Cosmos-Oort-Webhook/1.0")
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(payload)
		req.Header.Set("X-Cosmos-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	client := &http.Client{Timeout: 5 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	duration := time.Since(start).Milliseconds()

	if err != nil {
		jsonOK(w, map[string]any{"ok": false, "error": err.Error(), "duration_ms": duration})
		return
	}
	defer resp.Body.Close()
	jsonOK(w, map[string]any{"ok": true, "status_code": resp.StatusCode, "duration_ms": duration})
}

func dispatchWebhooks(bucket, event, key string, size int64, contentType string) {
	if scyllaSession == nil {
		return
	}
	iter := scyllaSession.Query(`SELECT target_url, secret, events FROM cosmos.bucket_webhooks WHERE bucket = ?`, bucket).Iter()
	var targetUrl, secret string
	var events []string
	for iter.Scan(&targetUrl, &secret, &events) {
		matched := false
		for _, ev := range events {
			if ev == event || ev == "*" {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}

		go func(u, s string) {
			payload, err := json.Marshal(map[string]any{
				"event":        event,
				"bucket":       bucket,
				"key":          key,
				"size":         size,
				"content_type": contentType,
				"timestamp":    time.Now().UTC().Format(time.RFC3339),
			})
			if err != nil {
				return
			}
			req, err := http.NewRequest("POST", u, bytes.NewReader(payload))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "Cosmos-Oort-Webhook/1.0")
			if s != "" {
				mac := hmac.New(sha256.New, []byte(s))
				mac.Write(payload)
				req.Header.Set("X-Cosmos-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
			}
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Do(req)
			if err == nil && resp != nil {
				resp.Body.Close()
			}
		}(targetUrl, secret)
	}
	iter.Close()
}

// ─── STORAGE BREAKDOWN ────────────────────────────────────────────────────────

func handleStorageBreakdown(w http.ResponseWriter, r *http.Request) {
	ak, sk, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = defBucket
	}

	mc, err := minioClient(ak, sk)
	if err != nil {
		jsonErr(w, "storage client error", 500)
		return
	}

	type CatStat struct {
		Count int64 `json:"count"`
		Bytes int64 `json:"bytes"`
	}
	stats := map[string]*CatStat{
		"images":    {0, 0},
		"videos":    {0, 0},
		"audio":     {0, 0},
		"documents": {0, 0},
		"code":      {0, 0},
		"archives":  {0, 0},
		"others":    {0, 0},
	}
	var totalBytes, totalCount int64

	ctx := r.Context()
	for obj := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil || strings.HasSuffix(obj.Key, "/") {
			continue
		}
		ext := strings.ToLower(path.Ext(obj.Key))
		cat := "others"
		switch ext {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp", ".ico", ".tiff":
			cat = "images"
		case ".mp4", ".webm", ".mkv", ".mov", ".avi", ".flv":
			cat = "videos"
		case ".mp3", ".wav", ".ogg", ".flac", ".aac", ".m4a":
			cat = "audio"
		case ".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".txt", ".csv":
			cat = "documents"
		case ".js", ".ts", ".html", ".css", ".go", ".py", ".json", ".yml", ".yaml", ".sql", ".sh", ".c", ".cpp", ".rs", ".md":
			cat = "code"
		case ".zip", ".tar", ".gz", ".rar", ".7z", ".iso", ".bz2":
			cat = "archives"
		}
		stats[cat].Count++
		stats[cat].Bytes += obj.Size
		totalBytes += obj.Size
		totalCount++
	}

	jsonOK(w, map[string]any{
		"bucket":      bucket,
		"total_bytes": totalBytes,
		"total_count": totalCount,
		"categories":  stats,
	})
}
