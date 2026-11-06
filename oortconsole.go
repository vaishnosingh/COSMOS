package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/base64"
	"github.com/google/uuid"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"sync"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"golang.org/x/crypto/bcrypt"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const minioEndpoint = "127.0.0.1:9010"
const publicMinioEndpoint = "cosmos.linuxguy.space"

// authS3Console validates the session and returns (accessKey, secretKey, bucket).
// It looks up the session from the cosmos.users table.
func authS3Console(r *http.Request) (ak, sk, bucket string, ok bool) {
	sid := r.URL.Query().Get("sid")
	if sid == "" {
		sid = r.Header.Get("X-Session-Id")
	}
	if sid == "" {
		cookie, err := r.Cookie("cosmos_sid")
		if err == nil {
			sid = cookie.Value
		}
	}
	if sid == "" {
		cookie, err := r.Cookie("oort_session")
		if err == nil {
			sid = cookie.Value
		}
	}
	if sid == "" {
		cookie, err := r.Cookie("orbit_session")
		if err == nil {
			sid = cookie.Value
		}
	}
	if sid == "" {
		return "", "", "", false
	}
	_, ak, sk, bucket, err := lookupBySession("", sid)
	if err != nil || ak == "" {
		return "", "", "", false
	}
	return ak, sk, bucket, true
}

func minioClient(ak, sk string) (*minio.Client, error) {
	return minio.New(minioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(ak, sk, ""),
		Secure: false,
	})
}

// ─── API Response helpers ─────────────────────────────────────────────────────

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ─── Route dispatcher ─────────────────────────────────────────────────────────

func RegisterS3ConsoleRoutes(mux *http.ServeMux) {
	// Ensure required tables exist
	scyllaSession.Query(`CREATE TABLE IF NOT EXISTS cosmos.quota_requests (
		req_token text PRIMARY KEY,
		email text,
		amount int
	)`).Exec()

	scyllaSession.Query(`CREATE TABLE IF NOT EXISTS cosmos.bucket_domains (
		domain text PRIMARY KEY,
		bucket_name text,
		owner_email text
	)`).Exec()

	RegisterExpansionRoutes(mux)

	mux.HandleFunc("/oort/api/objects/versions", handleS3ObjectVersions)
	mux.HandleFunc("/oort/api/objects/restore", handleS3ObjectRestore)
	mux.HandleFunc("/oort/api/objects/share", handleS3ShareObject)
	mux.HandleFunc("/oort/console", handleS3ConsolePage)
	mux.HandleFunc("/oort/api/buckets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" { handleS3CreateBucket(w, r); return }
		if r.Method == "DELETE" { handleS3DeleteBucket(w, r); return }
		handleS3Buckets(w, r)
	})
	mux.HandleFunc("/oort/api/bucket/settings", handleS3BucketSettings)
	mux.HandleFunc("/oort/api/analytics", handleS3Analytics)
	mux.HandleFunc("/oort/api/stats", handleS3Stats)
	mux.HandleFunc("/oort/api/objects", handleS3Objects)
	mux.HandleFunc("/oort/api/upload/init", handleS3UploadInit)
	mux.HandleFunc("/oort/api/upload/part", handleS3UploadPart)
	mux.HandleFunc("/oort/api/upload/complete", handleS3UploadComplete)
	mux.HandleFunc("/oort/api/upload/abort", handleS3UploadAbort)
	mux.HandleFunc("/oort/api/delete", handleS3Delete)
	mux.HandleFunc("/oort/api/bucket/empty", handleS3EmptyBucket)
	mux.HandleFunc("/oort/api/cdn_proxy/", handleS3CDNProxy)
	mux.HandleFunc("/oort/api/bucket/domains", handleS3BucketDomains)
	mux.HandleFunc("/oort/api/bucket/domain/add", handleS3BucketDomainAdd)
	mux.HandleFunc("/oort/api/bucket/domain/remove", handleS3BucketDomainRemove)
	mux.HandleFunc("/oort/api/presign", handleS3Presign)
	mux.HandleFunc("/oort/api/mkdir", handleS3Mkdir)
	mux.HandleFunc("/oort/api/profile", handleS3Profile)
	mux.HandleFunc("/oort/api/profile/image", handleS3ProfileImage)
	mux.HandleFunc("/oort/api/profile/update", handleS3ProfileUpdate)
	mux.HandleFunc("/oort/api/quota", handleS3Quota)
	mux.HandleFunc("/oort/api/quota/approve", handleS3ApproveQuota)
	mux.HandleFunc("/oort/api/password", handleS3Password)
	mux.HandleFunc("/oort/api/objects/move", handleS3Move)
	mux.HandleFunc("/oort/api/buckets/rename", handleS3RenameBucket)
	mux.HandleFunc("/oort/api/nas/info", handleMornyeNASInfo)
	mux.HandleFunc("/oort/api/nas/enable-streaming", handleMornyeNASEnableStreaming)
}

// ─── Console HTML page ────────────────────────────────────────────────────────

func handleS3ConsolePage(w http.ResponseWriter, r *http.Request) {
	_, _, _, ok := authS3Console(r)
	if !ok {
		http.Redirect(w, r, "/oort/login", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net https://static.cloudflareinsights.com; img-src 'self' data: blob:;")

	sid := r.URL.Query().Get("sid")
	if sid == "" {
		if c, err := r.Cookie("cosmos_sid"); err == nil {
			sid = c.Value
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "cosmos_sid",
		Value:    sid,
		Path:     "/oort",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400 * 7,
	})
		var username string
	scyllaSession.Query(`SELECT username FROM cosmos.users WHERE session_id=? ALLOW FILTERING`, sid).Scan(&username)
	tmpl.ExecuteTemplate(w, "oortconsole.html", map[string]string{"SID": sid, "Username": username})
}

// ─── GET /oort/api/stats ────────────────────────────────────────────────────────

func handleS3Stats(w http.ResponseWriter, r *http.Request) {
	ak, sk, defBucket, ok := authS3Console(r)
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" { bucket = defBucket }
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	mc, err := minio.New(minioEndpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: false})
	if err != nil {
		jsonErr(w, "minio error", 500)
		return
	}
	ctx := context.Background()
	var totalSize int64
	var objectCount int64
	for obj := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			continue
		}
		totalSize += obj.Size
		objectCount++
	}
	exists, _ := mc.BucketExists(ctx, bucket)

	// Fetch real quota from MinIO via mc CLI
	var quotaBytes int64 = 10 * 1024 * 1024 * 1024 // default 10GB
	mcOut, mcErr := exec.Command("mc", "quota", "info", "cosmos/"+bucket, "--json").Output()
	if mcErr == nil {
		var qr struct {
			Quota int64  `json:"quota"`
			Type  string `json:"type"`
		}
		if json.Unmarshal(mcOut, &qr) == nil && qr.Quota > 0 {
			quotaBytes = qr.Quota
		}
	}

	jsonOK(w, map[string]any{
		"bucket":       bucket,
		"object_count": objectCount,
		"total_size":   totalSize,
		"exists":       exists,
		"quota":        quotaBytes,
	})
}

// ─── GET /oort/api/objects?prefix=xxx ──────────────────────────────────────────

type S3Object struct {
	Key          string    `json:"key"`
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
	IsDir        bool      `json:"is_dir"`
	ContentType  string    `json:"content_type"`
	ETag         string    `json:"etag"`
}

func handleS3Objects(w http.ResponseWriter, r *http.Request) {
	ak, sk, defBucket, ok := authS3Console(r)
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" { bucket = defBucket }
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	prefix := r.URL.Query().Get("prefix")
	mc, err := minio.New(minioEndpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: false})
	if err != nil {
		jsonErr(w, "minio error", 500)
		return
	}
	recursive := r.URL.Query().Get("recursive") == "true"
	ctx := context.Background()
	var objects []S3Object
	for obj := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: recursive,
	}) {
		if obj.Err != nil {
			log.Printf("[S3] list error: %v", obj.Err)
			continue
		}
		
		// Skip the exact directory object of the current prefix
		if prefix != "" && obj.Key == prefix {
			continue
		}
		
		isDir := strings.HasSuffix(obj.Key, "/")
		name := obj.Key
		if prefix != "" {
			name = strings.TrimPrefix(name, prefix)
		}
		name = strings.TrimSuffix(name, "/")
		objects = append(objects, S3Object{
			Key:          obj.Key,
			Name:         name,
			Size:         obj.Size,
			LastModified: obj.LastModified,
			IsDir:        isDir,
			ETag:         strings.Trim(obj.ETag, "\""),
		})
	}
	if objects == nil {
		objects = []S3Object{}
	}
	jsonOK(w, map[string]any{
		"prefix":  prefix,
		"bucket":  bucket,
		"objects": objects,
	})
}

// ─── POST /oort/api/upload/init — initiate multipart ───────────────────────────

