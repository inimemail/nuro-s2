#!/usr/bin/env bash

set -Eeuo pipefail

export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:${PATH:-}"

APP_NAME="Nuro-Sub2api"
APP_SLUG="nuro-sub2api"
DEFAULT_INSTALL_PATH="/opt/nuro-sub2api"
ENV_RECORD_FILE="/etc/nuro-sub2api_env"

COMPOSE_PROJECT_NAME="nuro-sub2api"
APP_CONTAINER="nuro-sub2api"
EDGE_CONTAINER="nuro-sub2api-edge-rs"
POSTGRES_CONTAINER="nuro-sub2api-postgres"
REDIS_CONTAINER="nuro-sub2api-redis"
IMAGE_NAME="nuro-sub2api-local:latest"
EDGE_IMAGE_NAME="nuro-sub2api-edge-rs-local:latest"
SOURCE_DIR_NAME="source"
SOURCE_REPO_URL="${SOURCE_REPO_URL:-https://github.com/inimemail/nuro-s2.git}"
SOURCE_REPO_BRANCH="${SOURCE_REPO_BRANCH:-main}"
SCRIPT_RAW_URL="${SCRIPT_RAW_URL:-https://raw.githubusercontent.com/inimemail/nuro-s2/${SOURCE_REPO_BRANCH}/deploy-nuro.sh}"
NURO_EDGE_ENABLED="${NURO_EDGE_ENABLED:-true}"

DEFAULT_WEB_PORT="6182"
CRON_TAG_BEGIN="# NURO_SUB2API_BACKUP_BEGIN"
CRON_TAG_END="# NURO_SUB2API_BACKUP_END"
BACKUP_LOG="/var/log/nuro-sub2api_backup.log"

ADMIN_PASS=""

GREEN="\033[32m"
RESET="\033[0m"

info() { echo -e "\033[32m[INFO]\033[0m $1"; }
warn() { echo -e "\033[33m[WARN]\033[0m $1" >&2; }
err()  { echo -e "\033[31m[ERROR]\033[0m $1" >&2; }
die()  { echo -e "\033[31m[FATAL]\033[0m $1" >&2; exit 1; }

require_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "系统缺少必要命令: $1"
}

get_local_ip() {
    hostname -I 2>/dev/null | awk '{print $1}' || echo "127.0.0.1"
}

valid_port() {
    local p="$1"
    [[ "$p" =~ ^[0-9]+$ ]] && [[ "$p" -ge 1 ]] && [[ "$p" -le 65535 ]]
}

port_in_use() {
    local p="$1"
    if command -v ss >/dev/null 2>&1; then
        ss -ltn 2>/dev/null | awk '{print $4}' | grep -Eq "(^|:)${p}$"
    elif command -v netstat >/dev/null 2>&1; then
        netstat -ltn 2>/dev/null | awk '{print $4}' | grep -Eq "(^|:)${p}$"
    else
        return 1
    fi
}

find_free_port() {
    local p="$1"
    while [[ "$p" -le 65535 ]]; do
        if ! port_in_use "$p"; then
            echo "$p"
            return 0
        fi
        p=$((p + 1))
    done
    return 1
}

docker_compose_cmd() {
    if command -v docker-compose >/dev/null 2>&1; then
        echo "docker-compose"
    elif docker compose version >/dev/null 2>&1; then
        echo "docker compose"
    else
        die "未检测到 Docker Compose，请先安装 docker compose 或 docker-compose。"
    fi
}

get_workdir() {
    if [[ -f "$ENV_RECORD_FILE" ]]; then
        local dir
        dir="$(cat "$ENV_RECORD_FILE" 2>/dev/null || true)"
        if [[ -n "$dir" && -d "$dir" ]]; then
            echo "$dir"
            return
        fi
    fi

    if [[ -d "$DEFAULT_INSTALL_PATH" && -f "${DEFAULT_INSTALL_PATH}/docker-compose.yml" ]]; then
        echo "$DEFAULT_INSTALL_PATH"
        return
    fi

    echo ""
}

get_script_dir() {
    cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd
}

safe_remove_dir() {
    local path="$1"
    local resolved

    [[ -n "$path" ]] || { err "删除路径为空，已取消。"; return 1; }
    resolved="$(readlink -f "$path" 2>/dev/null || realpath "$path" 2>/dev/null || echo "$path")"

    case "$resolved" in
        ""|"/"|"/bin"|"/boot"|"/dev"|"/etc"|"/home"|"/lib"|"/lib64"|"/opt"|"/proc"|"/root"|"/run"|"/sbin"|"/srv"|"/sys"|"/tmp"|"/usr"|"/var")
            err "拒绝删除危险路径: ${resolved}"
            return 1
        ;;
    esac

    rm -rf -- "$resolved"
}

set_env_value() {
    local file="$1"
    local key="$2"
    local value="$3"
    local tmp_file

    touch "$file"
    tmp_file="$(mktemp)" || return 1
    awk -v key="$key" -v value="$value" '
        BEGIN { found = 0 }
        $0 ~ "^" key "=" {
            print key "=" value
            found = 1
            next
        }
        { print }
        END {
            if (!found) {
                print key "=" value
            }
        }
    ' "$file" > "$tmp_file" && mv "$tmp_file" "$file"
}

persist_script() {
    local workdir="$1"
    local target="${workdir}/deploy.sh"

    if [[ -f "${BASH_SOURCE[0]}" ]]; then
        cp "${BASH_SOURCE[0]}" "$target" 2>/dev/null || true
    fi

    if [[ ! -s "$target" ]] && command -v curl >/dev/null 2>&1; then
        curl -fsSL "$SCRIPT_RAW_URL" -o "$target" 2>/dev/null || true
    fi

    chmod +x "$target" 2>/dev/null || true
}

is_project_root() {
    [[ -f "$1/Dockerfile" && -d "$1/backend" && -d "$1/frontend" ]]
}

