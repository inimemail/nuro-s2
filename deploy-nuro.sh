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
AUTOSCALER_CONTAINER="nuro-sub2api-autoscaler"
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
DEPLOY_EXPECTED_APP_REPLICAS=0

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

sysctl_min_value() {
    local key="$1"
    local minimum="$2"
    local current
    current="$(sysctl -n "$key" 2>/dev/null)" || return 1
    [[ "$current" =~ ^[0-9]+$ ]] || return 1
    if (( current < minimum )); then
        sysctl -w "${key}=${minimum}" >/dev/null || return 1
        current="$minimum"
    fi
    echo "$current"
}

ensure_haproxy_host_capacity() {
    require_cmd sysctl
    require_cmd install
    if (( EUID != 0 )); then
        die "HAProxy 百万连接部署需要 root 权限调整宿主机文件描述符上限"
    fi

    local nr_open file_max somaxconn syn_backlog conntrack=""
    nr_open="$(sysctl_min_value fs.nr_open 4194304)" || die "无法提升 fs.nr_open"
    file_max="$(sysctl_min_value fs.file-max 8388608)" || die "无法提升 fs.file-max"
    somaxconn="$(sysctl_min_value net.core.somaxconn 65535)" || die "无法提升 net.core.somaxconn"
    syn_backlog="$(sysctl_min_value net.ipv4.tcp_max_syn_backlog 262144)" || die "无法提升 TCP SYN backlog"
    if [[ -e /proc/sys/net/netfilter/nf_conntrack_max ]]; then
        conntrack="$(sysctl_min_value net.netfilter.nf_conntrack_max 4194304)" || die "无法提升 nf_conntrack_max"
    fi

    local sysctl_tmp
    sysctl_tmp="$(mktemp)" || die "无法创建 sysctl 临时文件"
    {
        echo "fs.nr_open=${nr_open}"
        echo "fs.file-max=${file_max}"
        echo "net.core.somaxconn=${somaxconn}"
        echo "net.ipv4.tcp_max_syn_backlog=${syn_backlog}"
        [[ -z "$conntrack" ]] || echo "net.netfilter.nf_conntrack_max=${conntrack}"
    } > "$sysctl_tmp"
    install -m 0644 "$sysctl_tmp" /etc/sysctl.d/99-nuro-sub2api-haproxy.conf
    rm -f "$sysctl_tmp"
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

remove_dynamic_admission_cells() {
    local ids
    ids="$(docker ps -aq --filter "name=^/${COMPOSE_PROJECT_NAME}-admission-(openai|anthropic)-" 2>/dev/null || true)"
    [[ -z "$ids" ]] || docker rm -f $ids >/dev/null 2>&1 || true
}

remove_legacy_runtime_containers() {
    local name
    for name in "$APP_CONTAINER" "$EDGE_CONTAINER"; do
        if docker container inspect "$name" >/dev/null 2>&1; then
            info "移除旧运行容器 ${name}，释放端口给成对副本入口 ..."
            docker rm -f "$name" >/dev/null || die "无法移除旧运行容器 ${name}"
        fi
    done
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
            --exclude='./edge-rs/target' \
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

edge_auto_disabled() {
    local env_file="$1"
    if [[ "$(read_env_value "$env_file" NURO_EDGE_AUTO_DISABLED)" == "true" ]]; then
        return 0
    fi
    if [[ "$(read_env_value "$env_file" NURO_EDGE_ENABLED)" == "false" &&
        "$(read_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_MODE)" == "off" &&
        "$(read_env_value "$env_file" SUB2API_EDGE_QUEUE_BUFFER_SIZE)" == "20000" &&
        "$(read_env_value "$env_file" SUB2API_EDGE_PER_ACCOUNT_WORKERS)" == "4" ]]; then
        return 0
    fi
    return 1
}

reenable_auto_disabled_edge_for_upgrade() {
    local env_file="$1"
    if edge_auto_disabled "$env_file"; then
        warn "检测到上次升级曾因 edge 构建/启动失败自动关闭，本次升级将重新尝试启用 Rust edge。"
        set_env_value "$env_file" NURO_EDGE_ENABLED true
        set_env_value "$env_file" NURO_EDGE_AUTO_DISABLED false
    fi
}

upgrade_env_default_value() {
    local file="$1"
    local key="$2"
    local old_value="$3"
    local new_value="$4"
    local current

    current="$(read_env_value "$file" "$key")"
    if [[ -z "$current" || "$current" == "$old_value" ]]; then
        set_env_value "$file" "$key" "$new_value"
    fi
}

ensure_edge_env_values() {
    local env_file="$1"
    local secret
    local prepare_timeout
    local complete_timeout

    touch "$env_file"
    ensure_env_value "$env_file" NURO_EDGE_ENABLED "$NURO_EDGE_ENABLED"
    ensure_env_value "$env_file" AUTOSCALE_MIN_REPLICAS 2
    ensure_env_value "$env_file" AUTOSCALE_MAX_REPLICAS 32
    ensure_env_value "$env_file" AUTOSCALE_INTERVAL_SECONDS 15
    ensure_env_value "$env_file" AUTOSCALE_TARGET_STREAMS_PER_PAIR 20000
    ensure_env_value "$env_file" AUTOSCALE_TARGET_RPS_PER_PAIR 3000
    ensure_env_value "$env_file" AUTOSCALE_TARGET_GO_ACTIVE_PER_PAIR 4000
    upgrade_env_default_value "$env_file" AUTOSCALE_TARGET_RELAY_WORKERS 256 512
    ensure_env_value "$env_file" AUTOSCALE_TARGET_RELAY_WORKERS 512
    ensure_env_value "$env_file" AUTOSCALE_MIN_CPU_PER_PAIR 4
    ensure_env_value "$env_file" AUTOSCALE_MIN_MEMORY_MB_PER_PAIR 2048
    ensure_env_value "$env_file" AUTOSCALE_SCALE_DOWN_SECONDS 600
    upgrade_env_default_value "$env_file" AUTOSCALE_DRAIN_SECONDS 1800 30
    ensure_env_value "$env_file" AUTOSCALE_DRAIN_SECONDS 30
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

    set_env_value "$env_file" NURO_EDGE_AUTO_DISABLED false
    set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_ENABLED true
    set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_INTERNAL_API_ENABLED true
    set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET "$secret"
    set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_MODE relay
    set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_INGRESS_PROXY_ENABLED true
    set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_LISTEN_ADDR 127.0.0.1:18080
    set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_GO_BASE_URL http://127.0.0.1:8080
    set_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_CONTROL_BASE_URL http://127.0.0.1:8080
    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_RELAY_CHAT_COMPLETIONS true
    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES true
    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES_WEBSOCKET true
    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_ROLLOUT_PERCENT 100
    prepare_timeout="$(read_env_value "$env_file" SUB2API_EDGE_PREPARE_TIMEOUT_MS)"
    complete_timeout="$(read_env_value "$env_file" SUB2API_EDGE_COMPLETE_TIMEOUT_MS)"
    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_PREPARE_TIMEOUT_MS "${prepare_timeout:-1500}"
    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_COMPLETE_TIMEOUT_MS "${complete_timeout:-1500}"
    upgrade_env_default_value "$env_file" GATEWAY_OPENAI_EDGE_RS_LEASE_TTL_MS 1800000 120000
    ensure_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_LEASE_TTL_MS 120000
    upgrade_env_default_value "$env_file" GATEWAY_CONCURRENCY_SLOT_TTL_MINUTES 30 2
    upgrade_env_default_value "$env_file" GATEWAY_CONCURRENCY_SLOT_TTL_MINUTES 15 2
    ensure_env_value "$env_file" GATEWAY_CONCURRENCY_SLOT_TTL_MINUTES 2
    ensure_env_value "$env_file" GATEWAY_STREAM_LOW_LATENCY_MODE smart
    ensure_env_value "$env_file" GATEWAY_LOW_LATENCY_STREAM_HEADERS true

    set_env_value "$env_file" SUB2API_EDGE_LISTEN_ADDR 0.0.0.0:18080
    set_env_value "$env_file" SUB2API_EDGE_GO_BASE_URL http://app:8080
    set_env_value "$env_file" SUB2API_EDGE_CONTROL_BASE_URL http://app:8080
    set_env_value "$env_file" SUB2API_EDGE_INTERNAL_SECRET "$secret"
    upgrade_env_default_value "$env_file" SUB2API_EDGE_NODE_ID nuro-edge-rs ""
    set_env_value "$env_file" SUB2API_EDGE_PREPARE_TIMEOUT_MS "$(read_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_PREPARE_TIMEOUT_MS)"
    set_env_value "$env_file" SUB2API_EDGE_COMPLETE_TIMEOUT_MS "$(read_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_COMPLETE_TIMEOUT_MS)"
    upgrade_env_default_value "$env_file" SUB2API_EDGE_DRAIN_TIMEOUT_SECS 1800 30
    ensure_env_value "$env_file" SUB2API_EDGE_DRAIN_TIMEOUT_SECS 30
    upgrade_env_default_value "$env_file" SUB2API_EDGE_INITIAL_POOL_SIZE 10000 512
    upgrade_env_default_value "$env_file" SUB2API_EDGE_QUEUE_BUFFER_SIZE 20000 512
    upgrade_env_default_value "$env_file" SUB2API_EDGE_QUEUE_BUFFER_SIZE 2000 512
    upgrade_env_default_value "$env_file" SUB2API_EDGE_QUEUE_BUFFER_SIZE 128 512
    upgrade_env_default_value "$env_file" SUB2API_EDGE_QUEUE_MAX_BYTES 67108864 268435456
    ensure_env_value "$env_file" SUB2API_EDGE_QUEUE_MAX_BYTES 268435456
    ensure_env_value "$env_file" SUB2API_EDGE_MAX_HEADER_BYTES 65536
    upgrade_env_default_value "$env_file" SUB2API_EDGE_INGRESS_BODY_MAX_BYTES 1073741824 2147483648
    ensure_env_value "$env_file" SUB2API_EDGE_INGRESS_BODY_MAX_BYTES 2147483648
    upgrade_env_default_value "$env_file" SUB2API_EDGE_GLOBAL_WORKERS 256 512
    ensure_env_value "$env_file" SUB2API_EDGE_GLOBAL_WORKERS 512
    upgrade_env_default_value "$env_file" SUB2API_EDGE_PER_ACCOUNT_WORKERS 4 128
    upgrade_env_default_value "$env_file" SUB2API_EDGE_PER_ACCOUNT_WORKERS 32 128
    upgrade_env_default_value "$env_file" SUB2API_EDGE_PER_ACCOUNT_WORKERS 64 128
    ensure_env_value "$env_file" SUB2API_EDGE_MAX_RELAY_DOMAINS 4096
    ensure_env_value "$env_file" SUB2API_EDGE_RELAY_DOMAIN_IDLE_SECS 300
    ensure_env_value "$env_file" SUB2API_EDGE_MAX_PROXY_CLIENTS 1024
    ensure_env_value "$env_file" SUB2API_EDGE_PROXY_CLIENT_IDLE_SECS 300
    upgrade_env_default_value "$env_file" SUB2API_EDGE_MAX_IDLE_PER_ACCOUNT 8 128
    upgrade_env_default_value "$env_file" SUB2API_EDGE_MAX_IDLE_PER_ACCOUNT 16 128
    upgrade_env_default_value "$env_file" SUB2API_EDGE_MAX_IDLE_PER_ACCOUNT 64 128
    ensure_env_value "$env_file" SUB2API_EDGE_QUEUE_WAIT_BUDGET_MS 150
    ensure_env_value "$env_file" SUB2API_EDGE_LARGE_PAYLOAD_PASSTHROUGH true
    ensure_env_value "$env_file" SUB2API_EDGE_LARGE_PAYLOAD_THRESHOLD_BYTES 262144
    ensure_env_value "$env_file" SUB2API_EDGE_WS_IDLE_PER_KEY 1
    ensure_env_value "$env_file" SUB2API_EDGE_MAX_WS_IDLE_KEYS 1024
    ensure_env_value "$env_file" SUB2API_EDGE_WS_IDLE_TTL_SECS 300
    ensure_env_value "$env_file" SUB2API_EDGE_MAX_DYNAMIC_WARM_KEYS 4096
    if [[ "$(read_env_value "$env_file" SUB2API_EDGE_UPSTREAM_WARM_URL)" == "https://api.openai.com/v1/chat/completions" ]]; then
        set_env_value "$env_file" SUB2API_EDGE_UPSTREAM_WARM_URL ""
    fi
    upgrade_env_default_value "$env_file" SUB2API_EDGE_UPSTREAM_WARM_INTERVAL_SECS 240 30
    ensure_env_value "$env_file" SUB2API_EDGE_UPSTREAM_DYNAMIC_WARM_ACTIVE_SECS 300
}

ensure_scheduler_env_values() {
    local env_file="$1"
    local legacy_redis_maxclients

    touch "$env_file"
	ensure_env_value "$env_file" SERVER_TRUSTED_PROXIES ""
	upgrade_env_default_value "$env_file" SERVER_GRACEFUL_SHUTDOWN_TIMEOUT 1800 5
	ensure_env_value "$env_file" SERVER_GRACEFUL_SHUTDOWN_TIMEOUT 5
    ensure_env_value "$env_file" SECURITY_TRUST_FORWARDED_IP_FOR_API_KEY_ACL false
    ensure_env_value "$env_file" SECURITY_FORWARDED_CLIENT_IP_HEADERS ""
    ensure_env_value "$env_file" REDIS_POOL_SIZE 1024
    ensure_env_value "$env_file" REDIS_MIN_IDLE_CONNS 128
    ensure_env_value "$env_file" REDIS_DIAL_TIMEOUT_SECONDS 1
    ensure_env_value "$env_file" REDIS_READ_TIMEOUT_SECONDS 1
    ensure_env_value "$env_file" REDIS_WRITE_TIMEOUT_SECONDS 1
    legacy_redis_maxclients="$(read_env_value "$env_file" REDIS_MAXCLIENTS)"
    if [[ -n "$legacy_redis_maxclients" && -z "$(read_env_value "$env_file" REDIS_MAX_CLIENTS)" ]]; then
        set_env_value "$env_file" REDIS_MAX_CLIENTS "$legacy_redis_maxclients"
    fi
    ensure_env_value "$env_file" REDIS_MAX_CLIENTS 100000
    ensure_env_value "$env_file" REDIS_NOFILE_LIMIT 200000

    ensure_env_value "$env_file" GATEWAY_SCHEDULING_LOAD_BATCH_ENABLED true
    ensure_env_value "$env_file" GATEWAY_SCHEDULING_LOAD_BATCH_CACHE_TTL_MS 200
    ensure_env_value "$env_file" GATEWAY_SCHEDULING_PREFER_SOONEST_RESET false
    ensure_env_value "$env_file" GATEWAY_SCHEDULING_CELL_ENABLED true
    ensure_env_value "$env_file" GATEWAY_SCHEDULING_CELL_ID cell-1
    ensure_env_value "$env_file" GATEWAY_SCHEDULING_CELL_IDS cell-1
    ensure_env_value "$env_file" GATEWAY_SCHEDULING_CANDIDATE_SLOT_ARBITER_ENABLED true
    ensure_env_value "$env_file" GATEWAY_SCHEDULING_CANDIDATE_SLOT_ARBITER_MAX_CANDIDATES 16
    ensure_env_value "$env_file" GATEWAY_SCHEDULING_LOCAL_SNAPSHOT_ENABLED true
    ensure_env_value "$env_file" GATEWAY_SCHEDULING_LOCAL_SNAPSHOT_TTL_MS 500
    ensure_env_value "$env_file" GATEWAY_SCHEDULING_LOCAL_SNAPSHOT_MAX_KEYS 4096
    ensure_env_value "$env_file" GATEWAY_SCHEDULING_EVENT_BUS_ENABLED true
    ensure_env_value "$env_file" GATEWAY_SCHEDULING_EVENT_BUS_BACKEND redis_stream
    ensure_env_value "$env_file" GATEWAY_SCHEDULING_SLOT_CLEANUP_INTERVAL 10s

	ensure_env_value "$env_file" GATEWAY_ADMISSION_ENABLED true
	ensure_env_value "$env_file" GATEWAY_ADMISSION_NODE_ID ""
	ensure_env_value "$env_file" GATEWAY_ADMISSION_OPENAI_CELLS "openai-001=redis://admission-openai-001:6379/0"
	ensure_env_value "$env_file" GATEWAY_ADMISSION_ANTHROPIC_CELLS "anthropic-001=redis://admission-anthropic-001:6379/0"
	ensure_env_value "$env_file" GATEWAY_ADMISSION_ESCROW_ENABLED true
	ensure_env_value "$env_file" GATEWAY_ADMISSION_ESCROW_GRANT_SIZE 16
	ensure_env_value "$env_file" GATEWAY_ADMISSION_NODE_TTL_SECONDS 30
	ensure_env_value "$env_file" GATEWAY_ADMISSION_DEAD_NODE_GRACE_SECONDS 900
	ensure_env_value "$env_file" ADMISSION_CELL_AUTOSCALE_ENABLED true
	ensure_env_value "$env_file" ADMISSION_CELL_MAX_PER_PLATFORM 8
	ensure_env_value "$env_file" ADMISSION_CELL_TARGET_OPS 50000
	ensure_env_value "$env_file" ADMISSION_CELL_TARGET_MEMORY_MB 8192
	ensure_env_value "$env_file" AUTOSCALE_UPSTREAM_ERROR_RATIO 0.20
    ensure_env_value "$env_file" AUTOSCALE_FORCE_STOP_SECONDS 30
	ensure_env_value "$env_file" SUB2API_STARTUP_JITTER_MAX_MS 30000
	ensure_env_value "$env_file" SUB2API_EDGE_STARTUP_JITTER_MAX_MS 30000
}

generate_admin_password() {
    ADMIN_PASS="$(openssl rand -hex 12)"
}

read_env_value() {
    local file="$1"
    local key="$2"
    grep -E "^${key}=" "$file" 2>/dev/null | tail -n 1 | cut -d= -f2- || true
}

pg_ident() {
    local value="$1"
    value="${value//\"/\"\"}"
    printf '"%s"' "$value"
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
SERVER_READ_HEADER_TIMEOUT=10
SERVER_MAX_HEADER_BYTES=65536
SERVER_GRACEFUL_SHUTDOWN_TIMEOUT=5
SERVER_TRUSTED_PROXIES=
RUN_MODE=standard
TZ=Asia/Shanghai

POSTGRES_USER=nuro_sub2api
POSTGRES_DB=nuro_sub2api
POSTGRES_PASSWORD=$(generate_secret)
POSTGRES_MAX_CONNECTIONS=2000
POSTGRES_SHARED_BUFFERS=1GB
POSTGRES_EFFECTIVE_CACHE_SIZE=4GB
POSTGRES_MAINTENANCE_WORK_MEM=128MB

REDIS_PASSWORD=
REDIS_DB=0
REDIS_POOL_SIZE=1024
REDIS_MIN_IDLE_CONNS=128
REDIS_MAX_CLIENTS=100000
REDIS_NOFILE_LIMIT=200000
REDIS_DIAL_TIMEOUT_SECONDS=1
REDIS_READ_TIMEOUT_SECONDS=1
REDIS_WRITE_TIMEOUT_SECONDS=1
REDIS_IO_THREADS=8

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
SECURITY_TRUST_FORWARDED_IP_FOR_API_KEY_ACL=false
SECURITY_FORWARDED_CLIENT_IP_HEADERS=
UPDATE_PROXY_URL=
UPDATE_GITHUB_TOKEN=

GATEWAY_OPENAI_RESPONSE_HEADER_TIMEOUT=0
GATEWAY_OPENAI_HTTP2_ENABLED=true
GATEWAY_OPENAI_HTTP2_ALLOW_PROXY_FALLBACK_TO_HTTP1=true
GATEWAY_OPENAI_HTTP2_FALLBACK_ERROR_THRESHOLD=2
GATEWAY_OPENAI_HTTP2_FALLBACK_WINDOW_SECONDS=60
GATEWAY_OPENAI_HTTP2_FALLBACK_TTL_SECONDS=600
NURO_EDGE_ENABLED=true
AUTOSCALE_MIN_REPLICAS=2
AUTOSCALE_MAX_REPLICAS=32
AUTOSCALE_INTERVAL_SECONDS=15
AUTOSCALE_TARGET_STREAMS_PER_PAIR=20000
AUTOSCALE_TARGET_RPS_PER_PAIR=3000
AUTOSCALE_TARGET_GO_ACTIVE_PER_PAIR=4000
AUTOSCALE_TARGET_RELAY_WORKERS=512
AUTOSCALE_MIN_CPU_PER_PAIR=4
AUTOSCALE_MIN_MEMORY_MB_PER_PAIR=2048
AUTOSCALE_SCALE_DOWN_SECONDS=600
AUTOSCALE_DRAIN_SECONDS=30
GATEWAY_OPENAI_EDGE_RS_ENABLED=true
GATEWAY_OPENAI_EDGE_RS_INTERNAL_API_ENABLED=true
GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET=$(generate_secret)
GATEWAY_OPENAI_EDGE_RS_MODE=relay
GATEWAY_OPENAI_EDGE_RS_INGRESS_PROXY_ENABLED=true
GATEWAY_OPENAI_EDGE_RS_LISTEN_ADDR=127.0.0.1:18080
GATEWAY_OPENAI_EDGE_RS_GO_BASE_URL=http://127.0.0.1:8080
GATEWAY_OPENAI_EDGE_RS_CONTROL_BASE_URL=http://127.0.0.1:8080
GATEWAY_OPENAI_EDGE_RS_RELAY_CHAT_COMPLETIONS=true
GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES=true
GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES_WEBSOCKET=true
GATEWAY_OPENAI_EDGE_RS_ROLLOUT_PERCENT=100
GATEWAY_OPENAI_EDGE_RS_PREPARE_TIMEOUT_MS=1500
GATEWAY_OPENAI_EDGE_RS_COMPLETE_TIMEOUT_MS=1500
GATEWAY_OPENAI_EDGE_RS_LEASE_TTL_MS=120000
GATEWAY_CONCURRENCY_SLOT_TTL_MINUTES=2
GATEWAY_STREAM_LOW_LATENCY_MODE=smart
GATEWAY_LOW_LATENCY_STREAM_HEADERS=true
SUB2API_EDGE_LISTEN_ADDR=0.0.0.0:18080
SUB2API_EDGE_GO_BASE_URL=http://127.0.0.1:8080
SUB2API_EDGE_CONTROL_BASE_URL=http://127.0.0.1:8080
SUB2API_EDGE_INTERNAL_SECRET=
SUB2API_EDGE_NODE_ID=
SUB2API_EDGE_PREPARE_TIMEOUT_MS=1500
SUB2API_EDGE_COMPLETE_TIMEOUT_MS=1500
SUB2API_EDGE_DRAIN_TIMEOUT_SECS=30
SUB2API_EDGE_INITIAL_POOL_SIZE=512
SUB2API_EDGE_QUEUE_BUFFER_SIZE=512
SUB2API_EDGE_QUEUE_MAX_BYTES=268435456
SUB2API_EDGE_MAX_HEADER_BYTES=65536
SUB2API_EDGE_INGRESS_BODY_MAX_BYTES=2147483648
SUB2API_EDGE_GLOBAL_WORKERS=512
SUB2API_EDGE_PER_ACCOUNT_WORKERS=128
SUB2API_EDGE_MAX_RELAY_DOMAINS=4096
SUB2API_EDGE_RELAY_DOMAIN_IDLE_SECS=300
SUB2API_EDGE_MAX_PROXY_CLIENTS=1024
SUB2API_EDGE_PROXY_CLIENT_IDLE_SECS=300
SUB2API_EDGE_MAX_IDLE_PER_ACCOUNT=128
SUB2API_EDGE_QUEUE_WAIT_BUDGET_MS=150
SUB2API_EDGE_LARGE_PAYLOAD_PASSTHROUGH=true
SUB2API_EDGE_LARGE_PAYLOAD_THRESHOLD_BYTES=262144
SUB2API_EDGE_WS_IDLE_PER_KEY=1
SUB2API_EDGE_MAX_WS_IDLE_KEYS=1024
SUB2API_EDGE_WS_IDLE_TTL_SECS=300
SUB2API_EDGE_MAX_DYNAMIC_WARM_KEYS=4096
SUB2API_EDGE_UPSTREAM_WARM_URL=
SUB2API_EDGE_UPSTREAM_WARM_INTERVAL_SECS=30
SUB2API_EDGE_UPSTREAM_DYNAMIC_WARM_ACTIVE_SECS=300
GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT=900
GATEWAY_IMAGE_STREAM_KEEPALIVE_INTERVAL=10
GATEWAY_IMAGE_CONCURRENCY_ENABLED=false
GATEWAY_IMAGE_CONCURRENCY_MAX_CONCURRENT_REQUESTS=0
GATEWAY_IMAGE_CONCURRENCY_OVERFLOW_MODE=reject
GATEWAY_IMAGE_CONCURRENCY_WAIT_TIMEOUT_SECONDS=30
GATEWAY_IMAGE_CONCURRENCY_MAX_WAITING_REQUESTS=100
GATEWAY_SCHEDULING_LOAD_BATCH_ENABLED=true
GATEWAY_SCHEDULING_LOAD_BATCH_CACHE_TTL_MS=200
GATEWAY_SCHEDULING_PREFER_SOONEST_RESET=false
GATEWAY_SCHEDULING_CELL_ENABLED=true
GATEWAY_SCHEDULING_CELL_ID=cell-1
GATEWAY_SCHEDULING_CELL_IDS=cell-1
GATEWAY_SCHEDULING_CANDIDATE_SLOT_ARBITER_ENABLED=true
GATEWAY_SCHEDULING_CANDIDATE_SLOT_ARBITER_MAX_CANDIDATES=16
GATEWAY_SCHEDULING_LOCAL_SNAPSHOT_ENABLED=true
GATEWAY_SCHEDULING_LOCAL_SNAPSHOT_TTL_MS=500
GATEWAY_SCHEDULING_LOCAL_SNAPSHOT_MAX_KEYS=4096
GATEWAY_SCHEDULING_EVENT_BUS_ENABLED=true
GATEWAY_SCHEDULING_EVENT_BUS_BACKEND=redis_stream
GATEWAY_SCHEDULING_SLOT_CLEANUP_INTERVAL=10s
GATEWAY_ADMISSION_ENABLED=true
GATEWAY_ADMISSION_NODE_ID=
GATEWAY_ADMISSION_OPENAI_CELLS=openai-001=redis://admission-openai-001:6379/0
GATEWAY_ADMISSION_ANTHROPIC_CELLS=anthropic-001=redis://admission-anthropic-001:6379/0
GATEWAY_ADMISSION_ESCROW_ENABLED=true
GATEWAY_ADMISSION_ESCROW_GRANT_SIZE=16
GATEWAY_ADMISSION_NODE_TTL_SECONDS=30
GATEWAY_ADMISSION_DEAD_NODE_GRACE_SECONDS=900
ADMISSION_CELL_AUTOSCALE_ENABLED=true
ADMISSION_CELL_MAX_PER_PLATFORM=8
ADMISSION_CELL_TARGET_OPS=50000
ADMISSION_CELL_TARGET_MEMORY_MB=8192
AUTOSCALE_UPSTREAM_ERROR_RATIO=0.20
AUTOSCALE_FORCE_STOP_SECONDS=30
SUB2API_STARTUP_JITTER_MAX_MS=30000
SUB2API_EDGE_STARTUP_JITTER_MAX_MS=30000
EOF

    local edge_secret
    edge_secret="$(read_env_value "${workdir}/.env" GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET)"
    set_env_value "${workdir}/.env" SUB2API_EDGE_INTERNAL_SECRET "$edge_secret"
    ensure_scheduler_env_values "${workdir}/.env"
    chmod 600 "${workdir}/.env"
}

create_haproxy_config() {
    local workdir="$1"
    cat > "${workdir}/haproxy.cfg" <<'EOF'
global
    # One proxied connection consumes a client and a server FD. The container
    # nofile limit is 3,000,000, leaving headroom above 2 * maxconn.
    maxconn 1200000
    log stdout format raw local0

defaults
    mode http
    log global
    option httplog
    option dontlognull
    option http-no-delay
    timeout connect 3s
    timeout client 1800s
    timeout server 1800s
    timeout tunnel 3600s

resolvers docker
    nameserver dns 127.0.0.11:53
    resolve_retries 3
    timeout resolve 1s
    timeout retry 1s
    hold valid 5s

frontend public_api
    bind *:8080
    acl private_runtime path -i /metrics /internal
    acl private_runtime path_beg -i /internal/
    http-request deny deny_status 404 if private_runtime
    acl openai_edge path -i /v1/chat/completions /v1/responses
    use_backend edge_pool if openai_edge
    default_backend go_pool

backend edge_pool
    balance leastconn
    option httpchk GET /readyz
    http-response set-header Cache-Control "no-cache, no-transform"
    http-response set-header X-Accel-Buffering "no"
    http-check expect status 200
    server-template edge 1-128 app:18080 check resolvers docker init-addr libc,none

backend go_pool
    balance leastconn
    option httpchk GET /readyz
    http-check expect status 200
    server-template go 1-128 app:8080 check resolvers docker init-addr libc,none

frontend stats
    bind *:8404
    http-request use-service prometheus-exporter if { path /metrics }
    stats enable
    stats uri /stats
EOF
}

create_compose_file() {
    local workdir="$1"

    create_haproxy_config "$workdir"

    cat > "${workdir}/docker-compose.yml" <<EOF
name: ${COMPOSE_PROJECT_NAME}

services:
  app:
    build:
      context: ./${SOURCE_DIR_NAME}
      dockerfile: Dockerfile
    image: ${IMAGE_NAME}
    restart: unless-stopped
    # paired-entrypoint drains Go/Edge for up to 30s before terminating Go.
    # Keep a small supervisor margin so Compose does not SIGKILL the pair
    # before the shutdown ordering and settlement callbacks complete.
    stop_grace_period: 45s
    command: ["/app/paired-entrypoint.sh"]
    ulimits:
      nofile:
        soft: 100000
        hard: 100000
    expose:
      - "8080"
      - "18080"
    volumes:
      - ./data:/app/data:Z
    environment:
      - AUTO_SETUP=true
      - SERVER_HOST=0.0.0.0
      - SERVER_PORT=8080
      - SERVER_MODE=\${SERVER_MODE:-release}
      - SERVER_READ_HEADER_TIMEOUT=\${SERVER_READ_HEADER_TIMEOUT:-10}
      - SERVER_MAX_HEADER_BYTES=\${SERVER_MAX_HEADER_BYTES:-65536}
      - SERVER_GRACEFUL_SHUTDOWN_TIMEOUT=\${SERVER_GRACEFUL_SHUTDOWN_TIMEOUT:-5}
      - SERVER_TRUSTED_PROXIES=\${SERVER_TRUSTED_PROXIES:-}
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
      - REDIS_MIN_IDLE_CONNS=\${REDIS_MIN_IDLE_CONNS:-128}
      - REDIS_DIAL_TIMEOUT_SECONDS=\${REDIS_DIAL_TIMEOUT_SECONDS:-1}
      - REDIS_READ_TIMEOUT_SECONDS=\${REDIS_READ_TIMEOUT_SECONDS:-1}
      - REDIS_WRITE_TIMEOUT_SECONDS=\${REDIS_WRITE_TIMEOUT_SECONDS:-1}
      - REDIS_ENABLE_TLS=\${REDIS_ENABLE_TLS:-false}
      - ADMIN_EMAIL=\${ADMIN_EMAIL:-admin@nuro-sub2api.local}
      - ADMIN_PASSWORD=\${ADMIN_PASSWORD:-}
      - JWT_SECRET=\${JWT_SECRET:-}
      - JWT_EXPIRE_HOUR=\${JWT_EXPIRE_HOUR:-24}
      - TOTP_ENCRYPTION_KEY=\${TOTP_ENCRYPTION_KEY:-}
      - TZ=\${TZ:-Asia/Shanghai}
      - SUB2API_STARTUP_JITTER_MAX_MS=\${SUB2API_STARTUP_JITTER_MAX_MS:-30000}
      - SUB2API_EDGE_STARTUP_JITTER_MAX_MS=\${SUB2API_EDGE_STARTUP_JITTER_MAX_MS:-30000}
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
      - SECURITY_TRUST_FORWARDED_IP_FOR_API_KEY_ACL=\${SECURITY_TRUST_FORWARDED_IP_FOR_API_KEY_ACL:-false}
      - SECURITY_FORWARDED_CLIENT_IP_HEADERS=\${SECURITY_FORWARDED_CLIENT_IP_HEADERS:-}
      - UPDATE_PROXY_URL=\${UPDATE_PROXY_URL:-}
      - UPDATE_GITHUB_TOKEN=\${UPDATE_GITHUB_TOKEN:-}
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
      - GATEWAY_OPENAI_EDGE_RS_LISTEN_ADDR=\${GATEWAY_OPENAI_EDGE_RS_LISTEN_ADDR:-127.0.0.1:18080}
      - GATEWAY_OPENAI_EDGE_RS_GO_BASE_URL=\${GATEWAY_OPENAI_EDGE_RS_GO_BASE_URL:-http://127.0.0.1:8080}
      - GATEWAY_OPENAI_EDGE_RS_CONTROL_BASE_URL=\${GATEWAY_OPENAI_EDGE_RS_CONTROL_BASE_URL:-http://127.0.0.1:8080}
      - GATEWAY_OPENAI_EDGE_RS_RELAY_CHAT_COMPLETIONS=\${GATEWAY_OPENAI_EDGE_RS_RELAY_CHAT_COMPLETIONS:-true}
      - GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES=\${GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES:-true}
      - GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES_WEBSOCKET=\${GATEWAY_OPENAI_EDGE_RS_RELAY_RESPONSES_WEBSOCKET:-true}
      - GATEWAY_OPENAI_EDGE_RS_ROLLOUT_PERCENT=\${GATEWAY_OPENAI_EDGE_RS_ROLLOUT_PERCENT:-100}
      - GATEWAY_OPENAI_EDGE_RS_LEASE_TTL_MS=\${GATEWAY_OPENAI_EDGE_RS_LEASE_TTL_MS:-120000}
      - GATEWAY_CONCURRENCY_SLOT_TTL_MINUTES=\${GATEWAY_CONCURRENCY_SLOT_TTL_MINUTES:-2}
      - GATEWAY_STREAM_LOW_LATENCY_MODE=\${GATEWAY_STREAM_LOW_LATENCY_MODE:-smart}
      - GATEWAY_LOW_LATENCY_STREAM_HEADERS=\${GATEWAY_LOW_LATENCY_STREAM_HEADERS:-true}
      - GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT=\${GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT:-900}
      - GATEWAY_IMAGE_STREAM_KEEPALIVE_INTERVAL=\${GATEWAY_IMAGE_STREAM_KEEPALIVE_INTERVAL:-10}
      - GATEWAY_IMAGE_CONCURRENCY_ENABLED=\${GATEWAY_IMAGE_CONCURRENCY_ENABLED:-false}
      - GATEWAY_IMAGE_CONCURRENCY_MAX_CONCURRENT_REQUESTS=\${GATEWAY_IMAGE_CONCURRENCY_MAX_CONCURRENT_REQUESTS:-0}
      - GATEWAY_IMAGE_CONCURRENCY_OVERFLOW_MODE=\${GATEWAY_IMAGE_CONCURRENCY_OVERFLOW_MODE:-reject}
      - GATEWAY_IMAGE_CONCURRENCY_WAIT_TIMEOUT_SECONDS=\${GATEWAY_IMAGE_CONCURRENCY_WAIT_TIMEOUT_SECONDS:-30}
      - GATEWAY_IMAGE_CONCURRENCY_MAX_WAITING_REQUESTS=\${GATEWAY_IMAGE_CONCURRENCY_MAX_WAITING_REQUESTS:-100}
      - GATEWAY_SCHEDULING_LOAD_BATCH_ENABLED=\${GATEWAY_SCHEDULING_LOAD_BATCH_ENABLED:-true}
      - GATEWAY_SCHEDULING_LOAD_BATCH_CACHE_TTL_MS=\${GATEWAY_SCHEDULING_LOAD_BATCH_CACHE_TTL_MS:-200}
      - GATEWAY_SCHEDULING_PREFER_SOONEST_RESET=\${GATEWAY_SCHEDULING_PREFER_SOONEST_RESET:-false}
      - GATEWAY_SCHEDULING_CELL_ENABLED=\${GATEWAY_SCHEDULING_CELL_ENABLED:-true}
      - GATEWAY_SCHEDULING_CELL_ID=\${GATEWAY_SCHEDULING_CELL_ID:-cell-1}
      - GATEWAY_SCHEDULING_CELL_IDS=\${GATEWAY_SCHEDULING_CELL_IDS:-cell-1}
      - GATEWAY_SCHEDULING_CANDIDATE_SLOT_ARBITER_ENABLED=\${GATEWAY_SCHEDULING_CANDIDATE_SLOT_ARBITER_ENABLED:-true}
      - RUST_LOG=\${RUST_LOG:-warn}
      - SUB2API_EDGE_LISTEN_ADDR=0.0.0.0:18080
      - SUB2API_EDGE_GO_BASE_URL=http://127.0.0.1:8080
      - SUB2API_EDGE_CONTROL_BASE_URL=http://127.0.0.1:8080
      - SUB2API_EDGE_INTERNAL_SECRET=\${SUB2API_EDGE_INTERNAL_SECRET:?SUB2API_EDGE_INTERNAL_SECRET is required}
      - SUB2API_EDGE_NODE_ID=
      - SUB2API_EDGE_PREPARE_TIMEOUT_MS=\${SUB2API_EDGE_PREPARE_TIMEOUT_MS:-1500}
      - SUB2API_EDGE_COMPLETE_TIMEOUT_MS=\${SUB2API_EDGE_COMPLETE_TIMEOUT_MS:-1500}
      - SUB2API_EDGE_DRAIN_TIMEOUT_SECS=\${SUB2API_EDGE_DRAIN_TIMEOUT_SECS:-30}
      - SUB2API_EDGE_INITIAL_POOL_SIZE=\${SUB2API_EDGE_INITIAL_POOL_SIZE:-512}
      - SUB2API_EDGE_QUEUE_BUFFER_SIZE=\${SUB2API_EDGE_QUEUE_BUFFER_SIZE:-512}
      - SUB2API_EDGE_QUEUE_MAX_BYTES=\${SUB2API_EDGE_QUEUE_MAX_BYTES:-268435456}
      - SUB2API_EDGE_MAX_HEADER_BYTES=\${SUB2API_EDGE_MAX_HEADER_BYTES:-65536}
      - SUB2API_EDGE_INGRESS_BODY_MAX_BYTES=\${SUB2API_EDGE_INGRESS_BODY_MAX_BYTES:-2147483648}
      - SUB2API_EDGE_GLOBAL_WORKERS=\${SUB2API_EDGE_GLOBAL_WORKERS:-512}
      - SUB2API_EDGE_PER_ACCOUNT_WORKERS=\${SUB2API_EDGE_PER_ACCOUNT_WORKERS:-128}
      - SUB2API_EDGE_MAX_RELAY_DOMAINS=\${SUB2API_EDGE_MAX_RELAY_DOMAINS:-4096}
      - SUB2API_EDGE_RELAY_DOMAIN_IDLE_SECS=\${SUB2API_EDGE_RELAY_DOMAIN_IDLE_SECS:-300}
      - SUB2API_EDGE_MAX_PROXY_CLIENTS=\${SUB2API_EDGE_MAX_PROXY_CLIENTS:-1024}
      - SUB2API_EDGE_PROXY_CLIENT_IDLE_SECS=\${SUB2API_EDGE_PROXY_CLIENT_IDLE_SECS:-300}
      - SUB2API_EDGE_MAX_IDLE_PER_ACCOUNT=\${SUB2API_EDGE_MAX_IDLE_PER_ACCOUNT:-128}
      - SUB2API_EDGE_QUEUE_WAIT_BUDGET_MS=\${SUB2API_EDGE_QUEUE_WAIT_BUDGET_MS:-150}
      - SUB2API_EDGE_LARGE_PAYLOAD_PASSTHROUGH=\${SUB2API_EDGE_LARGE_PAYLOAD_PASSTHROUGH:-true}
      - SUB2API_EDGE_LARGE_PAYLOAD_THRESHOLD_BYTES=\${SUB2API_EDGE_LARGE_PAYLOAD_THRESHOLD_BYTES:-262144}
      - SUB2API_EDGE_WS_IDLE_PER_KEY=\${SUB2API_EDGE_WS_IDLE_PER_KEY:-1}
      - SUB2API_EDGE_MAX_WS_IDLE_KEYS=\${SUB2API_EDGE_MAX_WS_IDLE_KEYS:-1024}
      - SUB2API_EDGE_WS_IDLE_TTL_SECS=\${SUB2API_EDGE_WS_IDLE_TTL_SECS:-300}
      - SUB2API_EDGE_MAX_DYNAMIC_WARM_KEYS=\${SUB2API_EDGE_MAX_DYNAMIC_WARM_KEYS:-4096}
      - GATEWAY_SCHEDULING_CANDIDATE_SLOT_ARBITER_MAX_CANDIDATES=\${GATEWAY_SCHEDULING_CANDIDATE_SLOT_ARBITER_MAX_CANDIDATES:-16}
      - GATEWAY_SCHEDULING_LOCAL_SNAPSHOT_ENABLED=\${GATEWAY_SCHEDULING_LOCAL_SNAPSHOT_ENABLED:-true}
      - GATEWAY_SCHEDULING_LOCAL_SNAPSHOT_TTL_MS=\${GATEWAY_SCHEDULING_LOCAL_SNAPSHOT_TTL_MS:-500}
      - GATEWAY_SCHEDULING_LOCAL_SNAPSHOT_MAX_KEYS=\${GATEWAY_SCHEDULING_LOCAL_SNAPSHOT_MAX_KEYS:-4096}
      - GATEWAY_SCHEDULING_EVENT_BUS_ENABLED=\${GATEWAY_SCHEDULING_EVENT_BUS_ENABLED:-true}
      - GATEWAY_SCHEDULING_EVENT_BUS_BACKEND=\${GATEWAY_SCHEDULING_EVENT_BUS_BACKEND:-redis_stream}
      - GATEWAY_SCHEDULING_SLOT_CLEANUP_INTERVAL=\${GATEWAY_SCHEDULING_SLOT_CLEANUP_INTERVAL:-10s}
      - GATEWAY_ADMISSION_ENABLED=\${GATEWAY_ADMISSION_ENABLED:-true}
      - GATEWAY_ADMISSION_NODE_ID=\${GATEWAY_ADMISSION_NODE_ID:-}
      - GATEWAY_ADMISSION_OPENAI_CELLS=\${GATEWAY_ADMISSION_OPENAI_CELLS:-openai-001=redis://admission-openai-001:6379/0}
      - GATEWAY_ADMISSION_ANTHROPIC_CELLS=\${GATEWAY_ADMISSION_ANTHROPIC_CELLS:-anthropic-001=redis://admission-anthropic-001:6379/0}
      - GATEWAY_ADMISSION_ESCROW_ENABLED=\${GATEWAY_ADMISSION_ESCROW_ENABLED:-true}
      - GATEWAY_ADMISSION_ESCROW_GRANT_SIZE=\${GATEWAY_ADMISSION_ESCROW_GRANT_SIZE:-16}
      - GATEWAY_ADMISSION_NODE_TTL_SECONDS=\${GATEWAY_ADMISSION_NODE_TTL_SECONDS:-30}
      - GATEWAY_ADMISSION_DEAD_NODE_GRACE_SECONDS=\${GATEWAY_ADMISSION_DEAD_NODE_GRACE_SECONDS:-900}
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
      admission-openai-001:
        condition: service_healthy
      admission-anthropic-001:
        condition: service_healthy
    networks:
      - nuro-sub2api-network
    healthcheck:
      test: ["CMD", "wget", "-q", "-T", "5", "-O", "/dev/null", "http://localhost:8080/health"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 30s

  haproxy:
    image: haproxy:3.1-alpine
    container_name: nuro-sub2api-haproxy
    restart: unless-stopped
    ulimits:
      nofile:
        soft: 3000000
        hard: 3000000
    sysctls:
      net.ipv4.ip_local_port_range: "1024 65535"
    ports:
      - "\${BIND_HOST:-0.0.0.0}:\${SERVER_PORT:-6182}:8080"
    volumes:
      - ./haproxy.cfg:/usr/local/etc/haproxy/haproxy.cfg:ro
    depends_on:
      app:
        condition: service_healthy
    networks:
      - nuro-sub2api-network
    healthcheck:
      test: ["CMD", "wget", "-q", "-T", "5", "-O", "/dev/null", "http://127.0.0.1:8404/metrics"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 10s

  autoscaler:
    image: docker:27-cli
    container_name: nuro-sub2api-autoscaler
    restart: unless-stopped
    working_dir: /workspace
    command: ["/bin/sh", "/autoscaler.sh"]
    environment:
      - COMPOSE_PROJECT_NAME=${COMPOSE_PROJECT_NAME}
      - AUTOSCALE_MIN_REPLICAS=\${AUTOSCALE_MIN_REPLICAS:-2}
      - AUTOSCALE_MAX_REPLICAS=\${AUTOSCALE_MAX_REPLICAS:-32}
      - AUTOSCALE_MIN_CPU_PER_PAIR=\${AUTOSCALE_MIN_CPU_PER_PAIR:-4}
      - AUTOSCALE_MIN_MEMORY_MB_PER_PAIR=\${AUTOSCALE_MIN_MEMORY_MB_PER_PAIR:-2048}
      - AUTOSCALE_INTERVAL_SECONDS=\${AUTOSCALE_INTERVAL_SECONDS:-15}
      - AUTOSCALE_TARGET_STREAMS_PER_PAIR=\${AUTOSCALE_TARGET_STREAMS_PER_PAIR:-20000}
      - AUTOSCALE_TARGET_RPS_PER_PAIR=\${AUTOSCALE_TARGET_RPS_PER_PAIR:-3000}
      - AUTOSCALE_TARGET_GO_ACTIVE_PER_PAIR=\${AUTOSCALE_TARGET_GO_ACTIVE_PER_PAIR:-4000}
      - AUTOSCALE_TARGET_RELAY_WORKERS=\${AUTOSCALE_TARGET_RELAY_WORKERS:-512}
      - AUTOSCALE_SCALE_DOWN_SECONDS=\${AUTOSCALE_SCALE_DOWN_SECONDS:-600}
      - AUTOSCALE_DRAIN_SECONDS=\${AUTOSCALE_DRAIN_SECONDS:-30}
      - AUTOSCALE_FORCE_STOP_SECONDS=\${AUTOSCALE_FORCE_STOP_SECONDS:-30}
      - AUTOSCALE_UPSTREAM_ERROR_RATIO=\${AUTOSCALE_UPSTREAM_ERROR_RATIO:-0.20}
      - REDIS_PASSWORD=\${REDIS_PASSWORD:-}
      - REDIS_NOFILE_LIMIT=\${REDIS_NOFILE_LIMIT:-200000}
      - ADMISSION_CELL_AUTOSCALE_ENABLED=\${ADMISSION_CELL_AUTOSCALE_ENABLED:-true}
      - ADMISSION_CELL_MAX_PER_PLATFORM=\${ADMISSION_CELL_MAX_PER_PLATFORM:-8}
      - ADMISSION_CELL_TARGET_OPS=\${ADMISSION_CELL_TARGET_OPS:-50000}
      - ADMISSION_CELL_TARGET_MEMORY_MB=\${ADMISSION_CELL_TARGET_MEMORY_MB:-8192}
      - ADMISSION_CELL_IO_THREADS=\${REDIS_IO_THREADS:-8}
      - ADMISSION_CELL_MAX_CLIENTS=\${REDIS_MAX_CLIENTS:-100000}
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./:/workspace
      - ./${SOURCE_DIR_NAME}/deploy/single-host-autoscaler.sh:/autoscaler.sh:ro
      - autoscaler_state:/state
    depends_on:
      haproxy:
        condition: service_healthy
    networks:
      - nuro-sub2api-network

  admission-openai-001:
    image: redis:8-alpine
    container_name: nuro-sub2api-admission-openai-001
    restart: unless-stopped
    ulimits:
      nofile:
        soft: \${REDIS_NOFILE_LIMIT:-200000}
        hard: \${REDIS_NOFILE_LIMIT:-200000}
    command:
      - sh
      - -c
      - |
        set -- redis-server \\
          --save "300 10" \\
          --appendonly yes \\
          --appendfsync everysec \\
          --maxmemory \${ADMISSION_CELL_TARGET_MEMORY_MB:-8192}mb \\
          --maxmemory-policy noeviction \\
          --tcp-backlog 8192 \\
          --timeout 0 \\
          --hz 20 \\
          --maxclients \${REDIS_MAX_CLIENTS:-100000} \\
          --io-threads \${REDIS_IO_THREADS:-8} \\
          --io-threads-do-reads yes
        if [ -n "\$\$REDIS_PASSWORD" ]; then
          set -- "\$\$@" --requirepass "\$\$REDIS_PASSWORD"
        fi
        exec "\$\$@"
    environment:
      - REDIS_PASSWORD=\${REDIS_PASSWORD:-}
      - REDIS_IO_THREADS=\${REDIS_IO_THREADS:-8}
      - REDISCLI_AUTH=\${REDIS_PASSWORD:-}
    volumes:
      - ./admission_openai_001_data:/data:Z
    networks:
      - nuro-sub2api-network
    healthcheck:
      test: ["CMD-SHELL", "redis-cli ping"]
      interval: 10s
      timeout: 5s
      retries: 5

  admission-anthropic-001:
    image: redis:8-alpine
    container_name: nuro-sub2api-admission-anthropic-001
    restart: unless-stopped
    ulimits:
      nofile:
        soft: \${REDIS_NOFILE_LIMIT:-200000}
        hard: \${REDIS_NOFILE_LIMIT:-200000}
    command:
      - sh
      - -c
      - |
        set -- redis-server \\
          --save "300 10" \\
          --appendonly yes \\
          --appendfsync everysec \\
          --maxmemory \${ADMISSION_CELL_TARGET_MEMORY_MB:-8192}mb \\
          --maxmemory-policy noeviction \\
          --tcp-backlog 8192 \\
          --timeout 0 \\
          --hz 20 \\
          --maxclients \${REDIS_MAX_CLIENTS:-100000} \\
          --io-threads \${REDIS_IO_THREADS:-8} \\
          --io-threads-do-reads yes
        if [ -n "\$\$REDIS_PASSWORD" ]; then
          set -- "\$\$@" --requirepass "\$\$REDIS_PASSWORD"
        fi
        exec "\$\$@"
    environment:
      - REDIS_PASSWORD=\${REDIS_PASSWORD:-}
      - REDIS_IO_THREADS=\${REDIS_IO_THREADS:-8}
      - REDISCLI_AUTH=\${REDIS_PASSWORD:-}
    volumes:
      - ./admission_anthropic_001_data:/data:Z
    networks:
      - nuro-sub2api-network
    healthcheck:
      test: ["CMD-SHELL", "redis-cli ping"]
      interval: 10s
      timeout: 5s
      retries: 5

  postgres:
    image: postgres:18-alpine
    container_name: ${POSTGRES_CONTAINER}
    restart: unless-stopped
    command:
      - postgres
      - -c
      - max_connections=\${POSTGRES_MAX_CONNECTIONS:-2000}
      - -c
      - shared_buffers=\${POSTGRES_SHARED_BUFFERS:-1GB}
      - -c
      - effective_cache_size=\${POSTGRES_EFFECTIVE_CACHE_SIZE:-4GB}
      - -c
      - maintenance_work_mem=\${POSTGRES_MAINTENANCE_WORK_MEM:-128MB}
    ulimits:
      nofile:
        soft: 100000
        hard: 100000
    volumes:
      - ./postgres_data:/var/lib/postgresql/data:Z
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
        soft: \${REDIS_NOFILE_LIMIT:-200000}
        hard: \${REDIS_NOFILE_LIMIT:-200000}
    volumes:
      - ./redis_data:/data:Z
    command:
      - sh
      - -c
      - |
        set -- redis-server \\
          --save "300 10" \\
          --appendonly yes \\
          --appendfsync everysec \\
          --maxmemory-policy noeviction \\
          --maxclients \${REDIS_MAX_CLIENTS:-100000} \\
          --tcp-backlog 8192 \\
          --timeout 0 \\
          --hz 20 \\
          --io-threads \${REDIS_IO_THREADS:-8} \\
          --io-threads-do-reads yes
        if [ -n "\$\$REDIS_PASSWORD" ]; then
          set -- "\$\$@" --requirepass "\$\$REDIS_PASSWORD"
        fi
        exec "\$\$@"
    environment:
      - TZ=\${TZ:-Asia/Shanghai}
      - REDIS_PASSWORD=\${REDIS_PASSWORD:-}
      - REDISCLI_AUTH=\${REDIS_PASSWORD:-}
    networks:
      - nuro-sub2api-network
    healthcheck:
      test: ["CMD-SHELL", "redis-cli ping"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 5s

networks:
  nuro-sub2api-network:
    driver: bridge

volumes:
  autoscaler_state:
EOF
}

compose_build_with_edge_fallback() {
    local workdir="$1"
    local dc_cmd="$2"
    $dc_cmd -p "$COMPOSE_PROJECT_NAME" -f docker-compose.yml build
}

compose_up_with_edge_fallback() {
    local workdir="$1"
    local dc_cmd="$2"
    local env_file="${workdir}/.env"
    local app_ids current_replicas min_replicas desired_replicas
    local max_replicas min_cpu_per_pair min_memory_mb_per_pair
    local host_cpus host_memory_mb cpu_limit memory_limit capacity_limit

    ensure_haproxy_host_capacity
    remove_legacy_runtime_containers

    # Stop the controller before reading the count so it cannot scale while
    # this replacement is taking its snapshot.
    if docker container inspect "$AUTOSCALER_CONTAINER" >/dev/null 2>&1; then
        docker rm -f "$AUTOSCALER_CONTAINER" >/dev/null || die "无法停止 autoscaler"
    fi

    current_replicas="$(docker ps \
        --filter "label=com.docker.compose.project=${COMPOSE_PROJECT_NAME}" \
        --filter 'label=com.docker.compose.service=app' \
        --format '{{.ID}}' | awk 'NF { count++ } END { print count + 0 }')"
    min_replicas="$(read_env_value "$env_file" AUTOSCALE_MIN_REPLICAS)"
    min_replicas="${min_replicas:-2}"
    [[ "$min_replicas" =~ ^[1-9][0-9]*$ ]] || min_replicas=2
    max_replicas="$(read_env_value "$env_file" AUTOSCALE_MAX_REPLICAS)"
    max_replicas="${max_replicas:-32}"
    [[ "$max_replicas" =~ ^[1-9][0-9]*$ ]] || max_replicas=32
    min_cpu_per_pair="$(read_env_value "$env_file" AUTOSCALE_MIN_CPU_PER_PAIR)"
    min_cpu_per_pair="${min_cpu_per_pair:-4}"
    [[ "$min_cpu_per_pair" =~ ^[1-9][0-9]*$ ]] || min_cpu_per_pair=4
    min_memory_mb_per_pair="$(read_env_value "$env_file" AUTOSCALE_MIN_MEMORY_MB_PER_PAIR)"
    min_memory_mb_per_pair="${min_memory_mb_per_pair:-2048}"
    [[ "$min_memory_mb_per_pair" =~ ^[1-9][0-9]*$ ]] || min_memory_mb_per_pair=2048

    host_cpus="$(getconf _NPROCESSORS_ONLN 2>/dev/null || awk '/^processor/ { count++ } END { print count + 0 }' /proc/cpuinfo)"
    host_memory_mb="$(awk '/MemTotal:/ { print int($2 / 1024); exit }' /proc/meminfo)"
    [[ "$host_cpus" =~ ^[1-9][0-9]*$ ]] || host_cpus=1
    [[ "$host_memory_mb" =~ ^[1-9][0-9]*$ ]] || host_memory_mb=1
    cpu_limit=$((host_cpus * 80 / 100 / min_cpu_per_pair))
    memory_limit=$((host_memory_mb * 80 / 100 / min_memory_mb_per_pair))
    (( cpu_limit >= 1 )) || cpu_limit=1
    (( memory_limit >= 1 )) || memory_limit=1
    capacity_limit="$max_replicas"
    (( capacity_limit <= cpu_limit )) || capacity_limit="$cpu_limit"
    (( capacity_limit <= memory_limit )) || capacity_limit="$memory_limit"
    (( min_replicas <= capacity_limit )) || min_replicas="$capacity_limit"

    desired_replicas="$current_replicas"
    (( desired_replicas > 0 )) || desired_replicas="$min_replicas"
    DEPLOY_EXPECTED_APP_REPLICAS="$desired_replicas"

    # Upgrade policy is intentionally fail-fast: the freshly built image is
    # ready, so do not let long-lived streams delay replacement for minutes.
    app_ids="$(docker ps -aq \
        --filter "label=com.docker.compose.project=${COMPOSE_PROJECT_NAME}" \
        --filter 'label=com.docker.compose.service=app')"
    if [[ -n "$app_ids" ]]; then
        # Intentional splitting is safe because Docker IDs are hexadecimal tokens.
        docker rm -f $app_ids >/dev/null || die "无法删除旧 app 副本"
    fi

    $dc_cmd -p "$COMPOSE_PROJECT_NAME" -f docker-compose.yml up -d --remove-orphans --scale "app=${desired_replicas}"
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
    echo "容器拓扑:   app 成对副本 + HAProxy + autoscaler + PostgreSQL + 主 Redis + OpenAI/Anthropic Admission Cells"
    echo "端口映射:   ${host_port} -> 8080"
    echo "=================================================="
    echo ""
}

wait_app_ready() {
    local app_log_container app_healthy haproxy_health expected_replicas
    expected_replicas="${DEPLOY_EXPECTED_APP_REPLICAS:-1}"
    if ! [[ "$expected_replicas" =~ ^[1-9][0-9]*$ ]]; then
        expected_replicas="$(docker ps \
            --filter "label=com.docker.compose.project=${COMPOSE_PROJECT_NAME}" \
            --filter 'label=com.docker.compose.service=app' \
            --filter status=running \
            --format '{{.ID}}' | awk 'NF { count++ } END { print count + 0 }')"
        (( expected_replicas > 0 )) || expected_replicas=1
    fi
    info "正在等待 ${APP_NAME} 启动 ..."
    for _ in $(seq 1 60); do
        app_healthy="$(docker ps \
            --filter "label=com.docker.compose.project=${COMPOSE_PROJECT_NAME}" \
            --filter 'label=com.docker.compose.service=app' \
            --filter status=running \
            --format '{{.Status}}' | awk '/\(healthy\)/ { count++ } END { print count + 0 }')"
        haproxy_health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}unknown{{end}}' nuro-sub2api-haproxy 2>/dev/null || true)"
        if (( app_healthy >= expected_replicas )) && [[ "$haproxy_health" == "healthy" ]]; then
            info "${APP_NAME} HAProxy 和成对副本已运行。"
            return 0
        fi
        sleep 2
    done

    warn "${APP_NAME} 可能未正常启动，最近日志如下："
    docker logs --tail=120 nuro-sub2api-haproxy 2>/dev/null || true
    app_log_container="$(docker ps --filter "label=com.docker.compose.project=${COMPOSE_PROJECT_NAME}" --filter 'label=com.docker.compose.service=app' --format '{{.ID}}' | head -n 1)"
    [[ -z "$app_log_container" ]] || docker logs --tail=120 "$app_log_container" 2>/dev/null || true
    return 1
}

wait_postgres_ready() {
    local pg_container="$1"
    local pg_user="$2"
    local pg_db="$3"

    info "正在等待 PostgreSQL 就绪 ..."
    for _ in $(seq 1 60); do
        if docker exec "$pg_container" pg_isready -U "$pg_user" -d "$pg_db" >/dev/null 2>&1; then
            info "PostgreSQL 已就绪。"
            return 0
        fi
        sleep 2
    done

    err "PostgreSQL 等待超时。"
    return 1
}

container_running() {
    local container_name="$1"
    [[ -n "$container_name" ]] || return 1
    [[ "$(docker inspect -f '{{.State.Running}}' "$container_name" 2>/dev/null || true)" == "true" ]]
}

cleanup_old_backups() {
    local backup_dir="$1"
    find "$backup_dir" -maxdepth 1 -name 'nuro_sub2api_backup_*.tar.gz' -type f \
        | sort -r \
        | awk 'NR>5' \
        | xargs -r rm -f
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
    ensure_scheduler_env_values "${install_path}/.env"
    sync_project_source "$install_path" || die "源码同步失败。请检查服务器是否能访问 ${SOURCE_REPO_URL}，或在项目根目录执行本脚本。"
    create_compose_file "$install_path"

    info "正在使用项目源码构建本地镜像并启动 ${APP_NAME} ..."
    compose_build_with_edge_fallback "$install_path" "$dc_cmd" || die "镜像构建失败"
    compose_up_with_edge_fallback "$install_path" "$dc_cmd" || die "容器启动失败"

    wait_app_ready || die "容器启动失败"
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
    reenable_auto_disabled_edge_for_upgrade "${workdir}/.env"
    ensure_edge_env_values "${workdir}/.env"
    ensure_scheduler_env_values "${workdir}/.env"
    create_compose_file "$workdir"

    info "正在使用项目源码重建 ${APP_NAME} ..."
    compose_build_with_edge_fallback "$workdir" "$dc_cmd" || die "镜像构建失败"
    compose_up_with_edge_fallback "$workdir" "$dc_cmd" || die "容器启动失败"

    wait_app_ready || die "容器启动失败"
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
    DEPLOY_EXPECTED_APP_REPLICAS=0
    $(docker_compose_cmd) -p "$COMPOSE_PROJECT_NAME" -f docker-compose.yml restart
    wait_app_ready || die "容器启动失败"
    show_access "$workdir"
}

do_backup() {
    local workdir
    workdir="$(get_workdir)"
    [[ -z "$workdir" ]] && { err "未检测到部署环境。"; return; }

    local backup_dir="${workdir}/backups"
    mkdir -p "$backup_dir"

    local timestamp backup_file stage_dir hot_dir
    timestamp="$(date +"%Y%m%d_%H%M%S")"
    backup_file="${backup_dir}/nuro_sub2api_backup_${timestamp}.tar.gz"
    stage_dir="$(mktemp -d "${backup_dir}/tmp_${timestamp}_XXXXXX")" || return
    hot_dir="${stage_dir}/hot_backup"
    mkdir -p "$hot_dir"

    cp "${workdir}/docker-compose.yml" "${stage_dir}/" 2>/dev/null || true
    cp "${workdir}/.env" "${stage_dir}/" 2>/dev/null || true
    cp "${workdir}/deploy.sh" "${stage_dir}/" 2>/dev/null || true
    [[ -d "${workdir}/${SOURCE_DIR_NAME}" ]] && cp -a "${workdir}/${SOURCE_DIR_NAME}" "${stage_dir}/${SOURCE_DIR_NAME}" 2>/dev/null || true

    local env_file pg_container redis_container pg_user pg_db pg_password redis_password
    env_file="${workdir}/.env"
    pg_container="${POSTGRES_CONTAINER}"
    redis_container="${REDIS_CONTAINER}"
    pg_user="$(read_env_value "$env_file" POSTGRES_USER)"
    pg_user="${pg_user:-nuro_sub2api}"
    pg_db="$(read_env_value "$env_file" POSTGRES_DB)"
    pg_db="${pg_db:-nuro_sub2api}"
    pg_password="$(read_env_value "$env_file" POSTGRES_PASSWORD)"
    redis_password="$(read_env_value "$env_file" REDIS_PASSWORD)"

    if ! container_running "$pg_container"; then
        rm -rf "$stage_dir"
        err "PostgreSQL 容器未运行，无法热备份: ${pg_container}"
        return
    fi

    info "正在热备份 PostgreSQL 数据库，不停止服务 ..."
    if ! docker exec -e PGPASSWORD="$pg_password" "$pg_container" \
        pg_dump -U "$pg_user" -d "$pg_db" --no-owner --no-privileges \
        | gzip -c > "${hot_dir}/postgres_dump.sql.gz"; then
        rm -rf "$stage_dir"
        err "PostgreSQL pg_dump 失败。"
        return
    fi

    if [[ -d "${workdir}/data" ]]; then
        info "正在打包业务 data 目录，自动排除实时日志 ..."
        tar --warning=no-file-changed --ignore-failed-read \
            -czf "${hot_dir}/data.tar.gz" \
            -C "$workdir" \
            --exclude='data/logs' \
            --exclude='data/*.log' \
            --exclude='data/**/*.log' \
            data || warn "data 目录存在运行中变化，已尽量打包；核心数据库备份不受影响。"
    fi

    if container_running "$redis_container"; then
        info "正在热备份 Redis 快照（缓存状态，失败不影响核心数据库备份） ..."
        if docker exec -e REDISCLI_AUTH="$redis_password" "$redis_container" redis-cli PING >/dev/null 2>&1; then
            docker exec -e REDISCLI_AUTH="$redis_password" "$redis_container" redis-cli BGSAVE >/dev/null 2>&1 || true
            for _ in $(seq 1 60); do
                local rdb_in_progress
                rdb_in_progress="$(docker exec -e REDISCLI_AUTH="$redis_password" "$redis_container" sh -c "redis-cli INFO persistence 2>/dev/null | tr -d '\r' | awk -F: '/^rdb_bgsave_in_progress:/ {print \$2}'" || true)"
                [[ "$rdb_in_progress" == "0" ]] && break
                sleep 1
            done
            if ! docker cp "${redis_container}:/data/dump.rdb" "${hot_dir}/redis_dump.rdb" >/dev/null 2>&1; then
                warn "Redis RDB 复制失败，跳过 Redis 缓存快照。"
            fi
        else
            warn "Redis PING 失败，跳过 Redis 缓存快照。"
        fi
    else
        warn "Redis 容器未运行，跳过 Redis 缓存快照: ${redis_container}"
    fi

    {
        echo "BACKUP_TYPE=hot"
        echo "APP_NAME=${APP_NAME}"
        echo "BACKUP_TIME=$(date -Iseconds)"
        echo "POSTGRES_CONTAINER=${pg_container}"
        echo "POSTGRES_DB=${pg_db}"
        echo "REDIS_CONTAINER=${redis_container}"
    } > "${hot_dir}/backup_manifest.txt"

    info "正在生成热备份压缩包 ..."
    if ! tar -czf "$backup_file" -C "$stage_dir" .; then
        rm -rf "$stage_dir"
        rm -f "$backup_file"
        err "生成备份包失败。"
        return
    fi

    rm -rf "$stage_dir"
    cleanup_old_backups "$backup_dir"

    info "热备份完成: ${backup_file}"
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
    local tmp_extract
    tmp_extract="$(mktemp -d)" || { rm -f "$safe_backup"; return; }

    if [[ -d "$target" ]]; then
        read -r -p "目标路径已存在，是否覆盖？(y/N): " confirm
        [[ ! "$confirm" =~ ^[Yy]$ ]] && { rm -rf "$tmp_extract"; rm -f "$safe_backup"; return; }
        cd "$target" 2>/dev/null && $(docker_compose_cmd) -p "$COMPOSE_PROJECT_NAME" -f docker-compose.yml down 2>/dev/null || true
        docker rm -f "$APP_CONTAINER" "$EDGE_CONTAINER" "$POSTGRES_CONTAINER" "$REDIS_CONTAINER" 2>/dev/null || true
		remove_dynamic_admission_cells
        safe_remove_dir "$target" || { rm -rf "$tmp_extract"; rm -f "$safe_backup"; return; }
    fi

    mkdir -p "$target"
    tar -xzf "$safe_backup" -C "$tmp_extract" || { rm -rf "$tmp_extract"; rm -f "$safe_backup"; die "备份解压失败"; }

    local is_hot_backup=0
    [[ -f "${tmp_extract}/hot_backup/postgres_dump.sql.gz" ]] && is_hot_backup=1

    if [[ "$is_hot_backup" -eq 1 ]]; then
        info "检测到热备份包，按 pg_dump 方式恢复。"
        cp "${tmp_extract}/docker-compose.yml" "${target}/docker-compose.yml" 2>/dev/null || create_compose_file "$target"
        cp "${tmp_extract}/.env" "${target}/.env" 2>/dev/null || true
        cp "${tmp_extract}/deploy.sh" "${target}/deploy.sh" 2>/dev/null || true
        [[ -d "${tmp_extract}/${SOURCE_DIR_NAME}" ]] && cp -a "${tmp_extract}/${SOURCE_DIR_NAME}" "${target}/${SOURCE_DIR_NAME}" 2>/dev/null || true
        mkdir -p "${target}/backups"
        cp "${tmp_extract}/hot_backup/postgres_dump.sql.gz" "${target}/backups/postgres_dump.sql.gz"
        [[ -f "${tmp_extract}/hot_backup/backup_manifest.txt" ]] && cp "${tmp_extract}/hot_backup/backup_manifest.txt" "${target}/backups/backup_manifest.txt" 2>/dev/null || true
        if [[ -f "${tmp_extract}/hot_backup/data.tar.gz" ]]; then
            tar -xzf "${tmp_extract}/hot_backup/data.tar.gz" -C "$target" || warn "业务 data 恢复不完整，请检查。"
        fi
        if [[ -f "${tmp_extract}/hot_backup/redis_dump.rdb" ]]; then
            mkdir -p "${target}/redis_data"
            cp "${tmp_extract}/hot_backup/redis_dump.rdb" "${target}/redis_data/dump.rdb" || warn "Redis RDB 恢复文件复制失败。"
        fi
    else
        info "检测到旧版目录备份包，按原目录结构恢复。"
        cp -a "${tmp_extract}/." "$target/" || { rm -rf "$tmp_extract"; rm -f "$safe_backup"; die "恢复文件复制失败"; }
    fi

    mkdir -p "${target}/backups"
    cp "$safe_backup" "${target}/backups/$(basename "$safe_backup")" 2>/dev/null || true
    rm -f "$safe_backup"
    rm -rf "$tmp_extract"
    echo "$target" > "$ENV_RECORD_FILE"

    cd "$target" || return
    [[ -f docker-compose.yml ]] || create_compose_file "$target"
    reenable_auto_disabled_edge_for_upgrade "${target}/.env"
    ensure_edge_env_values "${target}/.env"
    ensure_scheduler_env_values "${target}/.env"
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
    if [[ ! -d "${target}/${SOURCE_DIR_NAME}" ]]; then
        sync_project_source "$target" || die "源码同步失败，无法构建恢复后的镜像"
    fi
    compose_build_with_edge_fallback "$target" "$restore_dc_cmd" || die "镜像构建失败"
    if [[ "$is_hot_backup" -eq 1 ]]; then
        info "正在启动 PostgreSQL / Redis，用于导入热备份数据 ..."
        $restore_dc_cmd -p "$COMPOSE_PROJECT_NAME" -f docker-compose.yml up -d postgres redis || die "恢复基础容器启动失败"

        local pg_user pg_db pg_password
        pg_user="$(read_env_value "${target}/.env" POSTGRES_USER)"
        pg_user="${pg_user:-nuro_sub2api}"
        pg_db="$(read_env_value "${target}/.env" POSTGRES_DB)"
        pg_db="${pg_db:-nuro_sub2api}"
        pg_password="$(read_env_value "${target}/.env" POSTGRES_PASSWORD)"
        wait_postgres_ready "$POSTGRES_CONTAINER" "$pg_user" "$pg_db" || die "PostgreSQL 未就绪"

        info "正在清空并导入 PostgreSQL 热备份 SQL ..."
        local pg_user_ident
        pg_user_ident="$(pg_ident "$pg_user")"
        docker exec -e PGPASSWORD="$pg_password" "$POSTGRES_CONTAINER" \
            psql -U "$pg_user" -d "$pg_db" -v ON_ERROR_STOP=1 \
            -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public; GRANT ALL ON SCHEMA public TO ${pg_user_ident}; GRANT ALL ON SCHEMA public TO public;" >/dev/null || die "清空 PostgreSQL schema 失败"
        gzip -dc "${target}/backups/postgres_dump.sql.gz" | docker exec -i -e PGPASSWORD="$pg_password" "$POSTGRES_CONTAINER" \
            psql -U "$pg_user" -d "$pg_db" -v ON_ERROR_STOP=1 >/dev/null || die "导入 PostgreSQL 热备份 SQL 失败"

        compose_up_with_edge_fallback "$target" "$restore_dc_cmd" || die "恢复后的容器启动失败"
    else
        compose_up_with_edge_fallback "$target" "$restore_dc_cmd" || die "容器启动失败"
    fi

    wait_app_ready || die "容器启动失败"
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
	remove_dynamic_admission_cells
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