func handleS3UploadInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonErr(w, "method not allowed", 405)
		return
	}
	ak, sk, defBucket, ok := authS3Console(r)
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" { bucket = defBucket }
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	var req struct {
		Key         string `json:"key"`
		ContentType string `json:"content_type"`
		Bucket      string `json:"bucket"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		jsonErr(w, "bad request", 400)
		return
	}
	if req.Bucket != "" { bucket = req.Bucket }
	// Sanitise key
	req.Key = path.Clean(req.Key)
	req.Key = strings.TrimPrefix(req.Key, "/")
	if req.ContentType == "" {
		req.ContentType = "application/octet-stream"
	}

	mc, err := minio.New(minioEndpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: false})
	if err != nil {
		jsonErr(w, "minio error", 500)
		return
	}

	core := minio.Core{Client: mc}
	uploadID, err := core.NewMultipartUpload(context.Background(), bucket, req.Key, minio.PutObjectOptions{
		ContentType: req.ContentType,
	})
	if err != nil {
		log.Printf("[S3] init multipart: %v", err)
		jsonErr(w, "failed to init upload", 500)
		return
	}
	jsonOK(w, map[string]string{
		"upload_id": uploadID,
		"key":       req.Key,
		"bucket":    bucket,
	})
}

// ─── POST /oort/api/upload/part — upload one chunk ─────────────────────────────

func handleS3UploadPart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonErr(w, "method not allowed", 405)
		return
	}
	ak, sk, defBucket, ok := authS3Console(r)
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" { bucket = defBucket }
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	key := r.URL.Query().Get("key")
	uploadID := r.URL.Query().Get("upload_id")
	partNumStr := r.URL.Query().Get("part")
	sizeStr := r.URL.Query().Get("size")
	if key == "" || uploadID == "" || partNumStr == "" {
		jsonErr(w, "missing params", 400)
		return
	}
	partNum, _ := strconv.Atoi(partNumStr)
	size, _ := strconv.ParseInt(sizeStr, 10, 64)
	if partNum < 1 || partNum > 10000 {
		jsonErr(w, "invalid part number", 400)
		return
	}

	mc, err := minio.New(minioEndpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: false})
	if err != nil {
		jsonErr(w, "minio error", 500)
		return
	}
	core := minio.Core{Client: mc}

	var reader io.Reader = r.Body
	part, err := core.PutObjectPart(context.Background(), bucket, key, uploadID, partNum, reader, size, minio.PutObjectPartOptions{})
	if err != nil {
		log.Printf("[S3] put part %d: %v", partNum, err)
		jsonErr(w, "failed to upload part", 500)
		return
	}
	jsonOK(w, map[string]any{
		"part":   partNum,
		"etag":   strings.Trim(part.ETag, "\""),
		"size":   part.Size,
	})
}

// ─── POST /oort/api/upload/complete ────────────────────────────────────────────

func handleS3UploadComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonErr(w, "method not allowed", 405)
		return
	}
	ak, sk, defBucket, ok := authS3Console(r)
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" { bucket = defBucket }
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	var req struct {
		Key      string `json:"key"`
		UploadID string `json:"upload_id"`
		Bucket   string `json:"bucket"`
		Parts    []struct {
			Part int    `json:"part"`
			ETag string `json:"etag"`
		} `json:"parts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "bad request", 400)
		return
	}
	if req.Bucket != "" { bucket = req.Bucket }

	mc, err := minio.New(minioEndpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: false})
	if err != nil {
		jsonErr(w, "minio error", 500)
		return
	}
	core := minio.Core{Client: mc}

	var completeParts []minio.CompletePart
	for _, p := range req.Parts {
		completeParts = append(completeParts, minio.CompletePart{
			PartNumber: p.Part,
			ETag:       p.ETag,
		})
	}
	upload, err := core.CompleteMultipartUpload(context.Background(), bucket, req.Key, req.UploadID, completeParts, minio.PutObjectOptions{})
	if err != nil {
		log.Printf("[S3] complete multipart: %v", err)
		jsonErr(w, "failed to complete upload", 500)
		return
	}
	
	// Record a write hit for analytics
	go recordHit(bucket, 200, 0, false, r.Header.Get("CF-IPCountry"), "write")
	go dispatchWebhooks(bucket, "object:created", upload.Key, 0, "")
	
	jsonOK(w, map[string]any{
		"key":    upload.Key,
		"etag":   strings.Trim(upload.ETag, "\""),
		"bucket": upload.Bucket,
	})
}

// ─── POST /oort/api/upload/abort ────────────────────────────────────────────────

func handleS3UploadAbort(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonErr(w, "method not allowed", 405)
		return
	}
	ak, sk, defBucket, ok := authS3Console(r)
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" { bucket = defBucket }
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	var req struct {
		Key      string `json:"key"`
		UploadID string `json:"upload_id"`
		Bucket   string `json:"bucket"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Bucket != "" { bucket = req.Bucket }
	mc, err := minio.New(minioEndpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: false})
	if err != nil {
		jsonErr(w, "minio error", 500)
		return
	}
	core := minio.Core{Client: mc}
	core.AbortMultipartUpload(context.Background(), bucket, req.Key, req.UploadID)
	jsonOK(w, map[string]bool{"aborted": true})
}

// ─── DELETE /oort/api/delete?key=xxx ───────────────────────────────────────────

func handleS3Delete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" && r.Method != "POST" {
		jsonErr(w, "method not allowed", 405)
		return
	}
	ak, sk, defBucket, ok := authS3Console(r)
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" { bucket = defBucket }
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		jsonErr(w, "missing key", 400)
		return
	}
	mc, err := minio.New(minioEndpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: false})
	if err != nil {
		jsonErr(w, "minio error", 500)
		return
	}
	ctx := context.Background()
	// If it's a "folder", delete all objects and their versions with that prefix
	if strings.HasSuffix(key, "/") {
		for obj := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: key, Recursive: true, WithVersions: true}) {
			if obj.Err == nil {
				mc.RemoveObject(ctx, bucket, obj.Key, minio.RemoveObjectOptions{VersionID: obj.VersionID})
			}
		}
	} else {
		if err := mc.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil {
			jsonErr(w, "delete failed", 500)
			return
		}
	}
	
	go dispatchWebhooks(bucket, "object:deleted", key, 0, "")
	jsonOK(w, map[string]bool{"deleted": true})
}

// ─── GET /oort/api/presign?key=xxx ─────────────────────────────────────────────

func handleS3Presign(w http.ResponseWriter, r *http.Request) {
	ak, sk, defBucket, ok := authS3Console(r)
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" { bucket = defBucket }
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		jsonErr(w, "missing key", 400)
		return
	}
	mc, err := minio.New(publicMinioEndpoint, &minio.Options{
		Creds: credentials.NewStaticV4(ak, sk, ""),
		Secure: true,
	})
	if err != nil {
		jsonErr(w, "minio error", 500)
		return
	}
	u, err := mc.PresignedGetObject(context.Background(), bucket, key, 15*time.Minute, nil)
	if err != nil {
		jsonErr(w, "presign failed", 500)
		return
	}
	jsonOK(w, map[string]string{"url": u.String()})
}

// ─── POST /oort/api/mkdir — create a "folder" placeholder ──────────────────────

func handleS3Mkdir(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonErr(w, "method not allowed", 405)
		return
	}
	ak, sk, defBucket, ok := authS3Console(r)
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" { bucket = defBucket }
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	var req struct {
		Prefix string `json:"prefix"`
		Key    string `json:"key"`
		Bucket string `json:"bucket"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Bucket != "" { bucket = req.Bucket }
	// Accept either "key" or "prefix" field
	if req.Prefix == "" && req.Key != "" {
		req.Prefix = req.Key
	}
	// Auto-append trailing slash if missing
	if req.Prefix != "" && !strings.HasSuffix(req.Prefix, "/") {
		req.Prefix = req.Prefix + "/"
	}
	if req.Prefix == "" {
		jsonErr(w, "folder name is required", 400)
		return
	}
	mc, err := minio.New(minioEndpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: false})
	if err != nil {
		jsonErr(w, "minio error", 500)
		return
	}
	// Put a zero-byte directory object (ends with /)
	key := req.Prefix
	_, err = mc.PutObject(context.Background(), bucket, key, strings.NewReader(""), 0,
		minio.PutObjectOptions{ContentType: "application/x-directory"})
	if err != nil {
		log.Printf("[S3] mkdir: %v", err)
		jsonErr(w, "mkdir failed", 500)
		return
	}
	jsonOK(w, map[string]string{"prefix": req.Prefix})
}