find_project_root() {
    if [[ -n "${PROJECT_ROOT:-}" ]] && is_project_root "$PROJECT_ROOT"; then
        cd "$PROJECT_ROOT" >/dev/null 2>&1 && pwd
        return 0
    fi

    local script_dir
    script_dir="$(get_script_dir)"
    if is_project_root "$script_dir"; then
        echo "$script_dir"
        return 0
    fi

    if is_project_root "$PWD"; then
        pwd
        return 0
    fi

    return 1
}

sync_project_source() {
    local workdir="$1"
    local dest="${workdir}/${SOURCE_DIR_NAME}"
    local project_root=""

    if project_root="$(find_project_root)"; then
        rm -rf "$dest"
        mkdir -p "$dest"
        info "正在同步当前项目源码到 ${dest} ..."
        tar \
            --exclude='./.git' \
            --exclude='./node_modules' \
            --exclude='./frontend/node_modules' \
            --exclude='./frontend/dist' \
            --exclude='./backend/data' \
            --exclude='./logs' \
            --exclude='./backups' \
            --exclude='./.env' \
            --exclude='./.env.*' \
            --exclude='./tmp' \
            --exclude='./temp' \
            -cf - -C "$project_root" . | tar -xf - -C "$dest"

        persist_script "$workdir"
        return 0
    fi

    require_cmd git
    if [[ -d "${dest}/.git" ]]; then
        info "正在从 ${SOURCE_REPO_URL} (${SOURCE_REPO_BRANCH}) 更新源码到 ${dest} ..."
        git -C "$dest" fetch --depth 1 origin "$SOURCE_REPO_BRANCH" || return 1
        git -C "$dest" checkout -f FETCH_HEAD || return 1
    else
        info "正在从 ${SOURCE_REPO_URL} (${SOURCE_REPO_BRANCH}) 拉取源码到 ${dest} ..."
        rm -rf "$dest"
        git clone --depth 1 --branch "$SOURCE_REPO_BRANCH" "$SOURCE_REPO_URL" "$dest" || return 1
    fi

    persist_script "$workdir"
    return 0
}

generate_secret() {
    openssl rand -hex 32
}

ensure_env_value() {
    local file="$1"
    local key="$2"
    local value="$3"
    local current

    current="$(read_env_value "$file" "$key")"
    if [[ -z "$current" ]]; then
        set_env_value "$file" "$key" "$value"
    fi
}

ensure_edge_env_values() {
    local env_file="$1"
    local secret

    touch "$env_file"
    ensure_env_value "$env_file" NURO_EDGE_ENABLED "$NURO_EDGE_ENABLED"
    if [[ "$(read_env_value "$env_file" NURO_EDGE_ENABLED)" != "true" ]]; then
        secret="$(read_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET)"
        if [[ -z "$secret" ]]; then
            secret="$(read_env_value "$env_file" SUB2API_EDGE_INTERNAL_SECRET)"
        fi
        if [[ -z "$secret" ]]; then
            secret="$(generate_secret)"
        fi
        set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_ENABLED false
        set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_INTERNAL_API_ENABLED false
        set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET "$secret"
        set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_MODE off
        set_env_value "$env_file" GATEWAY_STREAM_LOW_LATENCY_MODE smart
        set_env_value "$env_file" GATEWAY_LOW_LATENCY_STREAM_HEADERS true
        set_env_value "$env_file" SUB2API_EDGE_INTERNAL_SECRET "$secret"
        return
    fi

    secret="$(read_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET)"
    if [[ -z "$secret" ]]; then
        secret="$(read_env_value "$env_file" SUB2API_EDGE_INTERNAL_SECRET)"
    fi
    if [[ -z "$secret" ]]; then
        secret="$(generate_secret)"
    fi

    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_ENABLED true
    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_INTERNAL_API_ENABLED true
    set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET "$secret"
    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_MODE relay
    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_INGRESS_PROXY_ENABLED true
    set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_LISTEN_ADDR edge-rs:18080
    set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_GO_BASE_URL http://app:8080
    set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_CONTROL_BASE_URL http://app:8080
    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_RELAY_CHAT_COMPLETIONS true
    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES true
    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES_WEBSOCKET true
    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_ROLLOUT_PERCENT 100
    ensure_env_value "$env_file" GATEWAY_STREAM_LOW_LATENCY_MODE smart
    ensure_env_value "$env_file" GATEWAY_LOW_LATENCY_STREAM_HEADERS true

    set_env_value "$env_file" SUB2API_EDGE_LISTEN_ADDR 0.0.0.0:18080
    set_env_value "$env_file" SUB2API_EDGE_GO_BASE_URL http://app:8080
    set_env_value "$env_file" SUB2API_EDGE_CONTROL_BASE_URL http://app:8080
    set_env_value "$env_file" SUB2API_EDGE_INTERNAL_SECRET "$secret"
    ensure_env_value "$env_file" SUB2API_EDGE_INITIAL_POOL_SIZE 10000
    ensure_env_value "$env_file" SUB2API_EDGE_QUEUE_BUFFER_SIZE 20000
    ensure_env_value "$env_file" SUB2API_EDGE_PER_ACCOUNT_WORKERS 4
    ensure_env_value "$env_file" SUB2API_EDGE_MAX_IDLE_PER_ACCOUNT 8
    ensure_env_value "$env_file" SUB2API_EDGE_LARGE_PAYLOAD_PASSTHROUGH true
    ensure_env_value "$env_file" SUB2API_EDGE_LARGE_PAYLOAD_THRESHOLD_BYTES 262144
    ensure_env_value "$env_file" SUB2API_EDGE_WS_IDLE_PER_KEY 1
}

generate_admin_password() {
    ADMIN_PASS="$(openssl rand -hex 12)"
}

read_env_value() {
    local file="$1"
    local key="$2"
    grep -E "^${key}=" "$file" 2>/dev/null | tail -n 1 | cut -d= -f2- || true
}

