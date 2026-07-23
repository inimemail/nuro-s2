#!/bin/sh
set -eu

project="${COMPOSE_PROJECT_NAME:-nuro-sub2api}"
service="${AUTOSCALE_SERVICE:-app}"
interval="${AUTOSCALE_INTERVAL_SECONDS:-15}"
min_replicas="${AUTOSCALE_MIN_REPLICAS:-2}"
max_replicas="${AUTOSCALE_MAX_REPLICAS:-32}"
min_cpu_per_pair="${AUTOSCALE_MIN_CPU_PER_PAIR:-4}"
min_memory_mb_per_pair="${AUTOSCALE_MIN_MEMORY_MB_PER_PAIR:-2048}"
target_streams="${AUTOSCALE_TARGET_STREAMS_PER_PAIR:-20000}"
target_rps="${AUTOSCALE_TARGET_RPS_PER_PAIR:-3000}"
target_go_active="${AUTOSCALE_TARGET_GO_ACTIVE_PER_PAIR:-4000}"
target_queue="${AUTOSCALE_TARGET_QUEUE_DEPTH:-128}"
target_workers="${AUTOSCALE_TARGET_RELAY_WORKERS:-512}"
go_5xx_ratio_limit="${AUTOSCALE_GO_5XX_RATIO_LIMIT:-${AUTOSCALE_UPSTREAM_ERROR_RATIO:-0.20}}"
edge_connect_error_ratio_limit="${AUTOSCALE_EDGE_CONNECT_ERROR_RATIO_LIMIT:-0.10}"
edge_5xx_ratio_limit="${AUTOSCALE_EDGE_5XX_RATIO_LIMIT:-0.20}"
edge_429_ratio_limit="${AUTOSCALE_EDGE_429_RATIO_LIMIT:-0}"
edge_retries_per_request_limit="${AUTOSCALE_EDGE_RETRIES_PER_REQUEST_LIMIT:-0}"
error_min_samples="${AUTOSCALE_ERROR_MIN_SAMPLES:-20}"
lane_pressure_enabled="${AUTOSCALE_UPSTREAM_LANE_PRESSURE_ENABLED:-true}"
scale_down_seconds="${AUTOSCALE_SCALE_DOWN_SECONDS:-600}"
drain_seconds="${AUTOSCALE_DRAIN_SECONDS:-30}"
force_stop_seconds="${AUTOSCALE_FORCE_STOP_SECONDS:-30}"
state_dir="${AUTOSCALE_STATE_DIR:-/state}"
state_file="${state_dir}/state"
state_schema="v5"
config_invalid=0
config_invalid_fields=""
min_replicas_invalid=0
max_safe_config_integer=1000000000

valid_nonnegative_decimal() {
    awk -v value="$1" 'BEGIN {
        if (value !~ /^[+]?[0-9]+([.][0-9]+)?$/ || value + 0 < 0) exit 1
    }'
}

valid_ratio() {
    valid_nonnegative_decimal "$1" && awk -v value="$1" 'BEGIN { exit !(value + 0 <= 1) }'
}

valid_positive_integer() {
    awk -v value="$1" -v maximum="$max_safe_config_integer" 'BEGIN {
        if (value !~ /^[1-9][0-9]*$/ || value + 0 > maximum) exit 1
    }'
}

valid_nonnegative_integer() {
    awk -v value="$1" -v maximum="$max_safe_config_integer" 'BEGIN {
        if (value !~ /^(0|[1-9][0-9]*)$/ || value + 0 > maximum) exit 1
    }'
}

safe_nonnegative_integer() {
    case "${1:-}" in
        ''|*[!0-9]*) printf '0\n' ;;
        *)
            safe_integer_value="$(printf '%s\n' "$1" | sed 's/^0*//')"
            [ -n "$safe_integer_value" ] || safe_integer_value=0
            printf '%s\n' "$safe_integer_value"
        ;;
    esac
}

mark_invalid_config() {
    config_invalid=1
    if [ -n "$config_invalid_fields" ]; then
        config_invalid_fields="${config_invalid_fields},$1"
    else
        config_invalid_fields="$1"
    fi
}

if ! valid_ratio "$go_5xx_ratio_limit"; then
    mark_invalid_config AUTOSCALE_GO_5XX_RATIO_LIMIT
    go_5xx_ratio_limit=0
fi
if ! valid_ratio "$edge_connect_error_ratio_limit"; then
    mark_invalid_config AUTOSCALE_EDGE_CONNECT_ERROR_RATIO_LIMIT
    edge_connect_error_ratio_limit=0
fi
if ! valid_ratio "$edge_5xx_ratio_limit"; then
    mark_invalid_config AUTOSCALE_EDGE_5XX_RATIO_LIMIT
    edge_5xx_ratio_limit=0
fi
if ! valid_ratio "$edge_429_ratio_limit"; then
    mark_invalid_config AUTOSCALE_EDGE_429_RATIO_LIMIT
    edge_429_ratio_limit=0
fi
if ! valid_nonnegative_decimal "$edge_retries_per_request_limit"; then
    mark_invalid_config AUTOSCALE_EDGE_RETRIES_PER_REQUEST_LIMIT
    edge_retries_per_request_limit=0
fi
if ! valid_positive_integer "$error_min_samples"; then
    mark_invalid_config AUTOSCALE_ERROR_MIN_SAMPLES
    error_min_samples=1
fi
lane_pressure_enabled="$(printf '%s' "$lane_pressure_enabled" | tr '[:upper:]' '[:lower:]')"
case "$lane_pressure_enabled" in
    true|1|yes|on) lane_pressure_enabled=true ;;
    false|0|no|off) lane_pressure_enabled=false ;;
    *)
        mark_invalid_config AUTOSCALE_UPSTREAM_LANE_PRESSURE_ENABLED
        lane_pressure_enabled=false
        ;;
esac

if ! valid_nonnegative_integer "$min_replicas"; then
    mark_invalid_config AUTOSCALE_MIN_REPLICAS
    min_replicas_invalid=1
    min_replicas=2
fi
if ! valid_positive_integer "$max_replicas"; then
    mark_invalid_config AUTOSCALE_MAX_REPLICAS
    max_replicas=32
