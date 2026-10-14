# 🛠️ COSMOS Setup & Deployment Guide

This guide details how to turn an old desktop PC, home server, or cloud instance into a fully functional Cosmos workstation.

---

## 💻 Hardware Prerequisites

| Requirement | Minimum | Recommended |
|---|---|---|
| **CPU** | 2 cores (x86_64) | 4+ cores |
| **RAM** | 4 GB | 8 GB or more |
| **Storage** | 30 GB available | SSD / NVMe drive for ScyllaDB + HDD for bulk S3 storage |
| **OS** | Debian 12 / Ubuntu 22.04 LTS | Debian 12 Minimal |

---

## 🚀 Option 1: Quick Start with Docker Compose (Recommended)

The fastest method to deploy the entire stack on an old PC is using Docker and Docker Compose.

### Step 1: Install Docker & Docker Compose
```bash
sudo apt update && sudo apt install -y curl git docker.io docker-compose
sudo systemctl enable --now docker
```

### Step 2: Clone & Configure
```bash
git clone https://github.com/vaishnosingh/COSMOS.git cosmos
cd cosmos

# Copy and configure environment variables
cp .env.example .env
nano .env
```

### Step 3: Launch Stack
```bash
cd deploy
docker-compose up -d
```

### Step 4: Initialize ScyllaDB Schema
Wait 15-20 seconds for ScyllaDB to start, then execute:
```bash
docker exec -i cosmos-scylla cqlsh < schema.cql
```

Access your workstation portal at `http://<your-pc-ip>:8080`.

---

## 🖥️ Option 2: Bare-Metal Native Setup

For maximum performance on hardware with limited RAM, install the native binaries directly on the host system.

### 1. Install Prerequisites
```bash
sudo apt update && sudo apt install -y golang git nginx curl msmtp ca-certificates
```

### 2. Install MinIO & MinIO Client (mc)
```bash
# MinIO Server
wget https://dl.min.io/server/minio/release/linux-amd64/minio
chmod +x minio && sudo mv minio /usr/local/bin/

# MinIO Client
wget https://dl.min.io/client/mc/release/linux-amd64/mc
chmod +x mc && sudo mv mc /usr/local/bin/
```

Start MinIO as a systemd service or background daemon:
```bash
export MINIO_ROOT_USER=minioadmin
export MINIO_ROOT_PASSWORD=your_secure_password
minio server /var/lib/minio/data --console-address ":9001" &
```

### 3. Install ScyllaDB or Apache Cassandra
```bash
sudo apt-key adv --keyserver keyserver.ubuntu.com --recv-keys 5e08f18826677713
sudo curl -s https://repositories.scylladb.com/scylla/repo/deb/debian/scylla-5.4.list -o /etc/apt/sources.list.d/scylla.list
sudo apt update && sudo apt install -y scylla-server
sudo systemctl enable --now scylla-server
```

Apply the database schema:
```bash
cqlsh -f deploy/schema.cql
```

### 4. Install Gitea
```bash
sudo wget -O /usr/local/bin/gitea https://dl.gitea.com/gitea/1.21.11/gitea-1.21.11-linux-amd64
sudo chmod +x /usr/local/bin/gitea
```

### 5. Build and Launch Cosmos
```bash
cd /opt/cosmos
go build -o portals .
sudo cp deploy/cosmos.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cosmos
```

---

## 🌐 Production Domain & SSL (Nginx + Certbot)

To access your workstation securely across the internet:

### 1. Configure Nginx
```bash
sudo cp deploy/nginx.conf /etc/nginx/sites-available/cosmos.conf
sudo ln -s /etc/nginx/sites-available/cosmos.conf /etc/nginx/sites-enabled/
# Edit server_name to match your registered domain:
sudo nano /etc/nginx/sites-available/cosmos.conf
```

### 2. Generate Free SSL Certificate
```bash
sudo apt install -y certbot python3-certbot-nginx
sudo certbot --nginx -d yourdomain.com -d *.yourdomain.com
sudo systemctl restart nginx
```

---

## 🔍 Verification Checklist

- [ ] **Portal Home:** Visit `https://yourdomain.com/` (or `http://localhost:8080/`).
- [ ] **Account Creation:** Sign up for an account and check OTP verification.
- [ ] **Storage Console:** Open `/oort/console` and test uploading/downloading a file.
- [ ] **Git Engine:** Open `/source/` to inspect repositories and commit via SSH/HTTPS.
- [ ] **Live Telemetry:** Open the Analytics tab to view real-time request tracking and the 3D globe.
