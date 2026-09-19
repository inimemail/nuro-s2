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
$env:SUB2API_EDGE_DRAIN_TIMEOUT_SECS = "30"
$env:SUB2API_EDGE_INITIAL_POOL_SIZE = "512"
$env:SUB2API_EDGE_QUEUE_BUFFER_SIZE = "8192"
$env:SUB2API_EDGE_INGRESS_BODY_MAX_BYTES = "2147483648"
$env:SUB2API_EDGE_QUEUE_MAX_BYTES = "2147483648"
# 1-999999999 concurrent relay requests per Edge replica.
$env:SUB2API_EDGE_GLOBAL_WORKERS = "9999"
# 0 disables the per-account relay semaphore.
$env:SUB2API_EDGE_PER_ACCOUNT_WORKERS = "0"
$env:SUB2API_EDGE_MAX_RELAY_DOMAINS = "4096"
$env:SUB2API_EDGE_MAX_DYNAMIC_WARM_KEYS = "4096"
$env:SUB2API_EDGE_MAX_IDLE_PER_ACCOUNT = "128"
$env:SUB2API_EDGE_UPSTREAM_LANE_POOL_ENABLED = "true"
$env:SUB2API_EDGE_UPSTREAM_LANE_INITIAL = "64"
$env:SUB2API_EDGE_UPSTREAM_LANE_TARGET_INFLIGHT = "64"
$env:SUB2API_EDGE_UPSTREAM_LANE_HIGH_WATER = "32"
$env:SUB2API_EDGE_UPSTREAM_LANE_PRESSURE_MS = "10"
$env:SUB2API_EDGE_UPSTREAM_LANE_UNKNOWN_MAX = "64"
$env:SUB2API_EDGE_UPSTREAM_LANE_MAX = "256"
$env:SUB2API_EDGE_UPSTREAM_LANE_EXPANSION_STEP = "32"
$env:SUB2API_EDGE_UPSTREAM_LANE_EXPANSION_COOLDOWN_MS = "50"
$env:SUB2API_EDGE_UPSTREAM_LANE_IDLE_SECS = "300"
$env:SUB2API_EDGE_UPSTREAM_POOL_IDLE_SECS = "1200"
$env:SUB2API_EDGE_UPSTREAM_MAX_POOL_KEYS = "8192"
$env:SUB2API_EDGE_UPSTREAM_MAX_TOTAL_LANES = "65536"
$env:SUB2API_EDGE_TRANSIENT_PROXY_MAX_ACTIVE = "32"
$env:SUB2API_EDGE_QUEUE_WAIT_BUDGET_MS = "50"
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

Request-body admission has independent idle and total limits:
`SUB2API_EDGE_INGRESS_BODY_IDLE_TIMEOUT_MS` (default 30000) and
`SUB2API_EDGE_INGRESS_BODY_TOTAL_TIMEOUT_MS` (default 300000). These only cover
uploading the request, never model generation or SSE. Timeout releases both
the request permit and accumulated body-byte permits.

Go fallback now inherits Go's queue/upstream/stream timeout policies instead
of imposing fixed 30-second header and 180-second stream limits. Optional
deployment ceilings are `SUB2API_EDGE_FALLBACK_HEADER_TIMEOUT_MS` and
`SUB2API_EDGE_FALLBACK_BODY_IDLE_TIMEOUT_MS`; both default to 0 (no additional
Edge limit). Configure Go's timeout policies or these ceilings when an overall
limit is required. These settings cover both HTTP and WebSocket fallback.

Completion callbacks carry an authenticated encrypted billing snapshot issued
by Go, retained with the callback in the settlement WAL. Go can recover billing
after lease expiry, a Go restart, or delivery to another Go node, and only
acknowledges completion after idempotent billing succeeds. Missing/negative
acknowledgements remain pending. Permanently rejected callbacks remain in the
WAL for reconciliation without occupying retry workers indefinitely; transient
failures continue automatic retries. Deploy both Go and Edge for this protection;
older callbacks without a snapshot cannot reconstruct lost context. Keep the
same internal secret on control nodes and preserve it and the WAL across
restarts; rotating it while callbacks are pending prevents snapshot recovery.
Snapshots are internal data and must never be exposed in public responses or
logs. Lease renewal now fences active work before the last confirmed lease
expires, or immediately upon a definitive renewal rejection.
New leases carry a stable billing identity, independent of missing or late
provider request IDs. Snapshots from older leases retain their original identity
rules. Retry usage already observed is also retained when a client disconnects.
Pricing is still resolved by Go's existing billing service. A delayed replay
whose usage or recalculated price conflicts with an already committed charge
returns a conflict and stays in the WAL for reconciliation; it is not charged
again and does not monopolize an automatic retry worker.