fi
if [ "$min_replicas" -gt "$max_replicas" ]; then
    # A reversed floor/ceiling would otherwise make clamp_replicas return a
    # value below the configured floor (and could make the zero-replica
    # recovery path oscillate). Treat the pair as invalid and fail closed.
    mark_invalid_config AUTOSCALE_MIN_REPLICAS_MAX_REPLICAS
    min_replicas_invalid=1
    min_replicas=2
    max_replicas=32
fi
if ! valid_positive_integer "$min_cpu_per_pair"; then
    mark_invalid_config AUTOSCALE_MIN_CPU_PER_PAIR
    min_cpu_per_pair=4
fi
if ! valid_positive_integer "$min_memory_mb_per_pair"; then
    mark_invalid_config AUTOSCALE_MIN_MEMORY_MB_PER_PAIR
    min_memory_mb_per_pair=2048
fi
if ! valid_positive_integer "$interval"; then
    mark_invalid_config AUTOSCALE_INTERVAL_SECONDS
    interval=15
fi
if ! valid_positive_integer "$target_streams"; then
    mark_invalid_config AUTOSCALE_TARGET_STREAMS_PER_PAIR
    target_streams=20000
fi
if ! valid_positive_integer "$target_rps"; then
    mark_invalid_config AUTOSCALE_TARGET_RPS_PER_PAIR
    target_rps=3000
fi
if ! valid_positive_integer "$target_go_active"; then
    mark_invalid_config AUTOSCALE_TARGET_GO_ACTIVE_PER_PAIR
    target_go_active=4000
fi
if ! valid_positive_integer "$target_queue"; then
    mark_invalid_config AUTOSCALE_TARGET_QUEUE_DEPTH
    target_queue=128
fi
if ! valid_positive_integer "$target_workers"; then
    mark_invalid_config AUTOSCALE_TARGET_RELAY_WORKERS
    target_workers=512
fi
if ! valid_nonnegative_integer "$scale_down_seconds"; then
    mark_invalid_config AUTOSCALE_SCALE_DOWN_SECONDS
    scale_down_seconds=600
fi
if ! valid_nonnegative_integer "$drain_seconds"; then
    mark_invalid_config AUTOSCALE_DRAIN_SECONDS
    drain_seconds=30
fi
if ! valid_nonnegative_integer "$force_stop_seconds"; then
    mark_invalid_config AUTOSCALE_FORCE_STOP_SECONDS
    force_stop_seconds=30
fi
configured_max_replicas="$max_replicas"

autoscaler_start_time="$(date +%s)"
max_state_age=$((interval * 4))
[ "$max_state_age" -ge 300 ] || max_state_age=300

mkdir -p "$state_dir"

host_cpus="$(getconf _NPROCESSORS_ONLN 2>/dev/null || awk '/^processor/ { count++ } END { print count + 0 }' /proc/cpuinfo || true)"
host_memory_mb="$(awk '/MemTotal:/ { print int($2 / 1024) }' /proc/meminfo 2>/dev/null || true)"
# Some minimal container runtimes expose neither a usable getconf value nor
# MemTotal. Keep the controller alive with the conservative one-unit limit
# instead of letting shell arithmetic terminate it under `set -e`.
case "$host_cpus" in
    ''|*[!0-9]*) host_cpus=1 ;;
esac
case "$host_memory_mb" in
    ''|*[!0-9]*) host_memory_mb=1 ;;
esac
[ "$host_cpus" -ge 1 ] || host_cpus=1
[ "$host_memory_mb" -ge 1 ] || host_memory_mb=1
cpu_limit=$((host_cpus * 80 / 100 / min_cpu_per_pair))
memory_limit=$((host_memory_mb * 80 / 100 / min_memory_mb_per_pair))
[ "$cpu_limit" -ge 1 ] || cpu_limit=1
[ "$memory_limit" -ge 1 ] || memory_limit=1
[ "$max_replicas" -le "$cpu_limit" ] || max_replicas="$cpu_limit"
[ "$max_replicas" -le "$memory_limit" ] || max_replicas="$memory_limit"
[ "$max_replicas" -ge "$min_replicas" ] || min_replicas="$max_replicas"
echo "autoscaler startup host_cpus=${host_cpus} host_memory_mb=${host_memory_mb} configured_max=${configured_max_replicas} effective_min=${min_replicas} effective_max=${max_replicas} cpu_limit=${cpu_limit} memory_limit=${memory_limit} interval_seconds=${interval} target_streams=${target_streams} target_rps=${target_rps} target_go_active=${target_go_active} target_queue=${target_queue} target_workers=${target_workers} lane_pressure_enabled=${lane_pressure_enabled}" >&2
echo "autoscaler guards go_5xx=${go_5xx_ratio_limit} edge_connect=${edge_connect_error_ratio_limit} edge_5xx=${edge_5xx_ratio_limit} edge_429=${edge_429_ratio_limit} retries_per_request=${edge_retries_per_request_limit} min_samples=${error_min_samples}" >&2

ceil_div() {
    awk -v value="$1" -v divisor="$2" 'BEGIN { if (divisor <= 0) print 1; else print int((value + divisor - 1) / divisor) }'
}

metric() {
    metric_name="$1"
    awk -v name="$metric_name" '
        function is_number(value) {
            return value ~ /^[+-]?([0-9]+([.][0-9]*)?|[.][0-9]+)([eE][+-]?[0-9]+)?$/
        }
        $1 == name {
            if (seen || NF != 2 || !is_number($2)) {
                invalid = 1
                next
            }
            value = $2
            seen = 1
        }
        END {
            if (invalid || !seen) exit 1
            print value
        }
    '
}

integer_metric() {
    integer_metric_name="$1"
    integer_metric_value="$(metric "$integer_metric_name")" || return 1
    case "$integer_metric_value" in
        ''|*[!0-9]*) return 1 ;;
    esac
    integer_metric_value="$(printf '%s\n' "$integer_metric_value" | sed 's/^0*//')"
    [ -n "$integer_metric_value" ] || integer_metric_value=0
    printf '%s\n' "$integer_metric_value"
}

nonnegative_metric() {
    nonnegative_metric_name="$1"
    nonnegative_metric_value="$(metric "$nonnegative_metric_name")" || return 1
    awk -v value="$nonnegative_metric_value" 'BEGIN { if (value + 0 < 0) exit 1; print value }'
}