// ─── POST /oort/api/objects/move ────────────────────────────────────────────────
func handleS3Move(w http.ResponseWriter, r *http.Request) {
	ak, sk, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	var req struct {
		Bucket    string `json:"bucket"`
		SourceKey string `json:"source_key"`
		DestKey   string `json:"dest_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid request", 400)
		return
	}
	if req.Bucket == "" { req.Bucket = defBucket }
	
	mc, err := minio.New(minioEndpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: false})
	if err != nil {
		jsonErr(w, "minio error", 500)
		return
	}
	
	ctx := context.Background()
	// If it's a folder, do a recursive move
	if strings.HasSuffix(req.SourceKey, "/") {
		if !strings.HasSuffix(req.DestKey, "/") {
			req.DestKey += "/"
		}
		for obj := range mc.ListObjects(ctx, req.Bucket, minio.ListObjectsOptions{Prefix: req.SourceKey, Recursive: true}) {
			if obj.Err != nil { continue }
			newKey := req.DestKey + strings.TrimPrefix(obj.Key, req.SourceKey)
			
			// Copy each object
			srcOpts := minio.CopySrcOptions{Bucket: req.Bucket, Object: obj.Key}
			dstOpts := minio.CopyDestOptions{Bucket: req.Bucket, Object: newKey}
			_, copyErr := mc.CopyObject(ctx, dstOpts, srcOpts)
			
			// Remove old object if copy succeeds
			if copyErr == nil {
				mc.RemoveObject(ctx, req.Bucket, obj.Key, minio.RemoveObjectOptions{})
			}
		}
	} else {
		// Copy single object
		srcOpts := minio.CopySrcOptions{Bucket: req.Bucket, Object: req.SourceKey}
		dstOpts := minio.CopyDestOptions{Bucket: req.Bucket, Object: req.DestKey}
		_, err = mc.CopyObject(ctx, dstOpts, srcOpts)
		if err != nil {
			log.Printf("[S3] copy error: %v", err)
			jsonErr(w, "failed to copy object", 500)
			return
		}
		
		// Remove original object
		err = mc.RemoveObject(ctx, req.Bucket, req.SourceKey, minio.RemoveObjectOptions{})
		if err != nil {
			log.Printf("[S3] remove error after copy: %v", err)
			jsonErr(w, "failed to remove original object", 500)
			return
		}
	}
	
	jsonOK(w, map[string]string{"status": "moved"})
}

// ─── POST /oort/api/buckets/rename ─────────────────────────────────────────────
func handleS3RenameBucket(w http.ResponseWriter, r *http.Request) {
	ak, _, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	var req struct {
		OldName string `json:"old_name"`
		NewName string `json:"new_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid request", 400)
		return
	}

	displayName := strings.TrimSpace(req.NewName)
	if displayName == "" {
		jsonErr(w, "Name cannot be empty", 400)
		return
	}

	if req.OldName == defBucket {
		// Rename default bucket
		scyllaSession.Query(`UPDATE cosmos.users SET bucket_display_name = ? WHERE service = 'git' AND email = ?`, displayName, ak).Exec()
	} else {
		// Verify ownership in DB
		var owner string
		scyllaSession.Query(`SELECT owner_email FROM cosmos.buckets WHERE bucket_name = ?`, req.OldName).Scan(&owner)
		if owner != ak {
			jsonErr(w, "unauthorized or not found", 403)
			return
		}
		// Rename custom bucket
		scyllaSession.Query(`UPDATE cosmos.buckets SET display_name = ? WHERE bucket_name = ?`, displayName, req.OldName).Exec()
	}

	jsonOK(w, map[string]string{"status": "renamed"})
}
// ─── Format helpers (used in template) ───────────────────────────────────────

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// ─── GET /oort/api/buckets ──────────────────────────────────────────────────────

// ─── GET /oort/api/profile ──────────────────────────────────────────────────────
func handleS3Profile(w http.ResponseWriter, r *http.Request) {
	ak, sk, defaultBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	var name, username, profileImage string
	scyllaSession.Query(`SELECT name, username, profile_image FROM cosmos.users WHERE service = 'git' AND email = ?`, ak).Scan(&name, &username, &profileImage)

	jsonOK(w, map[string]any{
		"access_key":     ak,
		"secret_key":     sk,
		"default_bucket": defaultBucket,
		"endpoint":       "https://cosmos.linuxguy.space",
		"name":           name,
		"username":       username,
		"profile_image":  profileImage,
	})
}

// ─── POST /oort/api/quota ───────────────────────────────────────────────────────

// ─── POST /oort/api/password ────────────────────────────────────────────────────
// ─── POST /oort/api/quota ───────────────────────────────────────────────────────
func handleS3Quota(w http.ResponseWriter, r *http.Request) {
	ak, _, _, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	
	var body struct{ Amount int `json:"amount"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, "invalid request", 400)
		return
	}
	if body.Amount > 100 {
		jsonErr(w, "maximum requested amount is 100GB", 400)
		return
	}
	
	reqToken := uuid.New().String()
	if err := scyllaSession.Query(`INSERT INTO cosmos.quota_requests (req_token, email, amount) VALUES (?, ?, ?)`, reqToken, ak, body.Amount).Exec(); err != nil {
		jsonErr(w, "db error", 500)
		return
	}
	
	go sendQuotaEmail(ak, reqToken, body.Amount)
	
	jsonOK(w, map[string]bool{"success": true})
}

func sendQuotaEmail(userEmail, token string, amount int) {
	link := fmt.Sprintf("https://cosmos.linuxguy.space/oort/api/quota/approve?token=%s", token)
	msg := fmt.Sprintf("From: serverdmncr@gmail.com\r\nTo: serverdmncr@gmail.com\r\nSubject: Quota Request from %s\r\n\r\nUser %s has requested a quota increase to %d GB.\r\n\r\nClick here to approve: %s\r\n", userEmail, userEmail, amount, link)
	cmd := exec.Command("msmtp", "-t")
	cmd.Stdin = bytes.NewBufferString(msg)
	if err := cmd.Run(); err != nil {
		log.Printf("[MAIL] Failed to send quota email: %v", err)
	}
}

func handleS3ApproveQuota(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token", 400)
		return
	}
	var email string
	var amount int
	if err := scyllaSession.Query(`SELECT email, amount FROM cosmos.quota_requests WHERE req_token = ?`, token).Scan(&email, &amount); err != nil {
		http.Error(w, "invalid token", 400)
		return
	}
	
	// Apply quota to all user's buckets
	iter := scyllaSession.Query(`SELECT bucket_name FROM cosmos.buckets WHERE owner_email = ? ALLOW FILTERING`, email).Iter()
	var bName string
	for iter.Scan(&bName) {
		exec.Command("mc", "quota", "set", "cosmos/"+bName, "--size", fmt.Sprintf("%dG", amount)).Run()
	}
	iter.Close()
	
	scyllaSession.Query(`DELETE FROM cosmos.quota_requests WHERE req_token = ?`, token).Exec()
	w.Write([]byte("Quota approved and applied successfully!"))
}
func handleS3Password(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonErr(w, "method not allowed", 405)
		return
	}
	ak, _, _, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	var req struct {
		NewPassword string `json:"new_password"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if len(req.NewPassword) < 8 {
		jsonErr(w, "password must be at least 8 characters", 400)
		return
	}

	// Change in ScyllaDB ONLY. MinIO API token (secret_key) remains unchanged.
	hash, _ := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	err := scyllaSession.Query(`UPDATE cosmos.users SET password_hash = ? WHERE service = 'git' AND email = ?`,
		string(hash), ak).Exec()
	if err != nil {
		log.Printf("[S3] password update scylla: %v", err)
	}
	var gUser string
	scyllaSession.Query(`SELECT username FROM cosmos.users WHERE service = 'git' AND email = ?`, ak).Scan(&gUser)
	if gUser != "" {
		syncGiteaPassword(gUser, req.NewPassword)
	}

	jsonOK(w, map[string]bool{"success": true})
}


func syncUserPolicy(email string) error {
	var buckets []string
	iter := scyllaSession.Query(`SELECT bucket_name FROM cosmos.buckets WHERE owner_email = ?`, email).Iter()
	var b string
	for iter.Scan(&b) {
		buckets = append(buckets, b)
	}
	if err := iter.Close(); err != nil {
		return err
	}

	var resources []string
	for _, b := range buckets {
		resources = append(resources, fmt.Sprintf(`"arn:aws:s3:::%s"`, b), fmt.Sprintf(`"arn:aws:s3:::%s/*"`, b))
	}
	
	resStr := strings.Join(resources, ",")
	if resStr == "" {
		resStr = `""`
	}
	
	policyStr := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":[%s]}]}`, resStr)
	safeEmail := strings.ReplaceAll(email, "@", "_")
	safeEmail = strings.ReplaceAll(safeEmail, ".", "_")
	policyName := "policy_" + safeEmail
	policyFile := "/tmp/" + policyName + ".json"
	
	os.WriteFile(policyFile, []byte(policyStr), 0644)
	exec.Command("mc", "admin", "policy", "create", "cosmos", policyName, policyFile).Run()
	exec.Command("mc", "admin", "policy", "attach", "cosmos", policyName, "--user", email).Run()
	log.Printf("[S3] synced policy for %s with %d buckets", email, len(buckets))
	return nil
}

// ─── GET /oort/api/buckets ──────────────────────────────────────────────────────
func handleS3Buckets(w http.ResponseWriter, r *http.Request) {
	ak, _, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	var buckets []map[string]any
	
	// Always include the default private bucket
	if defBucket != "" {
		var defDisp string
		scyllaSession.Query(`SELECT bucket_display_name FROM cosmos.users WHERE service='git' AND email=?`, ak).Scan(&defDisp)
		if defDisp == "" {
			defDisp = "mybucket"
		}
		buckets = append(buckets, map[string]any{"Name": defBucket, "DisplayName": defDisp})
	}
	
	// Fetch additional custom buckets
	iter := scyllaSession.Query(`SELECT bucket_name, display_name FROM cosmos.buckets WHERE owner_email = ?`, ak).Iter()
	var b, disp string
	for iter.Scan(&b, &disp) {
		if disp == "" {
			disp = b
		}
		buckets = append(buckets, map[string]any{"Name": b, "DisplayName": disp})
	}
	iter.Close()
	jsonOK(w, map[string]any{"buckets": buckets})
}

// ─── POST /oort/api/buckets (CREATE) ──────────────────────────────────────────
func handleS3CreateBucket(w http.ResponseWriter, r *http.Request) {
	ak, _, _, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	var req struct {
		Name string `json:"name"` // Used as display name
	}
	json.NewDecoder(r.Body).Decode(&req)
	
	displayName := strings.TrimSpace(req.Name)
	if displayName == "" {
		jsonErr(w, "Bucket name required", 400)
		return
	}
	
	internalName := "cosmos-" + strings.ReplaceAll(uuid.New().String(), "-", "")[:16]
	
	// Try creating it in MinIO.
	if err := exec.Command("mc", "mb", "cosmos/"+internalName).Run(); err != nil {
		jsonErr(w, "Failed to provision backend bucket", 500)
		return
	}
	
	// Apply 10GB quota
	exec.Command("mc", "quota", "set", "cosmos/"+internalName, "--size", "10G").Run()
	
	// Enable versioning
	exec.Command("mc", "version", "enable", "cosmos/"+internalName).Run()
	
	// Save to DB
	scyllaSession.Query(`INSERT INTO cosmos.buckets (bucket_name, owner_email, display_name) VALUES (?, ?, ?)`, internalName, ak, displayName).Exec()
	
	// Sync policy
	syncUserPolicy(ak)
	
	jsonOK(w, map[string]string{"name": internalName})
}

// ─── DELETE /oort/api/buckets (DELETE) ────────────────────────────────────────
func handleS3DeleteBucket(w http.ResponseWriter, r *http.Request) {
	ak, _, _, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		jsonErr(w, "missing bucket name", 400)
		return
	}
	
	// Verify ownership
	var owner string
	scyllaSession.Query(`SELECT owner_email FROM cosmos.buckets WHERE bucket_name = ?`, bucket).Scan(&owner)
	if owner != ak {
		jsonErr(w, "unauthorized or not found", 403)
		return
	}
	
	// Delete from MinIO
	exec.Command("mc", "rb", "--force", "cosmos/"+bucket).Run()
	
	// Delete from DB
	scyllaSession.Query(`DELETE FROM cosmos.buckets WHERE bucket_name = ?`, bucket).Exec()
	
	// Sync policy
	syncUserPolicy(ak)
	
	jsonOK(w, map[string]bool{"deleted": true})
}