## Internal stream recovery

With both binaries updated, Go reserves an authenticated stream session before
sending an eligible OpenAI SSE request to Edge. This adds one internal round
trip before execution. The session starts at most once. If the Go-to-Edge
connection fails, Go keeps its downstream connection open and reads the original
session at the exact byte offset already received; tools, preamble events,
comments and progress placeholders are not generated again. The upstream relay
and its usage collector continue independently of that internal connection.
Recovery never re-enters prepare or billing for already forwarded bytes.

Sessions use a 512 KiB rolling replay window and a 256-session admission limit
(at most 128 MiB of replay bytes, excluding in-flight relay chunks, headers and
metadata). A full session registry falls back to Go **before execution**.
Each journal is also capped at 1,024 chunks so tiny upstream fragments cannot
create unbounded metadata. Attached readers release consumed chunks as needed.
Detached sessions expire after 75 seconds; Go retries unreachable internal
transport for up to 25 seconds. A live session still waiting for upstream
headers reports pending and keeps the original upstream timeout policy.
Internal sockets also switch to recovery after 15 seconds without initial
headers after the POST upload finishes, or 35 seconds without body bytes; these cancel only the Go-to-Edge
socket, not the upstream execution or the downstream connection.
An explicit downstream cancellation stops the session immediately when the
cancel message arrives; expiry bounds orphan execution if it cannot arrive.
Session data is process-local: Go must address the same Edge process for reserve,
start, read and cancel. Do not put those requests behind random per-request load
balancing. A missing session, an expired replay cursor or an Edge process crash
does not authorize a fresh generation. Public WebSocket/direct-to-Edge client
reconnects and a broken upstream stream are outside this recovery mechanism.
The one exception is a first POST explicitly rejected by Edge before execution
because its reservation is absent; Go may then handle the untouched request.
This fallback is never used after an ambiguous transport failure.

New Go leases advertise an additional 120-second renewal grace and retain their
actual user/account concurrency slots during it. Rust uses the advertised grace
when tolerating transient control-plane failures, and stops before slot release
if renewal never recovers. Cancellation, ownership conflicts and missing leases
still stop execution; this does not reconstruct live leases after a Go restart.
With the default 120-second lease, the safety fence is approximately 200 seconds
after the last confirmed renewal attempt began, while Go retains slots for 240
seconds. Normal completion/cancellation releases slots without waiting for this
deadline. Older Go plans omit grace and retain the conservative renewal policy.
A replacement plan initially advertises only the remaining ownership window;
after a confirmed renewal, Rust uses the advertised full renewal TTL again.
The 75-second replay journal does not change the separate 24-hour prompt-cache
policy or cap the lifetime of an attached generation.

Local cross-language fault injection can be run after building Edge:

```sh
cd backend
NURO_EDGE_TEST_BINARY=/absolute/path/to/sub2api-edge-rs go test -race -tags unit ./internal/handler -run TestEdgeStreamRealRustTransportRecovery -count=1
```

The test closes real internal TCP responses before headers and inside SSE
frames, checks exact output, one upstream execution and complete usage delivery.
It uses local upstream/control fixtures; it does not validate production Redis,
PostgreSQL replicas, external load balancers or provider behavior.

