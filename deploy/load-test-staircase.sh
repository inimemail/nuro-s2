#!/bin/sh
set -eu

: "${BASE_URL:?BASE_URL is required}"
: "${API_KEY:?API_KEY is required}"

targets="${LOAD_TARGETS:-300000,500000,1000000}"
duration="${LOAD_STAGE_DURATION:-10m}"
model="${LOAD_MODEL:-gpt-5.6}"
generator_share="${LOAD_GENERATOR_SHARE:-100}"
results="${LOAD_RESULTS_DIR:-./load-results}"

mkdir -p "$results"
old_ifs="$IFS"
IFS=,
for global_target in $targets; do
    local_target=$((global_target * generator_share / 100))
    [ "$local_target" -gt 0 ] || local_target=1
    echo "load stage global=${global_target} local=${local_target} duration=${duration}" >&2
    docker run --rm --network host \
        -e BASE_URL="$BASE_URL" -e API_KEY="$API_KEY" -e LOAD_MODEL="$model" \
        -e LOAD_VUS="$local_target" -e LOAD_DURATION="$duration" \
        -v "$(pwd)/deploy/load-test-stream.js:/script.js:ro" \
        -v "$(pwd)/$results:/results" \
        grafana/k6:0.57.0 run --summary-export "/results/${global_target}.json" /script.js
done
IFS="$old_ifs"