parse_replica_metrics() {
    replica_edge_metrics="$1"
    replica_go_metrics="$2"

    edge_active="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_active_requests)" || return 1
    streams="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_active_streams)" || return 1
    accepted="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_accepted_requests)" || return 1
    relay_requests="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_relay_requests)" || return 1
    queue="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_relay_queue_depth)" || return 1
    queue_bytes="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_relay_queue_bytes)" || return 1
    workers="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_relay_workers_active)" || return 1
    open_fds="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_open_fds)" || return 1
    max_fds="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_max_fds)" || return 1
    # edge-rs reports zero when /proc/self/limits cannot be read. A running
    # process cannot have a real FD ceiling of zero, so treating it as a 0%
    # ratio would be fail-open precisely when capacity is unknown.
    [ "$max_fds" -gt 0 ] || return 1
    settlements="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_settlement_retry_queue_depth)" || return 1
    settlement_workers="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_settlement_workers_active)" || return 1
    payload_commits="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_payload_commit_queue_depth)" || return 1
    payload_commit_workers="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_payload_commit_workers_active)" || return 1
    edge_upstream_attempts="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_upstream_attempts_total)" || return 1
    edge_connect_errors="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_upstream_connect_errors_total)" || return 1
    edge_upstream_responses="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_upstream_responses_total)" || return 1
    edge_upstream_5xx="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_upstream_5xx_total)" || return 1
    edge_upstream_429="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_upstream_429_total)" || return 1
    edge_retry_attempts="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_retry_attempts_total)" || return 1
    lane_overflow_total="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_upstream_lane_overflow_total)" || return 1
    lane_pools_under_pressure="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_upstream_lane_pools_under_pressure)" || return 1
    lane_pools_at_cap_pressure="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_upstream_lane_pools_at_cap_pressure)" || return 1
    lane_pools_overflowing="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_upstream_lane_pools_overflowing)" || return 1
    lane_overflow_active="$(printf '%s\n' "$replica_edge_metrics" | integer_metric sub2api_edge_upstream_lane_overflow_active)" || return 1

    go_active="$(printf '%s\n' "$replica_go_metrics" | integer_metric sub2api_go_active_requests)" || return 1
    go_requests="$(printf '%s\n' "$replica_go_metrics" | integer_metric sub2api_go_requests_total)" || return 1
    go_errors="$(printf '%s\n' "$replica_go_metrics" | integer_metric sub2api_go_server_errors_total)" || return 1
    go_uptime="$(printf '%s\n' "$replica_go_metrics" | nonnegative_metric sub2api_go_process_uptime_seconds)" || return 1
    gc_fraction="$(printf '%s\n' "$replica_go_metrics" | nonnegative_metric sub2api_go_gc_cpu_fraction)" || return 1
}

topology_fingerprint() {
    topology_containers="$1"
    topology_lines=""
    for topology_container in $topology_containers; do
        topology_identity="$(docker inspect --format '{{.Id}} {{.RestartCount}}' "$topology_container" 2>/dev/null)" || return 1
        [ -n "$topology_identity" ] || return 1
        topology_lines="${topology_lines}${topology_identity}
"
    done
    printf '%s' "$topology_lines" | sort | awk '
        NF != 2 || $2 !~ /^[0-9]+$/ { invalid = 1; next }
        {
            printf "%s%s:%s", separator, $1, $2
            separator = ","
            count++
        }
        END {
            if (invalid || count == 0) exit 1
            print ""
        }
    '
}

state_integer() {
    case "$1" in
        # Keep values in canonical decimal form.  dash and several other
        # POSIX shells interpret arithmetic literals with a leading zero as
        # octal, so accepting e.g. `08` here would make a later $((...)) fail.
        ''|*[!0-9]*) return 1 ;;
        0|[1-9]|[1-9][0-9]*) return 0 ;;
        *) return 1 ;;
    esac
}

write_state() {
    state_tmp="${state_file}.tmp.$$"
    printf '%s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s\n' \
        "$state_schema" "$state_topology" "$prev_ts" "$prev_accepted" "$ewma_rps" \
        "$up_samples" "$low_since" "$prev_go_requests" "$prev_go_errors" \
        "$prev_edge_upstream_attempts" "$prev_edge_connect_errors" "$prev_edge_upstream_responses" \
        "$prev_edge_upstream_5xx" "$prev_edge_upstream_429" "$prev_edge_retry_attempts" \
        "$prev_relay_requests" "$prev_lane_overflow_total" > "$state_tmp"
    mv "$state_tmp" "$state_file"
}

reset_baselines() {
    state_topology="$1"
    prev_ts="$2"
    prev_accepted="$3"
    prev_go_requests="$4"
    prev_go_errors="$5"
    prev_edge_upstream_attempts="$6"
    prev_edge_connect_errors="$7"
    prev_edge_upstream_responses="$8"
    prev_edge_upstream_5xx="$9"
    prev_edge_upstream_429="${10}"
    prev_edge_retry_attempts="${11}"
    prev_relay_requests="${12}"
    prev_lane_overflow_total="${13}"
    ewma_rps=0
    up_samples=0
    low_since=0
    state_needs_reset=0
    write_state
}

evaluate_ratio_guard() {
    guard_reason="$1"
    guard_numerator="$2"
    guard_denominator="$3"
    guard_limit="$4"
    [ "$guard_denominator" -ge "$error_min_samples" ] || return 0

    guard_value="$(awk -v numerator="$guard_numerator" -v denominator="$guard_denominator" \
        'BEGIN { printf "%.6f", (denominator > 0 ? numerator / denominator : 0) }')"
    guard_exceeded="$(awk -v value="$guard_value" -v limit="$guard_limit" \
        'BEGIN { print (limit > 0 && value >= limit ? 1 : 0) }')"
    if [ "$guard_exceeded" -eq 1 ]; then
        echo "autoscaler hold reason=${guard_reason} value=${guard_value} limit=${guard_limit} samples=${guard_denominator}" >&2
        return 1
    fi
    return 0
}

clamp_replicas() {
    value="$1"
    [ "$value" -ge "$min_replicas" ] || value="$min_replicas"
    [ "$value" -le "$max_replicas" ] || value="$max_replicas"
    printf '%s\n' "$value"
}

scale_to() {
    requested="$1"
    current="$2"
    desired="$(clamp_replicas "$requested")"
    if [ "$requested" -lt "$current" ] && [ "$desired" -lt "$requested" ]; then
        # A host-derived max below the already-running replica count is not a
        # license to remove multiple pairs after drain_one prepared only one.
        # Converge toward the ceiling one explicitly drained replica at a time.
        desired="$requested"
    fi
    [ "$desired" -ne "$current" ] || return 0
    echo "autoscaler decision current=${current} desired=${desired} reason=$3" >&2
    docker compose -p "$project" -f /workspace/docker-compose.yml --env-file /workspace/.env \
        up -d --no-deps --no-recreate --scale "${service}=${desired}" "$service"
}

