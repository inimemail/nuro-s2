# sub2api-edge-rs

Optional Rust data plane for OpenAI-compatible streaming paths.

The Go service remains the control plane. Auth, billing, account scheduling,
soft cooling, priority, pool mode, sticky routing, failover policy, and usage
accounting stay in Go. The Rust edge only handles high-frequency I/O after Go
returns an executable edge plan.

## Environment

```powershell
$env:SUB2API_EDGE_LISTEN_ADDR = "127.0.0.1:18080"
$env:SUB2API_EDGE_GO_BASE_URL = "http://127.0.0.1:8080"
# Optional. Defaults to SUB2API_EDGE_GO_BASE_URL.
$env:SUB2API_EDGE_CONTROL_BASE_URL = "http://127.0.0.1:8080"
$env:SUB2API_EDGE_INTERNAL_SECRET = "change-me"
# Stable and unique per edge node; enables old-instance lease recovery.
$env:SUB2API_EDGE_NODE_ID = "edge-node-1"
$env:SUB2API_EDGE_PREPARE_TIMEOUT_MS = "1500"
$env:SUB2API_EDGE_COMPLETE_TIMEOUT_MS = "1500"
$env:SUB2API_EDGE_DRAIN_TIMEOUT_SECS = "1800"
$env:SUB2API_EDGE_INITIAL_POOL_SIZE = "512"
$env:SUB2API_EDGE_QUEUE_BUFFER_SIZE = "512"
$env:SUB2API_EDGE_INGRESS_BODY_MAX_BYTES = "2147483648"
$env:SUB2API_EDGE_QUEUE_MAX_BYTES = "268435456"
$env:SUB2API_EDGE_GLOBAL_WORKERS = "512"
$env:SUB2API_EDGE_PER_ACCOUNT_WORKERS = "128"
$env:SUB2API_EDGE_MAX_RELAY_DOMAINS = "4096"
$env:SUB2API_EDGE_MAX_DYNAMIC_WARM_KEYS = "4096"
$env:SUB2API_EDGE_MAX_IDLE_PER_ACCOUNT = "128"
$env:SUB2API_EDGE_QUEUE_WAIT_BUDGET_MS = "150"
$env:SUB2API_EDGE_LARGE_PAYLOAD_PASSTHROUGH = "true"
$env:SUB2API_EDGE_LARGE_PAYLOAD_THRESHOLD_BYTES = "262144"
$env:SUB2API_EDGE_WS_IDLE_PER_KEY = "1"
# Optional. Background direct upstream connection keep-warm (defaults off).
# Set a URL only when direct egress is expected for real traffic.
$env:SUB2API_EDGE_UPSTREAM_WARM_URL = ""
$env:SUB2API_EDGE_UPSTREAM_WARM_INTERVAL_SECS = "30"
$env:SUB2API_EDGE_UPSTREAM_DYNAMIC_WARM_ACTIVE_SECS = "300"
$env:RUST_LOG = "warn"
cargo run --manifest-path edge-rs/Cargo.toml
```

Active requests renew their 120-second Go lease in the background. Failed
complete/abort callbacks retry without adding a request-path round trip.

The Go config must also enable the internal control API:

```yaml
gateway:
  openai_edge_rs:
    enabled: true
    internal_api_enabled: true
    internal_secret: change-me
    mode: relay
    ingress_proxy_enabled: true
    go_base_url: http://127.0.0.1:8080
    control_base_url: http://127.0.0.1:8080
```

`shadow` and `off` always fall back to the existing Go path. `relay` allows the
edge to directly connect upstream only when Go returns a relay plan.

The Linux one-click systemd installer writes these values to
`/opt/sub2api/edge-rs.env` and enables `sub2api-edge-rs.service` automatically.
With `ingress_proxy_enabled=true`, clients can keep using the Go port; Go
forwards eligible hot paths to the local Rust edge and falls back locally if the
edge is unavailable.

Current relay scope is intentionally narrow:

- `/v1/chat/completions` and `/openai/v1/chat/completions`
- `stream=true`
- OpenAI APIKey accounts whose upstream path is already raw
  `/v1/chat/completions`
- `/v1/responses` and `/openai/v1/responses`, only native OpenAI APIKey
  passthrough accounts with Responses API enabled
- Responses WebSocket upgrades, only when Go resolves the selected account to
  WSv2 passthrough mode and no proxy is required
- no Go-side protocol conversion is required

Everything else falls back to Go, including OAuth/ChatGPT accounts, Responses
WebSocket ctx_pool/shared/dedicated stateful modes, WS proxy accounts, images,
multipart requests, non-stream requests, subscription or IP-ACL cases that
require the normal public middleware path, and any account that still needs Go
to translate protocols.

## Supported Data Path

The edge accepts OpenAI hot-path requests and calls:

```text
POST /internal/edge/openai/prepare
POST /internal/edge/openai/retry
POST /internal/edge/openai/renew
POST /internal/edge/openai/complete
POST /internal/edge/openai/abort
```

When Go returns `fallback_go`, the edge reverse-proxies the request to the
existing Go route so current behavior is preserved.

When Go returns `relay`, the edge opens the upstream request itself. It keeps
separate HTTP clients per proxy URL, streams bytes back to the client without
waiting for the full response, and reports completion metrics back to Go. HTTP
relay plans are submitted to one bounded process-wide executor; account + proxy
and upstream host domains use lightweight in-flight permits. It uses prewarmed
SSE scratch buffers and prefers raw body bytes over JSON re-materialization.
Safe Responses WSv2 passthrough can keep unused
preconnected upstream sockets per account/header key; sockets that have carried
a client session are not returned to the idle pool.

If the upstream returns a 4xx/5xx before any client response is committed, the
edge calls `/retry`; Go remains responsible for same-account retry, account
switch, soft cooling, pool-mode decisions, and final fallback. Normal completion
calls `/complete`; dropped streams, relay setup failures, and WebSocket client
disconnects call `/abort`, with Go's lease TTL left as crash recovery.
