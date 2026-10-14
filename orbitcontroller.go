package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// OrbitApp represents an application managed by Orbit.
type OrbitApp struct {
	AppID        string            `json:"app_id"`
	OwnerEmail   string            `json:"owner_email"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	RepoURL      string            `json:"repo_url"`
	Branch       string            `json:"branch"`
	BuildType    string            `json:"build_type"`
	Status       string            `json:"status"` // 'building', 'running', 'stopped', 'failed'
	ContainerID  string            `json:"container_id"`
	ImageTag     string            `json:"image_tag"`
	InternalPort int               `json:"internal_port"`
	ExposedPort  int               `json:"exposed_port"`
	CustomDomain string            `json:"custom_domain,omitempty"`
	EnvVars      map[string]string `json:"env_vars"`
	CreatedAt    int64             `json:"created_at"`
	UpdatedAt    int64             `json:"updated_at"`

	// Dynamic live fields (not in DB)
	LiveStats *ContainerLiveStats `json:"live_stats,omitempty"`
	ProxyURL  string              `json:"proxy_url,omitempty"`
}

// In-memory active build logs cache [app_id]*bytes.Buffer
var (
	orbitBuildLogs   sync.Map
	orbitActiveBuild sync.Map
)

// initOrbitDB initializes ScyllaDB schema for Orbit.
func initOrbitDB() {
	if scyllaSession == nil {
		log.Println("[ORBIT] ScyllaDB session is nil, skipping schema init")
		return
	}

	err := scyllaSession.Query(`CREATE TABLE IF NOT EXISTS cosmos.orbit_apps (
		app_id        text,
		owner_email   text,
		name          text,
		description   text,
		repo_url      text,
		branch        text,
		build_type    text,
		status        text,
		container_id  text,
		image_tag     text,
		internal_port int,
		exposed_port  int,
		custom_domain text,
		env_vars      map<text, text>,
		created_at    bigint,
		updated_at    bigint,
		PRIMARY KEY (app_id)
	)`).Exec()
	if err != nil {
		log.Printf("[ORBIT] Warning: failed to create orbit_apps table: %v", err)
	}

	// Add custom_domain column if it doesn't exist (migration for existing tables)
	_ = scyllaSession.Query(`ALTER TABLE cosmos.orbit_apps ADD custom_domain text`).Exec()

	_ = scyllaSession.Query(`CREATE INDEX IF NOT EXISTS orbit_owner_idx ON cosmos.orbit_apps (owner_email)`).Exec()

	err = scyllaSession.Query(`CREATE TABLE IF NOT EXISTS cosmos.orbit_deployments (
		deploy_id        text,
		app_id           text,
		commit_hash      text,
		commit_msg       text,
		status           text,
		build_duration_s int,
		build_log        text,
		created_at       bigint,
		PRIMARY KEY (deploy_id)
	)`).Exec()
	if err != nil {
		log.Printf("[ORBIT] Warning: failed to create orbit_deployments table: %v", err)
	}

	_ = scyllaSession.Query(`CREATE INDEX IF NOT EXISTS orbit_deploy_app_idx ON cosmos.orbit_deployments (app_id)`).Exec()

	log.Println("[ORBIT] ScyllaDB Orbit schema initialized")
}

// allocateOrbitPort finds an available port in the 4100-4999 range.
func allocateOrbitPort() (int, error) {
	for port := 4100; port <= 4999; port++ {
		// Check if port is already recorded in ScyllaDB
		var dummy string
		err := scyllaSession.Query(`SELECT app_id FROM cosmos.orbit_apps WHERE exposed_port = ? ALLOW FILTERING`, port).Scan(&dummy)
		if err == nil && dummy != "" {
			continue // Already in use
		}

		// Check if port can be bound locally
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		l, err := net.Listen("tcp", addr)
		if err == nil {
			_ = l.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available ports in range 4100-4999")
}

// getSessionUser extracts the email from the session cookie.
func getSessionUser(r *http.Request) (string, error) {
	var sid string
	if c, err := r.Cookie("cosmos_sid"); err == nil && c.Value != "" {
		sid = c.Value
	} else if c, err := r.Cookie("oort_session"); err == nil && c.Value != "" {
		sid = c.Value
	} else if c, err := r.Cookie("orbit_session"); err == nil && c.Value != "" {
		sid = c.Value
	}
	if sid == "" {
		return "", fmt.Errorf("unauthorized")
	}

	var email, name string
	err := scyllaSession.Query(
		`SELECT email, name FROM cosmos.users WHERE session_id = ? ALLOW FILTERING`,
		sid,
	).Scan(&email, &name)
	if err != nil || email == "" {
		return "", fmt.Errorf("invalid session")
	}
	return email, nil
}

// packageDirectoryToTar creates a tarball in memory of all files in a directory.
func packageDirectoryToTar(dirPath string) (*bytes.Buffer, error) {
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	defer tw.Close()

	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}

		relPath, err := filepath.Rel(dirPath, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}

		header, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relPath)

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			if _, err := io.Copy(tw, file); err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return buf, nil
}

// detectAndInjectDockerfile checks if a Dockerfile exists, or generates a zero-config buildpack.
func detectAndInjectDockerfile(dirPath string) (buildType string, internalPort int, err error) {
	dockerfilePath := filepath.Join(dirPath, "Dockerfile")
	if _, err := os.Stat(dockerfilePath); err == nil {
		// Read dockerfile to guess exposed port
		content, _ := os.ReadFile(dockerfilePath)
		lines := strings.Split(string(content), "\n")
		internalPort = 8080 // default
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(strings.ToUpper(l), "EXPOSE ") {
				var p int
				fmt.Sscanf(strings.TrimPrefix(strings.ToUpper(l), "EXPOSE "), "%d", &p)
				if p > 0 {
					internalPort = p
					break
				}
			}
		}
		return "dockerfile", internalPort, nil
	}

	// Helper: check if package.json has a "start" script
	hasNpmStart := func() bool {
		data, err := os.ReadFile(filepath.Join(dirPath, "package.json"))
		if err != nil {
			return false
		}
		var pkg map[string]interface{}
		if err := json.Unmarshal(data, &pkg); err != nil {
			return false
		}
		scripts, ok := pkg["scripts"].(map[string]interface{})
		if !ok {
			return false
		}
		_, hasStart := scripts["start"]
		return hasStart
	}

	// 1. Go
	if _, err := os.Stat(filepath.Join(dirPath, "go.mod")); err == nil {
		dockerfile := `FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 go build -o /app/server .
FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/server /app/server
EXPOSE 8080
CMD ["/app/server"]
`
		if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0644); err != nil {
			return "", 0, err
		}
		return "pack-go", 8080, nil
	}

	// 2. Python
	if _, err := os.Stat(filepath.Join(dirPath, "requirements.txt")); err == nil {
		// Check if there's a better entrypoint
		entry := "main.py"
		for _, f := range []string{"app.py", "wsgi.py", "run.py", "server.py", "main.py"} {
			if _, e := os.Stat(filepath.Join(dirPath, f)); e == nil {
				entry = f
				break
			}
		}
		dockerfile := fmt.Sprintf(`FROM python:3.11-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt || true
COPY . .
EXPOSE 8000
CMD ["python", "%s"]
`, entry)
		if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0644); err != nil {
			return "", 0, err
		}
		return "pack-python", 8000, nil
	}

	// 3. Node.js — only if package.json has a "start" script
	if _, errPkg := os.Stat(filepath.Join(dirPath, "package.json")); errPkg == nil && hasNpmStart() {
		dockerfile := `FROM node:20-alpine
WORKDIR /app
COPY package*.json ./
RUN npm install --omit=dev 2>/dev/null || npm install
COPY . .
EXPOSE 3000
CMD ["npm", "start"]
`
		if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0644); err != nil {
			return "", 0, err
		}
		return "pack-node", 3000, nil
	}

	// 4. Static HTML (index.html present, or package.json without start script)
	_, hasIndex := os.Stat(filepath.Join(dirPath, "index.html"))
	_, hasPkg := os.Stat(filepath.Join(dirPath, "package.json"))
	if hasIndex == nil || hasPkg == nil {
		// Use nginx to serve static files
		// Build static assets first if there's a build script
		buildCmd := ""
		if hasPkg == nil {
			data, _ := os.ReadFile(filepath.Join(dirPath, "package.json"))
			var pkg map[string]interface{}
			_ = json.Unmarshal(data, &pkg)
			if scripts, ok := pkg["scripts"].(map[string]interface{}); ok {
				if _, hasBuild := scripts["build"]; hasBuild {
					buildCmd = "RUN npm install && npm run build 2>/dev/null || true\n"
					buildCmd += "RUN test -d dist && cp -r dist/* /usr/share/nginx/html/ 2>/dev/null || test -d build && cp -r build/* /usr/share/nginx/html/ 2>/dev/null || true\n"
				}
			}
		}
		var dockerfile string
		if buildCmd != "" {
			// Multi-stage: build then serve with nginx
			dockerfile = `FROM node:22-alpine AS builder
WORKDIR /app
COPY package*.json ./
RUN npm install --legacy-peer-deps 2>/dev/null || npm install || true
COPY . .
RUN npm run build 2>/dev/null || true
FROM nginx:alpine
WORKDIR /usr/share/nginx/html
RUN rm -rf ./*
COPY --from=builder /app/dist ./  2>/dev/null || true
COPY --from=builder /app/build ./ 2>/dev/null || true
COPY --from=builder /app/public ./ 2>/dev/null || true
COPY --from=builder /app/out ./ 2>/dev/null || true
COPY --from=builder /app ./ 2>/dev/null || true
EXPOSE 80
`
		} else {
			dockerfile = `FROM nginx:alpine
COPY . /usr/share/nginx/html
EXPOSE 80
`
		}
		if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0644); err != nil {
			return "", 0, err
		}
		return "pack-static", 80, nil
	}

	return "unknown", 8080, fmt.Errorf("no Dockerfile, package.json (with start script), go.mod, requirements.txt, or index.html found in repository root")
}


// deployAppAsync handles cloning, building, and running an application container in background.
func deployAppAsync(appID string) {
	orbitActiveBuild.Store(appID, true)
	defer orbitActiveBuild.Delete(appID)

	var app OrbitApp
	err := scyllaSession.Query(`SELECT app_id, owner_email, name, repo_url, branch, exposed_port, env_vars FROM cosmos.orbit_apps WHERE app_id = ?`, appID).
		Scan(&app.AppID, &app.OwnerEmail, &app.Name, &app.RepoURL, &app.Branch, &app.ExposedPort, &app.EnvVars)
	if err != nil {
		log.Printf("[ORBIT] Error loading app %s for deployment: %v", appID, err)
		return
	}

	// Update status to 'building'
	_ = scyllaSession.Query(`UPDATE cosmos.orbit_apps SET status = 'building', updated_at = ? WHERE app_id = ?`,
		time.Now().Unix(), appID).Exec()

	logBuf := new(bytes.Buffer)
	orbitBuildLogs.Store(appID, logBuf)

	startTime := time.Now()
	fmt.Fprintf(logBuf, "[ORBIT] Deployment initiated for %s (%s) at %s\n", app.Name, appID, startTime.Format(time.RFC3339))
	fmt.Fprintf(logBuf, "[ORBIT] Target repository: %s (branch: %s)\n", app.RepoURL, app.Branch)

	scratchDir := filepath.Join("/home/debian/portals-go/scratch/orbit", appID)
	_ = os.RemoveAll(scratchDir)
	_ = os.MkdirAll(scratchDir, 0755)
	defer os.RemoveAll(scratchDir)

	// Step 1: Git clone
	branch := app.Branch
	if branch == "" {
		branch = "main"
	}
	fmt.Fprintf(logBuf, "[GIT] Cloning repository...\n")
	cmd := exec.Command("git", "clone", "--depth", "1", "-b", branch, app.RepoURL, scratchDir)
	cmdOut, err := cmd.CombinedOutput()
	if err != nil {
		// Retry without branch if default branch differs (e.g. master)
		cmd = exec.Command("git", "clone", "--depth", "1", app.RepoURL, scratchDir)
		cmdOut, err = cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(logBuf, "[GIT ERROR] Failed to clone: %s\n", string(cmdOut))
			_ = scyllaSession.Query(`UPDATE cosmos.orbit_apps SET status = 'failed', updated_at = ? WHERE app_id = ?`,
				time.Now().Unix(), appID).Exec()
			saveDeploymentRecord(appID, "failed", int(time.Since(startTime).Seconds()), logBuf.String())
			orbitBuildLogs.Delete(appID)
			return
		}
	}
	fmt.Fprintf(logBuf, "[GIT] Clone completed successfully.\n")

	// Step 2: Detect stack / Dockerfile
	buildType, intPort, err := detectAndInjectDockerfile(scratchDir)
	if err != nil {
		fmt.Fprintf(logBuf, "[BUILD ERROR] Stack detection failed: %v\n", err)
		_ = scyllaSession.Query(`UPDATE cosmos.orbit_apps SET status = 'failed', updated_at = ? WHERE app_id = ?`,
			time.Now().Unix(), appID).Exec()
		saveDeploymentRecord(appID, "failed", int(time.Since(startTime).Seconds()), logBuf.String())
		orbitBuildLogs.Delete(appID)
		return
	}
	fmt.Fprintf(logBuf, "[DETECT] Architecture resolved: %s (internal port %d)\n", buildType, intPort)

	// Step 3: Package build context to tar
	fmt.Fprintf(logBuf, "[TAR] Compacting build context...\n")
	tarBuf, err := packageDirectoryToTar(scratchDir)
	if err != nil {
		fmt.Fprintf(logBuf, "[TAR ERROR] Failed to package files: %v\n", err)
		_ = scyllaSession.Query(`UPDATE cosmos.orbit_apps SET status = 'failed', updated_at = ? WHERE app_id = ?`,
			time.Now().Unix(), appID).Exec()
		saveDeploymentRecord(appID, "failed", int(time.Since(startTime).Seconds()), logBuf.String())
		orbitBuildLogs.Delete(appID)
		return
	}

	// Step 4: Docker build
	imageTag := fmt.Sprintf("orbit-%s:latest", appID)
	fmt.Fprintf(logBuf, "[DOCKER] Building image %s...\n", imageTag)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := BuildDockerImage(ctx, tarBuf, imageTag, logBuf); err != nil {
		fmt.Fprintf(logBuf, "[DOCKER ERROR] Build failed: %v\n", err)
		_ = scyllaSession.Query(`UPDATE cosmos.orbit_apps SET status = 'failed', updated_at = ? WHERE app_id = ?`,
			time.Now().Unix(), appID).Exec()
		saveDeploymentRecord(appID, "failed", int(time.Since(startTime).Seconds()), logBuf.String())
		orbitBuildLogs.Delete(appID)
		return
	}
	fmt.Fprintf(logBuf, "[DOCKER] Image %s built successfully.\n", imageTag)

	// Step 5: Stop & remove old container if exists
	oldContainerName := "orbit-" + appID
	_ = RemoveDockerContainer(context.Background(), oldContainerName)

	// Step 6: Spawn and start new container
	fmt.Fprintf(logBuf, "[ORCHESTRATOR] Starting container on host port %d...\n", app.ExposedPort)
	var envList []string
	for k, v := range app.EnvVars {
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}
	// Inject default environment metadata
	envList = append(envList, fmt.Sprintf("PORT=%d", intPort))
	envList = append(envList, fmt.Sprintf("COSMOS_APP_ID=%s", appID))
	envList = append(envList, "NODE_ENV=production")

	spec := OrbitContainerSpec{
		Name:         app.Name,
		Image:        imageTag,
		Env:          envList,
		AppID:        appID,
		InternalPort: intPort,
		ExposedPort:  app.ExposedPort,
		MemLimitMB:   512,
		CPULimit:     1.0,
	}

	containerID, err := CreateAndStartOrbitContainer(context.Background(), spec)
	if err != nil {
		fmt.Fprintf(logBuf, "[ORCHESTRATOR ERROR] Failed to run container: %v\n", err)
		_ = scyllaSession.Query(`UPDATE cosmos.orbit_apps SET status = 'failed', updated_at = ? WHERE app_id = ?`,
			time.Now().Unix(), appID).Exec()
		saveDeploymentRecord(appID, "failed", int(time.Since(startTime).Seconds()), logBuf.String())
		orbitBuildLogs.Delete(appID)
		return
	}

	fmt.Fprintf(logBuf, "[ORCHESTRATOR] Container %s is active and healthy.\n", containerID[:12])
	fmt.Fprintf(logBuf, "[ORBIT] Deployment completed in %s. Container is now running.\n", time.Since(startTime).Round(time.Millisecond))
	fmt.Fprintf(logBuf, "[ORBIT] Access via proxy: /orbit/proxy/%s/\n", appID)

	// Step 7: Update DB to 'running'
	_ = scyllaSession.Query(`UPDATE cosmos.orbit_apps SET status = 'running', container_id = ?, image_tag = ?, internal_port = ?, build_type = ?, updated_at = ? WHERE app_id = ?`,
		containerID, imageTag, intPort, buildType, time.Now().Unix(), appID).Exec()

	saveDeploymentRecord(appID, "success", int(time.Since(startTime).Seconds()), logBuf.String())
	// Clear in-memory build log — live container logs will be served from Docker from now on
	time.Sleep(2 * time.Second) // let any pending log reads finish
	orbitBuildLogs.Delete(appID)
}

// saveDeploymentRecord stores build log in ScyllaDB.
func saveDeploymentRecord(appID, status string, durationSec int, logText string) {
	deployID := uuid.New().String()
	_ = scyllaSession.Query(`INSERT INTO cosmos.orbit_deployments (deploy_id, app_id, status, build_duration_s, build_log, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		deployID, appID, status, durationSec, logText, time.Now().Unix()).Exec()
}

// RegisterOrbitRoutes registers all Orbit API endpoints and the console UI router.
func RegisterOrbitRoutes(mux *http.ServeMux) {
	initOrbitDB()

	// Console UI View
	mux.HandleFunc("/orbit", handleOrbitConsole)
	mux.HandleFunc("/orbit/", handleOrbitSubRouter)

	log.Println("[ORBIT] Orbit routes registered")
}

// handleOrbitConsole serves the Orbit web dashboard.
func handleOrbitConsole(w http.ResponseWriter, r *http.Request) {
	email, err := getSessionUser(r)
	if err != nil {
		http.Redirect(w, r, "/oort/login?next=/orbit", http.StatusFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := map[string]interface{}{
		"UserEmail": email,
		"Year":      time.Now().Year(),
	}
	if err := tmpl.ExecuteTemplate(w, "orbitconsole.html", data); err != nil {
		log.Printf("[ORBIT UI ERROR] %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// handleOrbitSubRouter dispatches API endpoints and the dynamic reverse proxy.
func handleOrbitSubRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/orbit/")

	// Reverse Proxy for running apps: /orbit/proxy/:appId/...
	if strings.HasPrefix(path, "proxy/") {
		handleOrbitProxy(w, r, strings.TrimPrefix(path, "proxy/"))
		return
	}

	// API endpoints: /orbit/api/...
	if strings.HasPrefix(path, "api/") {
		apiPath := strings.TrimPrefix(path, "api/")
		switch {
		case apiPath == "apps" && r.Method == http.MethodGet:
			handleOrbitListApps(w, r)
		case apiPath == "apps" && r.Method == http.MethodPost:
			handleOrbitCreateApp(w, r)
		case strings.HasPrefix(apiPath, "apps/") && strings.HasSuffix(apiPath, "/action"):
			appID := strings.TrimSuffix(strings.TrimPrefix(apiPath, "apps/"), "/action")
			handleOrbitAppAction(w, r, appID)
		case strings.HasPrefix(apiPath, "apps/") && strings.HasSuffix(apiPath, "/logs"):
			appID := strings.TrimSuffix(strings.TrimPrefix(apiPath, "apps/"), "/logs")
			handleOrbitAppLogs(w, r, appID)
		case strings.HasPrefix(apiPath, "apps/") && strings.HasSuffix(apiPath, "/env"):
			appID := strings.TrimSuffix(strings.TrimPrefix(apiPath, "apps/"), "/env")
			handleOrbitAppEnv(w, r, appID)
		case strings.HasPrefix(apiPath, "apps/") && r.Method == http.MethodDelete:
			appID := strings.TrimPrefix(apiPath, "apps/")
			handleOrbitDeleteApp(w, r, appID)
		case strings.HasPrefix(apiPath, "apps/") && r.Method == http.MethodGet:
			appID := strings.TrimPrefix(apiPath, "apps/")
			handleOrbitGetApp(w, r, appID)
		case strings.HasPrefix(apiPath, "apps/") && strings.HasSuffix(apiPath, "/backup-to-oort") && r.Method == http.MethodPost:
			appID := strings.TrimSuffix(strings.TrimPrefix(apiPath, "apps/"), "/backup-to-oort")
			handleOrbitBackupToOort(w, r, appID)
		case strings.HasPrefix(apiPath, "apps/") && strings.HasSuffix(apiPath, "/domain"):
			appID := strings.TrimSuffix(strings.TrimPrefix(apiPath, "apps/"), "/domain")
			handleOrbitDomainUpdate(w, r, appID)
		case apiPath == "webhook/forge":
			handleOrbitForgeWebhook(w, r)
		case apiPath == "forge/repos":
			handleOrbitListForgeRepos(w, r)
		case apiPath == "templates":
			handleOrbitListTemplates(w, r)
		default:
			http.NotFound(w, r)
		}
		return
	}

	http.NotFound(w, r)
}

// handleOrbitListApps returns all apps owned by the current user.
func handleOrbitListApps(w http.ResponseWriter, r *http.Request) {
	email, err := getSessionUser(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	iter2 := scyllaSession.Query(`SELECT app_id, owner_email, name, description, repo_url, branch, build_type, status, container_id, image_tag, internal_port, exposed_port, custom_domain, env_vars, created_at, updated_at FROM cosmos.orbit_apps WHERE owner_email = ? ALLOW FILTERING`, email).Iter()

	var apps []OrbitApp
	var app OrbitApp
	for iter2.Scan(&app.AppID, &app.OwnerEmail, &app.Name, &app.Description, &app.RepoURL, &app.Branch, &app.BuildType, &app.Status, &app.ContainerID, &app.ImageTag, &app.InternalPort, &app.ExposedPort, &app.CustomDomain, &app.EnvVars, &app.CreatedAt, &app.UpdatedAt) {
		app.ProxyURL = fmt.Sprintf("/orbit/proxy/%s/", app.AppID)

		// Fetch live stats if container is running
		if app.Status == "running" && app.ContainerID != "" {
			stats, _ := GetContainerStats(r.Context(), app.ContainerID)
			app.LiveStats = stats
		}

		apps = append(apps, app)
	}

	if apps == nil {
		apps = []OrbitApp{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"apps":        apps,
		"total_apps":  len(apps),
		"port_quota":  10,
		"ports_used":  len(apps),
		"ports_free":  10 - len(apps),
	})
}

// handleOrbitCreateApp creates and begins deployment of a new application.
func handleOrbitCreateApp(w http.ResponseWriter, r *http.Request) {
	email, err := getSessionUser(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		Name        string            `json:"name"`
		Description string            `json:"description"`
		RepoURL     string            `json:"repo_url"`
		Branch      string            `json:"branch"`
		EnvVars     map[string]string `json:"env_vars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request payload"}`, http.StatusBadRequest)
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.RepoURL = strings.TrimSpace(req.RepoURL)
	if req.Name == "" || req.RepoURL == "" {
		http.Error(w, `{"error":"name and repository URL are required"}`, http.StatusBadRequest)
		return
	}

	if req.Branch == "" {
		req.Branch = "main"
	}
	if req.EnvVars == nil {
		req.EnvVars = make(map[string]string)
	}

	// Enforce 10-app limit per user
	var appCount int
	iter := scyllaSession.Query(`SELECT COUNT(app_id) FROM cosmos.orbit_apps WHERE owner_email = ? ALLOW FILTERING`, email).Iter()
	iter.Scan(&appCount)
	_ = iter.Close()
	if appCount >= 10 {
		http.Error(w, `{"error":"port_limit_exceeded","message":"You have reached the maximum of 10 deployed applications. Delete an existing app to deploy a new one."}`, http.StatusForbidden)
		return
	}

	// Allocate free host port
	port, err := allocateOrbitPort()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"port allocation failed: %v"}`, err), http.StatusInternalServerError)
		return
	}

	appID := strings.ToLower(uuid.New().String()[:8])
	now := time.Now().Unix()

	err = scyllaSession.Query(`INSERT INTO cosmos.orbit_apps (app_id, owner_email, name, description, repo_url, branch, build_type, status, container_id, image_tag, internal_port, exposed_port, custom_domain, env_vars, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		appID, email, req.Name, req.Description, req.RepoURL, req.Branch, "detecting", "building", "", "", 0, port, "", req.EnvVars, now, now).Exec()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to create app record: %v"}`, err), http.StatusInternalServerError)
		return
	}

	// Trigger deployment in background goroutine
	go deployAppAsync(appID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"app_id":    appID,
		"proxy_url": fmt.Sprintf("/orbit/proxy/%s/", appID),
		"message":   "Application created. Build started.",
	})
}

// handleOrbitGetApp retrieves app details and live container telemetry.
func handleOrbitGetApp(w http.ResponseWriter, r *http.Request, appID string) {
	email, err := getSessionUser(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var app OrbitApp
	err = scyllaSession.Query(`SELECT app_id, owner_email, name, description, repo_url, branch, build_type, status, container_id, image_tag, internal_port, exposed_port, custom_domain, env_vars, created_at, updated_at FROM cosmos.orbit_apps WHERE app_id = ?`, appID).
		Scan(&app.AppID, &app.OwnerEmail, &app.Name, &app.Description, &app.RepoURL, &app.Branch, &app.BuildType, &app.Status, &app.ContainerID, &app.ImageTag, &app.InternalPort, &app.ExposedPort, &app.CustomDomain, &app.EnvVars, &app.CreatedAt, &app.UpdatedAt)
	if err != nil || app.AppID == "" || app.OwnerEmail != email {
		http.Error(w, `{"error":"app not found"}`, http.StatusNotFound)
		return
	}

	app.ProxyURL = fmt.Sprintf("/orbit/proxy/%s/", app.AppID)
	if app.Status == "running" && app.ContainerID != "" {
		app.LiveStats, _ = GetContainerStats(r.Context(), app.ContainerID)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(app)
}

// handleOrbitAppAction executes lifecycle actions (start, stop, restart, rebuild).
func handleOrbitAppAction(w http.ResponseWriter, r *http.Request, appID string) {
	email, err := getSessionUser(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var app OrbitApp
	err = scyllaSession.Query(`SELECT app_id, owner_email, name, status, container_id FROM cosmos.orbit_apps WHERE app_id = ?`, appID).
		Scan(&app.AppID, &app.OwnerEmail, &app.Name, &app.Status, &app.ContainerID)
	if err != nil || app.AppID == "" || app.OwnerEmail != email {
		http.Error(w, `{"error":"app not found"}`, http.StatusNotFound)
		return
	}

	var req struct {
		Action string `json:"action"` // "start", "stop", "restart", "rebuild"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	switch req.Action {
	case "start":
		if app.ContainerID != "" {
			if err := StartDockerContainer(r.Context(), app.ContainerID); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":"failed to start: %v"}`, err), http.StatusInternalServerError)
				return
			}
			_ = scyllaSession.Query(`UPDATE cosmos.orbit_apps SET status = 'running', updated_at = ? WHERE app_id = ?`,
				time.Now().Unix(), appID).Exec()
		}
	case "stop":
		if app.ContainerID != "" {
			if err := StopDockerContainer(r.Context(), app.ContainerID); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":"failed to stop: %v"}`, err), http.StatusInternalServerError)
				return
			}
			_ = scyllaSession.Query(`UPDATE cosmos.orbit_apps SET status = 'stopped', updated_at = ? WHERE app_id = ?`,
				time.Now().Unix(), appID).Exec()
		}
	case "restart":
		if app.ContainerID != "" {
			if err := RestartDockerContainer(r.Context(), app.ContainerID); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":"failed to restart: %v"}`, err), http.StatusInternalServerError)
				return
			}
			_ = scyllaSession.Query(`UPDATE cosmos.orbit_apps SET status = 'running', updated_at = ? WHERE app_id = ?`,
				time.Now().Unix(), appID).Exec()
		}
	case "rebuild":
		go deployAppAsync(appID)
	default:
		http.Error(w, `{"error":"unknown action"}`, http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"action":  req.Action,
	})
}

// handleOrbitAppLogs returns current build log or live container stdout/stderr.
func handleOrbitAppLogs(w http.ResponseWriter, r *http.Request, appID string) {
	_, err := getSessionUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var app OrbitApp
	err = scyllaSession.Query(`SELECT app_id, status, container_id FROM cosmos.orbit_apps WHERE app_id = ?`, appID).
		Scan(&app.AppID, &app.Status, &app.ContainerID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Set headers so UI can poll-detect completion
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-App-Status", app.Status)

	// 1. If currently building: serve live in-memory build log
	if val, ok := orbitBuildLogs.Load(appID); ok {
		buf := val.(*bytes.Buffer)
		logText := buf.String()
		w.Header().Set("X-App-Status", "building")
		w.Write([]byte(logText))
		return
	}

	// 2. If running: stream live container stdout/stderr
	if app.Status == "running" && app.ContainerID != "" {
		// Try container logs first
		err2 := StreamDockerLogs(r.Context(), app.ContainerID, "500", false, w)
		if err2 == nil {
			return
		}
		// Fallback: last deployment log
	}

	// 3. For stopped/failed or if container stream failed: return stored build log
	var latestLog string
	iter := scyllaSession.Query(
		`SELECT build_log, created_at FROM cosmos.orbit_deployments WHERE app_id = ? ALLOW FILTERING`,
		appID,
	).Iter()
	var buildLog string
	var createdAt int64
	var newestAt int64
	for iter.Scan(&buildLog, &createdAt) {
		if createdAt > newestAt {
			newestAt = createdAt
			latestLog = buildLog
		}
	}
	_ = iter.Close()

	if latestLog != "" {
		w.Write([]byte(latestLog))
	} else {
		w.Write([]byte("[ORBIT] No logs recorded yet.\n[ORBIT] Deploy an application to see build and runtime logs here."))
	}
}

// handleOrbitAppEnv updates environment variables for an application.
func handleOrbitAppEnv(w http.ResponseWriter, r *http.Request, appID string) {
	email, err := getSessionUser(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var app OrbitApp
	err = scyllaSession.Query(`SELECT app_id, owner_email FROM cosmos.orbit_apps WHERE app_id = ?`, appID).
		Scan(&app.AppID, &app.OwnerEmail)
	if err != nil || app.AppID == "" || app.OwnerEmail != email {
		http.Error(w, `{"error":"app not found"}`, http.StatusNotFound)
		return
	}

	var req struct {
		EnvVars map[string]string `json:"env_vars"`
		Redeploy bool              `json:"redeploy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	err = scyllaSession.Query(`UPDATE cosmos.orbit_apps SET env_vars = ?, updated_at = ? WHERE app_id = ?`,
		req.EnvVars, time.Now().Unix(), appID).Exec()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to update env vars: %v"}`, err), http.StatusInternalServerError)
		return
	}

	if req.Redeploy {
		go deployAppAsync(appID)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"redeploy": req.Redeploy,
	})
}

// handleOrbitDeleteApp tears down container and removes app record.
func handleOrbitDeleteApp(w http.ResponseWriter, r *http.Request, appID string) {
	email, err := getSessionUser(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var app OrbitApp
	err = scyllaSession.Query(`SELECT app_id, owner_email, container_id FROM cosmos.orbit_apps WHERE app_id = ?`, appID).
		Scan(&app.AppID, &app.OwnerEmail, &app.ContainerID)
	if err != nil || app.AppID == "" || app.OwnerEmail != email {
		http.Error(w, `{"error":"app not found"}`, http.StatusNotFound)
		return
	}

	// Remove docker container
	containerName := "orbit-" + appID
	_ = RemoveDockerContainer(r.Context(), containerName)
	if app.ContainerID != "" {
		_ = RemoveDockerContainer(r.Context(), app.ContainerID)
	}

	// Delete from DB
	_ = scyllaSession.Query(`DELETE FROM cosmos.orbit_apps WHERE app_id = ?`, appID).Exec()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Application deleted",
	})
}

// handleOrbitListForgeRepos fetches repositories from local Gitea instance for 1-click deploy.
func handleOrbitListForgeRepos(w http.ResponseWriter, r *http.Request) {
	email, err := getSessionUser(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var giteaUser string
	_ = scyllaSession.Query(`SELECT username FROM cosmos.users WHERE email = ? ALLOW FILTERING`, email).
		Scan(&giteaUser)

	var repos []map[string]interface{}
	if giteaUser != "" {
		// Call Gitea API locally
		giteaURL := fmt.Sprintf("http://127.0.0.1:3000/api/v1/users/%s/repos", url.PathEscape(giteaUser))
		resp, err := http.Get(giteaURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
			_ = json.NewDecoder(resp.Body).Decode(&repos)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"username": giteaUser,
		"repos":    repos,
	})
}

// handleOrbitListTemplates returns starter quick-launch templates.
func handleOrbitListTemplates(w http.ResponseWriter, r *http.Request) {
	templates := []map[string]interface{}{
		{
			"id":          "node-starter",
			"name":        "Node.js & Express API",
			"description": "High-performance Node.js REST server with JSON routing",
			"icon":        "nodejs",
			"repo_url":    "https://github.com/expressjs/express.git",
			"branch":      "master",
		},
		{
			"id":          "go-server",
			"name":        "Go Lightweight Web Server",
			"description": "Ultra-fast compiled HTTP server in under 15MB container",
			"icon":        "go",
			"repo_url":    "https://github.com/gin-gonic/examples.git",
			"branch":      "master",
		},
		{
			"id":          "python-fastapi",
			"name":        "Python FastAPI",
			"description": "Modern asynchronous web framework with automatic OpenAPI docs",
			"icon":        "python",
			"repo_url":    "https://github.com/tiangolo/fastapi.git",
			"branch":      "master",
		},
		{
			"id":          "static-landing",
			"name":        "Static HTML5 / CSS3 Portal",
			"description": "Nginx Alpine container hosting static documentation or landing pages",
			"icon":        "html",
			"repo_url":    "https://github.com/h5bp/html5-boilerplate.git",
			"branch":      "main",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"templates": templates,
	})
}

// handleOrbitProxy is a transparent reverse proxy routing /orbit/proxy/:appId/* to the container's allocated port.
func handleOrbitProxy(w http.ResponseWriter, r *http.Request, path string) {
	// Format of path: <appId> or <appId>/<subpath...>
	parts := strings.SplitN(path, "/", 2)
	appID := parts[0]
	subPath := ""
	if len(parts) > 1 {
		subPath = parts[1]
	}

	var port int
	var status string
	err := scyllaSession.Query(`SELECT exposed_port, status FROM cosmos.orbit_apps WHERE app_id = ?`, appID).
		Scan(&port, &status)
	if err != nil || port <= 0 {
		http.Error(w, "Orbit application not found", http.StatusNotFound)
		return
	}

	if status != "running" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html><html><body style="background:#090d16;color:#e2eaf2;font-family:sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;"><div style="text-align:center;"><h2 style="font-size:24px;">Application is %s</h2><p style="color:#64748b;">Please start the container from the Orbit console to access it.</p><a href="/orbit" style="display:inline-block;margin-top:16px;color:#38bdf8;text-decoration:none;padding:8px 16px;border:1px solid rgba(56,189,248,0.3);border-radius:8px;">Open Orbit Console</a></div></body></html>`, status)
		return
	}

	targetURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// Rewrite request URL to point to subpath
	r.URL.Path = "/" + subPath
	r.Header.Set("X-Forwarded-Host", r.Host)
	r.Header.Set("X-Forwarded-Prefix", fmt.Sprintf("/orbit/proxy/%s", appID))
	r.Header.Set("X-Real-IP", r.RemoteAddr)

	proxy.ServeHTTP(w, r)
}

// handleOrbitBackupToOort exports container filesystem snapshot and uploads directly into the user's Oort S3 bucket.
func handleOrbitBackupToOort(w http.ResponseWriter, r *http.Request, appID string) {
	var sid string
	if c, err := r.Cookie("cosmos_sid"); err == nil && c.Value != "" {
		sid = c.Value
	} else if c, err := r.Cookie("oort_session"); err == nil && c.Value != "" {
		sid = c.Value
	} else if c, err := r.Cookie("orbit_session"); err == nil && c.Value != "" {
		sid = c.Value
	}
	if sid == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	_, ak, sk, defaultBucket, err := lookupBySession("", sid)
	if err != nil || ak == "" {
		http.Error(w, `{"error":"failed to authenticate S3 storage credentials"}`, http.StatusUnauthorized)
		return
	}

	var app OrbitApp
	err = scyllaSession.Query(`SELECT app_id, name, container_id FROM cosmos.orbit_apps WHERE app_id = ?`, appID).
		Scan(&app.AppID, &app.Name, &app.ContainerID)
	if err != nil || app.AppID == "" {
		http.Error(w, `{"error":"app not found"}`, http.StatusNotFound)
		return
	}

	if app.ContainerID == "" {
		http.Error(w, `{"error":"container is not running"}`, http.StatusBadRequest)
		return
	}

	// Call Docker API to export container filesystem tar
	client := DockerClient()
	exportURL := fmt.Sprintf("http://localhost/v1.45/containers/%s/export", url.PathEscape(app.ContainerID))
	req, err := http.NewRequestWithContext(r.Context(), "GET", exportURL, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to create export request: %v"}`, err), http.StatusInternalServerError)
		return
	}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		http.Error(w, `{"error":"failed to export container filesystem from Docker"}`, http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	mc, err := minioClient(ak, sk)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to initialize MinIO client: %v"}`, err), http.StatusInternalServerError)
		return
	}

	// Upload to Oort bucket
	objKey := fmt.Sprintf("orbit-backups/%s-%d.tar", app.AppID, time.Now().Unix())
	tempFile, err := os.CreateTemp("", "orbit-backup-*.tar")
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"temp file creation failed: %v"}`, err), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	written, err := io.Copy(tempFile, resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to write backup archive: %v"}`, err), http.StatusInternalServerError)
		return
	}
	tempFile.Seek(0, 0)

	_, err = mc.PutObject(r.Context(), defaultBucket, objKey, tempFile, written, minio.PutObjectOptions{
		ContentType: "application/x-tar",
	})
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to upload backup to Oort S3: %v"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"bucket":     defaultBucket,
		"object_key": objKey,
		"size_bytes": written,
		"message":    fmt.Sprintf("Backup saved to Oort bucket '%s' as '%s'", defaultBucket, objKey),
	})
}

// handleOrbitForgeWebhook processes push event webhooks from Forge (Gitea) to auto-redeploy.
func handleOrbitForgeWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		Ref        string `json:"ref"`
		Repository struct {
			CloneURL string `json:"clone_url"`
			HTMLURL  string `json:"html_url"`
			SSHURL   string `json:"ssh_url"`
		} `json:"repository"`
	}

	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid webhook payload", http.StatusBadRequest)
		return
	}

	branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
	cloneURL := payload.Repository.CloneURL
	if cloneURL == "" {
		cloneURL = payload.Repository.HTMLURL
	}

	iter := scyllaSession.Query(`SELECT app_id, repo_url, branch FROM cosmos.orbit_apps`).Iter()
	var appID, repoURL, appBranch string
	var triggered []string

	for iter.Scan(&appID, &repoURL, &appBranch) {
		if (strings.EqualFold(repoURL, cloneURL) || strings.EqualFold(repoURL, payload.Repository.HTMLURL)) &&
			(appBranch == "" || appBranch == branch) {
			go deployAppAsync(appID)
			triggered = append(triggered, appID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"triggered": triggered,
		"count":     len(triggered),
	})
}

// handleOrbitDomainUpdate sets or removes a custom domain for an app.
// POST /orbit/api/apps/:id/domain  body: {"domain":"example.com"} or {"domain":""}
func handleOrbitDomainUpdate(w http.ResponseWriter, r *http.Request, appID string) {
	email, err := getSessionUser(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		Domain string `json:"domain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request payload"}`, http.StatusBadRequest)
		return
	}

	// Verify app ownership
	var ownerEmail string
	if err := scyllaSession.Query(`SELECT owner_email FROM cosmos.orbit_apps WHERE app_id = ?`, appID).Scan(&ownerEmail); err != nil || ownerEmail != email {
		http.Error(w, `{"error":"app not found"}`, http.StatusNotFound)
		return
	}

	domain := strings.TrimSpace(strings.ToLower(req.Domain))

	// If setting a non-empty domain, check for collisions across all apps
	if domain != "" {
		var existingID string
		if err2 := scyllaSession.Query(`SELECT app_id FROM cosmos.orbit_apps WHERE custom_domain = ? ALLOW FILTERING`, domain).Scan(&existingID); err2 == nil && existingID != "" && existingID != appID {
			http.Error(w, `{"error":"domain_conflict","message":"This domain is already mapped to another application."}`, http.StatusConflict)
			return
		}
	}

	_ = scyllaSession.Query(`UPDATE cosmos.orbit_apps SET custom_domain = ?, updated_at = ? WHERE app_id = ?`,
		domain, time.Now().Unix(), appID).Exec()

	w.Header().Set("Content-Type", "application/json")
	if domain == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "Custom domain removed."})
	} else {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"domain":  domain,
			"message": fmt.Sprintf("Domain '%s' mapped. Point a CNAME record to this server's hostname to activate it.", domain),
			"cname_instructions": map[string]string{
				"type":   "CNAME",
				"name":   domain,
				"value":  r.Host,
				"ttl":    "300",
				"note":   "Also set an A record if CNAME is not supported at apex. SSL is handled automatically once DNS propagates.",
			},
		})
	}
}

// handleOrbitDomainRouteProxy handles incoming requests on custom domains by looking up the app and proxying.
// This is called from main.go when a Host: header doesn't match known subpaths.
func handleOrbitDomainRouteProxy(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	// Strip port if present
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSpace(host))

	var appID string
	var portNum int
	var status string
	if err := scyllaSession.Query(`SELECT app_id, exposed_port, status FROM cosmos.orbit_apps WHERE custom_domain = ? ALLOW FILTERING`, host).Scan(&appID, &portNum, &status); err != nil || appID == "" {
		http.Error(w, "Domain not mapped to any Orbit application", http.StatusNotFound)
		return
	}

	if status != "running" || portNum <= 0 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html><html><body style="background:#090d16;color:#e2eaf2;font-family:sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;"><div style="text-align:center;"><h2>Application is %s</h2><p style="color:#64748b;">Start the container from the Orbit console to access it.</p><a href="/orbit" style="display:inline-block;margin-top:16px;color:#38bdf8;text-decoration:none;padding:8px 16px;border:1px solid rgba(56,189,248,0.3);border-radius:8px;">Open Orbit Console</a></div></body></html>`, status)
		return
	}

	targetURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", portNum))
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	r.Header.Set("X-Forwarded-Host", host)
	r.Header.Set("X-Real-IP", r.RemoteAddr)
	proxy.ServeHTTP(w, r)
}
