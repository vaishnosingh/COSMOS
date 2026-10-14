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
	"strconv"
	"strings"
	"time"
	"golang.org/x/crypto/bcrypt"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const minioEndpoint = "127.0.0.1:9000"

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
		return "", "", "", false
	}
	_, ak, sk, bucket, err := lookupBySession("oort", sid)
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
	
	// Invalidate cache
	cdnCache.Delete("/" + bucket + "/" + req.Key)
	
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
		cdnCache.Delete("/" + bucket + "/" + key)
	}
	
	// If it was a folder, clear cache for all files in it
	if strings.HasSuffix(key, "/") {
		prefix := "/" + bucket + "/" + key
		cdnCache.Range(func(k, v any) bool {
			if strings.HasPrefix(k.(string), prefix) {
				cdnCache.Delete(k)
			}
			return true
		})
	}
	
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
	mc, err := minio.New(getPublicMinioEndpoint(), &minio.Options{
		Creds: credentials.NewStaticV4(ak, sk, ""),
		Secure: !strings.HasPrefix(getPublicMinioEndpoint(), "localhost") && !strings.HasPrefix(getPublicMinioEndpoint(), "127.0.0.1"),
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
		scyllaSession.Query(`UPDATE cosmos.users SET bucket_display_name = ? WHERE service = 'oort' AND email = ?`, displayName, ak).Exec()
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
	scyllaSession.Query(`SELECT name, username, profile_image FROM cosmos.users WHERE service = 'oort' AND email = ?`, ak).Scan(&name, &username, &profileImage)

	jsonOK(w, map[string]any{
		"access_key":     ak,
		"secret_key":     sk,
		"default_bucket": defaultBucket,
		"endpoint":       getCosmosBaseURL(),
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
	link := fmt.Sprintf("%s/oort/api/quota/approve?token=%s", getCosmosBaseURL(), token)
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: Quota Request from %s\r\n\r\nUser %s has requested a quota increase to %d GB.\r\n\r\nClick here to approve: %s\r\n", getEmailFrom(), getAdminEmail(), userEmail, userEmail, amount, link)
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
	err := scyllaSession.Query(`UPDATE cosmos.users SET password_hash = ? WHERE service = 'oort' AND email = ?`,
		string(hash), ak).Exec()
	if err != nil {
		log.Printf("[S3] password update scylla: %v", err)
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
		scyllaSession.Query(`SELECT bucket_display_name FROM cosmos.users WHERE service='oort' AND email=?`, ak).Scan(&defDisp)
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
	if err := scyllaSession.Query(`UPDATE cosmos.users SET profile_image = ? WHERE service = 'oort' AND email = ?`, encoded, ak).Exec(); err != nil {
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
	if scyllaSession.Query(`SELECT email FROM cosmos.users WHERE service = 'oort' AND username = ? ALLOW FILTERING`, req.Username).Scan(&ex) == nil && ex != ak {
		jsonErr(w, "username already taken", 409)
		return
	}

	if err := scyllaSession.Query(`UPDATE cosmos.users SET name = ?, username = ? WHERE service = 'oort' AND email = ?`, req.Name, req.Username, ak).Exec(); err != nil {
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
	
	jsonOK(w, map[string]string{"url": strings.Replace(presignedURL.String(), "http://"+minioEndpoint, getCosmosBaseURL(), 1)})
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


func handleS3PublicProxy(w http.ResponseWriter, r *http.Request, username, path string) {
	idx := strings.Index(path, "/")
	if idx == -1 {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(404)
		w.Write([]byte("<html><body><h1>404 - Not Found</h1></body></html>"))
		return
	}
	displayBucket := path[:idx]
	objectKey := path[idx+1:]

	cacheKey := username + ":" + displayBucket
	var actualBucket string

	if cached, ok := publicBucketCache.Load(cacheKey); ok {
		actualBucket = cached.(string)
	} else {
		var email, defBucket string
		err := scyllaSession.Query(`SELECT email, bucket FROM cosmos.users WHERE service='oort' AND username=? ALLOW FILTERING`, username).Scan(&email, &defBucket)
		if err != nil {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(404)
			w.Write([]byte("<html><body><h1>404 - User Not Found</h1></body></html>"))
			return
		}

		var defDisp string
		scyllaSession.Query(`SELECT bucket_display_name FROM cosmos.users WHERE service='oort' AND email=?`, email).Scan(&defDisp)
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

	targetPath := fmt.Sprintf("/%s/%s", actualBucket, objectKey)
	
	// Check Cache for GET requests without Range
	if r.Method == "GET" && r.Header.Get("Range") == "" {
		if val, ok := cdnCache.Load(targetPath); ok {
			entry := val.(*cacheEntry)
			if time.Now().Before(entry.ExpiresAt) {
				for k, v := range entry.Headers {
					w.Header()[k] = v
				}
				w.Header().Set("X-Cache", "HIT")
				w.WriteHeader(entry.StatusCode)
				w.Write(entry.Body)
				recordHit(actualBucket, entry.StatusCode, int64(len(entry.Body)), true, r.Header.Get("CF-IPCountry"), "read")
				return
			}
			cdnCache.Delete(targetPath)
		}
	}

	targetUrl, _ := url.Parse("http://127.0.0.1:9000")
	proxy := httputil.NewSingleHostReverseProxy(targetUrl)
	proxy.Transport = proxyTransport
	
	r.URL.Path = targetPath
	r.URL.RawPath = targetPath

	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode == 403 {
			resp.StatusCode = 403
			resp.Body = io.NopCloser(strings.NewReader("<html><body><h1>403 - Access Denied</h1><p>Public access is not enabled for this bucket.</p></body></html>"))
			resp.Header.Set("Content-Length", fmt.Sprintf("%d", len("<html><body><h1>403 - Access Denied</h1><p>Public access is not enabled for this bucket.</p></body></html>")))
			resp.Header.Set("Content-Type", "text/html")
		} else {
			ct := resp.Header.Get("Content-Type")
			if strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "video/") {
				resp.Header.Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				resp.Header.Set("Cache-Control", "public, max-age=3600")
			}
		}
		resp.Header.Set("X-Cache", "MISS")
		return nil
	}
	
	// If it's a GET request without Range, we can cache it
	if r.Method == "GET" && r.Header.Get("Range") == "" {
		cw := &cachingResponseWriter{ResponseWriter: w, statusCode: 200}
		proxy.ServeHTTP(cw, r)
		
		recordHit(actualBucket, cw.statusCode, int64(cw.buf.Len()), false, r.Header.Get("CF-IPCountry"), "read")

		// Cache files under 10MB
		if cw.statusCode == 200 && cw.buf.Len() < 10*1024*1024 {
			cdnCache.Store(targetPath, &cacheEntry{
				Body:       cw.buf.Bytes(),
				Headers:    cw.Header().Clone(),
				StatusCode: cw.statusCode,
				ExpiresAt:  time.Now().Add(1 * time.Hour),
			})
		}
	} else {
		tw := &trackingResponseWriter{ResponseWriter: w, statusCode: 200}
		proxy.ServeHTTP(tw, r)
		recordHit(actualBucket, tw.statusCode, tw.bytes, false, r.Header.Get("CF-IPCountry"), "read")
	}
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
			scyllaSession.Query(`SELECT bucket_display_name FROM cosmos.users WHERE service='oort' AND email=?`, ak).Scan(&disp)
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
	
	// Check Cache for GET requests without Range
	if r.Method == "GET" && r.Header.Get("Range") == "" {
		if val, ok := cdnCache.Load(targetPath); ok {
			entry := val.(*cacheEntry)
			if time.Now().Before(entry.ExpiresAt) {
				for k, v := range entry.Headers {
					w.Header()[k] = v
				}
				w.Header().Set("X-Cache", "HIT")
				w.WriteHeader(entry.StatusCode)
				w.Write(entry.Body)
				recordHit(bucket, entry.StatusCode, int64(len(entry.Body)), true, r.Header.Get("CF-IPCountry"), "read")
				return
			}
			cdnCache.Delete(targetPath)
		}
	}

	targetUrl, _ := url.Parse("http://127.0.0.1:9000")
	proxy := httputil.NewSingleHostReverseProxy(targetUrl)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = targetUrl.Scheme
		req.URL.Host = targetUrl.Host
		req.URL.Path = targetPath
		req.Host = targetUrl.Host
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		statusCode := resp.StatusCode
		var bodyBytes []byte
		if r.Method == "GET" && r.Header.Get("Range") == "" {
			if resp.ContentLength > 0 && resp.ContentLength < 10*1024*1024 {
				bodyBytes, _ = io.ReadAll(resp.Body)
				resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
				if statusCode == 200 {
					headersCopy := make(http.Header)
					for k, v := range resp.Header {
						headersCopy[k] = v
					}
					cdnCache.Store(targetPath, &cacheEntry{
						Body:       bodyBytes,
						Headers:    headersCopy,
						StatusCode: statusCode,
						ExpiresAt:  time.Now().Add(5 * time.Minute),
					})
					resp.Header.Set("X-Cache", "MISS")
				}
			}
		}
		respBytes := resp.ContentLength
		if respBytes < 0 {
			respBytes = int64(len(bodyBytes))
		}
		recordHit(bucket, statusCode, respBytes, false, r.Header.Get("CF-IPCountry"), "read")
		return nil
	}
	proxy.ServeHTTP(w, r)
}