// ─── POST /oort/api/profile/image ───────────────────────────────────────────────
func handleS3ProfileImage(w http.ResponseWriter, r *http.Request) {
	ak, _, _, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	r.ParseMultipartForm(10 << 20)
	file, _, err := r.FormFile("profile_image")
	if err != nil {
		jsonErr(w, "missing image", 400)
		return
	}
	defer file.Close()
	bytes, err := io.ReadAll(file)
	if err != nil || len(bytes) == 0 {
		jsonErr(w, "invalid image", 400)
		return
	}
	encoded := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(bytes)
	if err := scyllaSession.Query(`UPDATE cosmos.users SET profile_image = ? WHERE service = 'git' AND email = ?`, encoded, ak).Exec(); err != nil {
		jsonErr(w, "db error", 500)
		return
	}
	jsonOK(w, map[string]bool{"success": true})
}

// ─── POST /oort/api/profile/update ──────────────────────────────────────────────
func handleS3ProfileUpdate(w http.ResponseWriter, r *http.Request) {
	ak, _, _, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	var req struct {
		Name     string `json:"name"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid request", 400)
		return
	}
	
	req.Name = strings.TrimSpace(req.Name)
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	
	if req.Name == "" || req.Username == "" {
		jsonErr(w, "name and username are required", 400)
		return
	}

	var ex string
	if scyllaSession.Query(`SELECT email FROM cosmos.users WHERE username = ? ALLOW FILTERING`, req.Username).Scan(&ex) == nil && ex != ak {
		jsonErr(w, "username already taken", 409)
		return
	}

	if err := scyllaSession.Query(`UPDATE cosmos.users SET name = ?, username = ? WHERE service = 'git' AND email = ?`, req.Name, req.Username, ak).Exec(); err != nil {
		jsonErr(w, "db error", 500)
		return
	}
	jsonOK(w, map[string]bool{"success": true})
}

// ─── GET /oort/api/objects/share ───────────────────────────────────────────────

func handleS3ShareObject(w http.ResponseWriter, r *http.Request) {
	ak, sk, _, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	if bucket == "" || key == "" {
		jsonErr(w, "missing bucket or key", 400)
		return
	}
	
	mc, err := minio.New(minioEndpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: false})
	if err != nil {
		jsonErr(w, "minio error", 500)
		return
	}
	
	// 7 days max expiry
	reqParams := make(url.Values)
	presignedURL, err := mc.PresignedGetObject(context.Background(), bucket, key, time.Hour * 24 * 7, reqParams)
	if err != nil {
		jsonErr(w, err.Error(), 500)
		return
	}
	
	jsonOK(w, map[string]string{"url": strings.Replace(presignedURL.String(), "http://"+minioEndpoint, "https://"+publicMinioEndpoint, 1)})
}

// ─── GET /oort/api/objects/versions ──────────────────────────────────────────────

func handleS3ObjectVersions(w http.ResponseWriter, r *http.Request) {
	ak, sk, _, ok := authS3Console(r)
	if !ok { jsonErr(w, "unauthorized", 401); return }
	
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("prefix") // we use prefix for key
	
	mc, err := minioClient(ak, sk)
	if err != nil { jsonErr(w, "minio error", 500); return }
	
	var versions []map[string]any
	for objInfo := range mc.ListObjects(context.Background(), bucket, minio.ListObjectsOptions{Prefix: key, WithVersions: true}) {
		if objInfo.Err != nil { continue }
		if objInfo.Key != key { continue } // only exact matches
		versions = append(versions, map[string]any{
			"VersionID": objInfo.VersionID,
			"IsLatest": objInfo.IsLatest,
			"IsDeleteMarker": objInfo.IsDeleteMarker,
			"LastModified": objInfo.LastModified,
			"Size": objInfo.Size,
		})
	}
	jsonOK(w, map[string]any{"versions": versions})
}

// ─── POST /oort/api/objects/restore ──────────────────────────────────────────────

func handleS3ObjectRestore(w http.ResponseWriter, r *http.Request) {
	ak, sk, _, ok := authS3Console(r)
	if !ok { jsonErr(w, "unauthorized", 401); return }
	
	var req struct {
		Bucket    string `json:"bucket"`
		Key       string `json:"key"`
		VersionID string `json:"version_id"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	
	mc, err := minioClient(ak, sk)
	if err != nil { jsonErr(w, "minio error", 500); return }
	
	src := minio.CopySrcOptions{
		Bucket: req.Bucket,
		Object: req.Key,
		VersionID: req.VersionID,
	}
	dst := minio.CopyDestOptions{
		Bucket: req.Bucket,
		Object: req.Key,
	}
	
	_, err = mc.CopyObject(context.Background(), dst, src)
	if err != nil {
		jsonErr(w, err.Error(), 500)
		return
	}
	jsonOK(w, map[string]bool{"success": true})
}



var publicBucketCache sync.Map
var proxyTransport = &http.Transport{
	MaxIdleConns:        10000,
	MaxIdleConnsPerHost: 10000,
	IdleConnTimeout:     90 * time.Second,
}


type MornyeTrackItem struct {
	Key          string `json:"key"`
	Name         string `json:"name"`
	Title        string `json:"title"`
	Size         int64  `json:"size"`
	Format       string `json:"format"`
	StreamURL    string `json:"stream_url"`
	LastModified string `json:"last_modified"`
}

func handleMornyeNASInfo(w http.ResponseWriter, r *http.Request) {
	ak, sk, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	var username, name string
	scyllaSession.Query(`SELECT username, name FROM cosmos.users WHERE service='git' AND email=?`, ak).Scan(&username, &name)
	if username == "" {
		username = strings.Split(ak, "@")[0]
	}

	var defDisp string
	scyllaSession.Query(`SELECT bucket_display_name FROM cosmos.users WHERE service='git' AND email=?`, ak).Scan(&defDisp)
	if defDisp == "" {
		defDisp = "mybucket"
	}

	type NASBucketInfo struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		IsDefault   bool   `json:"is_default"`
		IsPublic    bool   `json:"is_public"`
		NASUrl      string `json:"nas_url"`
		PlaylistUrl string `json:"playlist_url"`
		TrackCount  int    `json:"track_count"`
		MusicBytes  int64  `json:"music_bytes"`
	}

	var bucketList []NASBucketInfo
	rawBuckets := []struct{ name, disp string }{ {defBucket, defDisp} }

	iter := scyllaSession.Query(`SELECT bucket_name, display_name FROM cosmos.buckets WHERE owner_email = ?`, ak).Iter()
	var b, disp string
	for iter.Scan(&b, &disp) {
		if disp == "" { disp = b }
		rawBuckets = append(rawBuckets, struct{ name, disp string }{b, disp})
	}
	iter.Close()

	mc, _ := minioClient(ak, sk)
	ctx := context.Background()

	for i, rb := range rawBuckets {
		if rb.name == "" { continue }
		
		cmd := exec.Command("mc", "anonymous", "get", "cosmos/"+rb.name)
		out, _ := cmd.CombinedOutput()
		isPub := strings.Contains(string(out), "download") || strings.Contains(string(out), "public")

		trackCount := 0
		var musicBytes int64 = 0
		if mc != nil {
			for obj := range mc.ListObjects(ctx, rb.name, minio.ListObjectsOptions{Recursive: true}) {
				if obj.Err == nil && !strings.HasSuffix(obj.Key, "/") {
					lower := strings.ToLower(obj.Key)
					if strings.HasSuffix(lower, ".flac") || strings.HasSuffix(lower, ".mp3") ||
					   strings.HasSuffix(lower, ".wav") || strings.HasSuffix(lower, ".m4a") ||
					   strings.HasSuffix(lower, ".aac") || strings.HasSuffix(lower, ".ogg") ||
					   strings.HasSuffix(lower, ".opus") || strings.HasSuffix(lower, ".alac") {
						trackCount++
						musicBytes += obj.Size
					}
				}
			}
		}

		bucketList = append(bucketList, NASBucketInfo{
			Name:        rb.name,
			DisplayName: rb.disp,
			IsDefault:   (i == 0),
			IsPublic:    isPub,
			NASUrl:      fmt.Sprintf("https://cosmos.linuxguy.space/oort/%s/%s/", username, rb.disp),
			PlaylistUrl: fmt.Sprintf("https://cosmos.linuxguy.space/oort/%s/%s/playlist.m3u", username, rb.disp),
			TrackCount:  trackCount,
			MusicBytes:  musicBytes,
		})
	}

	jsonOK(w, map[string]any{
		"username":       username,
		"name":           name,
		"email":          ak,
		"secret_key":     sk,
		"s3_endpoint":    "https://cosmos.linuxguy.space",
		"default_bucket": defBucket,
		"buckets":        bucketList,
	})
}

