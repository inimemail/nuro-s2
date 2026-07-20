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
upstream_error_ratio_limit="${AUTOSCALE_UPSTREAM_ERROR_RATIO:-0.20}"
scale_down_seconds="${AUTOSCALE_SCALE_DOWN_SECONDS:-600}"
drain_seconds="${AUTOSCALE_DRAIN_SECONDS:-1800}"
force_stop_seconds="${AUTOSCALE_FORCE_STOP_SECONDS:-30}"
state_dir="${AUTOSCALE_STATE_DIR:-/state}"
state_file="${state_dir}/state"

mkdir -p "$state_dir"

host_cpus="$(getconf _NPROCESSORS_ONLN 2>/dev/null || awk '/^processor/ { count++ } END { print count + 0 }' /proc/cpuinfo)"
host_memory_mb="$(awk '/MemTotal:/ { print int($2 / 1024) }' /proc/meminfo)"
cpu_limit=$((host_cpus * 80 / 100 / min_cpu_per_pair))
memory_limit=$((host_memory_mb * 80 / 100 / min_memory_mb_per_pair))
[ "$cpu_limit" -ge 1 ] || cpu_limit=1
[ "$memory_limit" -ge 1 ] || memory_limit=1
[ "$max_replicas" -le "$cpu_limit" ] || max_replicas="$cpu_limit"
[ "$max_replicas" -le "$memory_limit" ] || max_replicas="$memory_limit"
[ "$max_replicas" -ge "$min_replicas" ] || min_replicas="$max_replicas"

ceil_div() {
    awk -v value="$1" -v divisor="$2" 'BEGIN { if (divisor <= 0) print 1; else print int((value + divisor - 1) / divisor) }'
}

metric() {
    name="$1"
    awk -v name="$name" '$1 == name { print $2; exit }'
}

clamp_replicas() {
    value="$1"
    [ "$value" -ge "$min_replicas" ] || value="$min_replicas"
    [ "$value" -le "$max_replicas" ] || value="$max_replicas"
    printf '%s\n' "$value"
}

scale_to() {
    desired="$(clamp_replicas "$1")"
    current="$2"
    [ "$desired" -ne "$current" ] || return 0
    echo "autoscaler decision current=${current} desired=${desired} reason=$3" >&2
    docker compose -p "$project" -f /workspace/docker-compose.yml --env-file /workspace/.env \
        up -d --no-deps --no-recreate --scale "${service}=${desired}" "$service"
}

