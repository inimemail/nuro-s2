#!/bin/sh
set -eu

go_pid=""
edge_pid=""
stopping=0

terminate_children() {
    [ "$stopping" -eq 0 ] || return 0
    stopping=1
    # Stop new Go traffic first but keep the control plane alive for Edge
    # complete/abort/commit callbacks while existing streams drain.
    wget -q -O /dev/null --post-data='' \
        --header="X-Sub2API-Edge-Secret: ${GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET:-}" \
        http://127.0.0.1:8080/internal/runtime/drain 2>/dev/null || true
    if [ -n "$edge_pid" ]; then
        kill -TERM "$edge_pid" 2>/dev/null || true
        edge_grace="${PAIRED_EDGE_SHUTDOWN_GRACE_SECONDS:-1740}"
        case "$edge_grace" in
            ''|*[!0-9]*) edge_grace=1740 ;;
        esac
        started="$(date +%s)"
        while kill -0 "$edge_pid" 2>/dev/null; do
            now="$(date +%s)"
            [ $((now - started)) -lt "$edge_grace" ] || break
            sleep 1
        done
    fi
    [ -z "$go_pid" ] || kill -TERM "$go_pid" 2>/dev/null || true
}

trap terminate_children INT TERM HUP

export SUB2API_EDGE_NODE_ID="${SUB2API_EDGE_NODE_ID:-$(hostname)}"

/app/sub2api "$@" &
go_pid=$!
/app/sub2api-edge-rs &
edge_pid=$!

status=0
while kill -0 "$go_pid" 2>/dev/null && kill -0 "$edge_pid" 2>/dev/null; do
    sleep 1
done

if ! kill -0 "$go_pid" 2>/dev/null; then
    wait "$go_pid" || status=$?
fi
if ! kill -0 "$edge_pid" 2>/dev/null; then
    edge_status=0
    wait "$edge_pid" || edge_status=$?
    [ "$status" -ne 0 ] || status=$edge_status
fi

terminate_children
wait "$go_pid" 2>/dev/null || true
wait "$edge_pid" 2>/dev/null || true
exit "$status"