restore_drained_pair() {
    restore_container="$1"
    [ -n "$restore_container" ] || return 1
    echo "autoscaler restoring drained container=${restore_container}" >&2
    if ! docker restart "$restore_container" >/dev/null 2>&1; then
        echo "autoscaler restore_failed container=${restore_container}" >&2
        return 1
    fi
    return 0
}

drain_one() {
    container="$(docker ps \
        --filter "label=com.docker.compose.project=${project}" \
        --filter "label=com.docker.compose.service=${service}" \
        --format '{{.Label "com.docker.compose.container-number"}} {{.ID}}' | sort -nr | awk 'NR == 1 { print $2 }')"
    [ -n "$container" ] || return 1
    if ! docker exec "$container" sh -ec '
        wget -q -O /dev/null --post-data='' \
          --header="X-Sub2API-Edge-Secret: ${SUB2API_EDGE_INTERNAL_SECRET}" \
          http://127.0.0.1:18080/internal/drain
        wget -q -O /dev/null --post-data='' \
          --header="X-Sub2API-Edge-Secret: ${GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET}" \
          http://127.0.0.1:8080/internal/runtime/drain
    '; then
        restore_drained_pair "$container" || true
        return 1
    fi

    started="$(date +%s)"
    while :; do
        edge_metrics="$(docker exec "$container" wget -q -O - http://127.0.0.1:18080/metrics 2>/dev/null || true)"
        go_metrics="$(docker exec "$container" wget -q -O - http://127.0.0.1:8080/metrics 2>/dev/null || true)"
        if [ -z "$edge_metrics" ] || [ -z "$go_metrics" ] || ! parse_replica_metrics "$edge_metrics" "$go_metrics"; then
            echo "autoscaler drain_hold container=${container} reason=invalid_metrics" >&2
            restore_drained_pair "$container" || true
            return 1
        fi
        if [ "$edge_active" -eq 0 ] && [ "$streams" -eq 0 ] && [ "$go_active" -le 1 ] && \
            [ "$settlements" -eq 0 ] && \
            [ "$settlement_workers" -eq 0 ] && [ "$payload_commits" -eq 0 ] && [ "$payload_commit_workers" -eq 0 ]; then
            return 0
        fi
        now="$(date +%s)"
        if [ $((now - started)) -ge "$drain_seconds" ]; then
            echo "autoscaler force_stop container=${container} reason=drain_deadline" >&2
            docker stop --time "$force_stop_seconds" "$container" >/dev/null 2>&1 || true
            return 0
        fi
        sleep 2
    done
}

prev_ts=0
prev_accepted=0
prev_relay_requests=0
prev_go_requests=0
prev_go_errors=0
prev_edge_upstream_attempts=0
prev_edge_connect_errors=0
prev_edge_upstream_responses=0
prev_edge_upstream_5xx=0
prev_edge_upstream_429=0
prev_edge_retry_attempts=0
prev_lane_overflow_total=0
ewma_rps=0
up_samples=0
low_since=0
state_topology=""
state_needs_reset=1
if [ -s "$state_file" ]; then
    state_version=""
    state_extra=""
    state_records="$(awk 'NF { count++ } END { print count + 0 }' "$state_file" 2>/dev/null || printf '0\n')"
    if read -r state_version state_topology prev_ts prev_accepted ewma_rps up_samples low_since \
        prev_go_requests prev_go_errors prev_edge_upstream_attempts prev_edge_connect_errors \
        prev_edge_upstream_responses prev_edge_upstream_5xx prev_edge_upstream_429 \
        prev_edge_retry_attempts prev_relay_requests prev_lane_overflow_total state_extra < "$state_file" && \
        state_integer "$state_records" && [ "$state_records" -eq 1 ] && \
        [ "$state_version" = "$state_schema" ] && [ -z "$state_extra" ] && \
        [ -n "$state_topology" ] && state_integer "$prev_ts" && [ "$prev_ts" -gt 0 ] && \
        [ "$prev_ts" -le "$autoscaler_start_time" ] && \
        [ $((autoscaler_start_time - prev_ts)) -le "$max_state_age" ] && \
        state_integer "$prev_accepted" && state_integer "$ewma_rps" && state_integer "$up_samples" && \
        state_integer "$low_since" && [ "$low_since" -le "$prev_ts" ] && \
        state_integer "$prev_go_requests" && state_integer "$prev_go_errors" && \
        state_integer "$prev_edge_upstream_attempts" && state_integer "$prev_edge_connect_errors" && \
        state_integer "$prev_edge_upstream_responses" && state_integer "$prev_edge_upstream_5xx" && \
        state_integer "$prev_edge_upstream_429" && state_integer "$prev_edge_retry_attempts" && \
        state_integer "$prev_relay_requests" && state_integer "$prev_lane_overflow_total"; then
        state_needs_reset=0
    fi
fi
if [ "$state_needs_reset" -eq 1 ]; then
    prev_ts=0
    prev_accepted=0
    prev_relay_requests=0
    prev_go_requests=0
    prev_go_errors=0
    prev_edge_upstream_attempts=0
    prev_edge_connect_errors=0
    prev_edge_upstream_responses=0
    prev_edge_upstream_5xx=0
    prev_edge_upstream_429=0
    prev_edge_retry_attempts=0
    prev_lane_overflow_total=0
    ewma_rps=0
    up_samples=0
    low_since=0
    state_topology=""
fi

cell_platforms="openai anthropic"
cell_enabled="${ADMISSION_CELL_AUTOSCALE_ENABLED:-true}"
cell_max="${ADMISSION_CELL_MAX_PER_PLATFORM:-8}"
cell_target_ops="${ADMISSION_CELL_TARGET_OPS:-50000}"
cell_target_memory_mb="${ADMISSION_CELL_TARGET_MEMORY_MB:-8192}"
valid_nonnegative_integer "$cell_max" || cell_max=8
valid_nonnegative_integer "$cell_target_ops" || cell_target_ops=50000
valid_nonnegative_integer "$cell_target_memory_mb" || cell_target_memory_mb=8192
cell_state_dir="${state_dir}/cells"
mkdir -p "$cell_state_dir"

