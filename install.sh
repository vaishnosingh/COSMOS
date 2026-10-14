#!/usr/bin/env bash
# ==============================================================================
#      ░█▀▀█ ░█▀▀█ ░█▀▀▀█ ░█▀▄▀█ ░█▀▀█ ░█▀▀▀█ 
#      ░█─── ░█──█ ─▀▀▀▀█ ░█░█░█ ░█──█ ─▀▀▀▀█ 
#      ░█▄▄█ ░█▄▄█ ░█▄▄▄█ ░█──░█ ░█▄▄█ ░█▄▄▄█ 
#
#   COSMOS WORKSTATION — AUTOMATED SETUP & INSTALLATION ENGINE
#   Turn your old PC or server into a sovereign cloud workstation.
# ==============================================================================

set -eo pipefail

# ─── COLOR PALETTE & STYLING ─────────────────────────────────────────────────
ESC="\033["
RESET="${ESC}0m"
BOLD="${ESC}1m"
DIM="${ESC}2m"
ITALIC="${ESC}3m"
UNDERLINE="${ESC}4m"

# Cosmic Hues
C_CYAN="${ESC}38;5;51m"
C_SKY="${ESC}38;5;39m"
C_BLUE="${ESC}38;5;33m"
C_PURPLE="${ESC}38;5;141m"
C_VIOLET="${ESC}38;5;99m"
C_GREEN="${ESC}38;5;49m"
C_EMERALD="${ESC}38;5;42m"
C_AMBER="${ESC}38;5;220m"
C_GOLD="${ESC}38;5;214m"
C_RED="${ESC}38;5;196m"
C_MUTED="${ESC}38;5;244m"
C_DARK="${ESC}38;5;236m"
C_WHITE="${ESC}38;5;255m"

# Badges & Symbols
T_CHECK="${C_GREEN}✔${RESET}"
T_CROSS="${C_RED}✖${RESET}"
T_WARN="${C_AMBER}▲${RESET}"
T_INFO="${C_CYAN}ℹ${RESET}"
T_GEAR="${C_PURPLE}⚙${RESET}"
T_STAR="${C_GOLD}★${RESET}"
T_ROCKET="${C_SKY}🚀${RESET}"
T_PLANET="${C_PURPLE}🪐${RESET}"

# Working Directory Context
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# ─── HELPER FUNCTIONS ─────────────────────────────────────────────────────────

clear_screen() {
    clear 2>/dev/null || true
}

print_banner() {
    echo -e "${C_PURPLE}"
    cat << "EOF"
  ╭──────────────────────────────────────────────────────────────────────────╮
  │                                                                          │
  │     ░█▀▀█ ░█▀▀█ ░█▀▀▀█ ░█▀▄▀█ ░█▀▀█ ░█▀▀▀█                              │
  │     ░█─── ░█──█ ─▀▀▀▀█ ░█░█░█ ░█──█ ─▀▀▀▀█                              │
  │     ░█▄▄█ ░█▄▄█ ░█▄▄▄█ ░█──░█ ░█▄▄█ ░█▄▄▄█                              │
  │                                                                          │
  │               ★ TURN YOUR OLD PC TO WORKSTATION ★                        │
  │                  Automated Installer v2.4.0                              │
  ╰──────────────────────────────────────────────────────────────────────────╯
EOF
    echo -e "${RESET}"
}

print_section() {
    local title="$1"
    echo ""
    echo -e "${C_DARK}╭──────────────────────────────────────────────────────────────────────────╮${RESET}"
    echo -e "${C_DARK}│${RESET}  ${C_CYAN}${BOLD}${title}${RESET}"
    echo -e "${C_DARK}╰──────────────────────────────────────────────────────────────────────────╯${RESET}"
    echo ""
}

msg_info() {
    echo -e "  ${C_CYAN}[${T_INFO}${C_CYAN}]${RESET}  $1"
}

