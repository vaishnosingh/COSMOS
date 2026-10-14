# 🌌 COSMOS Architecture & Technical Specifications

Cosmos is an open-source, self-hosted cloud platform engineered to transform ordinary consumer hardware or retired office computers into an enterprise-class personal workstation and private cloud.

---

## 🏗️ High-Level Topology

```
                  ┌──────────────────────────────────────────────┐
                  │                 Client / Web                 │
                  └──────────────────────┬───────────────────────┘
                                         │ HTTPS / SSL (:443)
                                         ▼
                  ┌──────────────────────────────────────────────┐
                  │              Nginx Reverse Proxy             │
                  └──────┬────────────────┬───────────────┬──────┘
                         │                │               │
               HTTP /    │     HTTP /     │    HTTP /     │
             Portals     │     Gitea      │    MinIO      │
              (:8080)    │    (:3000)     │   (:9000)     │
                         ▼                ▼               ▼
                 ┌───────────────┐ ┌─────────────┐ ┌─────────────┐
                 │  Cosmos Core  │ │ Gitea Forge │ │  MinIO S3   │
                 │   (Go App)    │ │ (Git Engine)│ │(Object Store│
                 └───────┬───────┘ └─────────────┘ └──────┬──────┘
                         │                                │
                         │ CQL (:9042)                    │ Local Storage
                         ▼                                ▼
                 ┌───────────────┐                 ┌─────────────┐
                 │   ScyllaDB    │                 │ Local Disks │
                 │ (NoSQL Store) │                 │(NVMe / HDD) │
                 └───────────────┘                 └─────────────┘
```

---

## 🧩 Core Architectural Components

### 1. Unified Identity & Application Core (`portals-go`)
* **Language:** Go (Golang) 1.22+
* **Serving:** Native `net/http` server with embedded HTML/CSS templates via `embed.FS`.
* **Single Sign-On (SSO):** A centralized identity provider managing accounts across S3 Storage and Git Source Control.
* **Security Model:**
  * Passwords hashed with `golang.org/x/crypto/bcrypt`.
  * Temporary registration passwords stored in transient, self-expiring memory structures (`sync.Map`) only during automated provisioning—never stored plain-text in database storage.
  * Session identification using cryptographically random UUIDs stored in HTTP-only cookies and ScyllaDB secondary index lookups.

### 2. Distributed Persistence Layer (`ScyllaDB`)
* **Database Engine:** ScyllaDB 5.x / Apache Cassandra compatibility mode.
* **Protocol:** CQL native binary protocol over port `9042`.
* **Keyspace Model:**
  * `cosmos.users`: Master user directory holding bcrypt hashes, S3 credentials, display metadata, and active session tokens.
  * `cosmos.pending`: Ephemeral registration tokens and OTP codes.
  * `cosmos.password_reset_tokens`: Time-expiring password reset tokens.
  * `cosmos.buckets`: User bucket metadata, ownership tracking, and friendly aliases.
  * `cosmos.bucket_domains`: Vanity domain and CNAME mapping for public bucket serving.
  * `cosmos.quota_requests`: Administrative quota elevation queue.
  * `cosmos.s3_analytics` & `cosmos.s3_analytics_hourly`: Telemetry tracking counters.

### 3. High-Performance Object Storage (`MinIO S3`)
* **Engine:** MinIO S3-compatible object storage daemon.
* **Automated Tenant Provisioning:**
  * Upon account activation, the Cosmos core automatically invokes the MinIO Client (`mc`) to provision dedicated IAM users.
  * Each user is constrained by an isolated AWS IAM policy restricting `s3:*` actions solely to their assigned bucket ARN (`arn:aws:s3:::cosmos-<id>/*`).
  * Automatic object versioning is enabled by default to prevent accidental data loss.
  * Storage quotas (default 10GB, expandable via admin approval) are enforced at the bucket level.

### 4. Git Engine & Source Control (`Gitea`)
* **Engine:** Gitea self-hosted Git service running on port `3000`.
* **Authentication Sync:**
  * Cosmos manages registration and credential synchronization via CLI/API hooks (`gitea admin user create`, `gitea admin user change-password`).
  * Auto-healing authentication logic: if credentials fall out of sync during login, Cosmos automatically re-synchronizes the bcrypt hash with Gitea's internal database and retries transparently.

### 5. Real-Time Telemetry & Observability
* **In-Memory Accumulator:** High-throughput `atomic.Int64` counters for request counts, bytes transferred, and status code categories (2xx, 4xx, 5xx).
* **Periodic Flushing:** Background goroutine flushes aggregated metrics to ScyllaDB hourly tables (`cosmos.s3_analytics_hourly`).
* **Visual Telemetry:** Interactive 3D rotating orthographic globe rendered on HTML5 Canvas using vector latitude/longitude projections, simulating real-time global traffic node activity.

---

## 🔒 Security Hardening

1. **Least-Privilege Storage Isolation:**
   Users can never view or manipulate storage belonging to other accounts. Every S3 access request is validated against ScyllaDB session keys and verified via MinIO IAM policies.
2. **Environment Variable Decoupling:**
   All sensitive endpoints, administrative emails, domain names, and database credentials are fully externalized via `.env` configurations.
3. **Transient Memory Buffering:**
   Credentials required for downstream provisioning (such as Gitea account creation) exist in memory for a maximum 15-minute window before automatic garbage collection.