cell_count() {
    platform="$1"
    docker ps --filter "name=${project}-admission-${platform}-" --filter status=running --format '{{.Names}}' | wc -l | awk '{print $1}'
}

cell_info() {
    container="$1"
    docker exec "$container" redis-cli INFO stats 2>/dev/null || true
}

cell_memory_bytes() {
    container="$1"
    docker exec "$container" redis-cli INFO memory 2>/dev/null | awk -F: '/^used_memory:/ { print $2; exit }' | tr -d '\r'
}

cell_ops() {
    info="$1"
    printf '%s\n' "$info" | awk -F: '/^instantaneous_ops_per_sec:/ { print $2; exit }' | tr -d '\r'
}

control_redis_container() {
    docker ps \
        --filter "label=com.docker.compose.project=${project}" \
        --filter "label=com.docker.compose.service=redis" \
        --filter status=running --format '{{.ID}}' | awk 'NR == 1 { print; exit }'
}

cell_register() {
    platform="$1"
    cell_id="$2"
    endpoint="$3"
    control="$(control_redis_container)"
    [ -n "$control" ] || return 1
    docker exec -e REDISCLI_AUTH="${REDIS_PASSWORD:-}" "$control" redis-cli HSET admission:account-cell:endpoints "$cell_id" "$endpoint" >/dev/null
    docker exec -e REDISCLI_AUTH="${REDIS_PASSWORD:-}" "$control" redis-cli SADD "admission:account-cell:catalog:${platform}" "$cell_id" >/dev/null
}

create_cell() {
    platform="$1"
    suffix="$2"
    cell_id="${platform}-${suffix}"
    container="${project}-admission-${platform}-${suffix}"
    if docker inspect "$container" >/dev/null 2>&1; then
        docker start "$container" >/dev/null 2>&1 || true
    else
        available_mb="$(awk '/MemAvailable:/ { print int($2 / 1024) }' /proc/meminfo 2>/dev/null || true)"
        available_mb="$(safe_nonnegative_integer "$available_mb")"
        required_mb=$((cell_target_memory_mb * 120 / 100))
        if [ "$available_mb" -lt "$required_mb" ]; then
            echo "autoscaler cell_create_suppressed platform=${platform} available_mb=${available_mb} required_mb=${required_mb}" >&2
            return 1
        fi
        network="${project}_nuro-sub2api-network"
        volume="${project}_admission_${platform}_${suffix}"
        docker volume create "$volume" >/dev/null
        docker run -d --name "$container" --restart unless-stopped \
            --label "com.nuro.sub2api.admission.platform=${platform}" \
            --label "com.nuro.sub2api.admission.cell=${cell_id}" \
            --network "$network" --network-alias "admission-${platform}-${suffix}" \
            --ulimit "nofile=${REDIS_NOFILE_LIMIT:-200000}:${REDIS_NOFILE_LIMIT:-200000}" \
            -e "REDIS_PASSWORD=${REDIS_PASSWORD:-}" \
            -e "REDISCLI_AUTH=${REDIS_PASSWORD:-}" \
            -e "ADMISSION_CELL_IO_THREADS=${ADMISSION_CELL_IO_THREADS:-8}" \
            -e "ADMISSION_CELL_MAXMEMORY_MB=${cell_target_memory_mb}" \
            -e "ADMISSION_CELL_MAX_CLIENTS=${ADMISSION_CELL_MAX_CLIENTS:-100000}" \
            -v "${volume}:/data" redis:8-alpine sh -ec \
            'set -- redis-server \
                --save "300 10" \
                --appendonly yes \
                --appendfsync everysec \
                --maxmemory "${ADMISSION_CELL_MAXMEMORY_MB}mb" \
                --maxmemory-policy noeviction \
                --maxclients "${ADMISSION_CELL_MAX_CLIENTS}" \
                --tcp-backlog 8192 \
                --timeout 0 \
                --hz 20 \
                --io-threads "${ADMISSION_CELL_IO_THREADS}" \
                --io-threads-do-reads yes
             if [ -n "${REDIS_PASSWORD:-}" ]; then
                 set -- "$@" --requirepass "$REDIS_PASSWORD"
             fi
             exec "$@"' >/dev/null
    fi
    for _ in $(seq 1 30); do
        if docker exec "$container" redis-cli ping >/dev/null 2>&1; then
            cell_register "$platform" "$cell_id" "redis://admission-${platform}-${suffix}:6379/0"
            echo "autoscaler cell_created platform=${platform} cell=${cell_id}" >&2
            return 0
        fi
        sleep 1
    done
    echo "autoscaler cell_create_failed platform=${platform} cell=${cell_id}" >&2
    return 1
}

scale_cell() {
    platform="$1"
    max_suffix="$(docker ps -a --filter "name=${project}-admission-${platform}-" --format '{{.Names}}' | awk -F- '{ value=$NF + 0; if (value > max) max=value } END { print max + 0 }')"
    next=$((max_suffix + 1))
    suffix="$(printf '%03d' "$next")"
    create_cell "$platform" "$suffix"
}

reconcile_catalog_cells() {
    control="$(control_redis_container)"
    [ -n "$control" ] || return 0
    for platform in $cell_platforms; do
        cells="$(docker exec -e REDISCLI_AUTH="${REDIS_PASSWORD:-}" "$control" redis-cli --raw SMEMBERS "admission:account-cell:catalog:${platform}" 2>/dev/null || true)"
        for cell_id in $cells; do
            suffix="${cell_id##*-}"
            container="${project}-admission-${platform}-${suffix}"
            running="$(docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null || true)"
            if [ "$running" != "true" ]; then
                create_cell "$platform" "$suffix" || true
            fi
        done
    done
}