func handleMornyeNASEnableStreaming(w http.ResponseWriter, r *http.Request) {
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
		Bucket string `json:"bucket"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Bucket == "" { req.Bucket = defBucket }

	isOwner := false
	if req.Bucket == defBucket {
		isOwner = true
	} else {
		var owner string
		if err := scyllaSession.Query(`SELECT owner_email FROM cosmos.buckets WHERE bucket_name=?`, req.Bucket).Scan(&owner); err == nil && owner == ak {
			isOwner = true
		}
	}
	if !isOwner {
		jsonErr(w, "forbidden", 403)
		return
	}

	cmd := exec.Command("mc", "anonymous", "set", "download", "cosmos/"+req.Bucket)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[NAS] enable streaming error for %s: %v | %s", req.Bucket, err, string(out))
		jsonErr(w, "failed to enable streaming", 500)
		return
	}
	jsonOK(w, map[string]any{"success": true, "bucket": req.Bucket, "status": "streaming_enabled"})
}

func handleS3PublicProxy(w http.ResponseWriter, r *http.Request, username, path string) {
	trimmed := strings.Trim(path, "/")
	var displayBucket, objectKey string
	if idx := strings.Index(trimmed, "/"); idx != -1 {
		displayBucket = trimmed[:idx]
		objectKey = trimmed[idx+1:]
	} else {
		displayBucket = trimmed
		objectKey = ""
	}
	if displayBucket == "" {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(404)
		w.Write([]byte("<html><body><h1>404 - Not Found</h1></body></html>"))
		return
	}

	cacheKey := username + ":" + displayBucket
	var actualBucket string

	if cached, ok := publicBucketCache.Load(cacheKey); ok {
		actualBucket = cached.(string)
	} else {
		var email, defBucket string
		err := scyllaSession.Query(`SELECT email, bucket FROM cosmos.users WHERE username=? ALLOW FILTERING`, username).Scan(&email, &defBucket)
		if err != nil {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(404)
			w.Write([]byte("<html><body><h1>404 - User Not Found</h1></body></html>"))
			return
		}

		var defDisp string
		scyllaSession.Query(`SELECT bucket_display_name FROM cosmos.users WHERE service='git' AND email=?`, email).Scan(&defDisp)
		if defDisp == "" {
			defDisp = "mybucket"
		}
		
		if displayBucket == defDisp {
			actualBucket = defBucket
		} else {
			iter := scyllaSession.Query(`SELECT bucket_name, display_name FROM cosmos.buckets WHERE owner_email = ?`, email).Iter()
			var b, disp string
			for iter.Scan(&b, &disp) {
				if disp == "" {
					disp = b
				}
				if disp == displayBucket {
					actualBucket = b
					break
				}
			}
			iter.Close()
		}

		if actualBucket == "" {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(404)
			w.Write([]byte("<html><body><h1>404 - Bucket Not Found</h1></body></html>"))
			return
		}
		publicBucketCache.Store(cacheKey, actualBucket)
	}

	// Check if this is a Mornye NAS root, index.json, or playlist.m3u request
	isPlaylist := objectKey == "playlist.m3u" || objectKey == "mornye.m3u" || r.URL.Query().Get("format") == "m3u"
	isJSON := objectKey == "index.json" || objectKey == "mornye.json" || r.URL.Query().Get("format") == "json" || strings.Contains(r.Header.Get("Accept"), "application/json")
	isRoot := (objectKey == "")

	if isRoot || isPlaylist || isJSON {
		mc, err := minio.New(minioEndpoint, &minio.Options{
			Creds:  credentials.NewStaticV4("unityadmin", "UnityAdmin2026!", ""),
			Secure: false,
		})
		if err != nil {
			http.Error(w, "NAS storage backend error", 500)
			return
		}

		var tracks []MornyeTrackItem
		var totalBytes int64 = 0

		ctx := context.Background()
		for obj := range mc.ListObjects(ctx, actualBucket, minio.ListObjectsOptions{Recursive: true}) {
			if obj.Err == nil && !strings.HasSuffix(obj.Key, "/") {
				lower := strings.ToLower(obj.Key)
				ext := strings.TrimPrefix(filepath.Ext(lower), ".")
				name := filepath.Base(obj.Key)
				title := strings.TrimSuffix(name, filepath.Ext(name))
				displayTitle := strings.ReplaceAll(title, "_", " ")

				isAudio := ext == "flac" || ext == "mp3" || ext == "wav" || ext == "m4a" || ext == "aac" || ext == "ogg" || ext == "opus" || ext == "alac"
				if isAudio || isRoot {
					streamURL := fmt.Sprintf("https://cosmos.linuxguy.space/oort/%s/%s/%s", username, displayBucket, url.PathEscape(obj.Key))
					tracks = append(tracks, MornyeTrackItem{
						Key:          obj.Key,
						Name:         name,
						Title:        displayTitle,
						Size:         obj.Size,
						Format:       strings.ToUpper(ext),
						StreamURL:    streamURL,
						LastModified: obj.LastModified.Format(time.RFC3339),
					})
					totalBytes += obj.Size
				}
			}
		}

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")

		if isPlaylist {
			w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
			w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"%s-mornye.m3u\"", displayBucket))
			var buf strings.Builder
			buf.WriteString("#EXTM3U\n")
			buf.WriteString(fmt.Sprintf("#PLAYLIST:Mornye NAS - %s - %s\n\n", username, displayBucket))
			for _, t := range tracks {
				buf.WriteString(fmt.Sprintf("#EXTINF:-1,%s\n%s\n", t.Title, t.StreamURL))
			}
			w.Write([]byte(buf.String()))
			return
		}

		if isJSON {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			json.NewEncoder(w).Encode(map[string]any{
				"service":      "Mornye NAS",
				"version":      "2.0",
				"status":       "online",
				"account":      username,
				"bucket":       displayBucket,
				"nas_url":      fmt.Sprintf("https://cosmos.linuxguy.space/oort/%s/%s/", username, displayBucket),
				"playlist_url": fmt.Sprintf("https://cosmos.linuxguy.space/oort/%s/%s/playlist.m3u", username, displayBucket),
				"s3_endpoint":  "https://cosmos.linuxguy.space",
				"tracks_count": len(tracks),
				"total_bytes":  totalBytes,
				"tracks":       tracks,
			})
			return
		}

		// Web player page
		renderMornyeNASWebPlayer(w, username, displayBucket, tracks, totalBytes)
		return
	}

	targetPath := fmt.Sprintf("/%s/%s", actualBucket, objectKey)

	targetUrl, _ := url.Parse("http://127.0.0.1:9010")
	proxy := httputil.NewSingleHostReverseProxy(targetUrl)
	proxy.Transport = proxyTransport
	
	r.URL.Path = targetPath
	r.URL.RawPath = targetPath

	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode == 403 {
			resp.StatusCode = 403
			resp.Body = io.NopCloser(strings.NewReader("<html><body><h1>403 - Access Denied</h1><p>Public streaming access is not enabled for this Mornye NAS bucket. Enable public streaming in Oort NAS Console.</p></body></html>"))
			resp.Header.Set("Content-Length", fmt.Sprintf("%d", len("<html><body><h1>403 - Access Denied</h1><p>Public streaming access is not enabled for this Mornye NAS bucket. Enable public streaming in Oort NAS Console.</p></body></html>")))
			resp.Header.Set("Content-Type", "text/html")
			return nil
		}
		resp.Header.Set("Access-Control-Allow-Origin", "*")
		resp.Header.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		resp.Header.Set("Access-Control-Allow-Headers", "Range, Authorization, Content-Type")
		resp.Header.Set("Access-Control-Expose-Headers", "Content-Range, Content-Length, Accept-Ranges")
		resp.Header.Set("Accept-Ranges", "bytes")

		lowerKey := strings.ToLower(objectKey)
		switch {
		case strings.HasSuffix(lowerKey, ".flac"):
			resp.Header.Set("Content-Type", "audio/flac")
		case strings.HasSuffix(lowerKey, ".mp3"):
			resp.Header.Set("Content-Type", "audio/mpeg")
		case strings.HasSuffix(lowerKey, ".wav"):
			resp.Header.Set("Content-Type", "audio/wav")
		case strings.HasSuffix(lowerKey, ".m4a"):
			resp.Header.Set("Content-Type", "audio/mp4")
		case strings.HasSuffix(lowerKey, ".aac"):
			resp.Header.Set("Content-Type", "audio/aac")
		case strings.HasSuffix(lowerKey, ".ogg"):
			resp.Header.Set("Content-Type", "audio/ogg")
		case strings.HasSuffix(lowerKey, ".opus"):
			resp.Header.Set("Content-Type", "audio/opus")
		case strings.HasSuffix(lowerKey, ".lrc"):
			resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
		}
		return nil
	}
	
	tw := &trackingResponseWriter{ResponseWriter: w, statusCode: 200}
	proxy.ServeHTTP(tw, r)
	recordHit(actualBucket, tw.statusCode, tw.bytes, false, r.Header.Get("CF-IPCountry"), "read")
}

func renderMornyeNASWebPlayer(w http.ResponseWriter, username, displayBucket string, tracks []MornyeTrackItem, totalBytes int64) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	nasURL := fmt.Sprintf("https://cosmos.linuxguy.space/oort/%s/%s/", username, displayBucket)
	playlistURL := fmt.Sprintf("https://cosmos.linuxguy.space/oort/%s/%s/playlist.m3u", username, displayBucket)
	jsonURL := fmt.Sprintf("https://cosmos.linuxguy.space/oort/%s/%s/index.json", username, displayBucket)

	var sizeStr string
	tb := float64(totalBytes)
	if tb > 1024*1024*1024 {
		sizeStr = fmt.Sprintf("%.2f GB", tb/(1024*1024*1024))
	} else {
		sizeStr = fmt.Sprintf("%.1f MB", tb/(1024*1024))
	}

	tracksJSON, _ := json.Marshal(tracks)

	html := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mornye NAS · @%s / %s</title>
<link rel="icon" href='data:image/svg+xml,<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><path fill="%%23a855f7" d="M12 3v10.55c-.59-.34-1.27-.55-2-.55-2.21 0-4 1.79-4 4s1.79 4 4 4 4-1.79 4-4V7h4V3h-6z"/></svg>'>
<style>
:root{--bg:#030712;--surface:rgba(255,255,255,0.035);--border:rgba(255,255,255,0.09);--accent:#a855f7;--accent-cyan:#38bdf8;--text:#f3f4f6;--dim:#9ca3af;--mono:ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;}
*{box-sizing:border-box;margin:0;padding:0;}
body{background:var(--bg);color:var(--text);font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;min-height:100vh;padding:24px 16px 120px;}
.container{max-width:980px;margin:0 auto;}
.glass{background:var(--surface);border:1px solid var(--border);border-radius:16px;backdrop-filter:blur(16px);padding:24px;margin-bottom:20px;}
.header{display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:16px;padding-bottom:16px;border-bottom:1px solid var(--border);margin-bottom:20px;}
.brand{display:flex;align-items:center;gap:12px;}
.icon-box{width:42px;height:42px;border-radius:12px;background:linear-gradient(135deg,#a855f7,#4f46e5);display:flex;align-items:center;justify-content:center;box-shadow:0 0 20px rgba(168,85,247,0.35);}
.title{font-size:20px;font-weight:700;letter-spacing:-0.5px;}
.badge{display:inline-flex;align-items:center;gap:6px;font-size:11px;font-family:var(--mono);text-transform:uppercase;padding:4px 10px;border-radius:99px;background:rgba(56,189,248,0.1);color:var(--accent-cyan);border:1px solid rgba(56,189,248,0.25);}
.dot{width:6px;height:6px;border-radius:50%%;background:#22c55e;box-shadow:0 0 8px #22c55e;}
.nas-box{background:rgba(0,0,0,0.5);border:1px solid rgba(168,85,247,0.3);border-radius:12px;padding:16px;display:flex;align-items:center;gap:12px;flex-wrap:wrap;}
.url-val{font-family:var(--mono);font-size:13px;color:#e9d5ff;flex:1;min-width:240px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;}
.btn{cursor:pointer;background:linear-gradient(135deg,#a855f7,#6366f1);color:#fff;border:none;padding:9px 18px;border-radius:10px;font-size:13px;font-weight:600;display:inline-flex;align-items:center;gap:6px;transition:0.2s;}
.btn:hover{filter:brightness(1.15);transform:translateY(-1px);}
.btn-ghost{background:rgba(255,255,255,0.06);border:1px solid var(--border);color:var(--text);}
.btn-ghost:hover{background:rgba(255,255,255,0.12);}
.player-card{position:fixed;bottom:16px;left:50%%;transform:translateX(-50%%);width:calc(100%% - 32px);max-width:980px;background:rgba(15,23,42,0.95);border:1px solid rgba(168,85,247,0.4);border-radius:16px;box-shadow:0 20px 50px rgba(0,0,0,0.7);padding:14px 20px;z-index:9999;backdrop-filter:blur(20px);display:flex;align-items:center;gap:16px;flex-wrap:wrap;}
.track-table{width:100%%;border-collapse:collapse;margin-top:12px;font-size:13px;}
.track-table th{text-align:left;color:var(--dim);font-size:11px;text-transform:uppercase;letter-spacing:1px;padding:12px 14px;border-bottom:1px solid var(--border);}
.track-table td{padding:14px;border-bottom:1px solid rgba(255,255,255,0.04);vertical-align:middle;}
.track-row:hover td{background:rgba(255,255,255,0.02);}
.pill-format{font-size:10px;font-family:var(--mono);padding:2px 8px;border-radius:4px;font-weight:700;}
.pill-FLAC{background:rgba(168,85,247,0.2);color:#d8b4fe;border:1px solid rgba(168,85,247,0.4);}
.pill-MP3{background:rgba(34,197,94,0.2);color:#86efac;border:1px solid rgba(34,197,94,0.4);}
.pill-WAV{background:rgba(56,189,248,0.2);color:#7dd3fc;border:1px solid rgba(56,189,248,0.4);}
.pill-OTHER{background:rgba(255,255,255,0.1);color:#e2e8f0;border:1px solid rgba(255,255,255,0.2);}
.vinyl{width:36px;height:36px;border-radius:50%%;background:#18181b;border:2px solid #3f3f46;display:flex;align-items:center;justify-content:center;position:relative;}
.spinning{animation:spin 3s linear infinite;}
@keyframes spin{100%%{transform:rotate(360deg);}}
</style>
</head>
<body>
<div class="container">
  <div class="glass">
    <div class="header">
      <div class="brand">
        <div class="icon-box">
          <svg width="22" height="22" fill="none" stroke="#fff" stroke-width="2" viewBox="0 0 24 24"><path d="M9 18V5l12-2v13"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="16" r="3"/></svg>
        </div>
        <div>
          <div class="title">Mornye NAS · Audio Stream Gateway</div>
          <div style="font-size:13px;color:var(--dim);margin-top:2px;">Account: <strong style="color:#fff">@%s</strong> · Space: <strong style="color:var(--accent-cyan)">%s</strong></div>
        </div>
      </div>
      <div style="display:flex;gap:10px;align-items:center;">
        <span class="badge"><span class="dot"></span>NAS Storage Online</span>
        <a href="/oort/console" class="btn btn-ghost" style="text-decoration:none;">Oort Console →</a>
      </div>
    </div>

    <div style="margin-bottom:16px;font-size:13px;color:var(--dim);">
      Configure this dedicated URL into your <strong>Mornye Music Streaming App</strong> to access your network-attached library with lossless byte-range playback:
    </div>

    <div class="nas-box">
      <div class="url-val" id="nasUrlVal">%s</div>
      <button class="btn" onclick="copyText('%s','Mornye NAS URL copied!')">📋 Copy Mornye NAS URL</button>
      <a href="%s" class="btn btn-ghost" style="text-decoration:none;" download>🎵 Playlist (.m3u)</a>
      <a href="%s" class="btn btn-ghost" style="text-decoration:none;" target="_blank">JSON Metadata</a>
    </div>

    <div style="display:flex;gap:20px;margin-top:20px;padding-top:16px;border-top:1px solid var(--border);font-size:13px;color:var(--dim);flex-wrap:wrap;">
      <div>Tracks: <strong style="color:#fff">%d audio file(s)</strong></div>
      <div>Library Size: <strong style="color:#fff">%s</strong></div>
      <div>Protocol: <strong style="color:#38bdf8">RFC 7233 Byte-Range Streaming</strong></div>
      <div>DAC / Hi-Res: <strong style="color:#a855f7">Bit-Perfect 24-bit/192kHz Ready</strong></div>
    </div>
  </div>

  <div class="glass">
    <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:12px;">
      <h3 style="font-size:16px;font-weight:600;">Music Library</h3>
      <button class="btn btn-ghost" onclick="playAll()">▶ Play All</button>
    </div>

    <div style="overflow-x:auto;">
      <table class="track-table">
        <thead>
          <tr>
            <th style="width:50px;">#</th>
            <th>Track Name</th>
            <th style="width:100px;">Format</th>
            <th style="width:110px;">Size</th>
            <th style="width:200px;text-align:right;">Actions</th>
          </tr>
        </thead>
        <tbody id="trackList">
          <!-- Injected by JS -->
        </tbody>
      </table>
    </div>
  </div>
</div>

<div class="player-card" id="playerBar" style="display:none;">
  <div class="vinyl spinning" id="disc">
    <div style="width:10px;height:10px;border-radius:50%%;background:#a855f7;"></div>
  </div>
  <div style="flex:1;min-width:200px;">
    <div id="nowPlayingTitle" style="font-size:14px;font-weight:600;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;">No track playing</div>
    <div id="nowPlayingSub" style="font-size:11px;color:var(--dim);font-family:var(--mono);margin-top:2px;">Select a track to stream</div>
  </div>
  <audio id="audioEl" controls style="flex:2;min-width:260px;height:36px;outline:none;border-radius:8px;"></audio>
  <button class="btn btn-ghost" onclick="playNext()" title="Next Track">⏭</button>
</div>

<script>
const tracks = %s;
let currentIndex = -1;
const audio = document.getElementById('audioEl');
const bar = document.getElementById('playerBar');
const disc = document.getElementById('disc');
const nowTitle = document.getElementById('nowPlayingTitle');
const nowSub = document.getElementById('nowPlayingSub');

function fmtBytes(b){
  if(!b) return '0 B';
  const u = ['B','KB','MB','GB'];
  let i=0; while(b>=1024 && i<u.length-1){b/=1024; i++;}
  return b.toFixed(1) + ' ' + u[i];
}

function renderTracks(){
  const tb = document.getElementById('trackList');
  if(!tracks || !tracks.length){
    tb.innerHTML = '<tr><td colspan="5" style="text-align:center;padding:40px;color:var(--dim);">No audio tracks uploaded yet. Open Oort Console to upload FLAC, MP3, or WAV files.</td></tr>';
    return;
  }
  tb.innerHTML = tracks.map((t, idx) => {
    const fmt = t.format || 'AUDIO';
    const pillClass = 'pill-' + (['FLAC','MP3','WAV'].includes(fmt) ? fmt : 'OTHER');
    return '<tr class="track-row">' +
      '<td style="color:var(--dim);font-family:var(--mono);">' + (idx + 1) + '</td>' +
      '<td><strong style="color:#fff;">' + escapeHtml(t.title) + '</strong><div style="font-size:11px;color:var(--dim);font-family:var(--mono);margin-top:2px;">' + escapeHtml(t.name) + '</div></td>' +
      '<td><span class="pill-format ' + pillClass + '">' + fmt + '</span></td>' +
      '<td style="color:var(--dim);font-family:var(--mono);">' + fmtBytes(t.size) + '</td>' +
      '<td style="text-align:right;">' +
        '<button class="btn" style="padding:6px 12px;margin-right:6px;font-size:12px;" onclick="playTrack(' + idx + ')">▶ Play</button>' +
        '<a href="' + t.stream_url + '" download class="btn btn-ghost" style="padding:6px 10px;font-size:12px;text-decoration:none;margin-right:6px;">⬇</a>' +
        '<button class="btn btn-ghost" style="padding:6px 10px;font-size:12px;" onclick="copyText(\'' + t.stream_url + '\',\'Direct stream URL copied!\')" title="Copy Link">🔗</button>' +
      '</td>' +
    '</tr>';
  }).join('');
}

function escapeHtml(s){
  if(!s) return '';
  return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

function playTrack(idx){
  if(idx < 0 || idx >= tracks.length) return;
  currentIndex = idx;
  const t = tracks[idx];
  nowTitle.textContent = t.title || t.name;
  nowSub.textContent = (t.format || 'AUDIO') + ' · ' + fmtBytes(t.size) + ' · Zero-Delay Stream';
  audio.src = t.stream_url;
  bar.style.display = 'flex';
  audio.play();
  disc.classList.add('spinning');
}

function playNext(){
  if(tracks.length === 0) return;
  const next = (currentIndex + 1) %% tracks.length;
  playTrack(next);
}

function playAll(){
  if(tracks.length > 0) playTrack(0);
}

audio.onended = () => { playNext(); };
audio.onpause = () => { disc.classList.remove('spinning'); };
audio.onplay  = () => { disc.classList.add('spinning'); };

function copyText(txt, msg){
  navigator.clipboard.writeText(txt).then(() => {
    alert(msg || 'Copied to clipboard!');
  }).catch(() => {
    prompt('Copy this URL:', txt);
  });
}

renderTracks();
</script>
</body>
</html>`,
		username, displayBucket,
		username, displayBucket,
		nasURL, nasURL, playlistURL, jsonURL,
		len(tracks), sizeStr,
		string(tracksJSON),
	)
	w.Write([]byte(html))
}