The upstream lane pool is enabled by default. A client lane is an independent
`reqwest::Client` and connection pool used to distribute in-flight attempts; it
is not a promise that exactly one physical TCP or HTTP/2 connection exists.
Each lane and request-scoped proxy client retains at most one idle connection
per host; the legacy cached proxy clients keep their existing idle-pool
configuration.
Only relay plans with a positive account ID and `account_type=apikey` enter this
pool. OAuth plans, plans from older control planes without `account_type`,
WebSocket sessions, and requests without an account ID retain the legacy HTTP
client path. While the lane feature is enabled, proxied legacy HTTP plans still
use the bounded cached/transient proxy-client lifecycle; they do not enter a
lane or change account selection.
Constructing a client lane does not establish a network connection. These
settings add no new prewarm request: the first real request still establishes
the connection lazily. Existing dynamic and explicit keep-warm work remains on
the legacy client and does not create, expand, target, or refresh the
business-idle lifetime of an adaptive lane. Pool-key or total-lane exhaustion
uses the legacy client path instead of rejecting an otherwise valid request.
New groups start with up to 64 unknown-protocol lanes. After any lane confirms
HTTP/2, sustained aggregate header pressure expands that group in batches of up
to 32 lanes, capped at 256 per group and 65,536 lanes per Edge process. HTTP/1
groups stop expanding and return to the legacy client path. Expansion happens in
the background, so the 10ms pressure sample and 50ms cooldown do not add to
request TTFT.
After an origin is observed using HTTP/1.x, later requests for that pool key
also return to the legacy reqwest client. If both the lane registry and the
legacy proxy-client registry are full, request-scoped proxy clients are bounded
by `SUB2API_EDGE_TRANSIENT_PROXY_MAX_ACTIVE`; excess work waits on that permit
and remains cancellable by downstream disconnect instead of creating unbounded
clients or file descriptors. The existing ingress permit bounds the waiter
count. Internal capacity alone does not return a new overload response or
switch accounts; invalid proxy configuration retains the legacy error path.
`sub2api_edge_upstream_lane_expansion_delay_seconds` measures the adaptive
pressure/cooldown timer used by the background lane-expansion scheduler. It
does not block the business request and does not expose reqwest/hyper's
internal HTTP/2 stream checkout time; use it together with
`lane_awaiting_headers`, `lane_pools_under_pressure`,
`lane_pools_at_cap_pressure`, and upstream header latency when diagnosing a
long tail. `lane_pools_at_cap_pressure` requires sustained high-water pressure
at the protocol-specific or process-wide lane cap; it does not change the
separate soft-cap meaning of the overflow counters.

`AUTOSCALE_UPSTREAM_LANE_PRESSURE_ENABLED=true` is the separate deployment
gate for allowing sustained lane pressure to participate in replica scaling.
It is consumed by the single-host autoscaler, not by the edge process, and
is enabled on the first deployment and remains available as an emergency
rollback switch.
The autoscaler's independent error guards default to:

```text
AUTOSCALE_GO_5XX_RATIO_LIMIT=0.20
AUTOSCALE_EDGE_CONNECT_ERROR_RATIO_LIMIT=0.10
AUTOSCALE_EDGE_5XX_RATIO_LIMIT=0.20
AUTOSCALE_EDGE_429_RATIO_LIMIT=0
AUTOSCALE_EDGE_RETRIES_PER_REQUEST_LIMIT=0
AUTOSCALE_ERROR_MIN_SAMPLES=20
```

`AUTOSCALE_EDGE_429_RATIO_LIMIT=0` and
`AUTOSCALE_EDGE_RETRIES_PER_REQUEST_LIMIT=0` are observe-only in the first
deployment. The legacy
`AUTOSCALE_UPSTREAM_ERROR_RATIO` remains supported as the fallback seed for the
Go 5xx limit when `AUTOSCALE_GO_5XX_RATIO_LIMIT` has not been set; it does not
replace the independent Edge connect, 5xx, or 429 guards.
Invalid ratio/minimum-sample values hold the autoscaler at its current replica
count. Edge upstream counters cover HTTP/SSE relay attempts; the retry ratio is
divided by `sub2api_edge_relay_requests`, so WebSocket upgrades do not dilute
that guard. Lane overflow contributes at most one scaling signal and still
requires another independent pressure signal for two consecutive samples.

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

The Linux one-click systemd installer writes the Edge lane values above to
`/opt/sub2api/edge-rs.env` and enables `sub2api-edge-rs.service` automatically.
The autoscaler settings are Docker Compose settings and are not used by the
systemd-only installer.
With `ingress_proxy_enabled=true`, clients can keep using the Go port; Go
forwards eligible hot paths to the local Rust edge and falls back locally if the
edge is unavailable.

Current relay scope is intentionally narrow:

- `/v1/chat/completions` and `/openai/v1/chat/completions`
- `stream=true`
- OpenAI APIKey accounts whose upstream path is already raw
  `/v1/chat/completions`
- `/v1/responses` and `/openai/v1/responses`, for native OpenAI APIKey
  passthrough accounts with Responses API enabled and supported OAuth/ChatGPT
  native Responses streams; OAuth remains on the legacy HTTP client path
- Responses WebSocket upgrades, only when Go resolves the selected account to
  WSv2 passthrough mode and no proxy is required
- no Go-side protocol conversion is required

Everything else falls back to Go, including Responses WebSocket
ctx_pool/shared/dedicated stateful modes, WS proxy accounts, images,
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