create_env_file() {
    local workdir="$1"
    local host_port="$2"

    [[ -f "${workdir}/.env" ]] && return 0

    generate_admin_password

    cat > "${workdir}/.env" <<EOF
COMPOSE_PROJECT_NAME=${COMPOSE_PROJECT_NAME}
BIND_HOST=0.0.0.0
SERVER_PORT=${host_port}
SERVER_MODE=release
RUN_MODE=standard
TZ=Asia/Shanghai

POSTGRES_USER=nuro_sub2api
POSTGRES_DB=nuro_sub2api
POSTGRES_PASSWORD=$(generate_secret)

REDIS_PASSWORD=
REDIS_DB=0

ADMIN_EMAIL=admin@nuro-sub2api.local
ADMIN_PASSWORD=${ADMIN_PASS}

JWT_SECRET=$(generate_secret)
TOTP_ENCRYPTION_KEY=$(generate_secret)

GEMINI_OAUTH_CLIENT_ID=
GEMINI_OAUTH_CLIENT_SECRET=
GEMINI_OAUTH_SCOPES=
GEMINI_QUOTA_POLICY=
GEMINI_CLI_OAUTH_CLIENT_SECRET=
ANTIGRAVITY_OAUTH_CLIENT_SECRET=
ANTIGRAVITY_USER_AGENT_VERSION=

SECURITY_URL_ALLOWLIST_ENABLED=false
SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP=false
SECURITY_URL_ALLOWLIST_ALLOW_PRIVATE_HOSTS=false
SECURITY_URL_ALLOWLIST_UPSTREAM_HOSTS=
UPDATE_PROXY_URL=

GATEWAY_OPENAI_RESPONSE_HEADER_TIMEOUT=0
GATEWAY_OPENAI_HTTP2_ENABLED=true
GATEWAY_OPENAI_HTTP2_ALLOW_PROXY_FALLBACK_TO_HTTP1=true
GATEWAY_OPENAI_HTTP2_FALLBACK_ERROR_THRESHOLD=2
GATEWAY_OPENAI_HTTP2_FALLBACK_WINDOW_SECONDS=60
GATEWAY_OPENAI_HTTP2_FALLBACK_TTL_SECONDS=600
NURO_EDGE_ENABLED=true
GATEWAY_OPENAI_EDGE_RS_ENABLED=true
GATEWAY_OPENAI_EDGE_RS_INTERNAL_API_ENABLED=true
GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET=$(generate_secret)
GATEWAY_OPENAI_EDGE_RS_MODE=relay
GATEWAY_OPENAI_EDGE_RS_INGRESS_PROXY_ENABLED=true
GATEWAY_OPENAI_EDGE_RS_LISTEN_ADDR=edge-rs:18080
GATEWAY_OPENAI_EDGE_RS_GO_BASE_URL=http://app:8080
GATEWAY_OPENAI_EDGE_RS_CONTROL_BASE_URL=http://app:8080
GATEWAY_OPENAI_EDGE_RS_RELAY_CHAT_COMPLETIONS=true
GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES=true
GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES_WEBSOCKET=true
GATEWAY_OPENAI_EDGE_RS_ROLLOUT_PERCENT=100
GATEWAY_STREAM_LOW_LATENCY_MODE=smart
GATEWAY_LOW_LATENCY_STREAM_HEADERS=true
SUB2API_EDGE_LISTEN_ADDR=0.0.0.0:18080
SUB2API_EDGE_GO_BASE_URL=http://app:8080
SUB2API_EDGE_CONTROL_BASE_URL=http://app:8080
SUB2API_EDGE_INTERNAL_SECRET=
SUB2API_EDGE_INITIAL_POOL_SIZE=10000
SUB2API_EDGE_QUEUE_BUFFER_SIZE=20000
SUB2API_EDGE_PER_ACCOUNT_WORKERS=4
SUB2API_EDGE_MAX_IDLE_PER_ACCOUNT=8
SUB2API_EDGE_LARGE_PAYLOAD_PASSTHROUGH=true
SUB2API_EDGE_LARGE_PAYLOAD_THRESHOLD_BYTES=262144
SUB2API_EDGE_WS_IDLE_PER_KEY=1
GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT=900
GATEWAY_IMAGE_STREAM_KEEPALIVE_INTERVAL=10
GATEWAY_IMAGE_CONCURRENCY_ENABLED=false
GATEWAY_IMAGE_CONCURRENCY_MAX_CONCURRENT_REQUESTS=0
GATEWAY_IMAGE_CONCURRENCY_OVERFLOW_MODE=reject
GATEWAY_IMAGE_CONCURRENCY_WAIT_TIMEOUT_SECONDS=30
GATEWAY_IMAGE_CONCURRENCY_MAX_WAITING_REQUESTS=100
EOF

    local edge_secret
    edge_secret="$(read_env_value "${workdir}/.env" GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET)"
    set_env_value "${workdir}/.env" SUB2API_EDGE_INTERNAL_SECRET "$edge_secret"
    chmod 600 "${workdir}/.env"
}