maybe_scale_admission_cells() {
    [ "$cell_enabled" = "true" ] || return 0
    for platform in $cell_platforms; do
        count="$(cell_count "$platform")"
        [ "$count" -gt 0 ] || continue
        [ "$count" -lt "$cell_max" ] || continue
        total_ops=0
        max_memory=0
        for container in $(docker ps --filter "name=${project}-admission-${platform}-" --filter status=running --format '{{.Names}}'); do
            info="$(cell_info "$container")"
            ops="$(cell_ops "$info")"
            ops="$(safe_nonnegative_integer "$ops")"
            memory="$(cell_memory_bytes "$container")"
            memory="$(safe_nonnegative_integer "$memory")"
            total_ops=$((total_ops + ops))
            [ "$memory" -le "$max_memory" ] || max_memory="$memory"
        done
        pressure=0
        [ "$total_ops" -gt $((count * cell_target_ops * 70 / 100)) ] && pressure=$((pressure + 1))
        [ "$max_memory" -gt $((cell_target_memory_mb * 1024 * 1024 * 65 / 100)) ] && pressure=$((pressure + 1))
        state="${cell_state_dir}/${platform}"
        samples=0
        [ -s "$state" ] && read -r samples < "$state" || true
        samples="$(safe_nonnegative_integer "$samples")"
        if [ "$pressure" -ge 1 ]; then
            samples=$((samples + 1))
            if [ "$samples" -ge 2 ]; then
                scale_cell "$platform" || true
                samples=0
            fi
        else
            samples=0
        fi
        printf '%s\n' "$samples" > "$state"
    done
}

