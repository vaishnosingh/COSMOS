<div align="center">

# 🌌 COSMOS

### *Turn your old PC into a personal cloud workstation.*

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat&logo=go)](https://golang.org)
[![Storage](https://img.shields.io/badge/S3-MinIO-C72C48?style=flat&logo=minio)](https://min.io)
[![Database](https://img.shields.io/badge/Database-ScyllaDB-55449A?style=flat)](https://www.scylladb.com)
[![Git](https://img.shields.io/badge/Git-Gitea-609926?style=flat&logo=gitea)](https://gitea.com)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

<br/>

**COSMOS** is a self-hosted cloud workstation operating system that transforms any spare PC, repurposed office desktop, or home server into an enterprise-grade personal cloud. It combines S3-compatible object storage, private Git repositories, edge CDN caching, and real-time 3D observability under a unified single-sign-on (SSO) experience.

[Architecture](docs/ARCHITECTURE.md) • [Deployment Guide](docs/SETUP_GUIDE.md) • [Schema](deploy/schema.cql) • [Report Issue](https://github.com/vaishnosingh/COSMOS/issues)

</div>

---

## ⚡ Highlights & Capabilities

### 🌌 Unified Workstation SSO
* Single credentials grant seamless access across all workstation tools.
* Account registration with automated OTP email verification and secure self-service password recovery.
* Zero plain-text credentials stored in database storage; bcrypt-salted authentication throughout.

### 🗄️ Oort Cloud Storage (S3 Engine)
* **S3 Compatible:** Direct API compatibility with AWS CLI, SDKs, Cyberduck, and rclone.
* **Automated Multi-Tenancy:** Each registered user automatically gets their own isolated bucket, credentials, and custom AWS IAM access policy.
* **Full Console UI:** Browse, upload, download, generate presigned URLs, inspect versions, and manage access policies directly in the browser.
* **Custom Vanity Domains:** Map arbitrary external domains (CNAME) directly to specific buckets.

### 🧑‍💻 Source Forge (Self-Hosted Git)
* Powered by lightweight Gitea engine.
* Instant repository creation, pull requests, issue tracking, and SSH/HTTPS Git clones.
* Automated credential synchronization with the Cosmos master identity directory.

### ⚡ Edge CDN & In-Memory Caching
* High-speed in-memory cache layer (`sync.Map`) for frequently accessed objects.
* Standard HTTP caching semantics: conditional ETags (`If-None-Match`), `304 Not Modified` responses, and cache-purge queries.

### 📊 Real-time 3D Telemetry Dashboard
* Live operational metrics for request volume, cache hit ratios, bandwidth consumption, and error rates.
* Interactive 3D rotating orthographic globe rendered on HTML5 Canvas visualizing global network activity.
* Granular hourly time-series stored in ScyllaDB for historical analytics.

---

## 🏗️ System Architecture

```
                          ┌───────────────────────────┐
                          │    Browser / Developer    │
                          └─────────────┬─────────────┘
                                        │ HTTPS (:443)
                                        ▼
                          ┌───────────────────────────┐
                          │    Nginx Reverse Proxy    │
                          └──────┬────────────┬───────┘
                                 │            │
                       Portals   │            │  Git Forge
                       (:8080)   │            │  (:3000)
                                 ▼            ▼
                         ┌──────────────┐ ┌──────────┐
                         │ Cosmos Core  │ │  Gitea   │
                         │   (Golang)   │ │  Engine  │
                         └───────┬──────┘ └──────────┘
                                 │
                   ┌─────────────┴─────────────┐
                   │                           │
                   ▼                           ▼
            ┌──────────────┐            ┌──────────────┐
            │   ScyllaDB   │            │   MinIO S3   │
            │ (CQL Store)  │            │(Object Store)│
            └──────────────┘            └──────────────┘
```

---

## 🚀 Quick Start (Docker Compose)

Get up and running on any PC with Docker in under 5 minutes:

```bash
# 1. Clone repository
git clone https://github.com/vaishnosingh/COSMOS.git
cd COSMOS

# 2. Configure environment
cp .env.example .env

# 3. Launch the complete stack
cd deploy
docker-compose up -d

# 4. Initialize database schema
docker exec -i cosmos-scylla cqlsh < schema.cql
```

Navigate to `http://localhost:8080` in your web browser.

For complete bare-metal setup instructions, systemd unit files, and Nginx SSL configuration, see the [Deployment Guide](docs/SETUP_GUIDE.md).

---

## 📁 Repository Structure

```
.
├── deploy/
│   ├── docker-compose.yml    # Full-stack container orchestration
│   ├── Dockerfile            # Multi-stage lightweight application build
│   ├── nginx.conf            # Production reverse proxy & SSL config
│   ├── cosmos.service        # Systemd service unit definition
│   └── schema.cql            # ScyllaDB keyspace & tables DDL
├── docs/
│   ├── ARCHITECTURE.md       # In-depth technical architecture
│   └── SETUP_GUIDE.md        # Step-by-step bare-metal & cloud setup
├── templates/                # Responsive HTML/CSS workstation portals
├── config.go                 # Dynamic environment configuration
├── main.go                   # HTTP server, auth, and workstation router
├── oortconsole.go            # S3 console API & MinIO client bridges
├── oortanalytics.go          # Telemetry engine & ScyllaDB aggregations
├── s3cache.go                # In-memory edge caching middleware
├── .env.example              # Sample environment configuration
├── go.mod                    # Go dependencies definition
└── LICENSE                   # MIT Open Source License
```

---

## 🔒 Security & Privacy

Cosmos is designed from the ground up for strict privacy and data ownership:
* **All data stays on your machine.** No telemetry, telemetry pings, or cloud reporting sent to third parties.
* **No hardcoded credentials.** Passwords, domains, secret keys, and tokens are read exclusively from environment variables or encrypted databases.
* **Per-user IAM Isolation.** Users cannot inspect or access other tenants' buckets or resources.

---

## 📜 License

Distributed under the MIT License. See [LICENSE](LICENSE) for more information.