create_compose_file() {
    local workdir="$1"
    local edge_enabled
    local edge_depends=""
    local edge_service=""

    edge_enabled="$(read_env_value "${workdir}/.env" NURO_EDGE_ENABLED)"
    edge_enabled="${edge_enabled:-true}"
    if [[ "$edge_enabled" == "true" ]]; then
        edge_depends='
      edge-rs:
        condition: service_healthy'
        edge_service="

  edge-rs:
    build:
      context: ./${SOURCE_DIR_NAME}/edge-rs
      dockerfile: Dockerfile
    image: ${EDGE_IMAGE_NAME}
    container_name: ${EDGE_CONTAINER}
    restart: unless-stopped
    ulimits:
      nofile:
        soft: 100000
        hard: 100000
    environment:
      - TZ=\${TZ:-Asia/Shanghai}
      - RUST_LOG=\${RUST_LOG:-info}
      - SUB2API_EDGE_LISTEN_ADDR=\${SUB2API_EDGE_LISTEN_ADDR:-0.0.0.0:18080}
      - SUB2API_EDGE_GO_BASE_URL=\${SUB2API_EDGE_GO_BASE_URL:-http://app:8080}
      - SUB2API_EDGE_CONTROL_BASE_URL=\${SUB2API_EDGE_CONTROL_BASE_URL:-http://app:8080}
      - SUB2API_EDGE_INTERNAL_SECRET=\${SUB2API_EDGE_INTERNAL_SECRET:?SUB2API_EDGE_INTERNAL_SECRET is required}
      - SUB2API_EDGE_INITIAL_POOL_SIZE=\${SUB2API_EDGE_INITIAL_POOL_SIZE:-10000}
      - SUB2API_EDGE_QUEUE_BUFFER_SIZE=\${SUB2API_EDGE_QUEUE_BUFFER_SIZE:-20000}
      - SUB2API_EDGE_PER_ACCOUNT_WORKERS=\${SUB2API_EDGE_PER_ACCOUNT_WORKERS:-4}
      - SUB2API_EDGE_MAX_IDLE_PER_ACCOUNT=\${SUB2API_EDGE_MAX_IDLE_PER_ACCOUNT:-8}
      - SUB2API_EDGE_LARGE_PAYLOAD_PASSTHROUGH=\${SUB2API_EDGE_LARGE_PAYLOAD_PASSTHROUGH:-true}
      - SUB2API_EDGE_LARGE_PAYLOAD_THRESHOLD_BYTES=\${SUB2API_EDGE_LARGE_PAYLOAD_THRESHOLD_BYTES:-262144}
      - SUB2API_EDGE_WS_IDLE_PER_KEY=\${SUB2API_EDGE_WS_IDLE_PER_KEY:-1}
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
    networks:
      nuro-sub2api-network:
        aliases:
          - edge-rs
    healthcheck:
      test: [\"CMD\", \"wget\", \"-q\", \"-T\", \"5\", \"-O\", \"/dev/null\", \"http://localhost:18080/healthz\"]
      interval: 10s
      timeout: 5s
      retries: 12
      start_period: 10s"
    fi

    cat > "${workdir}/docker-compose.yml" <<EOF
name: ${COMPOSE_PROJECT_NAME}

services:
  app:
    build:
      context: ./${SOURCE_DIR_NAME}
      dockerfile: Dockerfile
    image: ${IMAGE_NAME}
    container_name: ${APP_CONTAINER}
    restart: unless-stopped
    ulimits:
      nofile:
        soft: 100000
        hard: 100000
    ports:
      - "\${BIND_HOST:-0.0.0.0}:\${SERVER_PORT:-6182}:8080"
    volumes:
      - ./data:/app/data
    environment:
      - AUTO_SETUP=true
      - SERVER_HOST=0.0.0.0
      - SERVER_PORT=8080
      - SERVER_MODE=\${SERVER_MODE:-release}
      - RUN_MODE=\${RUN_MODE:-standard}
      - DATABASE_HOST=postgres
      - DATABASE_PORT=5432
      - DATABASE_USER=\${POSTGRES_USER:-nuro_sub2api}
      - DATABASE_PASSWORD=\${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}
      - DATABASE_DBNAME=\${POSTGRES_DB:-nuro_sub2api}
      - DATABASE_SSLMODE=disable
      - DATABASE_MAX_OPEN_CONNS=\${DATABASE_MAX_OPEN_CONNS:-50}
      - DATABASE_MAX_IDLE_CONNS=\${DATABASE_MAX_IDLE_CONNS:-10}
      - DATABASE_CONN_MAX_LIFETIME_MINUTES=\${DATABASE_CONN_MAX_LIFETIME_MINUTES:-30}
      - DATABASE_CONN_MAX_IDLE_TIME_MINUTES=\${DATABASE_CONN_MAX_IDLE_TIME_MINUTES:-5}
      - REDIS_HOST=redis
      - REDIS_PORT=6379
      - REDIS_PASSWORD=\${REDIS_PASSWORD:-}
      - REDIS_DB=\${REDIS_DB:-0}
      - REDIS_POOL_SIZE=\${REDIS_POOL_SIZE:-1024}
      - REDIS_MIN_IDLE_CONNS=\${REDIS_MIN_IDLE_CONNS:-10}
      - REDIS_ENABLE_TLS=\${REDIS_ENABLE_TLS:-false}
      - ADMIN_EMAIL=\${ADMIN_EMAIL:-admin@nuro-sub2api.local}
      - ADMIN_PASSWORD=\${ADMIN_PASSWORD:-}
      - JWT_SECRET=\${JWT_SECRET:-}
      - JWT_EXPIRE_HOUR=\${JWT_EXPIRE_HOUR:-24}
      - TOTP_ENCRYPTION_KEY=\${TOTP_ENCRYPTION_KEY:-}
      - TZ=\${TZ:-Asia/Shanghai}
      - GEMINI_OAUTH_CLIENT_ID=\${GEMINI_OAUTH_CLIENT_ID:-}
      - GEMINI_OAUTH_CLIENT_SECRET=\${GEMINI_OAUTH_CLIENT_SECRET:-}
      - GEMINI_OAUTH_SCOPES=\${GEMINI_OAUTH_SCOPES:-}
      - GEMINI_QUOTA_POLICY=\${GEMINI_QUOTA_POLICY:-}
      - GEMINI_CLI_OAUTH_CLIENT_SECRET=\${GEMINI_CLI_OAUTH_CLIENT_SECRET:-}
      - ANTIGRAVITY_OAUTH_CLIENT_SECRET=\${ANTIGRAVITY_OAUTH_CLIENT_SECRET:-}
      - ANTIGRAVITY_USER_AGENT_VERSION=\${ANTIGRAVITY_USER_AGENT_VERSION:-}
      - SECURITY_URL_ALLOWLIST_ENABLED=\${SECURITY_URL_ALLOWLIST_ENABLED:-false}
      - SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP=\${SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP:-false}
      - SECURITY_URL_ALLOWLIST_ALLOW_PRIVATE_HOSTS=\${SECURITY_URL_ALLOWLIST_ALLOW_PRIVATE_HOSTS:-false}
      - SECURITY_URL_ALLOWLIST_UPSTREAM_HOSTS=\${SECURITY_URL_ALLOWLIST_UPSTREAM_HOSTS:-}
      - UPDATE_PROXY_URL=\${UPDATE_PROXY_URL:-}
      - GATEWAY_OPENAI_RESPONSE_HEADER_TIMEOUT=\${GATEWAY_OPENAI_RESPONSE_HEADER_TIMEOUT:-0}
      - GATEWAY_OPENAI_HTTP2_ENABLED=\${GATEWAY_OPENAI_HTTP2_ENABLED:-true}
      - GATEWAY_OPENAI_HTTP2_ALLOW_PROXY_FALLBACK_TO_HTTP1=\${GATEWAY_OPENAI_HTTP2_ALLOW_PROXY_FALLBACK_TO_HTTP1:-true}
      - GATEWAY_OPENAI_HTTP2_FALLBACK_ERROR_THRESHOLD=\${GATEWAY_OPENAI_HTTP2_FALLBACK_ERROR_THRESHOLD:-2}
      - GATEWAY_OPENAI_HTTP2_FALLBACK_WINDOW_SECONDS=\${GATEWAY_OPENAI_HTTP2_FALLBACK_WINDOW_SECONDS:-60}
      - GATEWAY_OPENAI_HTTP2_FALLBACK_TTL_SECONDS=\${GATEWAY_OPENAI_HTTP2_FALLBACK_TTL_SECONDS:-600}
      - GATEWAY_OPENAI_EDGE_RS_ENABLED=\${GATEWAY_OPENAI_EDGE_RS_ENABLED:-true}
      - GATEWAY_OPENAI_EDGE_RS_INTERNAL_API_ENABLED=\${GATEWAY_OPENAI_EDGE_RS_INTERNAL_API_ENABLED:-true}
      - GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET=\${GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET:?GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET is required}
      - GATEWAY_OPENAI_EDGE_RS_MODE=\${GATEWAY_OPENAI_EDGE_RS_MODE:-relay}
      - GATEWAY_OPENAI_EDGE_RS_INGRESS_PROXY_ENABLED=\${GATEWAY_OPENAI_EDGE_RS_INGRESS_PROXY_ENABLED:-true}
      - GATEWAY_OPENAI_EDGE_RS_LISTEN_ADDR=\${GATEWAY_OPENAI_EDGE_RS_LISTEN_ADDR:-edge-rs:18080}
      - GATEWAY_OPENAI_EDGE_RS_GO_BASE_URL=\${GATEWAY_OPENAI_EDGE_RS_GO_BASE_URL:-http://app:8080}
      - GATEWAY_OPENAI_EDGE_RS_CONTROL_BASE_URL=\${GATEWAY_OPENAI_EDGE_RS_CONTROL_BASE_URL:-http://app:8080}
      - GATEWAY_OPENAI_EDGE_RS_RELAY_CHAT_COMPLETIONS=\${GATEWAY_OPENAI_EDGE_RS_RELAY_CHAT_COMPLETIONS:-true}
      - GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES=\${GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES:-true}
      - GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES_WEBSOCKET=\${GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES_WEBSOCKET:-true}
      - GATEWAY_OPENAI_EDGE_RS_ROLLOUT_PERCENT=\${GATEWAY_OPENAI_EDGE_RS_ROLLOUT_PERCENT:-100}
      - GATEWAY_STREAM_LOW_LATENCY_MODE=\${GATEWAY_STREAM_LOW_LATENCY_MODE:-smart}
      - GATEWAY_LOW_LATENCY_STREAM_HEADERS=\${GATEWAY_LOW_LATENCY_STREAM_HEADERS:-true}
      - GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT=\${GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT:-900}
      - GATEWAY_IMAGE_STREAM_KEEPALIVE_INTERVAL=\${GATEWAY_IMAGE_STREAM_KEEPALIVE_INTERVAL:-10}
      - GATEWAY_IMAGE_CONCURRENCY_ENABLED=\${GATEWAY_IMAGE_CONCURRENCY_ENABLED:-false}
      - GATEWAY_IMAGE_CONCURRENCY_MAX_CONCURRENT_REQUESTS=\${GATEWAY_IMAGE_CONCURRENCY_MAX_CONCURRENT_REQUESTS:-0}
      - GATEWAY_IMAGE_CONCURRENCY_OVERFLOW_MODE=\${GATEWAY_IMAGE_CONCURRENCY_OVERFLOW_MODE:-reject}
      - GATEWAY_IMAGE_CONCURRENCY_WAIT_TIMEOUT_SECONDS=\${GATEWAY_IMAGE_CONCURRENCY_WAIT_TIMEOUT_SECONDS:-30}
      - GATEWAY_IMAGE_CONCURRENCY_MAX_WAITING_REQUESTS=\${GATEWAY_IMAGE_CONCURRENCY_MAX_WAITING_REQUESTS:-100}
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
${edge_depends}
    networks:
      - nuro-sub2api-network
    healthcheck:
      test: ["CMD", "wget", "-q", "-T", "5", "-O", "/dev/null", "http://localhost:8080/health"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 30s
${edge_service}

  postgres:
    image: postgres:18-alpine
    container_name: ${POSTGRES_CONTAINER}
    restart: unless-stopped
    ulimits:
      nofile:
        soft: 100000
        hard: 100000
    volumes:
      - ./postgres_data:/var/lib/postgresql/data
    environment:
      - POSTGRES_USER=\${POSTGRES_USER:-nuro_sub2api}
      - POSTGRES_PASSWORD=\${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}
      - POSTGRES_DB=\${POSTGRES_DB:-nuro_sub2api}
      - PGDATA=/var/lib/postgresql/data
      - TZ=\${TZ:-Asia/Shanghai}
    networks:
      - nuro-sub2api-network
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U \${POSTGRES_USER:-nuro_sub2api} -d \${POSTGRES_DB:-nuro_sub2api}"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 10s

  redis:
    image: redis:8-alpine
    container_name: ${REDIS_CONTAINER}
    restart: unless-stopped
    ulimits:
      nofile:
        soft: 100000
        hard: 100000
    volumes:
      - ./redis_data:/data
    command: >
        sh -c '
          redis-server
          --save 60 1
          --appendonly yes
          --appendfsync everysec
          \${REDIS_PASSWORD:+--requirepass "\$REDIS_PASSWORD"}'
    environment:
      - TZ=\${TZ:-Asia/Shanghai}
      - REDISCLI_AUTH=\${REDIS_PASSWORD:-}
    networks:
      - nuro-sub2api-network
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 5s

networks:
  nuro-sub2api-network:
    driver: bridge
EOF
}

compose_build_with_edge_fallback() {
    local workdir="$1"
    local dc_cmd="$2"
    local env_file="${workdir}/.env"

    if $dc_cmd -p "$COMPOSE_PROJECT_NAME" -f docker-compose.yml build; then
        return 0
    fi

    if [[ "$(read_env_value "$env_file" NURO_EDGE_ENABLED)" == "true" ]]; then
        warn "包含 Rust edge 的镜像构建失败，自动关闭 edge 后重试，避免影响主服务升级。"
        set_env_value "$env_file" NURO_EDGE_ENABLED false
        ensure_edge_env_values "$env_file"
        create_compose_file "$workdir"
        $dc_cmd -p "$COMPOSE_PROJECT_NAME" -f docker-compose.yml build
        return $?
    fi

    return 1
}

show_access() {
    local workdir="$1"
    local env_file="${workdir}/.env"
    local host_port admin_email admin_password
    host_port="$(read_env_value "$env_file" SERVER_PORT)"
    admin_email="$(read_env_value "$env_file" ADMIN_EMAIL)"
    admin_password="$(read_env_value "$env_file" ADMIN_PASSWORD)"
    host_port="${host_port:-$DEFAULT_WEB_PORT}"

    echo ""
    echo "=================================================="
    echo "${APP_NAME} 已就绪"
    echo "--------------------------------------------------"
    echo "访问地址:   http://$(get_local_ip):${host_port}"
    echo "管理员账号: ${admin_email:-admin@nuro-sub2api.local}"
    echo "管理员密码: ${admin_password:-请查看 .env 或容器日志}"
    echo "部署目录:   ${workdir}"
    echo "环境文件:   ${workdir}/.env"
    echo "容器名称:   ${APP_CONTAINER}, ${POSTGRES_CONTAINER}, ${REDIS_CONTAINER}"
    echo "端口映射:   ${host_port} -> 8080"
    echo "=================================================="
    echo ""
}

wait_app_ready() {
    info "正在等待 ${APP_NAME} 启动 ..."
    for _ in $(seq 1 60); do
        if docker ps --format '{{.Names}} {{.Status}}' | grep -q "^${APP_CONTAINER} .*Up"; then
            info "${APP_NAME} 容器已运行。"
            return 0
        fi
        sleep 2
    done

    warn "${APP_NAME} 可能未正常启动，最近日志如下："
    docker logs --tail=120 "$APP_CONTAINER" 2>/dev/null || true
    return 1
}

deploy_service() {
    info "== 开始部署 ${APP_NAME} =="

    require_cmd docker
    require_cmd awk
    require_cmd openssl
    require_cmd tar
    require_cmd git
    require_cmd curl

    local dc_cmd
    dc_cmd="$(docker_compose_cmd)"

    read -r -p "安装路径 [默认: ${DEFAULT_INSTALL_PATH}]: " input_path
    local install_path="${input_path:-$DEFAULT_INSTALL_PATH}"

    if [[ -f "${install_path}/docker-compose.yml" ]]; then
        err "${install_path} 已存在部署文件，请使用 [2] 升级/重建。"
        return
    fi

    mkdir -p "$install_path"
    echo "$install_path" > "$ENV_RECORD_FILE"
    cd "$install_path" || return

    read -r -p "对外 Web/API 端口 [默认: ${DEFAULT_WEB_PORT}]: " input_port
    local host_port="${input_port:-$DEFAULT_WEB_PORT}"
    valid_port "$host_port" || die "端口无效: ${host_port}"
    if port_in_use "$host_port"; then
        local old_port="$host_port"
        host_port="$(find_free_port "$host_port")" || die "未找到可用的 Web/API 端口"
        warn "端口 ${old_port} 已被占用，自动改用 ${host_port}。"
    fi

    mkdir -p data postgres_data redis_data backups
    chmod 755 data postgres_data redis_data backups

    create_env_file "$install_path" "$host_port"
    ensure_edge_env_values "${install_path}/.env"
    sync_project_source "$install_path" || die "源码同步失败。请检查服务器是否能访问 ${SOURCE_REPO_URL}，或在项目根目录执行本脚本。"
    create_compose_file "$install_path"

    info "正在使用项目源码构建本地镜像并启动 ${APP_NAME} ..."
    compose_build_with_edge_fallback "$install_path" "$dc_cmd" || die "镜像构建失败"
    $dc_cmd -p "$COMPOSE_PROJECT_NAME" -f docker-compose.yml up -d --remove-orphans || die "容器启动失败"

    wait_app_ready || true
    show_access "$install_path"
}

upgrade_service() {
    local workdir
    workdir="$(get_workdir)"
    [[ -z "$workdir" ]] && { err "未检测到 ${APP_NAME} 部署，请先执行 [1] 一键部署。"; return; }

    cd "$workdir" || return
    local dc_cmd
    dc_cmd="$(docker_compose_cmd)"
    require_cmd git

    sync_project_source "$workdir" || die "源码同步失败。请检查服务器是否能访问 ${SOURCE_REPO_URL}，或在项目根目录执行本脚本。"
    ensure_edge_env_values "${workdir}/.env"
    create_compose_file "$workdir"

    info "正在使用项目源码重建 ${APP_NAME} ..."
    compose_build_with_edge_fallback "$workdir" "$dc_cmd" || die "镜像构建失败"
    $dc_cmd -p "$COMPOSE_PROJECT_NAME" -f docker-compose.yml up -d --remove-orphans || die "容器启动失败"

    wait_app_ready || true
    show_access "$workdir"
}

pause_service() {
    local workdir
    workdir="$(get_workdir)"
    [[ -z "$workdir" ]] && { err "未检测到部署环境。"; return; }

    cd "$workdir" || return
    $(docker_compose_cmd) -p "$COMPOSE_PROJECT_NAME" -f docker-compose.yml stop
    info "${APP_NAME} 已停止。"
}

restart_service() {
    local workdir
    workdir="$(get_workdir)"
    [[ -z "$workdir" ]] && { err "未检测到部署环境。"; return; }

    cd "$workdir" || return
    $(docker_compose_cmd) -p "$COMPOSE_PROJECT_NAME" -f docker-compose.yml restart
    wait_app_ready || true
    show_access "$workdir"
}

do_backup() {
    local workdir
    workdir="$(get_workdir)"
    [[ -z "$workdir" ]] && { err "未检测到部署环境。"; return; }

    local backup_dir="${workdir}/backups"
    mkdir -p "$backup_dir"

    local timestamp backup_file temp_dir
    timestamp="$(date +"%Y%m%d_%H%M%S")"
    backup_file="${backup_dir}/nuro_sub2api_backup_${timestamp}.tar.gz"
    temp_dir="${backup_dir}/tmp_${timestamp}"

    mkdir -p "$temp_dir"
    cp "${workdir}/docker-compose.yml" "${temp_dir}/" 2>/dev/null || true
    cp "${workdir}/.env" "${temp_dir}/" 2>/dev/null || true
    cp "${workdir}/deploy.sh" "${temp_dir}/" 2>/dev/null || true
    [[ -d "${workdir}/data" ]] && cp -a "${workdir}/data" "${temp_dir}/data"
    [[ -d "${workdir}/postgres_data" ]] && cp -a "${workdir}/postgres_data" "${temp_dir}/postgres_data"
    [[ -d "${workdir}/redis_data" ]] && cp -a "${workdir}/redis_data" "${temp_dir}/redis_data"
    [[ -d "${workdir}/${SOURCE_DIR_NAME}" ]] && cp -a "${workdir}/${SOURCE_DIR_NAME}" "${temp_dir}/${SOURCE_DIR_NAME}"

    tar -czf "$backup_file" -C "$temp_dir" .
    rm -rf "$temp_dir"

    find "$backup_dir" -maxdepth 1 -name 'nuro_sub2api_backup_*.tar.gz' -type f \
        | sort -r \
        | awk 'NR>5' \
        | xargs -r rm -f

    info "备份完成: ${backup_file}"
}

restore_backup() {
    local workdir
    workdir="$(get_workdir)"

    local search_dir="${workdir:-$DEFAULT_INSTALL_PATH}/backups"
    local default_backup
    default_backup="$(ls -t "${search_dir}"/nuro_sub2api_backup_*.tar.gz 2>/dev/null | head -n 1 || true)"

    read -r -p "备份文件路径 [直接回车使用默认: ${default_backup}]: " backup_path
    local path="${backup_path:-$default_backup}"
    [[ ! -f "$path" ]] && { err "未找到有效备份文件。"; return; }

    local safe_backup="/tmp/$(basename "$path")"
    cp "$path" "$safe_backup" || { err "备份文件复制到临时目录失败。"; return; }

    read -r -p "恢复目标路径 [默认: ${DEFAULT_INSTALL_PATH}]: " target_dir
    local target="${target_dir:-$DEFAULT_INSTALL_PATH}"

    if [[ -d "$target" ]]; then
        read -r -p "目标路径已存在，是否覆盖？(y/N): " confirm
        [[ ! "$confirm" =~ ^[Yy]$ ]] && { rm -f "$safe_backup"; return; }
        cd "$target" 2>/dev/null && $(docker_compose_cmd) -p "$COMPOSE_PROJECT_NAME" -f docker-compose.yml down 2>/dev/null || true
        docker rm -f "$APP_CONTAINER" "$EDGE_CONTAINER" "$POSTGRES_CONTAINER" "$REDIS_CONTAINER" 2>/dev/null || true
        safe_remove_dir "$target" || { rm -f "$safe_backup"; return; }
    fi

    mkdir -p "$target"
    tar -xzf "$safe_backup" -C "$target" || { rm -f "$safe_backup"; die "备份解压失败"; }
    mkdir -p "${target}/backups"
    cp "$safe_backup" "${target}/backups/$(basename "$safe_backup")" 2>/dev/null || true
    rm -f "$safe_backup"
    echo "$target" > "$ENV_RECORD_FILE"

    cd "$target" || return
    [[ -f docker-compose.yml ]] || create_compose_file "$target"
    ensure_edge_env_values "${target}/.env"
    create_compose_file "$target"

    local restored_port host_port
    restored_port="$(read_env_value "${target}/.env" SERVER_PORT)"
    restored_port="${restored_port:-$DEFAULT_WEB_PORT}"
    if port_in_use "$restored_port"; then
        warn "恢复出来的端口 ${restored_port} 似乎已被占用。"
        read -r -p "请输入新的对外 Web/API 端口 [默认: ${DEFAULT_WEB_PORT}]: " host_port
        host_port="${host_port:-$DEFAULT_WEB_PORT}"
        valid_port "$host_port" || die "端口无效: ${host_port}"
        set_env_value "${target}/.env" SERVER_PORT "$host_port"
    fi

    mkdir -p data postgres_data redis_data backups
    chmod 755 data postgres_data redis_data backups

    local restore_dc_cmd
    restore_dc_cmd="$(docker_compose_cmd)"
    compose_build_with_edge_fallback "$target" "$restore_dc_cmd" || die "镜像构建失败"
    $restore_dc_cmd -p "$COMPOSE_PROJECT_NAME" -f docker-compose.yml up -d --remove-orphans || die "容器启动失败"

    wait_app_ready || true
    show_access "$target"
}

setup_auto_backup() {
    require_cmd crontab

    local workdir
    workdir="$(get_workdir)"
    [[ -z "$workdir" ]] && { err "未检测到部署环境。"; return; }

    local cron_script="${workdir}/cron_backup.sh"
    local script_path="${workdir}/deploy.sh"
    persist_script "$workdir"

    echo " 1) 按固定分钟间隔备份，例如 15 或 30"
    echo " 2) 按每天固定时间备份，例如 04:30"
    echo " 3) 删除当前定时备份"
    read -r -p "请选择 [1/2/3]: " cron_type

    local cron_spec=""
    case "$cron_type" in
        1)
            read -r -p "备份间隔分钟数: " min_interval
            [[ "$min_interval" =~ ^[0-9]+$ && "$min_interval" -ge 1 && "$min_interval" -le 1440 ]] || { err "分钟数无效。"; return; }
            cron_spec="*/${min_interval} * * * *"
        ;;
        2)
            read -r -p "每天备份时间 (HH:MM): " cron_time
            [[ "$cron_time" =~ ^([0-1][0-9]|2[0-3]):[0-5][0-9]$ ]] || { err "时间格式无效。"; return; }
            cron_spec="${cron_time#*:} ${cron_time%:*} * * *"
        ;;
        3)
            crontab -l 2>/dev/null | sed "/^${CRON_TAG_BEGIN}$/,/^${CRON_TAG_END}$/d" | crontab - 2>/dev/null || true
            rm -f "$cron_script"
            info "定时备份已删除。"
            return
        ;;
        *)
            err "无效选择。"
            return
        ;;
    esac

    cat > "$cron_script" <<EOF
#!/usr/bin/env bash
bash "$script_path" run-backup >> "$BACKUP_LOG" 2>&1
EOF
    chmod +x "$cron_script"

    (
        crontab -l 2>/dev/null | sed "/^${CRON_TAG_BEGIN}$/,/^${CRON_TAG_END}$/d"
        echo "$CRON_TAG_BEGIN"
        echo "${cron_spec} bash ${cron_script}"
        echo "$CRON_TAG_END"
    ) | crontab -

    info "定时备份已设置: ${cron_spec}"
}

uninstall_service() {
    local workdir
    workdir="$(get_workdir)"
    [[ -z "$workdir" ]] && workdir="$DEFAULT_INSTALL_PATH"

    echo -e "\033[31m警告：这只会删除 ${APP_NAME} 的容器和 ${workdir} 数据，不会影响原 sub2api。 \033[0m"
    read -r -p "确认卸载？(y/N): " confirm
    [[ ! "$confirm" =~ ^[Yy]$ ]] && return

    if [[ -d "$workdir" ]]; then
        cd "$workdir" 2>/dev/null && $(docker_compose_cmd) -p "$COMPOSE_PROJECT_NAME" -f docker-compose.yml down 2>/dev/null || true
    fi

    docker rm -f "$APP_CONTAINER" "$EDGE_CONTAINER" "$POSTGRES_CONTAINER" "$REDIS_CONTAINER" 2>/dev/null || true
    safe_remove_dir "$workdir" || return
    rm -f "$ENV_RECORD_FILE"
    crontab -l 2>/dev/null | sed "/^${CRON_TAG_BEGIN}$/,/^${CRON_TAG_END}$/d" | crontab - 2>/dev/null || true

    info "${APP_NAME} 已卸载。"
}

install_ftp() {
    clear
    echo -e "${GREEN}📂 FTP/SFTP 备份工具...${RESET}"
    bash <(curl -L https://raw.githubusercontent.com/hiapb/ftp/main/back.sh)
    sleep 2
    exit 0
}

main_menu() {
    clear
    echo "==================================================="
    echo "                ${APP_NAME} 一键管理"
    echo "==================================================="
    local wd
    wd="$(get_workdir)"
    echo " 部署目录: ${wd:-未部署}"
    echo "---------------------------------------------------"
    echo "  1) 一键部署"
    echo "  2) 升级服务"
    echo "  3) 停止服务"
    echo "  4) 重启服务"
    echo "  5) 手动备份"
    echo "  6) 恢复备份"
    echo "  7) 定时备份"
    echo "  8) 完全卸载"
    echo "  9) 查看访问信息"
    echo " 10) FTP/SFTP 备份工具"
    echo "  0) 退出"
    echo "==================================================="
    read -r -p "请输入操作序号 [0-10]: " choice

    case "$choice" in
        1) deploy_service ;;
        2) upgrade_service ;;
        3) pause_service ;;
        4) restart_service ;;
        5) do_backup ;;
        6) restore_backup ;;
        7) setup_auto_backup ;;
        8) uninstall_service ;;
        9)
            local wd2
            wd2="$(get_workdir)"
            [[ -n "$wd2" ]] && show_access "$wd2" || err "未检测到部署环境。"
        ;;
        10) install_ftp ;;
        0) exit 0 ;;
        *) warn "无效选择。" ;;
    esac
}

if [[ "${1:-}" == "run-backup" ]]; then
    do_backup
else
    if [[ $EUID -ne 0 ]]; then
        die "请使用 root 权限执行，例如: sudo bash deploy-nuro.sh"
    fi

    while true; do
        main_menu
        echo ""
        read -r -p "按回车键返回主菜单..."
    done
fi