drain_one() {
    container="$(docker ps \
        --filter "label=com.docker.compose.project=${project}" \
        --filter "label=com.docker.compose.service=${service}" \
        --format '{{.Label "com.docker.compose.container-number"}} {{.ID}}' | sort -nr | awk 'NR == 1 { print $2 }')"
    [ -n "$container" ] || return 1
    docker exec "$container" sh -ec '
        wget -q -O /dev/null --post-data='' \
          --header="X-Sub2API-Edge-Secret: ${SUB2API_EDGE_INTERNAL_SECRET}" \
          http://127.0.0.1:18080/internal/drain
        wget -q -O /dev/null --post-data='' \
          --header="X-Sub2API-Edge-Secret: ${GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET}" \
          http://127.0.0.1:8080/internal/runtime/drain
    ' || return 1

    started="$(date +%s)"
    while :; do
        streams="$(docker exec "$container" wget -q -O - http://127.0.0.1:18080/metrics 2>/dev/null | metric sub2api_edge_active_streams || true)"
        settlements="$(docker exec "$container" wget -q -O - http://127.0.0.1:18080/metrics 2>/dev/null | metric sub2api_edge_settlement_retry_queue_depth || true)"
        settlement_workers="$(docker exec "$container" wget -q -O - http://127.0.0.1:18080/metrics 2>/dev/null | metric sub2api_edge_settlement_workers_active || true)"
        payload_commits="$(docker exec "$container" wget -q -O - http://127.0.0.1:18080/metrics 2>/dev/null | metric sub2api_edge_payload_commit_queue_depth || true)"
        payload_commit_workers="$(docker exec "$container" wget -q -O - http://127.0.0.1:18080/metrics 2>/dev/null | metric sub2api_edge_payload_commit_workers_active || true)"
        requests="$(docker exec "$container" wget -q -O - http://127.0.0.1:8080/metrics 2>/dev/null | metric sub2api_go_active_requests || true)"
        streams="${streams:-0}"
        requests="${requests:-0}"
        settlements="${settlements:-0}"
        settlement_workers="${settlement_workers:-0}"
        payload_commits="${payload_commits:-0}"
        payload_commit_workers="${payload_commit_workers:-0}"
        if [ "$streams" -eq 0 ] && [ "$requests" -le 1 ] && [ "$settlements" -eq 0 ] && \
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
prev_go_requests=0
prev_go_errors=0
ewma_rps=0
up_samples=0
low_since=0
if [ -s "$state_file" ]; then
    read -r prev_ts prev_accepted ewma_rps up_samples low_since prev_go_requests prev_go_errors < "$state_file" || true
fi

cell_platforms="openai anthropic"
cell_enabled="${ADMISSION_CELL_AUTOSCALE_ENABLED:-true}"
cell_max="${ADMISSION_CELL_MAX_PER_PLATFORM:-8}"
cell_target_ops="${ADMISSION_CELL_TARGET_OPS:-50000}"
cell_target_memory_mb="${ADMISSION_CELL_TARGET_MEMORY_MB:-8192}"
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
        available_mb="$(awk '/MemAvailable:/ { print int($2 / 1024) }' /proc/meminfo)"
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
            --ulimit nofile=100000:100000 \
            -e "REDIS_PASSWORD=${REDIS_PASSWORD:-}" \
            -e "REDISCLI_AUTH=${REDIS_PASSWORD:-}" \
            -e "ADMISSION_CELL_IO_THREADS=${ADMISSION_CELL_IO_THREADS:-8}" \
            -e "ADMISSION_CELL_MAXMEMORY_MB=${cell_target_memory_mb}" \
            -e "ADMISSION_CELL_MAX_CLIENTS=${ADMISSION_CELL_MAX_CLIENTS:-100000}" \
            -v "${volume}:/data" redis:8-alpine sh -ec \
            'exec redis-server --save "300 10" --appendonly yes --appendfsync everysec --maxmemory "${ADMISSION_CELL_MAXMEMORY_MB}mb" --maxmemory-policy noeviction --maxclients "${ADMISSION_CELL_MAX_CLIENTS}" --tcp-backlog 8192 --timeout 0 --hz 20 --io-threads "${ADMISSION_CELL_IO_THREADS}" --io-threads-do-reads yes ${REDIS_PASSWORD:+--requirepass "$REDIS_PASSWORD"}' >/dev/null
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
            ops="$(cell_ops "$info")"; ops="${ops:-0}"
            memory="$(cell_memory_bytes "$container")"; memory="${memory:-0}"
            total_ops=$((total_ops + ops))
            [ "$memory" -le "$max_memory" ] || max_memory="$memory"
        done
        pressure=0
        [ "$total_ops" -gt $((count * cell_target_ops * 70 / 100)) ] && pressure=$((pressure + 1))
        [ "$max_memory" -gt $((cell_target_memory_mb * 1024 * 1024 * 65 / 100)) ] && pressure=$((pressure + 1))
        state="${cell_state_dir}/${platform}"
        samples=0
        [ -s "$state" ] && read -r samples < "$state" || true
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
    now="$(date +%s)"
	reconcile_catalog_cells
    containers="$(docker ps \
        --filter "label=com.docker.compose.project=${project}" \
        --filter "label=com.docker.compose.service=${service}" \
        --filter status=running --format '{{.ID}}')"
    current="$(printf '%s\n' "$containers" | awk 'NF { count++ } END { print count + 0 }')"
    if [ "$current" -eq 0 ]; then
        maybe_scale_admission_cells
        sleep "$interval"
        continue
    fi
    if [ "$current" -lt "$min_replicas" ]; then
        scale_to "$min_replicas" "$current" "warm_headroom_minimum"
		maybe_scale_admission_cells
        sleep "$interval"
        continue
    fi

    total_streams=0
    total_go_active=0
    total_accepted=0
    total_go_requests=0
    total_go_errors=0
    max_queue=0
    max_queue_bytes=0
    max_gc_fraction=0
    max_fd_ratio=0
    max_worker_ratio=0
    healthy=0
    for container in $containers; do
        edge="$(docker exec "$container" wget -q -O - http://127.0.0.1:18080/metrics 2>/dev/null || true)"
        go="$(docker exec "$container" wget -q -O - http://127.0.0.1:8080/metrics 2>/dev/null || true)"
        [ -n "$edge" ] && [ -n "$go" ] || continue
        healthy=$((healthy + 1))
        streams="$(printf '%s\n' "$edge" | metric sub2api_edge_active_streams)"; streams="${streams:-0}"
        accepted="$(printf '%s\n' "$edge" | metric sub2api_edge_accepted_requests)"; accepted="${accepted:-0}"
        queue="$(printf '%s\n' "$edge" | metric sub2api_edge_relay_queue_depth)"; queue="${queue:-0}"
        queue_bytes="$(printf '%s\n' "$edge" | metric sub2api_edge_relay_queue_bytes)"; queue_bytes="${queue_bytes:-0}"
        go_active="$(printf '%s\n' "$go" | metric sub2api_go_active_requests)"; go_active="${go_active:-0}"
        go_requests="$(printf '%s\n' "$go" | metric sub2api_go_requests_total)"; go_requests="${go_requests:-0}"
        go_errors="$(printf '%s\n' "$go" | metric sub2api_go_server_errors_total)"; go_errors="${go_errors:-0}"
        gc_fraction="$(printf '%s\n' "$go" | metric sub2api_go_gc_cpu_fraction)"; gc_fraction="${gc_fraction:-0}"
        open_fds="$(printf '%s\n' "$edge" | metric sub2api_edge_open_fds)"; open_fds="${open_fds:-0}"
        max_fds="$(printf '%s\n' "$edge" | metric sub2api_edge_max_fds)"; max_fds="${max_fds:-0}"
        workers="$(printf '%s\n' "$edge" | metric sub2api_edge_relay_workers_active)"; workers="${workers:-0}"
        total_streams=$((total_streams + streams))
        total_go_active=$((total_go_active + go_active))
        total_go_requests=$((total_go_requests + go_requests))
        total_go_errors=$((total_go_errors + go_errors))
        total_accepted=$((total_accepted + accepted))
        [ "$queue" -le "$max_queue" ] || max_queue="$queue"
        [ "$queue_bytes" -le "$max_queue_bytes" ] || max_queue_bytes="$queue_bytes"
        max_gc_fraction="$(awk -v a="$max_gc_fraction" -v b="$gc_fraction" 'BEGIN { print (b > a) ? b : a }')"
        fd_ratio="$(awk -v used="$open_fds" -v limit="$max_fds" 'BEGIN { print limit > 0 ? used / limit : 0 }')"
        worker_ratio="$(awk -v used="$workers" -v limit="$target_workers" 'BEGIN { print limit > 0 ? used / limit : 0 }')"
        max_fd_ratio="$(awk -v a="$max_fd_ratio" -v b="$fd_ratio" 'BEGIN { print (b > a) ? b : a }')"
        max_worker_ratio="$(awk -v a="$max_worker_ratio" -v b="$worker_ratio" 'BEGIN { print (b > a) ? b : a }')"
    done

    [ "$healthy" -gt 0 ] || { sleep "$interval"; continue; }
    if [ "$healthy" -lt "$current" ]; then
        echo "autoscaler hold current=${current} healthy=${healthy} reason=warmup_in_progress" >&2
        sleep "$interval"
        continue
    fi
    elapsed=$((now - prev_ts))
    new_rps=0
    if [ "$prev_ts" -gt 0 ] && [ "$elapsed" -gt 0 ] && [ "$total_accepted" -ge "$prev_accepted" ]; then
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

    window_requests=$((total_go_requests - prev_go_requests))
    window_errors=$((total_go_errors - prev_go_errors))
    upstream_degraded=0
    if [ "$window_requests" -ge 10 ]; then
        error_ratio="$(awk -v errors="$window_errors" -v requests="$window_requests" 'BEGIN { print errors / requests }')"
        degraded="$(awk -v ratio="$error_ratio" -v limit="$upstream_error_ratio_limit" 'BEGIN { print ratio >= limit ? 1 : 0 }')"
        [ "$degraded" -eq 0 ] || upstream_degraded=1
    fi

    effective_streams=$((target_streams * 70 / 100))
    effective_rps=$((target_rps * 70 / 100))
    effective_go=$((target_go_active * 70 / 100))
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
    gc_high="$(awk -v value="$max_gc_fraction" 'BEGIN { print value > 0.10 ? 1 : 0 }')"
    [ "$gc_high" -eq 0 ] || signals=$((signals + 1))
    fd_high="$(awk -v value="$max_fd_ratio" 'BEGIN { print value > 0.65 ? 1 : 0 }')"
    [ "$fd_high" -eq 0 ] || signals=$((signals + 1))
    workers_high="$(awk -v value="$max_worker_ratio" 'BEGIN { print value > 0.70 ? 1 : 0 }')"
    [ "$workers_high" -eq 0 ] || signals=$((signals + 1))

    if [ "$upstream_degraded" -eq 1 ]; then
        up_samples=0
        low_since=0
        echo "autoscaler hold reason=upstream_error_ratio" >&2
    elif [ "$signals" -ge 2 ]; then
        up_samples=$((up_samples + 1))
        low_since=0
        if [ "$up_samples" -ge 2 ]; then
            [ "$desired" -gt "$current" ] || desired=$((current + 1))
            scale_to "$desired" "$current" "sustained_controllable_pressure"
            up_samples=0
        fi
    else
        up_samples=0
        low="$(awk -v streams="$total_streams" -v stream_cap="$((current * target_streams))" \
            -v rps="$ewma_rps" -v rps_cap="$((current * target_rps))" \
            -v active="$total_go_active" -v active_cap="$((current * target_go_active))" \
            'BEGIN { print (streams < stream_cap * 0.25 && rps < rps_cap * 0.25 && active < active_cap * 0.25) ? 1 : 0 }')"
        if [ "$low" -eq 1 ] && [ "$current" -gt "$min_replicas" ]; then
            [ "$low_since" -gt 0 ] || low_since="$now"
            if [ $((now - low_since)) -ge "$scale_down_seconds" ]; then
                if drain_one; then
                    scale_to $((current - 1)) "$current" "sustained_low_utilization"
                fi
                low_since=0
            fi
        else
            low_since=0
        fi
    fi

    prev_ts="$now"
    prev_accepted="$total_accepted"
    prev_go_requests="$total_go_requests"
    prev_go_errors="$total_go_errors"
    printf '%s %s %s %s %s %s %s\n' "$prev_ts" "$prev_accepted" "$ewma_rps" "$up_samples" "$low_since" "$prev_go_requests" "$prev_go_errors" > "$state_file"
	maybe_scale_admission_cells
    sleep "$interval"
done