func handleS3BucketSettings(w http.ResponseWriter, r *http.Request) {
	ak, _, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}

	if r.Method == "GET" {
		bucket := r.URL.Query().Get("bucket")
		if bucket == "" {
			jsonErr(w, "missing bucket", 400)
			return
		}
		
		isOwner := false
		if bucket == defBucket {
			isOwner = true
		} else {
			var owner string
			if err := scyllaSession.Query(`SELECT owner_email FROM cosmos.buckets WHERE bucket_name=?`, bucket).Scan(&owner); err == nil && owner == ak {
				isOwner = true
			}
		}
		if !isOwner {
			jsonErr(w, "forbidden", 403)
			return
		}

		cmd := exec.Command("mc", "anonymous", "get", "cosmos/"+bucket)
		out, _ := cmd.CombinedOutput()
		isPublic := strings.Contains(string(out), "download") || strings.Contains(string(out), "public")

		metrics := getMetrics(bucket)
		
		jsonOK(w, map[string]any{
			"is_public": isPublic,
			"metrics": map[string]any{
				"total": metrics.TotalRequests,
				"success": metrics.TotalSuccess,
				"errors": metrics.Total4xx + metrics.Total5xx,
				"bytes_served": metrics.TotalBytes,
				"last_hit": metrics.LastHit,
			},
		})
		return
	} else if r.Method == "POST" {
		var req struct {
			Bucket   string `json:"bucket"`
			IsPublic bool   `json:"is_public"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, "invalid request", 400)
			return
		}

		isOwner := false
		if req.Bucket == defBucket {
			isOwner = true
		} else {
			var owner string
			if err := scyllaSession.Query(`SELECT owner_email FROM cosmos.buckets WHERE bucket_name=?`, req.Bucket).Scan(&owner); err == nil && owner == ak {
				isOwner = true
			}
		}
		if !isOwner {
			jsonErr(w, "forbidden", 403)
			return
		}

		policyCmd := "private"
		if req.IsPublic {
			policyCmd = "download"
		}

		cmd := exec.Command("mc", "anonymous", "set", policyCmd, "cosmos/"+req.Bucket)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("[S3] Failed to set bucket %s to %s: %v | %s", req.Bucket, policyCmd, err, string(out))
			jsonErr(w, "Failed to update bucket privacy", 500)
			return
		}

		// Invalidate cache
		publicBucketCache.Range(func(key, value any) bool {
			if value.(string) == req.Bucket {
				publicBucketCache.Delete(key)
			}
			return true
		})

		jsonOK(w, map[string]any{"success": true})
		return
	}
	
	http.Error(w, "Method not allowed", 405)
}

func handleS3Analytics(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	ak, _, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}

	filterBucket := r.URL.Query().Get("bucket")
	filterStart := r.URL.Query().Get("start")
	filterEnd := r.URL.Query().Get("end")
	filterRegion := r.URL.Query().Get("region")
	filterOp := r.URL.Query().Get("op") // "read" or "write" or "all"

	var userBuckets []string
	userBuckets = append(userBuckets, defBucket)
	
	iter := scyllaSession.Query(`SELECT bucket_name FROM cosmos.buckets WHERE owner_email = ?`, ak).Iter()
	var b string
	for iter.Scan(&b) {
		userBuckets = append(userBuckets, b)
	}
	iter.Close()

	validBuckets := make(map[string]bool)
	for _, b := range userBuckets {
		validBuckets[b] = true
	}

	if filterBucket != "" && !validBuckets[filterBucket] && filterBucket != "all" {
		jsonErr(w, "forbidden bucket", 403)
		return
	}

	bucketsToQuery := userBuckets
	if filterBucket != "" && filterBucket != "all" {
		bucketsToQuery = []string{filterBucket}
	}

	type point struct {
		Reqs   int64
		Cache  int64
		Origin int64
	}
	timeseriesMap := make(map[int64]*point)
	var globeData []map[string]any

	var tReq, tCache, tOrigin, tSucc, t4, t5, tBytes int64

	now := time.Now().Truncate(time.Hour)
	startTime := now.Add(-23 * time.Hour)
	
	if filterEnd != "" {
		if t, err := time.Parse("2006-01-02T15:04", filterEnd); err == nil {
			now = t.Truncate(time.Hour)
		}
	}
	if filterStart != "" {
		if t, err := time.Parse("2006-01-02T15:04", filterStart); err == nil {
			startTime = t.Truncate(time.Hour)
		}
	} else if filterEnd != "" {
		startTime = now.Add(-23 * time.Hour)
	}

	if startTime.After(now) {
		startTime = now.Add(-23 * time.Hour) // fallback if user sets invalid range
	}
	
	hoursDiff := int(now.Sub(startTime).Hours())
	if hoursDiff < 1 { hoursDiff = 1 }
	if hoursDiff > 720 { hoursDiff = 720 } // limit points

	type countryData struct {
		Reads int64
		Writes int64
		Total int64
	}
	countryMap := make(map[string]*countryData)

	for _, b := range bucketsToQuery {
		q := `SELECT hour, country, total_requests, cache_hits, origin_hits, total_success, total_errors, total_bytes, total_reads, total_writes FROM cosmos.s3_analytics_hourly WHERE bucket_name = ? AND hour >= ? AND hour <= ? ALLOW FILTERING`
		iter := scyllaSession.Query(q, b, startTime, now).Iter()
		
		var hour time.Time
		var country string
		var req, cache, origin, succ, errs, bytes, reads, writes int64
		for iter.Scan(&hour, &country, &req, &cache, &origin, &succ, &errs, &bytes, &reads, &writes) {
			
			actualReq := req
			if filterOp == "read" {
				if reads == 0 { continue }
				actualReq = reads
			} else if filterOp == "write" {
				if writes == 0 { continue }
				actualReq = writes
			}
			
			// Always populate globe data for all countries
			if _, ok := countryMap[country]; !ok {
				countryMap[country] = &countryData{}
			}
			countryMap[country].Reads += reads
			countryMap[country].Writes += writes
			countryMap[country].Total += req
			
			if filterRegion != "" && filterRegion != "all" && country != filterRegion {
				continue
			}
			
			// Apply read/write filter to the totals being accumulated if possible
			// But for time series we will just use the totals.
			


			tReq += actualReq
			tCache += cache
			tOrigin += origin
			tSucc += succ
			t4 += errs 
			tBytes += bytes

			unixH := hour.Unix()
			if _, ok := timeseriesMap[unixH]; !ok {
				timeseriesMap[unixH] = &point{}
			}
			timeseriesMap[unixH].Reqs += actualReq
			timeseriesMap[unixH].Cache += cache
			timeseriesMap[unixH].Origin += origin

			// Country map populated above
		}
		iter.Close()
	}

	var labels []string
	var reqData, cacheData, originData []int64
	
	for i := 0; i <= hoursDiff; i++ {
		h := startTime.Add(time.Duration(i) * time.Hour)
		if hoursDiff > 48 {
			labels = append(labels, h.Format("01/02 15h"))
		} else {
			labels = append(labels, h.Format("15:00"))
		}
		unixH := h.Unix()
		if val, ok := timeseriesMap[unixH]; ok {
			reqData = append(reqData, val.Reqs)
			cacheData = append(cacheData, val.Cache)
			originData = append(originData, val.Origin)
		} else {
			reqData = append(reqData, 0)
			cacheData = append(cacheData, 0)
			originData = append(originData, 0)
		}
	}

	var bucketsInfo []map[string]any
	for _, b := range userBuckets {
		if filterBucket != "" && filterBucket != "all" && b != filterBucket {
			continue
		}
		var disp string
		if b == defBucket {
			scyllaSession.Query(`SELECT bucket_display_name FROM cosmos.users WHERE service='git' AND email=?`, ak).Scan(&disp)
			if disp == "" { disp = "mybucket" }
		} else {
			scyllaSession.Query(`SELECT display_name FROM cosmos.buckets WHERE bucket_name = ?`, b).Scan(&disp)
			if disp == "" { disp = b }
		}
		
		m := getMetrics(b)
		bucketsInfo = append(bucketsInfo, map[string]any{
			"name": b,
			"display": disp,
			"requests": m.TotalRequests,
			"cache_hits": m.CacheHits,
			"origin_hits": m.OriginHits,
			"errors": m.Total4xx + m.Total5xx,
			"bytes": m.TotalBytes,
			"last_hit": m.LastHit,
		})
	}
	
	for code, data := range countryMap {
		hits := data.Total
		if filterOp == "read" { hits = data.Reads }
		if filterOp == "write" { hits = data.Writes }
		
		if hits > 0 {
			globeData = append(globeData, map[string]any{
				"country": code,
				"hits": hits,
				"reads": data.Reads,
				"writes": data.Writes,
			})
		}
	}

	jsonOK(w, map[string]any{
		"total_requests": tReq,
		"total_cache_hits": tCache,
		"total_origin_hits": tOrigin,
		"total_success": tSucc,
		"total_4xx": t4,
		"total_5xx": t5,
		"total_bytes": tBytes,
		"buckets": bucketsInfo,
		"timeseries": map[string]any{
			"labels": labels,
			"reqs": reqData,
			"cache": cacheData,
			"origin": originData,
		},
		"globe": globeData,
	})
}

// ─── POST /oort/api/bucket/empty ───────────────────────────────────────────────

func handleS3EmptyBucket(w http.ResponseWriter, r *http.Request) {
	ak, sk, defBucket, ok := authS3Console(r)
	if !ok {
		jsonErr(w, "unauthorized", 401)
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = defBucket
	}
	
	// Verify ownership
	if bucket != defBucket {
		var owner string
		scyllaSession.Query(`SELECT owner_email FROM cosmos.buckets WHERE bucket_name = ?`, bucket).Scan(&owner)
		if owner != ak {
			jsonErr(w, "unauthorized or not found", 403)
			return
		}
	}
	
	mc, err := minio.New(minioEndpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: false})
	if err != nil {
		jsonErr(w, "minio error", 500)
		return
	}
	
	ctx := context.Background()
	// Loop through all objects AND versions and permanently delete them
	objectsCh := mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true, WithVersions: true})
	for obj := range objectsCh {
		if obj.Err == nil {
			mc.RemoveObject(ctx, bucket, obj.Key, minio.RemoveObjectOptions{VersionID: obj.VersionID})
		}
	}
	
	jsonOK(w, map[string]bool{"emptied": true})
}

// ─── CUSTOM DOMAINS ─────────────────────────────────────────────────────────

var domainCache sync.Map // map[string]string (domain -> bucketName)

func handleS3BucketDomains(w http.ResponseWriter, r *http.Request) {
	ak, _, defBucket, ok := authS3Console(r)
	if !ok { jsonErr(w, "unauthorized", 401); return }
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" { bucket = defBucket }
	
	// Verify ownership
	if bucket != defBucket {
		var owner string
		scyllaSession.Query(`SELECT owner_email FROM cosmos.buckets WHERE bucket_name = ?`, bucket).Scan(&owner)
		if owner != ak { jsonErr(w, "unauthorized", 403); return }
	}
	
	var domains []string
	iter := scyllaSession.Query(`SELECT domain FROM cosmos.bucket_domains WHERE bucket_name = ? ALLOW FILTERING`, bucket).Iter()
	var d string
	for iter.Scan(&d) { domains = append(domains, d) }
	if err := iter.Close(); err != nil {}
	if domains == nil { domains = []string{} }
	
	jsonOK(w, map[string]interface{}{"domains": domains})
}

func handleS3BucketDomainAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" { jsonErr(w, "method not allowed", 405); return }
	ak, _, defBucket, ok := authS3Console(r)
	if !ok { jsonErr(w, "unauthorized", 401); return }
	
	var req struct {
		Bucket string `json:"bucket"`
		Domain string `json:"domain"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Bucket == "" { req.Bucket = defBucket }
	if req.Domain == "" { jsonErr(w, "missing domain", 400); return }
	
	if req.Bucket != defBucket {
		var owner string
		scyllaSession.Query(`SELECT owner_email FROM cosmos.buckets WHERE bucket_name = ?`, req.Bucket).Scan(&owner)
		if owner != ak { jsonErr(w, "unauthorized", 403); return }
	}
	
	// Check if domain is already in use
	var existingBucket string
	scyllaSession.Query(`SELECT bucket_name FROM cosmos.bucket_domains WHERE domain = ?`, req.Domain).Scan(&existingBucket)
	if existingBucket != "" {
		jsonErr(w, "domain already in use", 400)
		return
	}
	
	scyllaSession.Query(`INSERT INTO cosmos.bucket_domains (domain, bucket_name, owner_email) VALUES (?, ?, ?)`, req.Domain, req.Bucket, ak).Exec()
	domainCache.Store(req.Domain, req.Bucket)
	
	jsonOK(w, map[string]bool{"success": true})
}

func handleS3BucketDomainRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" && r.Method != "DELETE" { jsonErr(w, "method not allowed", 405); return }
	ak, _, _, ok := authS3Console(r)
	if !ok { jsonErr(w, "unauthorized", 401); return }

	var req struct {
		Domain string `json:"domain"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	domain := req.Domain
	if domain == "" { domain = r.URL.Query().Get("domain") }
	if domain == "" { jsonErr(w, "missing domain", 400); return }

	var owner string
	scyllaSession.Query(`SELECT owner_email FROM cosmos.bucket_domains WHERE domain = ?`, domain).Scan(&owner)
	if owner != ak { jsonErr(w, "unauthorized", 403); return }

	scyllaSession.Query(`DELETE FROM cosmos.bucket_domains WHERE domain = ?`, domain).Exec()
	domainCache.Delete(domain)

	jsonOK(w, map[string]bool{"success": true})
}

// ─── CDN PROXY ─────────────────────────────────────────────────────────────

func handleS3CDNProxy(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if strings.Contains(host, ":") {
		host = strings.Split(host, ":")[0]
	}
	
	// Fast cache lookup
	bucketIntf, ok := domainCache.Load(host)
	var bucket string
	if ok {
		bucket = bucketIntf.(string)
	} else {
		scyllaSession.Query(`SELECT bucket_name FROM cosmos.bucket_domains WHERE domain = ?`, host).Scan(&bucket)
		if bucket == "" {
			http.Error(w, "Domain not mapped to any bucket", 404)
			return
		}
		domainCache.Store(host, bucket)
	}
	

	
	// Extract key
	key := strings.TrimPrefix(r.URL.Path, "/oort/api/cdn_proxy/")
	if key == "" { key = "index.html" }
	
	targetPath := "/" + bucket + "/" + key

	targetUrl, _ := url.Parse("http://127.0.0.1:9010")
	proxy := httputil.NewSingleHostReverseProxy(targetUrl)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = targetUrl.Scheme
		req.URL.Host = targetUrl.Host
		req.URL.Path = targetPath
		req.Host = targetUrl.Host
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		recordHit(bucket, resp.StatusCode, resp.ContentLength, false, r.Header.Get("CF-IPCountry"), "read")
		return nil
	}
	proxy.ServeHTTP(w, r)
}
