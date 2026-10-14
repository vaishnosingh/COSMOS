package main

import (
	"os"
	"strings"
)

// getCosmosDomain returns the configured domain or fallback "localhost".
func getCosmosDomain() string {
	if d := os.Getenv("COSMOS_DOMAIN"); d != "" {
		return strings.TrimSpace(d)
	}
	return "localhost"
}

// getCosmosBaseURL returns the full base URL including scheme (e.g. "https://example.com" or "http://localhost").
func getCosmosBaseURL() string {
	if u := os.Getenv("COSMOS_BASE_URL"); u != "" {
		return strings.TrimRight(strings.TrimSpace(u), "/")
	}
	d := getCosmosDomain()
	if strings.HasPrefix(d, "localhost") || strings.HasPrefix(d, "127.0.0.1") {
		return "http://" + d
	}
	return "https://" + d
}

// getEmailFrom returns the sender address for transactional emails.
func getEmailFrom() string {
	if f := os.Getenv("COSMOS_EMAIL_FROM"); f != "" {
		return strings.TrimSpace(f)
	}
	d := getCosmosDomain()
	return "noreply@" + d
}

// getAdminEmail returns the administrator email address.
func getAdminEmail() string {
	if a := os.Getenv("COSMOS_ADMIN_EMAIL"); a != "" {
		return strings.TrimSpace(a)
	}
	return "admin@example.com"
}

// getPublicMinioEndpoint returns the public endpoint for S3 access.
func getPublicMinioEndpoint() string {
	if ep := os.Getenv("COSMOS_PUBLIC_ENDPOINT"); ep != "" {
		return strings.TrimSpace(ep)
	}
	return getCosmosDomain()
}

// getMinioRootUser returns MinIO root username from env with default.
func getMinioRootUser() string {
	if u := os.Getenv("MINIO_ROOT_USER"); u != "" {
		return strings.TrimSpace(u)
	}
	return "minioadmin"
}

// getMinioRootPassword returns MinIO root password from env with default.
func getMinioRootPassword() string {
	if p := os.Getenv("MINIO_ROOT_PASSWORD"); p != "" {
		return strings.TrimSpace(p)
	}
	return "minioadmin"
}