while :; do
    reconcile_catalog_cells
    if ! containers="$(docker ps \
        --filter "label=com.docker.compose.project=${project}" \
        --filter "label=com.docker.compose.service=${service}" \
        --filter status=running --format '{{.ID}}' 2>/dev/null)"; then
        state_needs_reset=1
        echo "autoscaler hold reason=docker_unavailable" >&2
        sleep "$interval"
        continue
    fi
    current="$(printf '%s\n' "$containers" | awk 'NF { count++ } END { print count + 0 }')"
    # A zero-replica deployment cannot recover by observing metrics. Bring the
    # service back to its configured floor before applying a fail-closed config
    # hold; an explicit min=0 remains a deliberate scale-to-zero setting.
    if [ "$current" -eq 0 ]; then
        state_needs_reset=1
        if [ "$min_replicas_invalid" -eq 1 ]; then
            echo "autoscaler hold current=0 reason=invalid_min_replicas fields=${config_invalid_fields}" >&2
            maybe_scale_admission_cells
            sleep "$interval"
            continue
        fi
        if [ "$min_replicas" -eq 0 ]; then
            if [ "$config_invalid" -eq 1 ]; then
                echo "autoscaler hold current=0 reason=minimum_replicas_zero_invalid_config fields=${config_invalid_fields}" >&2
            else
                echo "autoscaler hold current=0 reason=minimum_replicas_zero" >&2
            fi
        elif ! scale_to "$min_replicas" "$current" "warm_headroom_minimum"; then
            echo "autoscaler recovery_failed current=0 desired=${min_replicas}" >&2
        fi
        maybe_scale_admission_cells
        sleep "$interval"
        continue
    fi
    if [ "$config_invalid" -eq 1 ]; then
        state_needs_reset=1
        echo "autoscaler hold current=${current} reason=invalid_config fields=${config_invalid_fields}" >&2
        sleep "$interval"
        continue
    fi
    if [ "$current" -lt "$min_replicas" ]; then
        state_needs_reset=1
        if ! scale_to "$min_replicas" "$current" "warm_headroom_minimum"; then
            echo "autoscaler recovery_failed current=${current} desired=${min_replicas}" >&2
        fi
        maybe_scale_admission_cells
        sleep "$interval"
        continue
    fi
    if ! topology="$(topology_fingerprint "$containers")"; then
        state_needs_reset=1
        echo "autoscaler hold current=${current} reason=topology_unavailable" >&2
        sleep "$interval"
        continue
    fi

    total_streams=0
    total_go_active=0
    total_accepted=0
    total_relay_requests=0
    total_go_requests=0
    total_go_errors=0
    total_edge_upstream_attempts=0
    total_edge_connect_errors=0
    total_edge_upstream_responses=0
    total_edge_upstream_5xx=0
    total_edge_upstream_429=0
    total_edge_retry_attempts=0
    total_lane_overflow_total=0
    max_queue=0
    max_queue_bytes=0
    max_gc_fraction=0
    max_fd_ratio=0
    max_worker_ratio=0
    max_lane_pressure_candidates=0
    healthy=0
    invalid=0
    for container in $containers; do
        edge="$(docker exec "$container" wget -q -O - http://127.0.0.1:18080/metrics 2>/dev/null || true)"
        go="$(docker exec "$container" wget -q -O - http://127.0.0.1:8080/metrics 2>/dev/null || true)"
        if [ -z "$edge" ] || [ -z "$go" ] || ! parse_replica_metrics "$edge" "$go"; then
            invalid=$((invalid + 1))
            echo "autoscaler metrics_invalid container=${container}" >&2
            continue
        fi
        healthy=$((healthy + 1))
        total_streams=$((total_streams + streams))
        total_go_active=$((total_go_active + go_active))
        total_go_requests=$((total_go_requests + go_requests))
        total_go_errors=$((total_go_errors + go_errors))
        total_accepted=$((total_accepted + accepted))
        total_relay_requests=$((total_relay_requests + relay_requests))
        total_edge_upstream_attempts=$((total_edge_upstream_attempts + edge_upstream_attempts))
        total_edge_connect_errors=$((total_edge_connect_errors + edge_connect_errors))
        total_edge_upstream_responses=$((total_edge_upstream_responses + edge_upstream_responses))
        total_edge_upstream_5xx=$((total_edge_upstream_5xx + edge_upstream_5xx))
        total_edge_upstream_429=$((total_edge_upstream_429 + edge_upstream_429))
        total_edge_retry_attempts=$((total_edge_retry_attempts + edge_retry_attempts))
        total_lane_overflow_total=$((total_lane_overflow_total + lane_overflow_total))
        [ "$queue" -le "$max_queue" ] || max_queue="$queue"
        [ "$queue_bytes" -le "$max_queue_bytes" ] || max_queue_bytes="$queue_bytes"
        max_gc_fraction="$(awk -v a="$max_gc_fraction" -v b="$gc_fraction" 'BEGIN { print (b > a ? b : a) }')"
        fd_ratio="$(awk -v used="$open_fds" -v limit="$max_fds" 'BEGIN { print (limit > 0 ? used / limit : 0) }')"
        worker_ratio="$(awk -v used="$workers" -v limit="$target_workers" 'BEGIN { print (limit > 0 ? used / limit : 0) }')"
        max_fd_ratio="$(awk -v a="$max_fd_ratio" -v b="$fd_ratio" 'BEGIN { print (b > a ? b : a) }')"
        max_worker_ratio="$(awk -v a="$max_worker_ratio" -v b="$worker_ratio" 'BEGIN { print (b > a ? b : a) }')"
        lane_pressure_candidate=0
        # Count current pressure only when a pool is at its protocol/global
        # lane cap and still owns an active overflow request. Historical
        # overflow counter deltas remain observability data, not a gauge.
        if [ "$lane_pools_at_cap_pressure" -gt 0 ] && \
            [ "$lane_pools_overflowing" -gt 0 ] && [ "$lane_overflow_active" -gt 0 ]; then
            lane_pressure_candidate=1
        fi
        [ "$lane_pressure_candidate" -le "$max_lane_pressure_candidates" ] || \
            max_lane_pressure_candidates="$lane_pressure_candidate"
    done

    if [ "$invalid" -gt 0 ]; then
        # Do not apply the next complete sample against a baseline that spans
        # an observation gap. The recovered counters may include an arbitrary
        # amount of traffic while one replica was unreadable.
        state_needs_reset=1
        echo "autoscaler hold current=${current} healthy=${healthy} invalid=${invalid} reason=invalid_metrics" >&2
        sleep "$interval"
        continue
    fi
    if [ "$healthy" -eq 0 ]; then
        state_needs_reset=1
        sleep "$interval"
        continue
    fi
    if [ "$healthy" -lt "$current" ]; then
        state_needs_reset=1
        echo "autoscaler hold current=${current} healthy=${healthy} reason=warmup_in_progress" >&2
        sleep "$interval"
        continue
    fi
    # Pair the timestamp with the complete metrics sample.  Both admission-cell
    # reconciliation above and drain_one() below can block, and POSIX functions
    # share variables with their caller.  This immutable sample time keeps
    # those waits from distorting the next counter-rate window.
    sample_now="$(date +%s)"
    if [ "$state_needs_reset" -eq 1 ]; then
        reset_baselines "$topology" "$sample_now" "$total_accepted" "$total_go_requests" "$total_go_errors" \
            "$total_edge_upstream_attempts" "$total_edge_connect_errors" "$total_edge_upstream_responses" \
            "$total_edge_upstream_5xx" "$total_edge_upstream_429" "$total_edge_retry_attempts" \
            "$total_relay_requests" "$total_lane_overflow_total"
        echo "autoscaler hold current=${current} reason=state_baseline_reset" >&2
        maybe_scale_admission_cells
        sleep "$interval"
        continue
    fi
    if [ "$topology" != "$state_topology" ]; then
        reset_baselines "$topology" "$sample_now" "$total_accepted" "$total_go_requests" "$total_go_errors" \
            "$total_edge_upstream_attempts" "$total_edge_connect_errors" "$total_edge_upstream_responses" \
            "$total_edge_upstream_5xx" "$total_edge_upstream_429" "$total_edge_retry_attempts" \
            "$total_relay_requests" "$total_lane_overflow_total"
        echo "autoscaler hold current=${current} reason=topology_changed" >&2
        maybe_scale_admission_cells
        sleep "$interval"
        continue
    fi
    if [ "$total_accepted" -lt "$prev_accepted" ] || \
        [ "$total_relay_requests" -lt "$prev_relay_requests" ] || \
        [ "$total_go_requests" -lt "$prev_go_requests" ] || \
        [ "$total_go_errors" -lt "$prev_go_errors" ] || \
        [ "$total_edge_upstream_attempts" -lt "$prev_edge_upstream_attempts" ] || \
        [ "$total_edge_connect_errors" -lt "$prev_edge_connect_errors" ] || \
        [ "$total_edge_upstream_responses" -lt "$prev_edge_upstream_responses" ] || \
        [ "$total_edge_upstream_5xx" -lt "$prev_edge_upstream_5xx" ] || \
        [ "$total_edge_upstream_429" -lt "$prev_edge_upstream_429" ] || \
        [ "$total_edge_retry_attempts" -lt "$prev_edge_retry_attempts" ] || \
        [ "$total_lane_overflow_total" -lt "$prev_lane_overflow_total" ]; then
        reset_baselines "$topology" "$sample_now" "$total_accepted" "$total_go_requests" "$total_go_errors" \
            "$total_edge_upstream_attempts" "$total_edge_connect_errors" "$total_edge_upstream_responses" \
            "$total_edge_upstream_5xx" "$total_edge_upstream_429" "$total_edge_retry_attempts" \
            "$total_relay_requests" "$total_lane_overflow_total"
        echo "autoscaler hold current=${current} reason=counter_rollback" >&2
        maybe_scale_admission_cells
        sleep "$interval"
        continue
    fi
    elapsed=$((sample_now - prev_ts))
    new_rps=0
    if [ "$elapsed" -gt 0 ]; then
        new_rps=$(((total_accepted - prev_accepted) / elapsed))
    fi
    previous_ewma="$ewma_rps"
    ewma_rps="$(awk -v old="$ewma_rps" -v sample="$new_rps" 'BEGIN { printf "%.0f", old * 0.8 + sample * 0.2 }')"
    predicted_rps="$(awk -v current="$ewma_rps" -v previous="$previous_ewma" -v interval="$interval" '
        BEGIN {
            growth = current - previous
            if (growth < 0) growth = 0
            steps = int(60 / interval)
            if (steps < 1) steps = 1
            printf "%.0f", current + growth * steps
        }')"

    window_accepted=$((total_accepted - prev_accepted))
    window_relay_requests=$((total_relay_requests - prev_relay_requests))
    window_go_requests=$((total_go_requests - prev_go_requests))
    window_go_errors=$((total_go_errors - prev_go_errors))
    window_edge_upstream_attempts=$((total_edge_upstream_attempts - prev_edge_upstream_attempts))
    window_edge_connect_errors=$((total_edge_connect_errors - prev_edge_connect_errors))
    window_edge_upstream_responses=$((total_edge_upstream_responses - prev_edge_upstream_responses))
    window_edge_upstream_5xx=$((total_edge_upstream_5xx - prev_edge_upstream_5xx))
    window_edge_upstream_429=$((total_edge_upstream_429 - prev_edge_upstream_429))
    window_edge_retry_attempts=$((total_edge_retry_attempts - prev_edge_retry_attempts))
    window_lane_overflow_total=$((total_lane_overflow_total - prev_lane_overflow_total))
    upstream_degraded=0
    if ! evaluate_ratio_guard go_5xx_ratio "$window_go_errors" "$window_go_requests" "$go_5xx_ratio_limit"; then
        upstream_degraded=1
    fi
    if ! evaluate_ratio_guard edge_connect_error_ratio "$window_edge_connect_errors" \
        "$window_edge_upstream_attempts" "$edge_connect_error_ratio_limit"; then
        upstream_degraded=1
    fi
    if ! evaluate_ratio_guard edge_5xx_ratio "$window_edge_upstream_5xx" \
        "$window_edge_upstream_responses" "$edge_5xx_ratio_limit"; then
        upstream_degraded=1
    fi
    if ! evaluate_ratio_guard edge_429_ratio "$window_edge_upstream_429" \
        "$window_edge_upstream_responses" "$edge_429_ratio_limit"; then
        upstream_degraded=1
    fi
    if ! evaluate_ratio_guard edge_retries_per_request "$window_edge_retry_attempts" \
        "$window_relay_requests" "$edge_retries_per_request_limit"; then
        upstream_degraded=1
    fi

    effective_streams=$((target_streams * 70 / 100))
    effective_rps=$((target_rps * 70 / 100))
    effective_go=$((target_go_active * 70 / 100))
    [ "$effective_streams" -ge 1 ] || effective_streams=1
    [ "$effective_rps" -ge 1 ] || effective_rps=1
    [ "$effective_go" -ge 1 ] || effective_go=1
    desired_streams="$(ceil_div "$total_streams" "$effective_streams")"
    desired_rps="$(ceil_div "$predicted_rps" "$effective_rps")"
    desired_go="$(ceil_div "$total_go_active" "$effective_go")"
    desired="$desired_streams"
    [ "$desired_rps" -le "$desired" ] || desired="$desired_rps"
    [ "$desired_go" -le "$desired" ] || desired="$desired_go"
    desired="$(clamp_replicas "$desired")"

    signals=0
    [ "$total_streams" -le $((current * effective_streams)) ] || signals=$((signals + 1))
    [ "$predicted_rps" -le $((current * effective_rps)) ] || signals=$((signals + 1))
    [ "$total_go_active" -le $((current * effective_go)) ] || signals=$((signals + 1))
    [ "$max_queue" -le $((target_queue * 60 / 100)) ] || signals=$((signals + 1))
    [ "$max_queue_bytes" -le 53687091 ] || signals=$((signals + 1))
    gc_high="$(awk -v value="$max_gc_fraction" 'BEGIN { print (value > 0.10 ? 1 : 0) }')"
    [ "$gc_high" -eq 0 ] || signals=$((signals + 1))
    fd_high="$(awk -v value="$max_fd_ratio" 'BEGIN { print (value > 0.65 ? 1 : 0) }')"
    [ "$fd_high" -eq 0 ] || signals=$((signals + 1))
    workers_high="$(awk -v value="$max_worker_ratio" 'BEGIN { print (value > 0.70 ? 1 : 0) }')"
    [ "$workers_high" -eq 0 ] || signals=$((signals + 1))
    lane_pressure=0
    if [ "$lane_pressure_enabled" = "true" ] && \
        [ "$max_lane_pressure_candidates" -gt 0 ]; then
        lane_pressure=1
    fi
    if [ "$lane_pressure" -eq 1 ]; then
        signals=$((signals + 1))
    fi

    if [ "$upstream_degraded" -eq 1 ]; then
        up_samples=0
        low_since=0
    elif [ "$signals" -ge 2 ]; then
        up_samples=$((up_samples + 1))
        low_since=0
        if [ "$up_samples" -ge 2 ]; then
            [ "$desired" -gt "$current" ] || desired=$((current + 1))
            desired="$(clamp_replicas "$desired")"
            if [ "$desired" -le "$current" ]; then
                # The host-derived ceiling can be lower than the number of
                # already-running replicas. Never let a scale-up decision turn
                # into an undrained scale-down after clamp_replicas.
                echo "autoscaler hold current=${current} ceiling=${max_replicas} reason=replica_ceiling" >&2
            elif ! scale_to "$desired" "$current" "sustained_controllable_pressure"; then
                echo "autoscaler scale_failed current=${current} desired=${desired} reason=sustained_controllable_pressure" >&2
            fi
            up_samples=0
        fi
    elif [ "$lane_pressure" -eq 1 ]; then
        # A single hot upstream key is intentionally insufficient for scale
        # out, but it must still block scale down; removing a pair while a
        # lane is overflowing would make the local bottleneck worse.
        up_samples=0
        low_since=0
    else
        up_samples=0
        low="$(awk -v streams="$total_streams" -v stream_cap="$((current * target_streams))" \
            -v rps="$ewma_rps" -v rps_cap="$((current * target_rps))" \
            -v active="$total_go_active" -v active_cap="$((current * target_go_active))" \
            'BEGIN { print (streams < stream_cap * 0.25 && rps < rps_cap * 0.25 && active < active_cap * 0.25 ? 1 : 0) }')"
        if [ "$low" -eq 1 ] && [ "$current" -gt "$min_replicas" ]; then
            [ "$low_since" -gt 0 ] || low_since="$sample_now"
            if [ $((sample_now - low_since)) -ge "$scale_down_seconds" ]; then
                if drain_one; then
                    desired=$((current - 1))
                    if ! scale_to "$desired" "$current" "sustained_low_utilization"; then
                        echo "autoscaler scale_failed current=${current} desired=${desired} reason=sustained_low_utilization" >&2
                        restore_drained_pair "$container" || true
                    fi
                fi
                low_since=0
            fi
        else
            low_since=0
        fi
    fi

    prev_ts="$sample_now"
    prev_accepted="$total_accepted"
    prev_relay_requests="$total_relay_requests"
    prev_go_requests="$total_go_requests"
    prev_go_errors="$total_go_errors"
    prev_edge_upstream_attempts="$total_edge_upstream_attempts"
    prev_edge_connect_errors="$total_edge_connect_errors"
    prev_edge_upstream_responses="$total_edge_upstream_responses"
    prev_edge_upstream_5xx="$total_edge_upstream_5xx"
    prev_edge_upstream_429="$total_edge_upstream_429"
    prev_edge_retry_attempts="$total_edge_retry_attempts"
    prev_lane_overflow_total="$total_lane_overflow_total"
    state_topology="$topology"
    write_state
	maybe_scale_admission_cells
    sleep "$interval"
done
