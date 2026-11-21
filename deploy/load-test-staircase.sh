#!/bin/sh
set -eu

: "${BASE_URL:?BASE_URL is required}"
: "${API_KEY:?API_KEY is required}"

targets="${LOAD_TARGETS:-300000,500000,1000000}"
duration="${LOAD_STAGE_DURATION:-10m}"
model="${LOAD_MODEL:-gpt-5.6}"
generator_share="${LOAD_GENERATOR_SHARE:-100}"
results="${LOAD_RESULTS_DIR:-./load-results}"
# Optional comma-separated name=metrics-url entries, one per Go or Edge process.
metrics_nodes="${LOAD_METRICS_NODES:-}"

mkdir -p "$results"
results="$(cd "$results" && pwd)"

scrape_nodes() {
    phase="$1"
    for node in $metrics_nodes; do
        name="${node%%=*}"
        case "$name" in
            ''|*[!a-zA-Z0-9_-]*) continue ;;
        esac
        url="${node#*=}"
        case "$url" in
            http://*|https://*) ;;
            *) continue ;;
        esac
        path="$results/$global_target.$name.$phase.prom"
        if curl --silent --fail --max-time 5 --output "$path.tmp" "$url"; then
            mv "$path.tmp" "$path"
        else
            rm -f "$path.tmp" "$path"
            echo "metrics unavailable for $name ($phase)" >&2
        fi
    done
}

summarize_nodes() {
    [ -n "$metrics_nodes" ] || return 0
    for node in $metrics_nodes; do
        name="${node%%=*}"
        case "$name" in
            ''|*[!a-zA-Z0-9_-]*) continue ;;
        esac
        before="$results/$global_target.$name.before.prom"
        after="$results/$global_target.$name.after.prom"
        summary="$results/$global_target.$name.metrics.tsv"
        if [ ! -s "$before" ] || [ ! -s "$after" ]; then
            rm -f "$summary"
            continue
        fi
        awk -v OFS='	' '
            $1 == "#" && $2 == "TYPE" { kind[$3] = $4; next }
            $1 !~ /^#/ && NF == 2 && $1 ~ /^sub2api_/ {
                if (FNR == NR) { start[$1] = $2; next }
                metric = $1
                sub(/\{.*/, "", metric)
                if (kind[metric] == "counter" && ($1 in start)) {
                    if ($2 >= start[$1]) print $1, "delta", $2 - start[$1]
                    else print $1, "reset", $2
                } else if (kind[metric] == "gauge") {
                    print $1, "end", $2
                }
            }
        ' "$before" "$after" > "$summary"
    done
}
old_ifs="$IFS"
IFS=,
for global_target in $targets; do
    local_target=$((global_target * generator_share / 100))
    [ "$local_target" -gt 0 ] || local_target=1
    echo "load stage global=${global_target} local=${local_target} duration=${duration}" >&2
    scrape_nodes before
    stage_status=0
    docker run --rm --network host \
        -e BASE_URL="$BASE_URL" -e API_KEY="$API_KEY" -e LOAD_MODEL="$model" \
        -e LOAD_VUS="$local_target" -e LOAD_DURATION="$duration" \
        -v "$(pwd)/deploy/load-test-stream.js:/script.js:ro" \
        -v "$results:/results" \
        grafana/k6:0.57.0 run --summary-export "/results/${global_target}.json" /script.js || stage_status=$?
    scrape_nodes after
    summarize_nodes
    if [ "$stage_status" -ne 0 ]; then
        exit "$stage_status"
    fi
done
IFS="$old_ifs"