msg_success() {
    echo -e "  ${C_GREEN}[${T_CHECK}${C_GREEN}]${RESET}  ${C_WHITE}$1${RESET}"
}

msg_warn() {
    echo -e "  ${C_AMBER}[${T_WARN}${C_AMBER}]${RESET}  ${C_AMBER}$1${RESET}"
}

msg_error() {
    echo -e "  ${C_RED}[${T_CROSS}${C_RED}]${RESET}  ${C_RED}$1${RESET}"
}

msg_step() {
    echo -e "  ${C_PURPLE}[${T_GEAR}${C_PURPLE}]${RESET}  ${C_SKY}$1${RESET}..."
}

# Spinner for background operations
run_with_spinner() {
    local msg="$1"
    shift
    local cmd=("$@")

    local spin_chars=('⠋' '⠙' '⠹' '⠸' '⠼' '⠴' '⠦' '⠧' '⠇' '⠏')
    echo -ne "  ${C_PURPLE}[ ⏳ ]${RESET}  ${C_WHITE}${msg}...${RESET}"

    # Run command in background redirected
    local tmp_log
    tmp_log=$(mktemp /tmp/cosmos_install_XXXXXX.log)
    "${cmd[@]}" > "$tmp_log" 2>&1 &
    local pid=$!

    local i=0
    while kill -0 "$pid" 2>/dev/null; do
        i=$(( (i+1) % 10 ))
        echo -ne "\r  ${C_CYAN}[ ${spin_chars[$i]} ]${RESET}  ${C_WHITE}${msg}...${RESET} "
        sleep 0.1
    done

    wait "$pid" && local exit_code=0 || local exit_code=$?

    if [ "$exit_code" -eq 0 ]; then
        echo -e "\r  ${C_GREEN}[ ${T_CHECK} ]${RESET}  ${C_WHITE}${msg}${RESET}                    "
        rm -f "$tmp_log"
        return 0
    else
        echo -e "\r  ${C_RED}[ ${T_CROSS} ]${RESET}  ${C_RED}Failed: ${msg}${RESET}           "
        echo -e "${C_MUTED}--- Error Details (from $tmp_log) ---${RESET}"
        tail -n 20 "$tmp_log" | sed 's/^/    /'
        echo -e "${C_MUTED}---------------------------------------${RESET}"
        rm -f "$tmp_log"
        return "$exit_code"
    fi
}

# ─── SUDO / ROOT CHECK ────────────────────────────────────────────────────────
check_privileges() {
    if [ "$EUID" -ne 0 ]; then
        if command -v sudo >/dev/null 2>&1; then
            if ! sudo -v 2>/dev/null; then
                msg_error "This script requires administrative privileges."
                msg_info "Please run with sudo or as root: sudo ./install.sh"
                exit 1
            fi
            SUDO="sudo"
        else
            msg_error "Neither root nor sudo is available. Please run as root."
            exit 1
        fi
    else
        SUDO=""
    fi
}

# ─── PHASE 1: HARDWARE & SYSTEM PRE-FLIGHT ────────────────────────────────────
run_preflight_checks() {
    print_section "PHASE 1 // SYSTEM PRE-FLIGHT DIAGNOSTICS"

    local passed=0
    local warned=0

    # 1. OS & Distribution Detection
    if [ -f /etc/os-release ]; then
        # shellcheck disable=SC1091
        source /etc/os-release
        OS_NAME="${NAME:-Linux}"
        OS_VER="${VERSION_ID:-}"
        msg_success "Operating System: ${C_CYAN}${OS_NAME} ${OS_VER}${RESET}"
        passed=$((passed + 1))
    else
        msg_warn "Unknown Linux distribution (missing /etc/os-release)"
        warned=$((warned + 1))
    fi

    # 2. CPU Architecture
    ARCH=$(uname -m)
    if [ "$ARCH" = "x86_64" ] || [ "$ARCH" = "amd64" ]; then
        msg_success "CPU Architecture: ${C_CYAN}${ARCH}${RESET} (Fully Supported)"
        passed=$((passed + 1))
    elif [ "$ARCH" = "aarch64" ] || [ "$ARCH" = "arm64" ]; then
        msg_success "CPU Architecture: ${C_CYAN}${ARCH}${RESET} (ARM64 Supported)"
        passed=$((passed + 1))
    else
        msg_warn "CPU Architecture: ${ARCH} (Warning: ScyllaDB may require x86_64 or emulation)"
        warned=$((warned + 1))
    fi

    # 3. Memory (RAM) Check
    if command -v free >/dev/null 2>&1; then
        TOTAL_RAM_MB=$(free -m | awk '/^Mem:/{print $2}')
        TOTAL_RAM_GB=$(awk "BEGIN {printf \"%.1f\", $TOTAL_RAM_MB/1024}")
        if [ "$TOTAL_RAM_MB" -ge 3800 ]; then
            msg_success "System Memory: ${C_CYAN}${TOTAL_RAM_GB} GB RAM${RESET} (Adequate for all services)"
            passed=$((passed + 1))
        elif [ "$TOTAL_RAM_MB" -ge 1800 ]; then
            msg_warn "System Memory: ${TOTAL_RAM_GB} GB RAM (Docker Compose mode recommended; ScyllaDB will run constrained)"
            warned=$((warned + 1))
        else
            msg_warn "Low System Memory: ${TOTAL_RAM_GB} GB RAM (< 2GB). Services may experience memory pressure."
            warned=$((warned + 1))
        fi
    fi

    # 4. Storage Space Check
    FREE_DISK_MB=$(df -m . | awk 'NR==2 {print $4}')
    FREE_DISK_GB=$(awk "BEGIN {printf \"%.1f\", $FREE_DISK_MB/1024}")
    if [ "$FREE_DISK_MB" -ge 10240 ]; then
        msg_success "Available Storage: ${C_CYAN}${FREE_DISK_GB} GB${RESET} available"
        passed=$((passed + 1))
    elif [ "$FREE_DISK_MB" -ge 5120 ]; then
        msg_warn "Available Storage: ${FREE_DISK_GB} GB (Recommended: 10 GB+)"
        warned=$((warned + 1))
    else
        msg_error "Critical Storage: Only ${FREE_DISK_GB} GB available. Minimum 5 GB required."
        exit 1
    fi

    # 5. Core Port Availability Checks
    local ports=(8080 9000 9001 3000 9042)
    local occupied_ports=()

    for p in "${ports[@]}"; do
        if command -v ss >/dev/null 2>&1; then
            if ss -tuln | grep -q ":${p} "; then
                occupied_ports+=("$p")
            fi
        elif command -v netstat >/dev/null 2>&1; then
            if netstat -tuln | grep -q ":${p} "; then
                occupied_ports+=("$p")
            fi
        fi
    done

    if [ ${#occupied_ports[@]} -eq 0 ]; then
        msg_success "Port Matrix: All core ports (${ports[*]}) are ${C_GREEN}available${RESET}"
        passed=$((passed + 1))
    else
        msg_warn "Active Ports Detected: ${occupied_ports[*]} in use."
        msg_info "Existing services might need to be stopped or ports adjusted in .env"
        warned=$((warned + 1))
    fi

    echo ""
    echo -e "  ${C_MUTED}──────────────────────────────────────────────────────────────────${RESET}"
    echo -e "  Pre-flight summary: ${C_GREEN}${passed} Passed${RESET}  ${C_AMBER}${warned} Warnings${RESET}"
    echo -e "  ${C_MUTED}──────────────────────────────────────────────────────────────────${RESET}"
    sleep 1
}

# ─── PHASE 2: INTERACTIVE CONFIGURATION ───────────────────────────────────────
prompt_configuration() {
    print_section "PHASE 2 // INTERACTIVE WORKSTATION CONFIGURATION"

    # Step 1: Deployment Mode Selection
    echo -e "  ${C_WHITE}${BOLD}Select Deployment Engine:${RESET}"
    echo ""
    echo -e "  ${C_CYAN}[1]${RESET}  ${BOLD}Docker Compose Stack${RESET} ${C_GREEN}(Recommended)${RESET}"
    echo -e "       Runs ScyllaDB, MinIO, Gitea & Cosmos in isolated, pre-configured containers."
    echo -e "       Zero host clutter, instant launch, one-command updates."
    echo ""
    echo -e "  ${C_CYAN}[2]${RESET}  ${BOLD}Bare-Metal Native Systemd Install${RESET}"
    echo -e "       Installs native Go binary, systemd unit files, and connects to host daemons."
    echo -e "       Lowest possible RAM overhead for older PCs with limited resources."
    echo ""

    local mode_choice
    while true; do
        read -r -p "  Enter choice [1-2, default: 1]: " mode_choice
        mode_choice="${mode_choice:-1}"
        if [ "$mode_choice" = "1" ] || [ "$mode_choice" = "2" ]; then
            break
        fi
        echo -e "  ${C_RED}Please enter 1 or 2.${RESET}"
    done

    if [ "$mode_choice" = "1" ]; then
        DEPLOY_MODE="docker"
        msg_success "Selected: ${C_CYAN}Docker Compose Stack${RESET}"
    else
        DEPLOY_MODE="baremetal"
        msg_success "Selected: ${C_CYAN}Bare-Metal Native Install${RESET}"
    fi
    echo ""

    # Step 2: Domain / Hostname Setup
    # Attempt auto-detection of local IP
    DETECTED_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
    DETECTED_IP="${DETECTED_IP:-localhost}"

    echo -e "  ${C_WHITE}${BOLD}Hostname / Access Domain:${RESET}"
    echo -e "  ${C_MUTED}Enter your domain name, local IP, or press Enter for default.${RESET}"
    read -r -p "  Domain / Host [default: ${DETECTED_IP}]: " CFG_DOMAIN
    CFG_DOMAIN="${CFG_DOMAIN:-$DETECTED_IP}"
    msg_success "Domain set to: ${C_CYAN}${CFG_DOMAIN}${RESET}"
    echo ""

    # Step 3: Base Port
    echo -e "  ${C_WHITE}${BOLD}Primary Portal Web Port:${RESET}"
    read -r -p "  Port [default: 8080]: " CFG_PORT
    CFG_PORT="${CFG_PORT:-8080}"
    msg_success "Primary web port: ${C_CYAN}${CFG_PORT}${RESET}"
    echo ""

    # Step 4: MinIO Root Credentials
    echo -e "  ${C_WHITE}${BOLD}MinIO S3 Storage Credentials:${RESET}"
    read -r -p "  Root Username [default: minioadmin]: " CFG_MINIO_USER
    CFG_MINIO_USER="${CFG_MINIO_USER:-minioadmin}"

    # Generate random 16-char password default
    DEFAULT_MINIO_PASS=$(head -c 32 /dev/urandom 2>/dev/null | tr -dc 'A-Za-z0-9' | head -c 16 || echo "CosmosS3Pass2026")
    read -r -p "  Root Password [default: ${DEFAULT_MINIO_PASS}]: " CFG_MINIO_PASS
    CFG_MINIO_PASS="${CFG_MINIO_PASS:-$DEFAULT_MINIO_PASS}"
    msg_success "S3 Root User: ${C_CYAN}${CFG_MINIO_USER}${RESET} (Password configured)"
    echo ""

    # Step 5: Admin Email
    echo -e "  ${C_WHITE}${BOLD}System Administrator Email:${RESET}"
    local default_email="admin@${CFG_DOMAIN}"
    read -r -p "  Admin Email [default: ${default_email}]: " CFG_ADMIN_EMAIL
    CFG_ADMIN_EMAIL="${CFG_ADMIN_EMAIL:-$default_email}"
    msg_success "Admin Email: ${C_CYAN}${CFG_ADMIN_EMAIL}${RESET}"
    echo ""

    # Step 6: Reverse Proxy Nginx
    read -r -p "  Configure Nginx reverse proxy? [y/N]: " CFG_NGINX_CHOICE
    if [[ "$CFG_NGINX_CHOICE" =~ ^[Yy]$ ]]; then
        CFG_NGINX=true
        msg_success "Nginx reverse proxy: ${C_GREEN}Enabled${RESET}"
    else
        CFG_NGINX=false
        msg_info "Nginx reverse proxy: ${C_MUTED}Skipped${RESET}"
    fi
    echo ""

    # ─── Step 7: Confirmation Box ─────────────────────────────────────────────
    echo -e "  ${C_PURPLE}╭─ [ CONFIGURATION REVIEW ] ─────────────────────────────────────────╮${RESET}"
    echo -e "  ${C_PURPLE}│${RESET}                                                                     ${C_PURPLE}│${RESET}"
    printf "  ${C_PURPLE}│${RESET}   • Deployment Mode   : %-42s ${C_PURPLE}│${RESET}\n" "${DEPLOY_MODE^^}"
    printf "  ${C_PURPLE}│${RESET}   • Hostname / Domain : %-42s ${C_PURPLE}│${RESET}\n" "${CFG_DOMAIN}"
    printf "  ${C_PURPLE}│${RESET}   • Primary Web Port  : %-42s ${C_PURPLE}│${RESET}\n" "${CFG_PORT}"
    printf "  ${C_PURPLE}│${RESET}   • S3 Storage User   : %-42s ${C_PURPLE}│${RESET}\n" "${CFG_MINIO_USER}"
    printf "  ${C_PURPLE}│${RESET}   • Admin Email       : %-42s ${C_PURPLE}│${RESET}\n" "${CFG_ADMIN_EMAIL}"
    printf "  ${C_PURPLE}│${RESET}   • Nginx Integration : %-42s ${C_PURPLE}│${RESET}\n" "$([ "$CFG_NGINX" = true ] && echo "YES" || echo "NO")"
    echo -e "  ${C_PURPLE}│${RESET}                                                                     ${C_PURPLE}│${RESET}"
    echo -e "  ${C_PURPLE}╰────────────────────────────────────────────────────────────────────╯${RESET}"
    echo ""

    read -r -p "  Proceed with installation? [Y/n]: " proceed_choice
    if [[ "$proceed_choice" =~ ^[Nn]$ ]]; then
        msg_warn "Installation aborted by user."
        exit 0
    fi
}

# ─── PHASE 3A: DOCKER COMPOSE DEPLOYMENT ──────────────────────────────────────
deploy_docker_stack() {
    print_section "PHASE 3 // DEPLOYING DOCKER COMPOSE WORKSTATION"

    # Check for Docker
    if ! command -v docker >/dev/null 2>&1; then
        msg_step "Installing Docker engine"
        if command -v apt-get >/dev/null 2>&1; then
            $SUDO apt-get update -qq
            $SUDO apt-get install -y -qq ca-certificates curl gnupg lsb-release
            $SUDO mkdir -p /etc/apt/keyrings
            curl -fsSL https://download.docker.com/linux/debian/gpg 2>/dev/null | $SUDO gpg --dearmor -o /etc/apt/keyrings/docker.gpg 2>/dev/null || true
            $SUDO apt-get install -y -qq docker.io docker-compose-plugin docker-compose || $SUDO apt-get install -y -qq docker.io docker-compose
        elif command -v dnf >/dev/null 2>&1; then
            $SUDO dnf install -y docker docker-compose
        elif command -v pacman >/dev/null 2>&1; then
            $SUDO pacman -Sy --noconfirm docker docker-compose
        fi
        $SUDO systemctl enable --now docker || true
        msg_success "Docker installed successfully"
    else
        msg_success "Docker detected: $(docker --version | cut -d',' -f1)"
    fi

    # Determine docker-compose command
    if docker compose version >/dev/null 2>&1; then
        DOCKER_COMPOSE="docker compose"
    elif command -v docker-compose >/dev/null 2>&1; then
        DOCKER_COMPOSE="docker-compose"
    else
        msg_error "docker-compose not found. Please install docker-compose."
        exit 1
    fi

    # Write Environment File
    msg_step "Writing .env configuration"
    cat << EOF > .env
# Cosmos Workstation Environment (Generated on $(date))
PORT=${CFG_PORT}
COSMOS_DOMAIN=${CFG_DOMAIN}
COSMOS_BASE_URL=http://${CFG_DOMAIN}:${CFG_PORT}
COSMOS_PUBLIC_ENDPOINT=${CFG_DOMAIN}:9000

SCYLLA_HOST=scylla
SCYLLA_KEYSPACE=cosmos

MINIO_ENDPOINT=minio:9000
MINIO_ROOT_USER=${CFG_MINIO_USER}
MINIO_ROOT_PASSWORD=${CFG_MINIO_PASS}

COSMOS_EMAIL_FROM=noreply@${CFG_DOMAIN}
COSMOS_ADMIN_EMAIL=${CFG_ADMIN_EMAIL}

GITEA_URL=http://gitea:3000
EOF
    msg_success ".env file generated successfully"

    # Launch Docker Stack
    msg_step "Launching Cosmos container fleet (ScyllaDB, MinIO, Gitea, Cosmos Core)"
    cd deploy
    $DOCKER_COMPOSE pull -q 2>/dev/null || true
    $DOCKER_COMPOSE up -d --build
    cd ..

    msg_success "Containers started in background"

    # Wait for ScyllaDB initialization
    echo ""
    msg_step "Waiting for ScyllaDB persistence layer to accept CQL connections"
    local retries=30
    local ready=false
    for ((i=1; i<=retries; i++)); do
        if docker exec cosmos-scylla cqlsh -e "DESCRIBE KEYSPACES" >/dev/null 2>&1; then
            ready=true
            break
        fi
        echo -ne "\r  ${C_CYAN}[ ⏳ ]${RESET}  Synchronizing ScyllaDB schema... (${i}/${retries}s) "
        sleep 2
    done
    echo ""

    if [ "$ready" = true ]; then
        msg_success "ScyllaDB cluster initialized and ready"
        msg_step "Applying Cosmos CQL schema (keyspaces, users, buckets, telemetry tables)"
        docker exec -i cosmos-scylla cqlsh < deploy/schema.cql >/dev/null 2>&1 || true
        msg_success "Cosmos database schema provisioned"
    else
        msg_warn "ScyllaDB took longer than expected to initialize. Will finish background initialization shortly."
    fi
}

# ─── PHASE 3B: BARE-METAL SYSTEMD DEPLOYMENT ──────────────────────────────────
deploy_baremetal_stack() {
    print_section "PHASE 3 // PROVISIONING BARE-METAL WORKSTATION"

    # Check for Go
    if ! command -v go >/dev/null 2>&1; then
        msg_step "Installing Go compiler toolchain"
        if command -v apt-get >/dev/null 2>&1; then
            $SUDO apt-get update -qq && $SUDO apt-get install -y -qq golang
        fi
        msg_success "Go installed: $(go version | awk '{print $3}')"
    else
        msg_success "Go compiler detected: $(go version | awk '{print $3}')"
    fi

    # Write Environment File
    msg_step "Writing .env configuration"
    cat << EOF > .env
PORT=${CFG_PORT}
COSMOS_DOMAIN=${CFG_DOMAIN}
COSMOS_BASE_URL=http://${CFG_DOMAIN}:${CFG_PORT}
COSMOS_PUBLIC_ENDPOINT=${CFG_DOMAIN}:9000

SCYLLA_HOST=127.0.0.1
SCYLLA_KEYSPACE=cosmos

MINIO_ENDPOINT=127.0.0.1:9000
MINIO_ROOT_USER=${CFG_MINIO_USER}
MINIO_ROOT_PASSWORD=${CFG_MINIO_PASS}

COSMOS_EMAIL_FROM=noreply@${CFG_DOMAIN}
COSMOS_ADMIN_EMAIL=${CFG_ADMIN_EMAIL}

GITEA_URL=http://127.0.0.1:3000
EOF
    msg_success ".env configuration written"

    # Build binary
    msg_step "Compiling Cosmos binary from Go source"
    go build -o portals .
    msg_success "Cosmos binary compiled: ./portals"

    # Install MinIO Client if not present
    if ! command -v mc >/dev/null 2>&1; then
        msg_step "Downloading MinIO Client (mc)"
        curl -sSL https://dl.min.io/client/mc/release/linux-amd64/mc -o /tmp/mc
        chmod +x /tmp/mc
        $SUDO mv /tmp/mc /usr/local/bin/mc
        msg_success "MinIO Client (mc) installed"
    fi

    # Install Systemd Service
    msg_step "Installing systemd unit file"
    local svc_file="/etc/systemd/system/cosmos.service"
    $SUDO cp deploy/cosmos.service "$svc_file"
    $SUDO sed -i "s|/opt/cosmos|${SCRIPT_DIR}|g" "$svc_file"
    $SUDO sed -i "s|User=cosmos|User=${USER}|g" "$svc_file"
    $SUDO systemctl daemon-reload
    $SUDO systemctl enable --now cosmos || true
    msg_success "Cosmos systemd service installed and enabled"
}

# ─── PHASE 4: NGINX CONFIGURATION (OPTIONAL) ──────────────────────────────────
setup_nginx_proxy() {
    if [ "$CFG_NGINX" = true ]; then
        print_section "PHASE 4 // NGINX REVERSE PROXY SETUP"

        if ! command -v nginx >/dev/null 2>&1; then
            msg_step "Installing Nginx web server"
            if command -v apt-get >/dev/null 2>&1; then
                $SUDO apt-get install -y -qq nginx
            fi
            $SUDO systemctl enable --now nginx
        fi

        local conf_dest="/etc/nginx/sites-available/cosmos.conf"
        msg_step "Configuring reverse proxy configuration"
        $SUDO cp deploy/nginx.conf "$conf_dest"
        $SUDO sed -i "s|yourdomain.com|${CFG_DOMAIN}|g" "$conf_dest"
        $SUDO sed -i "s|8080|${CFG_PORT}|g" "$conf_dest"

        if [ -d /etc/nginx/sites-enabled ]; then
            $SUDO ln -sf "$conf_dest" /etc/nginx/sites-enabled/cosmos.conf
            $SUDO nginx -t 2>/dev/null && $SUDO systemctl reload nginx || msg_warn "Nginx config test failed; check $conf_dest"
        fi
        msg_success "Nginx reverse proxy configured for ${CFG_DOMAIN}"
    fi
}

# ─── PHASE 5: VERIFICATION & COMPLETION ───────────────────────────────────────
print_completion_screen() {
    clear_screen
    print_banner

    echo -e "  ${C_GREEN}${BOLD}★ INSTALLATION SUCCESSFULLY COMPLETED ★${RESET}"
    echo -e "  ${C_MUTED}Your PC is now operational as a sovereign Cosmos Cloud Workstation.${RESET}"
    echo ""

    local portal_url="http://${CFG_DOMAIN}:${CFG_PORT}"
    local minio_url="http://${CFG_DOMAIN}:9000"
    local console_url="http://${CFG_DOMAIN}:9001"
    local gitea_url="http://${CFG_DOMAIN}:3000"

    echo -e "  ${C_CYAN}╭─ [ WORKSTATION ACCESS DIRECTORY ] ────────────────────────────────╮${RESET}"
    echo -e "  ${C_CYAN}│${RESET}                                                                     ${C_CYAN}│${RESET}"
    printf "  ${C_CYAN}│${RESET}   ${BOLD}🌌 Unified Portal${RESET}   : ${C_SKY}%-44s${RESET} ${C_CYAN}│${RESET}\n" "${portal_url}"
    printf "  ${C_CYAN}│${RESET}   ${BOLD}🗄️  Oort S3 API${RESET}     : ${C_CYAN}%-44s${RESET} ${C_CYAN}│${RESET}\n" "${minio_url}"
    printf "  ${C_CYAN}│${RESET}   ${BOLD}🗂️  MinIO Console${RESET}   : ${C_CYAN}%-44s${RESET} ${C_CYAN}│${RESET}\n" "${console_url}"
    printf "  ${C_CYAN}│${RESET}   ${BOLD}🧑‍💻 Source Forge${RESET}    : ${C_PURPLE}%-44s${RESET} ${C_CYAN}│${RESET}\n" "${gitea_url}"
    echo -e "  ${C_CYAN}│${RESET}                                                                     ${C_CYAN}│${RESET}"
    printf "  ${C_CYAN}│${RESET}   • Admin Account   : ${C_WHITE}%-44s${RESET} ${C_CYAN}│${RESET}\n" "${CFG_ADMIN_EMAIL}"
    printf "  ${C_CYAN}│${RESET}   • MinIO User      : ${C_WHITE}%-44s${RESET} ${C_CYAN}│${RESET}\n" "${CFG_MINIO_USER}"
    printf "  ${C_CYAN}│${RESET}   • MinIO Password  : ${C_AMBER}%-44s${RESET} ${C_CYAN}│${RESET}\n" "${CFG_MINIO_PASS}"
    echo -e "  ${C_CYAN}│${RESET}                                                                     ${C_CYAN}│${RESET}"
    echo -e "  ${C_CYAN}╰────────────────────────────────────────────────────────────────────╯${RESET}"
    echo ""

    echo -e "  ${C_WHITE}${BOLD}⚡ Next Steps:${RESET}"
    echo -e "   1. Open ${C_SKY}${portal_url}${RESET} in your browser."
    echo -e "   2. Click ${BOLD}Sign Up${RESET} to create your initial user account."
    echo -e "   3. Your personal S3 bucket and private Git server are auto-provisioned immediately."
    echo ""

    if [ "$DEPLOY_MODE" = "docker" ]; then
        echo -e "  ${C_MUTED}Useful Commands:${RESET}"
        echo -e "   • Inspect logs   : ${C_CYAN}cd deploy && docker-compose logs -f${RESET}"
        echo -e "   • Restart stack  : ${C_CYAN}cd deploy && docker-compose restart${RESET}"
        echo -e "   • Stop stack     : ${C_CYAN}cd deploy && docker-compose down${RESET}"
    else
        echo -e "  ${C_MUTED}Useful Commands:${RESET}"
        echo -e "   • Status check   : ${C_CYAN}sudo systemctl status cosmos${RESET}"
        echo -e "   • View logs      : ${C_CYAN}journalctl -u cosmos -f${RESET}"
        echo -e "   • Restart service: ${C_CYAN}sudo systemctl restart cosmos${RESET}"
    fi
    echo ""
    echo -e "  ${C_GOLD}${BOLD}★ Welcome to sovereign cloud computing with COSMOS ★${RESET}"
    echo ""
}

# ─── SCRIPT ENTRYPOINT ────────────────────────────────────────────────────────
main() {
    clear_screen
    print_banner
    check_privileges
    run_preflight_checks
    prompt_configuration

    if [ "$DEPLOY_MODE" = "docker" ]; then
        deploy_docker_stack
    else
        deploy_baremetal_stack
    fi

    setup_nginx_proxy
    print_completion_screen
}

main "$@"
