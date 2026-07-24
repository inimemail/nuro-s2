mod http_lane_pool;

use std::{
    collections::HashMap,
    env,
    fmt::Display,
    future::{pending, Future},
    net::SocketAddr,
    pin::Pin,
    sync::{
        atomic::{AtomicBool, AtomicU64, Ordering},
        Arc, Mutex,
    },
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

use async_stream::stream;
use axum::extract::ws::{Message as AxumWsMessage, WebSocket, WebSocketUpgrade};
use axum::{
    body::{to_bytes, Body},
    extract::{Request, State},
    http::{header, HeaderMap, HeaderName, HeaderValue, Method, StatusCode, Uri},
    response::Response,
    routing::any,
    Router,
};
use bytes::Bytes;
use futures_util::{SinkExt, StreamExt};
use http_lane_pool::{
    build_standalone_client, is_capacity_error, is_legacy_fallback_error, HttpLanePool,
    HttpLanePoolConfig, LaneGuard, LaneRequest,
};
use reqwest::{Client, ClientBuilder};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio::net::TcpStream;
use tokio::sync::{mpsc, oneshot, OwnedSemaphorePermit, Semaphore};
use tokio_tungstenite::{
    connect_async_tls_with_config,
    tungstenite::{
        client::IntoClientRequest,
        protocol::{Message as TungsteniteMessage, WebSocketConfig},
    },
    MaybeTlsStream, WebSocketStream,
};
use tracing::{debug, error, info, warn};
use uuid::Uuid;

const EDGE_SECRET_HEADER: &str = "X-Sub2API-Edge-Secret";
const EDGE_FALLBACK_HEADER: &str = "x-sub2api-edge-fallback";
const EDGE_FALLBACK_REASON_HEADER: &str = "x-sub2api-edge-fallback-reason";
const EDGE_PREPARE_MS_HEADER: &str = "x-sub2api-edge-prepare-ms";
const EDGE_QUEUE_WAIT_MS_HEADER: &str = "x-sub2api-edge-queue-wait-ms";
const EDGE_RELAY_START_MS_HEADER: &str = "x-sub2api-edge-relay-start-ms";
const EDGE_RETRY_COUNT_HEADER: &str = "x-sub2api-edge-retry-count";
const MAX_BODY_BYTES: usize = 512 * 1024 * 1024;
const SSE_STRING_INITIAL_CAPACITY: usize = 8192;
const SSE_STRING_IDLE_MAX_CAPACITY: usize = 64 * 1024;
const SETTLEMENT_RETRY_CONCURRENCY: usize = 32;
const PAYLOAD_COMMIT_CONCURRENCY: usize = 64;
const PAYLOAD_COMMIT_OVERFLOW_CONCURRENCY: usize = 64;
const PAYLOAD_COMMIT_QUEUE_SIZE: usize = 65_536;
const PAYLOAD_COMMIT_MAX_ATTEMPTS: usize = 5;
// Keep settlement delivery alive across a longer control-plane interruption so
// delayed usage/account-health callbacks still have a chance to recover.
const SETTLEMENT_RETRY_MAX_ATTEMPTS: usize = 70;
const SETTLEMENT_RETRY_INITIAL_DELAY: Duration = Duration::from_millis(50);
const SETTLEMENT_RETRY_MAX_DELAY: Duration = Duration::from_secs(30);
const EDGE_RECOVERY_MAX_DELAY: Duration = Duration::from_secs(30);

#[derive(Clone)]
struct AppState {
    cfg: Arc<EdgeConfig>,
    edge_instance_id: Arc<String>,
    client: Client,
    clients_by_proxy: Arc<Mutex<HashMap<String, Arc<ProxyClientEntry>>>>,
    http_lane_pool: Arc<HttpLanePool>,
    transient_proxy_permits: Arc<Semaphore>,
    relay_domains: Arc<Mutex<HashMap<String, Arc<Semaphore>>>>,
    relay_queue_last_used: Arc<Mutex<HashMap<String, Instant>>>,
    warm_keys: Arc<Mutex<HashMap<DynamicWarmKey, WarmKeyState>>>,
    ws_idle: Arc<tokio::sync::Mutex<HashMap<String, Vec<WsIdleConn>>>>,
    ws_idle_last_used: Arc<tokio::sync::Mutex<HashMap<String, Instant>>>,
    pools: Arc<BufferPools>,
    settlement_retry_tx: mpsc::Sender<SettlementRetryJob>,
    payload_commit_tx: mpsc::Sender<CommitRequest>,
    payload_commit_overflow: Arc<Semaphore>,
    relay_tx: mpsc::Sender<RelayJob>,
    relay_queue_bytes: Arc<Semaphore>,
    ingress_permits: Arc<Semaphore>,
    ingress_body_bytes: Arc<Semaphore>,
    metrics: Arc<EdgeMetrics>,
    draining: Arc<AtomicBool>,
}

impl AppState {
    fn metrics_is_draining(&self) -> bool {
        self.draining.load(Ordering::Acquire)
    }
}

#[derive(Default)]
struct EdgeMetrics {
    active_requests: AtomicU64,
    active_streams: AtomicU64,
    accepted_requests: AtomicU64,
    rejected_requests: AtomicU64,
    relay_requests: AtomicU64,
    fallback_requests: AtomicU64,
    drain_rejections: AtomicU64,
    prepare_failures: AtomicU64,
    active_relay_workers: AtomicU64,
    active_settlement_workers: AtomicU64,
    active_payload_commit_workers: AtomicU64,
    lease_renew_failures: AtomicU64,
    upstream_attempts: AtomicU64,
    upstream_connect_errors: AtomicU64,
    upstream_responses: AtomicU64,
    upstream_5xx: AtomicU64,
    upstream_429: AtomicU64,
    upstream_http1_responses: AtomicU64,
    upstream_http2_responses: AtomicU64,
    retry_attempts: AtomicU64,
    transient_proxy_active: AtomicU64,
    transient_proxy_waiters: AtomicU64,
    transient_proxy_total: AtomicU64,
    transient_proxy_wait_micros: AtomicU64,
    transient_proxy_wait_count: AtomicU64,
}

struct ActiveRequestGuard {
    metrics: Arc<EdgeMetrics>,
}

struct ActiveStreamGuard {
    metrics: Arc<EdgeMetrics>,
}

struct ActiveRelayWorkerGuard {
    metrics: Arc<EdgeMetrics>,
}

struct ActiveCallbackWorkerGuard {
    metrics: Arc<EdgeMetrics>,
    payload_commit: bool,
}

struct TransientProxyWaitGuard {
    metrics: Arc<EdgeMetrics>,
    started_at: Instant,
}

/// A lease on a cached proxy client. The map entry is only evicted after all
/// leases have been released, so a long-lived SSE cannot leave an orphaned
/// Client (and its sockets) behind the configured proxy-client cap.
struct ProxyClientEntry {
    client: Client,
    active: AtomicU64,
    last_used: Mutex<Instant>,
}

struct ProxyClientLease {
    entry: Arc<ProxyClientEntry>,
}

struct ProxyClientSelection {
    client: Client,
    lease: Option<ProxyClientLease>,
}

struct TransientProxyGuard {
    permit: Option<OwnedSemaphorePermit>,
    metrics: Arc<EdgeMetrics>,
}

enum UpstreamClientGuard {
    Legacy,
    LegacyProxy(Option<ProxyClientLease>),
    Lane(LaneGuard),
    Transient(TransientProxyGuard),
}

struct SelectedUpstreamClient {
    client: Client,
    guard: UpstreamClientGuard,
}

impl Drop for ActiveStreamGuard {
    fn drop(&mut self) {
        self.metrics.active_streams.fetch_sub(1, Ordering::Relaxed);
    }
}

impl Drop for ActiveRequestGuard {
    fn drop(&mut self) {
        self.metrics.active_requests.fetch_sub(1, Ordering::Relaxed);
    }
}

impl Drop for ActiveRelayWorkerGuard {
    fn drop(&mut self) {
        self.metrics
            .active_relay_workers
            .fetch_sub(1, Ordering::Relaxed);
    }
}

impl Drop for ActiveCallbackWorkerGuard {
    fn drop(&mut self) {
        let counter = if self.payload_commit {
            &self.metrics.active_payload_commit_workers
        } else {
            &self.metrics.active_settlement_workers
        };
        counter.fetch_sub(1, Ordering::Relaxed);
    }
}

impl Drop for TransientProxyWaitGuard {
    fn drop(&mut self) {
        self.metrics
            .transient_proxy_waiters
            .fetch_sub(1, Ordering::Relaxed);
        self.metrics.transient_proxy_wait_micros.fetch_add(
            self.started_at.elapsed().as_micros().min(u64::MAX as u128) as u64,
            Ordering::Relaxed,
        );
        self.metrics
            .transient_proxy_wait_count
            .fetch_add(1, Ordering::Relaxed);
    }
}

impl ProxyClientEntry {
    fn touch(&self) {
        if let Ok(mut last_used) = self.last_used.lock() {
            *last_used = Instant::now();
        }
    }
}

impl ProxyClientLease {
    fn new(entry: Arc<ProxyClientEntry>) -> Self {
        entry.active.fetch_add(1, Ordering::Relaxed);
        entry.touch();
        Self { entry }
    }
}

impl ProxyClientSelection {
    fn leased(entry: Arc<ProxyClientEntry>) -> Self {
        let lease = ProxyClientLease::new(Arc::clone(&entry));
        Self {
            client: entry.client.clone(),
            lease: Some(lease),
        }
    }

    fn legacy(entry: &Arc<ProxyClientEntry>) -> Self {
        // The pre-lane proxy cache only refreshed its lookup timestamp. The
        // returned Client clone kept an in-flight request alive independently
        // if the cache entry was reaped later.
        entry.touch();
        Self {
            client: entry.client.clone(),
            lease: None,
        }
    }
}

impl Drop for ProxyClientLease {
    fn drop(&mut self) {
        // Publish the fresh idle timestamp before the last lease exposes an
        // active count of zero. Otherwise the reaper could observe zero with
        // the old timestamp and evict a client at the exact request boundary.
        self.entry.touch();
        self.entry.active.fetch_sub(1, Ordering::AcqRel);
    }
}

impl TransientProxyGuard {
    fn new(permit: OwnedSemaphorePermit, metrics: Arc<EdgeMetrics>) -> Self {
        metrics
            .transient_proxy_active
            .fetch_add(1, Ordering::Relaxed);
        metrics
            .transient_proxy_total
            .fetch_add(1, Ordering::Relaxed);
        Self {
            permit: Some(permit),
            metrics,
        }
    }

    fn release(&mut self) {
        if self.permit.take().is_some() {
            self.metrics
                .transient_proxy_active
                .fetch_sub(1, Ordering::Relaxed);
        }
    }
}

impl Drop for TransientProxyGuard {
    fn drop(&mut self) {
        self.release();
    }
}

async fn acquire_transient_proxy_guard(
    metrics: Arc<EdgeMetrics>,
    permits: Arc<Semaphore>,
) -> anyhow::Result<TransientProxyGuard> {
    let wait_guard = metrics.begin_transient_proxy_wait();
    let permit = permits
        .acquire_owned()
        .await
        .map_err(|_| anyhow::anyhow!("transient proxy client capacity closed"))?;
    drop(wait_guard);
    Ok(TransientProxyGuard::new(permit, metrics))
}

impl UpstreamClientGuard {
    fn mark_headers(&mut self, version: http::Version) {
        if let Self::Lane(guard) = self {
            guard.mark_headers(Some(version));
        }
    }

    fn mark_stream_open(&mut self) {
        if let Self::Lane(guard) = self {
            guard.mark_stream_open();
        }
    }

    fn release(&mut self) {
        match self {
            Self::Lane(guard) => guard.release(),
            Self::Transient(guard) => guard.release(),
            Self::Legacy | Self::LegacyProxy(None) => {}
            Self::LegacyProxy(lease) => {
                lease.take();
            }
        }
    }
}

impl EdgeMetrics {
    fn begin_request(self: &Arc<Self>) -> ActiveRequestGuard {
        self.active_requests.fetch_add(1, Ordering::Relaxed);
        self.accepted_requests.fetch_add(1, Ordering::Relaxed);
        ActiveRequestGuard {
            metrics: Arc::clone(self),
        }
    }

    fn begin_stream(self: &Arc<Self>) -> ActiveStreamGuard {
        self.active_streams.fetch_add(1, Ordering::Relaxed);
        ActiveStreamGuard {
            metrics: Arc::clone(self),
        }
    }

    fn begin_relay_work(self: &Arc<Self>) -> ActiveRelayWorkerGuard {
        self.active_relay_workers.fetch_add(1, Ordering::Relaxed);
        ActiveRelayWorkerGuard {
            metrics: Arc::clone(self),
        }
    }

    fn begin_callback_work(self: &Arc<Self>, payload_commit: bool) -> ActiveCallbackWorkerGuard {
        let counter = if payload_commit {
            &self.active_payload_commit_workers
        } else {
            &self.active_settlement_workers
        };
        counter.fetch_add(1, Ordering::Relaxed);
        ActiveCallbackWorkerGuard {
            metrics: Arc::clone(self),
            payload_commit,
        }
    }

    fn begin_transient_proxy_wait(self: &Arc<Self>) -> TransientProxyWaitGuard {
        self.transient_proxy_waiters.fetch_add(1, Ordering::Relaxed);
        TransientProxyWaitGuard {
            metrics: Arc::clone(self),
            started_at: Instant::now(),
        }
    }

    fn record_upstream_attempt(&self, retry_count: i64) {
        self.upstream_attempts.fetch_add(1, Ordering::Relaxed);
        if retry_count > 0 {
            self.retry_attempts.fetch_add(1, Ordering::Relaxed);
        }
    }

    fn record_upstream_send_error(&self, error: &reqwest::Error) {
        if error.is_connect() {
            self.upstream_connect_errors.fetch_add(1, Ordering::Relaxed);
        }
    }

    fn record_upstream_response(&self, status: StatusCode, version: http::Version) {
        self.upstream_responses.fetch_add(1, Ordering::Relaxed);
        if status.is_server_error() {
            self.upstream_5xx.fetch_add(1, Ordering::Relaxed);
        }
        if status == StatusCode::TOO_MANY_REQUESTS {
            self.upstream_429.fetch_add(1, Ordering::Relaxed);
        }
        if version == http::Version::HTTP_2 {
            self.upstream_http2_responses
                .fetch_add(1, Ordering::Relaxed);
        } else if matches!(
            version,
            http::Version::HTTP_09 | http::Version::HTTP_10 | http::Version::HTTP_11
        ) {
            self.upstream_http1_responses
                .fetch_add(1, Ordering::Relaxed);
        }
    }

    fn render(&self, state: &AppState) -> String {
        let active = self.active_requests.load(Ordering::Relaxed);
        let active_streams = self.active_streams.load(Ordering::Relaxed);
        let (relay_domains, relay_domain_permits_used) = state
            .relay_domains
            .lock()
            .map(|domains| {
                let used = domains
                    .values()
                    .map(|domain| {
                        state
                            .cfg
                            .per_account_workers
                            .max(1)
                            .saturating_sub(domain.available_permits())
                    })
                    .sum::<usize>();
                (domains.len(), used)
            })
            .unwrap_or_default();
        let proxy_clients = state
            .clients_by_proxy
            .lock()
            .map(|clients| clients.len())
            .unwrap_or_default();
        let warm_keys = state
            .warm_keys
            .lock()
            .map(|keys| keys.len())
            .unwrap_or_default();
        let ws_idle_keys = state
            .ws_idle
            .try_lock()
            .map(|pools| pools.len())
            .unwrap_or_default();
        let relay_queue_depth = state
            .relay_tx
            .max_capacity()
            .saturating_sub(state.relay_tx.capacity());
        let relay_queue_bytes = state
            .cfg
            .queue_max_bytes
            .saturating_sub(state.relay_queue_bytes.available_permits());
        let ingress_limit = state
            .cfg
            .global_workers
            .max(1)
            .saturating_add(state.cfg.queue_buffer_size.max(128));
        let ingress_used = ingress_limit.saturating_sub(state.ingress_permits.available_permits());
        let settlement_retry_depth = state
            .settlement_retry_tx
            .max_capacity()
            .saturating_sub(state.settlement_retry_tx.capacity());
        let payload_commit_depth = state
            .payload_commit_tx
            .max_capacity()
            .saturating_sub(state.payload_commit_tx.capacity());
        let open_fds = linux_open_fd_count();
        let max_fds = linux_max_open_files();
        let lane = state.http_lane_pool.snapshot();
        let mut output = format!(
            "# TYPE sub2api_edge_active_requests gauge\nsub2api_edge_active_requests {active}\n\
             # TYPE sub2api_edge_active_streams gauge\nsub2api_edge_active_streams {active_streams}\n\
             # TYPE sub2api_edge_accepted_requests counter\nsub2api_edge_accepted_requests {}\n\
             # TYPE sub2api_edge_rejected_requests counter\nsub2api_edge_rejected_requests {}\n\
             # TYPE sub2api_edge_drain_rejections counter\nsub2api_edge_drain_rejections {}\n\
             # TYPE sub2api_edge_prepare_failures counter\nsub2api_edge_prepare_failures {}\n\
             # TYPE sub2api_edge_relay_requests counter\nsub2api_edge_relay_requests {}\n\
             # TYPE sub2api_edge_fallback_requests counter\nsub2api_edge_fallback_requests {}\n\
             # TYPE sub2api_edge_relay_domains gauge\nsub2api_edge_relay_domains {relay_domains}\n\
             # TYPE sub2api_edge_relay_domain_permits_used gauge\nsub2api_edge_relay_domain_permits_used {relay_domain_permits_used}\n\
             # TYPE sub2api_edge_relay_workers_active gauge\nsub2api_edge_relay_workers_active {}\n\
             # TYPE sub2api_edge_relay_queue_depth gauge\nsub2api_edge_relay_queue_depth {relay_queue_depth}\n\
             # TYPE sub2api_edge_relay_queue_bytes gauge\nsub2api_edge_relay_queue_bytes {relay_queue_bytes}\n\
             # TYPE sub2api_edge_ingress_permits_used gauge\nsub2api_edge_ingress_permits_used {ingress_used}\n\
             # TYPE sub2api_edge_ingress_permits_limit gauge\nsub2api_edge_ingress_permits_limit {ingress_limit}\n\
             # TYPE sub2api_edge_settlement_retry_queue_depth gauge\nsub2api_edge_settlement_retry_queue_depth {settlement_retry_depth}\n\
             # TYPE sub2api_edge_settlement_workers_active gauge\nsub2api_edge_settlement_workers_active {}\n\
             # TYPE sub2api_edge_payload_commit_queue_depth gauge\nsub2api_edge_payload_commit_queue_depth {payload_commit_depth}\n\
             # TYPE sub2api_edge_payload_commit_workers_active gauge\nsub2api_edge_payload_commit_workers_active {}\n\
             # TYPE sub2api_edge_lease_renew_failures_total counter\nsub2api_edge_lease_renew_failures_total {}\n\
             # TYPE sub2api_edge_proxy_clients gauge\nsub2api_edge_proxy_clients {proxy_clients}\n\
             # TYPE sub2api_edge_dynamic_warm_keys gauge\nsub2api_edge_dynamic_warm_keys {warm_keys}\n\
             # TYPE sub2api_edge_open_fds gauge\nsub2api_edge_open_fds {open_fds}\n\
             # TYPE sub2api_edge_max_fds gauge\nsub2api_edge_max_fds {max_fds}\n\
             # TYPE sub2api_edge_ws_idle_keys gauge\nsub2api_edge_ws_idle_keys {ws_idle_keys}\n\
             # TYPE sub2api_edge_draining gauge\nsub2api_edge_draining {}\n\
             # HELP sub2api_edge_node_info Stable node identity and capacity configuration.\n\
             sub2api_edge_node_info{{node_id=\"{}\",instance_id=\"{}\",global_workers=\"{}\"}} 1\n",
            self.accepted_requests.load(Ordering::Relaxed),
            self.rejected_requests.load(Ordering::Relaxed),
            self.drain_rejections.load(Ordering::Relaxed),
            self.prepare_failures.load(Ordering::Relaxed),
            self.relay_requests.load(Ordering::Relaxed),
            self.fallback_requests.load(Ordering::Relaxed),
            self.active_relay_workers.load(Ordering::Relaxed),
            self.active_settlement_workers.load(Ordering::Relaxed),
            self.active_payload_commit_workers.load(Ordering::Relaxed),
            self.lease_renew_failures.load(Ordering::Relaxed),
            u64::from(state.metrics_is_draining()),
            prometheus_label(state.cfg.edge_node_id.as_deref().unwrap_or("unknown")),
            prometheus_label(state.edge_instance_id.as_str()),
            state.cfg.global_workers,
        );
        output.push_str(&format!(
            "# TYPE sub2api_edge_upstream_attempts_total counter\nsub2api_edge_upstream_attempts_total {}\n\
             # TYPE sub2api_edge_upstream_connect_errors_total counter\nsub2api_edge_upstream_connect_errors_total {}\n\
             # TYPE sub2api_edge_upstream_responses_total counter\nsub2api_edge_upstream_responses_total {}\n\
             # TYPE sub2api_edge_upstream_5xx_total counter\nsub2api_edge_upstream_5xx_total {}\n\
             # TYPE sub2api_edge_upstream_429_total counter\nsub2api_edge_upstream_429_total {}\n\
             # TYPE sub2api_edge_retry_attempts_total counter\nsub2api_edge_retry_attempts_total {}\n\
             # TYPE sub2api_edge_upstream_responses_by_http_version_total counter\nsub2api_edge_upstream_responses_by_http_version_total{{version=\"h1\"}} {}\n\
             sub2api_edge_upstream_responses_by_http_version_total{{version=\"h2\"}} {}\n\
             # TYPE sub2api_edge_upstream_lane_pool_enabled gauge\nsub2api_edge_upstream_lane_pool_enabled {}\n\
             # TYPE sub2api_edge_upstream_lane_pools gauge\nsub2api_edge_upstream_lane_pools {}\n\
             # TYPE sub2api_edge_upstream_client_lanes gauge\nsub2api_edge_upstream_client_lanes {}\n\
             # TYPE sub2api_edge_upstream_lane_inflight gauge\nsub2api_edge_upstream_lane_inflight {}\n\
             # TYPE sub2api_edge_upstream_lane_awaiting_headers gauge\nsub2api_edge_upstream_lane_awaiting_headers {}\n\
             # TYPE sub2api_edge_upstream_lane_open_streams gauge\nsub2api_edge_upstream_lane_open_streams {}\n\
             # TYPE sub2api_edge_upstream_lane_pools_under_pressure gauge\nsub2api_edge_upstream_lane_pools_under_pressure {}\n\
             # HELP sub2api_edge_upstream_lane_pools_at_cap_pressure Pools at their protocol/global lane cap with sustained high-water response-header pressure.\n\
             # TYPE sub2api_edge_upstream_lane_pools_at_cap_pressure gauge\nsub2api_edge_upstream_lane_pools_at_cap_pressure {}\n\
             # TYPE sub2api_edge_upstream_lane_pools_overflowing gauge\nsub2api_edge_upstream_lane_pools_overflowing {}\n\
             # TYPE sub2api_edge_upstream_lane_overflow_active gauge\nsub2api_edge_upstream_lane_overflow_active {}\n\
             # TYPE sub2api_edge_upstream_lane_overflow_total counter\nsub2api_edge_upstream_lane_overflow_total {}\n\
             # TYPE sub2api_edge_upstream_lane_expansions_total counter\nsub2api_edge_upstream_lane_expansions_total {}\n\
             # TYPE sub2api_edge_upstream_lane_shrinks_total counter\nsub2api_edge_upstream_lane_shrinks_total {}\n\
             # TYPE sub2api_edge_upstream_lane_expansion_failures_total counter\nsub2api_edge_upstream_lane_expansion_failures_total {}\n\
             # TYPE sub2api_edge_upstream_lane_capacity_exhaustions_total counter\nsub2api_edge_upstream_lane_capacity_exhaustions_total {}\n\
             # TYPE sub2api_edge_upstream_lane_legacy_fallbacks_total counter\nsub2api_edge_upstream_lane_legacy_fallbacks_total {}\n\
             # HELP sub2api_edge_upstream_lane_expansion_waiters Background lane-expansion timers currently pending; business requests do not wait on these timers.\n\
             # TYPE sub2api_edge_upstream_lane_expansion_waiters gauge\nsub2api_edge_upstream_lane_expansion_waiters {}\n\
             # HELP sub2api_edge_upstream_lane_expansion_delay_seconds Background pressure/cooldown timer duration; not business-request or HTTP/2 checkout wait.\n\
             # TYPE sub2api_edge_upstream_lane_expansion_delay_seconds summary\nsub2api_edge_upstream_lane_expansion_delay_seconds_sum {:.6}\n\
             sub2api_edge_upstream_lane_expansion_delay_seconds_count {}\n\
             # TYPE sub2api_edge_upstream_lane_protocol_lanes gauge\nsub2api_edge_upstream_lane_protocol_lanes{{version=\"unknown\"}} {}\n\
             sub2api_edge_upstream_lane_protocol_lanes{{version=\"h1\"}} {}\n\
             sub2api_edge_upstream_lane_protocol_lanes{{version=\"h2\"}} {}\n\
             # TYPE sub2api_edge_transient_proxy_active gauge\nsub2api_edge_transient_proxy_active {}\n\
             # TYPE sub2api_edge_transient_proxy_waiters gauge\nsub2api_edge_transient_proxy_waiters {}\n\
             # TYPE sub2api_edge_transient_proxy_limit gauge\nsub2api_edge_transient_proxy_limit {}\n\
             # TYPE sub2api_edge_transient_proxy_total counter\nsub2api_edge_transient_proxy_total {}\n\
             # TYPE sub2api_edge_transient_proxy_wait_seconds summary\nsub2api_edge_transient_proxy_wait_seconds_sum {:.6}\n\
             sub2api_edge_transient_proxy_wait_seconds_count {}\n",
            self.upstream_attempts.load(Ordering::Relaxed),
            self.upstream_connect_errors.load(Ordering::Relaxed),
            self.upstream_responses.load(Ordering::Relaxed),
            self.upstream_5xx.load(Ordering::Relaxed),
            self.upstream_429.load(Ordering::Relaxed),
            self.retry_attempts.load(Ordering::Relaxed),
            self.upstream_http1_responses.load(Ordering::Relaxed),
            self.upstream_http2_responses.load(Ordering::Relaxed),
            u64::from(lane.enabled),
            lane.keys,
            lane.lanes,
            lane.inflight,
            lane.awaiting_headers,
            lane.open_streams,
            lane.pools_under_pressure,
            lane.pools_at_cap_pressure,
            lane.pools_overflowing,
            lane.overflow_active,
            lane.overflow_total,
            lane.expansions_total,
            lane.shrinks_total,
            lane.expansion_failures_total,
            lane.capacity_exhaustions_total,
            lane.legacy_fallbacks_total,
            lane.expansion_waiters,
            lane.expansion_delay_micros_total as f64 / 1_000_000.0,
            lane.expansion_delay_count,
            lane.unknown_lanes,
            lane.http1_lanes,
            lane.http2_lanes,
            self.transient_proxy_active.load(Ordering::Relaxed),
            self.transient_proxy_waiters.load(Ordering::Relaxed),
            state.cfg.transient_proxy_max_active,
            self.transient_proxy_total.load(Ordering::Relaxed),
            self.transient_proxy_wait_micros.load(Ordering::Relaxed) as f64 / 1_000_000.0,
            self.transient_proxy_wait_count.load(Ordering::Relaxed),
        ));
        output
    }
}

fn linux_open_fd_count() -> usize {
    std::fs::read_dir("/proc/self/fd")
        .map(|entries| entries.count())
        .unwrap_or_default()
}

fn linux_max_open_files() -> usize {
    let Ok(limits) = std::fs::read_to_string("/proc/self/limits") else {
        return 0;
    };
    limits
        .lines()
        .find(|line| line.starts_with("Max open files"))
        .and_then(|line| line.split_whitespace().nth(3))
        .and_then(|value| value.parse().ok())
        .unwrap_or_default()
}

fn prometheus_label(value: &str) -> String {
    value
        .replace('\\', "\\\\")
        .replace('"', "\\\"")
        .replace('\n', "\\n")
}

#[derive(Clone, Debug)]
struct EdgeConfig {
    listen_addr: SocketAddr,
    go_base_url: String,
    control_base_url: String,
    internal_secret: String,
    edge_node_id: Option<String>,
    prepare_timeout_ms: u64,
    complete_timeout_ms: u64,
    initial_pool_size: usize,
    queue_buffer_size: usize,
    queue_max_bytes: usize,
    max_header_bytes: usize,
    ingress_body_max_bytes: usize,
    global_workers: usize,
    per_account_workers: usize,
    max_relay_domains: usize,
    relay_domain_idle_secs: u64,
    max_proxy_clients: usize,
    proxy_client_idle_secs: u64,
    max_idle_conns_per_account: usize,
    transient_proxy_max_active: usize,
    queue_wait_budget_ms: u64,
    large_payload_passthrough: bool,
    large_payload_threshold_bytes: usize,
    ws_idle_per_key: usize,
    max_ws_idle_keys: usize,
    ws_idle_ttl_secs: u64,
    drain_timeout_secs: u64,
    upstream_warm_url: Option<String>,
    upstream_warm_interval_secs: u64,
    upstream_dynamic_warm_active_secs: u64,
    max_dynamic_warm_keys: usize,
}

#[derive(Debug, Serialize)]
struct PrepareRequest {
    edge_request_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    edge_node_id: Option<String>,
    edge_instance_id: String,
    method: String,
    path: String,
    raw_query: Option<String>,
    headers: HashMap<String, String>,
    body: Value,
    body_raw_base64: Option<String>,
    client_ip: Option<String>,
    stream: Option<bool>,
}

#[derive(Deserialize)]
struct StreamOnlyRequest {
    stream: Option<bool>,
}

#[derive(Clone, Debug, Deserialize)]
struct EdgePlan {
    action: String,
    reason: Option<String>,
    edge_request_id: String,
    lease_id: Option<String>,
    lease_ttl_ms: Option<u64>,
    account_id: Option<i64>,
    account_type: Option<String>,
    transport: Option<String>,
    response_dialect: Option<String>,
    upstream_url: Option<String>,
    headers: Option<HashMap<String, String>>,
    body: Option<Value>,
    body_raw_base64: Option<String>,
    proxy_url: Option<String>,
    low_latency_mode: Option<String>,
    lane: Option<String>,
    /// Account-level SSE comment preflush. Plans produced by older control
    /// planes omit this field, which must retain the legacy wire behavior.
    #[serde(default)]
    sse_comment_preflush: bool,
    /// Forward Responses preamble events immediately instead of buffering them
    /// until the first non-preamble output event.
    #[serde(default = "default_preamble_flush")]
    preamble_flush: bool,
    #[serde(default)]
    safe_token_placeholder: bool,
    first_token_timeout_placeholder_ms: Option<u64>,
    prompt_cache_creation_optimization_mode: Option<String>,
    prompt_cache_creation_optimization_model: Option<String>,
    #[serde(default)]
    prompt_cache_creation_optimization_applied: bool,
    // Older Go control planes do not include reasoning policy fields.
    #[serde(default)]
    max_reasoning_effort: Option<String>,
    #[serde(default)]
    reasoning_effort_mappings: Vec<ReasoningEffortMapping>,
}

#[derive(Clone, Debug, Deserialize)]
struct ReasoningEffortMapping {
    from: String,
    to: String,
}

fn default_preamble_flush() -> bool {
    // Before this field existed, edge-rs forwarded Responses preamble events
    // immediately. Keep that wire behavior for plans from an older control
    // plane; new Go plans always serialize an explicit bool.
    true
}

#[derive(Debug, Deserialize)]
struct EdgePlanEnvelope {
    plan: Option<EdgePlan>,
}

type WsStream = WebSocketStream<MaybeTlsStream<TcpStream>>;

struct WsIdleConn {
    socket: WsStream,
    request_id: Option<String>,
}

struct RelayJob {
    state: AppState,
    plan: EdgePlan,
    request_body: Vec<u8>,
    started_at: Instant,
    enqueued_at: Instant,
    timing: EdgeTiming,
    timing_shared: Option<Arc<Mutex<EdgeTiming>>>,
    response_tx: oneshot::Sender<anyhow::Result<Response>>,
    _domain_permit: OwnedSemaphorePermit,
    _queue_bytes_permit: OwnedSemaphorePermit,
    ingress_permit: OwnedSemaphorePermit,
}

#[derive(Clone, Debug)]
struct WarmKeyState {
    proxy_url: Option<String>,
    warm_url: String,
    last_seen: Instant,
    failures: u32,
    warming: bool,
}

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
struct DynamicWarmKey {
    proxy: String,
    warm_url: String,
}

#[derive(Clone, Debug, Default)]
struct EdgeTiming {
    prepare_ms: Option<i64>,
    queue_wait_ms: Option<i64>,
    relay_start_ms: Option<i64>,
    fallback_reason: Option<String>,
    retry_count: i64,
}

struct RelayAttemptContext {
    started_at: Instant,
    timing: EdgeTiming,
    timing_shared: Option<Arc<Mutex<EdgeTiming>>>,
    relay_attempted_marker: Option<Arc<AtomicBool>>,
    ingress_permit: Option<OwnedSemaphorePermit>,
}

fn update_edge_timing(
    timing_shared: Option<&Arc<Mutex<EdgeTiming>>>,
    update: impl FnOnce(&mut EdgeTiming),
) {
    let Some(timing_shared) = timing_shared else {
        return;
    };
    if let Ok(mut timing) = timing_shared.lock() {
        update(&mut timing);
    }
}

fn edge_timing_snapshot(timing_shared: &Arc<Mutex<EdgeTiming>>) -> EdgeTiming {
    timing_shared
        .lock()
        .map(|timing| timing.clone())
        .unwrap_or_default()
}

fn relay_error_fallback_reason(err: &anyhow::Error) -> &'static str {
    let message = err.to_string();
    if message.contains("retry_failure_already_recorded") {
        return "retry_failure_already_recorded";
    }
    if message.contains("queue wait budget") || message.contains("edge_queue_wait_timeout") {
        return "queue_wait_budget_fallback_go";
    }
    if message.contains("edge relay queue full") {
        return "edge_relay_queue_full";
    }
    if message.contains("edge transient proxy client build failed") {
        return "edge_transient_proxy_client_build_failed";
    }
    if message.contains("invalid upstream proxy")
        || message.contains("could not build upstream HTTP client")
    {
        return "edge_upstream_client_build_failed";
    }
    "relay_error_before_commit"
}

fn relay_error_is_local_capacity(err: &anyhow::Error) -> bool {
    let message = err.to_string();
    [
        "edge relay queue full",
        "edge relay queue byte budget exhausted",
        "edge relay domain capacity exhausted",
        "edge proxy client capacity exhausted",
        "queue wait budget",
        "edge_queue_wait_timeout",
    ]
    .iter()
    .any(|marker| message.contains(marker))
}

#[derive(Default)]
struct BufferPools {
    sse_strings: Mutex<Vec<String>>,
    max_sse_strings: usize,
}

impl BufferPools {
    fn prewarmed(initial_pool_size: usize) -> Self {
        let mut strings = Vec::with_capacity(initial_pool_size);
        for _ in 0..initial_pool_size {
            strings.push(String::with_capacity(SSE_STRING_INITIAL_CAPACITY));
        }
        Self {
            sse_strings: Mutex::new(strings),
            max_sse_strings: initial_pool_size.max(1),
        }
    }

    fn take_sse_string(&self) -> String {
        self.sse_strings
            .lock()
            .ok()
            .and_then(|mut pool| pool.pop())
            .unwrap_or_else(|| String::with_capacity(SSE_STRING_INITIAL_CAPACITY))
    }

    fn recycle_sse_string(&self, mut value: String) {
        if value.capacity() > SSE_STRING_IDLE_MAX_CAPACITY {
            return;
        }
        value.clear();
        if let Ok(mut pool) = self.sse_strings.lock() {
            if pool.len() < self.max_sse_strings {
                pool.push(value);
            }
        }
    }
}

#[derive(Debug, Deserialize)]
struct RetryDecision {
    action: String,
    reason: Option<String>,
    plan: Option<EdgePlan>,
    #[serde(default)]
    failure_recorded: bool,
    status_code: Option<u16>,
    error_type: Option<String>,
    error_message: Option<String>,
}

#[derive(Debug, Serialize)]
struct RetryRequest {
    edge_request_id: String,
    lease_id: Option<String>,
    account_id: Option<i64>,
    upstream_status_code: Option<u16>,
    upstream_request_id: Option<String>,
    error_type: Option<String>,
    error_message: Option<String>,
    request_body: Option<Value>,
    response_body: Option<Value>,
    wrote_client_response: bool,
}

#[derive(Clone, Debug, Serialize)]
struct CommitRequest {
    edge_request_id: String,
    lease_id: Option<String>,
    account_id: Option<i64>,
}

#[derive(Clone, Debug, Serialize)]
struct RenewRequest {
    edge_request_id: String,
    lease_id: String,
    account_id: Option<i64>,
}

#[derive(Clone, Debug, Serialize)]
struct CompleteRequest {
    edge_request_id: String,
    lease_id: Option<String>,
    account_id: Option<i64>,
    success: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    failure_class: Option<String>,
    client_disconnected: bool,
    request_id: Option<String>,
    response_id: Option<String>,
    model: Option<String>,
    upstream_model: Option<String>,
    usage: Usage,
    duration_ms: i64,
    upstream_header_ms: Option<i64>,
    upstream_first_byte_ms: Option<i64>,
    first_token_ms: Option<i64>,
    real_first_token_ms: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    guard_sample_at_unix_ns: Option<i64>,
    first_client_flush_ms: Option<i64>,
    edge_prepare_ms: Option<i64>,
    edge_queue_wait_ms: Option<i64>,
    edge_relay_start_ms: Option<i64>,
    edge_fallback_reason: Option<String>,
    edge_retry_count: i64,
    error_type: Option<String>,
    error_message: Option<String>,
    upstream_status_code: Option<u16>,
    terminal_event_type: Option<String>,
    cyber_blocked: bool,
}

#[derive(Clone, Debug, Default, Serialize)]
struct Usage {
    input_tokens: i64,
    output_tokens: i64,
    #[serde(skip_serializing_if = "is_zero")]
    cache_creation_input_tokens: i64,
    #[serde(skip_serializing_if = "is_zero")]
    cache_read_input_tokens: i64,
}

#[derive(Clone, Debug, Default)]
struct ChatStreamSummary {
    usage: Usage,
    request_id: Option<String>,
    response_id: Option<String>,
    model: Option<String>,
    upstream_model: Option<String>,
    pending: String,
    response_created_pending_boundary: bool,
    failed: bool,
    cyber_blocked: bool,
    terminal_event_type: Option<String>,
    failed_terminal_event_type: Option<String>,
    neutral_terminal_event_type: Option<String>,
    response_dialect: Option<String>,
}

#[derive(Clone, Copy, Debug, Default)]
struct ChatStreamObservation {
    starts_client_output: bool,
    starts_real_output: bool,
    saw_response_created: bool,
    response_created_boundary_offset: Option<usize>,
}

impl ChatStreamObservation {
    fn merge(&mut self, other: ChatStreamObservation) {
        self.starts_client_output |= other.starts_client_output;
        self.starts_real_output |= other.starts_real_output;
        self.saw_response_created |= other.saw_response_created;
        if self.response_created_boundary_offset.is_none() {
            self.response_created_boundary_offset = other.response_created_boundary_offset;
        }
    }
}

#[derive(Clone, Copy, Debug, Default)]
struct LowLatencyPolicy {
    enabled: bool,
    barrier: Option<Duration>,
}

#[derive(Clone, Debug, Serialize)]
struct AbortRequest {
    edge_request_id: String,
    lease_id: Option<String>,
    account_id: Option<i64>,
    reason: String,
    failure_class: String,
    client_disconnected: bool,
    relay_attempted: bool,
    fallback_to_go: bool,
}

#[derive(Clone, Debug)]
enum SettlementRetryJob {
    Complete(Box<CompleteRequest>),
    Abort(AbortRequest),
}

#[derive(Clone, Debug, Serialize)]
struct RecoverRequest {
    edge_node_id: String,
    edge_instance_id: String,
}

#[derive(Debug, Deserialize)]
struct RecoverAck {
    ok: bool,
    #[serde(default)]
    released: usize,
}

impl SettlementRetryJob {
    fn kind(&self) -> &'static str {
        match self {
            Self::Complete(_) => "complete",
            Self::Abort(_) => "abort",
        }
    }

    fn edge_request_id(&self) -> &str {
        match self {
            Self::Complete(request) => &request.edge_request_id,
            Self::Abort(request) => &request.edge_request_id,
        }
    }
}

struct LeaseAbortGuard {
    state: AppState,
    edge_request_id: String,
    lease_id: Option<String>,
    account_id: Option<i64>,
    reason: &'static str,
    client_disconnected: bool,
    relay_attempted: Arc<AtomicBool>,
    done: Arc<AtomicBool>,
}

struct ClientDisconnectCompleteGuard {
    state: AppState,
    started_at: Instant,
    request: Mutex<CompleteRequest>,
    definitive_failure: AtomicBool,
    done: AtomicBool,
}

struct LeaseRenewalGuard {
    stop: Option<oneshot::Sender<()>>,
}

impl LeaseRenewalGuard {
    fn start(state: &AppState, plan: &EdgePlan) -> Option<Self> {
        let lease_id = plan.lease_id.as_deref()?.trim();
        let ttl_ms = plan.lease_ttl_ms?;
        if lease_id.is_empty() || ttl_ms == 0 {
            return None;
        }
        let request = RenewRequest {
            edge_request_id: plan.edge_request_id.clone(),
            lease_id: lease_id.to_string(),
            account_id: plan.account_id,
        };
        let interval = Duration::from_millis((ttl_ms / 3).clamp(250, 30_000));
        let renew_state = state.clone();
        let (stop, mut stopped) = oneshot::channel();
        tokio::spawn(async move {
            loop {
                tokio::select! {
                    _ = tokio::time::sleep(interval) => {
                        if let Err(err) = send_renew_once(&renew_state, &request).await {
                            renew_state.metrics.lease_renew_failures.fetch_add(1, Ordering::Relaxed);
                            warn!("edge lease renew failed edge_request_id={}: {}", request.edge_request_id, safe_edge_error(&err));
                        }
                    }
                    _ = &mut stopped => return,
                }
            }
        });
        Some(Self { stop: Some(stop) })
    }
}

impl Drop for LeaseRenewalGuard {
    fn drop(&mut self) {
        if let Some(stop) = self.stop.take() {
            let _ = stop.send(());
        }
    }
}

impl ClientDisconnectCompleteGuard {
    fn new(state: AppState, started_at: Instant, request: CompleteRequest) -> Self {
        Self {
            state,
            started_at,
            request: Mutex::new(request),
            definitive_failure: AtomicBool::new(false),
            done: AtomicBool::new(false),
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn update_stream_snapshot(
        &self,
        summary: &ChatStreamSummary,
        success: bool,
        error_message: Option<String>,
        upstream_header_ms: Option<i64>,
        upstream_first_byte_ms: Option<i64>,
        first_token_ms: Option<i64>,
        real_first_token_ms: Option<i64>,
        first_client_flush_ms: Option<i64>,
        upstream_status_code: Option<u16>,
        definitive_failure: bool,
    ) {
        let Ok(mut request) = self.request.lock() else {
            return;
        };
        request.success = success;
        request.request_id = summary.request_id.clone();
        request.response_id = summary.response_id.clone();
        request.model = summary.model.clone();
        request.upstream_model = summary.upstream_model.clone();
        request.usage = summary.usage.clone();
        request.duration_ms = self.started_at.elapsed().as_millis() as i64;
        request.upstream_header_ms = upstream_header_ms;
        request.upstream_first_byte_ms = upstream_first_byte_ms;
        request.first_token_ms = first_token_ms;
        request.real_first_token_ms = real_first_token_ms;
        request.first_client_flush_ms = first_client_flush_ms;
        request.error_type = if success {
            None
        } else {
            Some("stream_error".to_string())
        };
        request.error_message = error_message;
        request.upstream_status_code = upstream_status_code;
        request.terminal_event_type = summary.terminal_event_type(None);
        request.cyber_blocked = summary.cyber_blocked;
        request.failure_class =
            classify_stream_failure_with_status(success, false, summary, upstream_status_code);
        if definitive_failure {
            self.definitive_failure.store(true, Ordering::SeqCst);
        }
    }

    fn mark_done(&self) {
        self.done.store(true, Ordering::SeqCst);
    }
}

fn mark_complete_request_client_disconnected(request: &mut CompleteRequest, duration_ms: i64) {
    request.success = false;
    request.client_disconnected = true;
    request.failure_class = Some("client_cancelled".to_string());
    request.duration_ms = duration_ms;
    request.first_token_ms = None;
    request.real_first_token_ms = None;
    request.error_type = Some("client_disconnect".to_string());
    request.error_message = Some("Client disconnected".to_string());
}

#[allow(clippy::too_many_arguments)]
fn pending_stream_complete_request(
    edge_request_id: String,
    lease_id: Option<String>,
    account_id: Option<i64>,
    edge_prepare_ms: Option<i64>,
    edge_queue_wait_ms: Option<i64>,
    edge_relay_start_ms: Option<i64>,
    edge_fallback_reason: Option<String>,
    edge_retry_count: i64,
) -> CompleteRequest {
    CompleteRequest {
        edge_request_id,
        lease_id,
        account_id,
        success: false,
        failure_class: Some("upstream_disconnect".to_string()),
        client_disconnected: false,
        request_id: None,
        response_id: None,
        model: None,
        upstream_model: None,
        usage: Usage::default(),
        duration_ms: 0,
        upstream_header_ms: None,
        upstream_first_byte_ms: None,
        first_token_ms: None,
        real_first_token_ms: None,
        guard_sample_at_unix_ns: None,
        first_client_flush_ms: None,
        edge_prepare_ms,
        edge_queue_wait_ms,
        edge_relay_start_ms,
        edge_fallback_reason,
        edge_retry_count,
        error_type: Some("stream_error".to_string()),
        error_message: None,
        upstream_status_code: None,
        terminal_event_type: None,
        cyber_blocked: false,
    }
}

fn classify_stream_failure(
    success: bool,
    client_disconnected: bool,
    summary: &ChatStreamSummary,
) -> Option<String> {
    if success {
        return None;
    }
    if client_disconnected {
        return Some("client_cancelled".to_string());
    }
    if summary.failed {
        return Some("upstream_error".to_string());
    }
    Some("upstream_disconnect".to_string())
}

fn classify_stream_failure_with_status(
    success: bool,
    client_disconnected: bool,
    summary: &ChatStreamSummary,
    upstream_status_code: Option<u16>,
) -> Option<String> {
    if !success && !client_disconnected && upstream_status_code.is_some_and(|status| status >= 400)
    {
        return Some("upstream_error".to_string());
    }
    classify_stream_failure(success, client_disconnected, summary)
}

impl Drop for ClientDisconnectCompleteGuard {
    fn drop(&mut self) {
        if self.done.load(Ordering::SeqCst) {
            return;
        }
        let Ok(mut request) = self.request.lock().map(|request| request.clone()) else {
            return;
        };
        if self.definitive_failure.load(Ordering::SeqCst) {
            request.success = false;
            request.client_disconnected = false;
            request.duration_ms = self.started_at.elapsed().as_millis() as i64;
            request.first_token_ms = None;
            request.real_first_token_ms = None;
        } else {
            mark_complete_request_client_disconnected(
                &mut request,
                self.started_at.elapsed().as_millis() as i64,
            );
        }
        stamp_complete_guard_sample(&mut request);
        if let Err(err) =
            enqueue_settlement_retry(&self.state, SettlementRetryJob::Complete(Box::new(request)))
        {
            error!(
                "dropped-stream complete callback could not be queued: {}",
                safe_edge_error(&err)
            );
        }
    }
}

impl LeaseAbortGuard {
    fn new(
        state: AppState,
        edge_request_id: String,
        lease_id: Option<String>,
        account_id: Option<i64>,
        reason: &'static str,
        client_disconnected: bool,
    ) -> Self {
        Self {
            state,
            edge_request_id,
            lease_id,
            account_id,
            reason,
            client_disconnected,
            relay_attempted: Arc::new(AtomicBool::new(false)),
            done: Arc::new(AtomicBool::new(false)),
        }
    }

    fn mark_relay_attempted(&self) {
        self.relay_attempted.store(true, Ordering::SeqCst);
    }

    fn relay_attempted_marker(&self) -> Arc<AtomicBool> {
        Arc::clone(&self.relay_attempted)
    }

    fn mark_done(&self) {
        self.done.store(true, Ordering::SeqCst);
    }

    fn set_client_disconnected(&mut self, client_disconnected: bool) {
        self.client_disconnected = client_disconnected;
    }
}

fn lease_abort_request(
    edge_request_id: &str,
    lease_id: Option<&str>,
    account_id: Option<i64>,
    reason: &str,
    client_disconnected: bool,
    relay_attempted: bool,
) -> AbortRequest {
    let failure_class = classify_edge_abort_failure(reason, client_disconnected);
    AbortRequest {
        edge_request_id: edge_request_id.to_string(),
        lease_id: lease_id.map(str::to_string),
        account_id,
        reason: reason.to_string(),
        failure_class: failure_class.to_string(),
        client_disconnected,
        relay_attempted,
        fallback_to_go: false,
    }
}

fn classify_edge_abort_failure(reason: &str, client_disconnected: bool) -> &'static str {
    if client_disconnected {
        return "client_cancelled";
    }
    let reason = reason.to_ascii_lowercase();
    if reason.contains("queue") && reason.contains("timeout") {
        "queue_timeout"
    } else if reason.contains("capacity")
        || reason.contains("permit")
        || reason.contains("overload")
        || reason.contains("queue full")
    {
        "local_capacity_rejected"
    } else if reason.contains("prepare")
        || reason.contains("unsupported_ws")
        || reason.contains("proxy_not_supported")
    {
        "prepare_failed"
    } else if reason.contains("retry") {
        "retry_exhausted"
    } else {
        "abort_failed"
    }
}

impl Drop for LeaseAbortGuard {
    fn drop(&mut self) {
        if self.done.load(Ordering::SeqCst) {
            return;
        }
        let req = lease_abort_request(
            &self.edge_request_id,
            self.lease_id.as_deref(),
            self.account_id,
            self.reason,
            self.client_disconnected,
            self.relay_attempted.load(Ordering::SeqCst),
        );
        if let Err(err) = enqueue_settlement_retry(&self.state, SettlementRetryJob::Abort(req)) {
            error!(
                "dropped-request abort callback could not be queued: {}",
                safe_edge_error(&err)
            );
        }
    }
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(env::var("RUST_LOG").unwrap_or_else(|_| "warn".to_string()))
        .init();

    let cfg = Arc::new(EdgeConfig::from_env()?);
    let startup_jitter_ms = env_u64("SUB2API_EDGE_STARTUP_JITTER_MAX_MS", 0);
    if startup_jitter_ms > 0 {
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|value| value.subsec_nanos() as u64)
            .unwrap_or_default();
        let delay_ms = nanos % (startup_jitter_ms + 1);
        if delay_ms > 0 {
            info!("startup jitter: delaying readiness by {}ms", delay_ms);
            tokio::time::sleep(Duration::from_millis(delay_ms)).await;
        }
    }
    let client = edge_http_client_builder(&cfg).build()?;
    let http_lane_pool = HttpLanePool::new(HttpLanePoolConfig::from_env());
    let (settlement_retry_tx, settlement_retry_rx) = mpsc::channel(cfg.queue_buffer_size.max(1024));
    let (payload_commit_tx, payload_commit_rx) = mpsc::channel(PAYLOAD_COMMIT_QUEUE_SIZE);
    let (relay_tx, relay_rx) = mpsc::channel(cfg.queue_buffer_size.max(128));
    let state = AppState {
        cfg: cfg.clone(),
        edge_instance_id: Arc::new(Uuid::new_v4().to_string()),
        client,
        clients_by_proxy: Arc::new(Mutex::new(HashMap::new())),
        http_lane_pool,
        transient_proxy_permits: Arc::new(Semaphore::new(cfg.transient_proxy_max_active.max(1))),
        relay_domains: Arc::new(Mutex::new(HashMap::new())),
        relay_queue_last_used: Arc::new(Mutex::new(HashMap::new())),
        warm_keys: Arc::new(Mutex::new(HashMap::new())),
        ws_idle: Arc::new(tokio::sync::Mutex::new(HashMap::new())),
        ws_idle_last_used: Arc::new(tokio::sync::Mutex::new(HashMap::new())),
        pools: Arc::new(BufferPools::prewarmed(cfg.initial_pool_size)),
        settlement_retry_tx,
        payload_commit_tx,
        payload_commit_overflow: Arc::new(Semaphore::new(PAYLOAD_COMMIT_OVERFLOW_CONCURRENCY)),
        relay_tx,
        relay_queue_bytes: Arc::new(Semaphore::new(cfg.queue_max_bytes.max(1))),
        ingress_permits: Arc::new(Semaphore::new(
            cfg.global_workers
                .max(1)
                .saturating_add(cfg.queue_buffer_size.max(128)),
        )),
        ingress_body_bytes: Arc::new(Semaphore::new(
            cfg.ingress_body_max_bytes.max(MAX_BODY_BYTES),
        )),
        metrics: Arc::new(EdgeMetrics::default()),
        draining: Arc::new(AtomicBool::new(false)),
    };
    tokio::spawn(run_settlement_retry_queue(
        state.clone(),
        settlement_retry_rx,
    ));
    tokio::spawn(run_payload_commit_queue(state.clone(), payload_commit_rx));
    tokio::spawn(run_relay_executor(state.clone(), relay_rx));
    tokio::spawn(recover_previous_edge_leases(state.clone()));
    tokio::spawn(run_edge_resource_reaper(state.clone()));
    let warm_client = state.client.clone();
    let dynamic_warm_state = state.clone();
    let app = Router::new()
        .route("/healthz", any(healthz))
        .route("/readyz", any(readyz))
        .route("/metrics", any(metrics))
        .route("/internal/drain", axum::routing::post(drain))
        .route("/*path", any(handle_openai_edge))
        .with_state(state.clone());

    info!("sub2api-edge-rs listening on {}", cfg.listen_addr);

    // P3: 启动后台上游连接保活（仅当显式配置了 warm url）。该流量只走
    // legacy client，不创建、扩容或刷新业务 lane 的空闲时间。
    if let Some(warm_url) = cfg.upstream_warm_url.clone() {
        let interval_secs = cfg.upstream_warm_interval_secs.max(30);
        tokio::spawn(async move {
            run_upstream_keep_warm(warm_client, warm_url, interval_secs).await;
        });
    }
    tokio::spawn(async move {
        run_dynamic_upstream_keep_warm(dynamic_warm_state).await;
    });

    let listener = tokio::net::TcpListener::bind(cfg.listen_addr).await?;
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal(state.clone()))
        .await?;
    Ok(())
}

async fn shutdown_signal(state: AppState) {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{signal, SignalKind};
        let mut terminate = signal(SignalKind::terminate()).ok();
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {},
            _ = async {
                if let Some(ref mut signal) = terminate {
                    let _ = signal.recv().await;
                } else {
                    pending::<()>().await;
                }
            } => {},
        }
    }
    #[cfg(not(unix))]
    let _ = tokio::signal::ctrl_c().await;

    state.draining.store(true, Ordering::Release);
    let deadline = Instant::now() + Duration::from_secs(state.cfg.drain_timeout_secs.max(30));
    while (state.metrics.active_requests.load(Ordering::Acquire) > 0
        || state.metrics.active_streams.load(Ordering::Acquire) > 0
        || state
            .metrics
            .active_settlement_workers
            .load(Ordering::Acquire)
            > 0
        || state
            .metrics
            .active_payload_commit_workers
            .load(Ordering::Acquire)
            > 0
        || state.settlement_retry_tx.capacity() < state.settlement_retry_tx.max_capacity()
        || state.payload_commit_tx.capacity() < state.payload_commit_tx.max_capacity())
        && Instant::now() < deadline
    {
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

fn edge_http_client_builder(cfg: &EdgeConfig) -> ClientBuilder {
    Client::builder()
        .tcp_nodelay(true)
        .http2_adaptive_window(true)
        .http2_keep_alive_interval(Duration::from_secs(20))
        .http2_keep_alive_timeout(Duration::from_secs(5))
        .http2_keep_alive_while_idle(true)
        .pool_idle_timeout(Duration::from_secs(300))
        .pool_max_idle_per_host(cfg.max_idle_conns_per_account)
}

async fn send_upstream_keep_warm(
    client: Client,
    warm_url: String,
    force_identity_encoding: bool,
) -> anyhow::Result<()> {
    let mut request = client.get(warm_url);
    if force_identity_encoding {
        request = request.header(
            header::ACCEPT_ENCODING,
            HeaderValue::from_static("identity"),
        );
    }
    let response = request.timeout(Duration::from_secs(10)).send().await?;
    let _ = response.status();
    Ok(())
}

// Preserve the legacy explicit keep-warm behavior. Adaptive lanes remain cold
// until a business request uses them and never receive synthetic warm traffic.
async fn run_upstream_keep_warm(client: Client, warm_url: String, interval_secs: u64) {
    let interval = Duration::from_secs(interval_secs);
    loop {
        if let Err(err) = send_upstream_keep_warm(client.clone(), warm_url.clone(), false).await {
            warn!(
                "upstream keep-warm request failed: {}",
                safe_edge_error(&err)
            );
        }
        tokio::time::sleep(interval).await;
    }
}

async fn run_dynamic_upstream_keep_warm(state: AppState) {
    let interval = Duration::from_secs(state.cfg.upstream_warm_interval_secs.max(30));
    let active_window = Duration::from_secs(state.cfg.upstream_dynamic_warm_active_secs.max(60));
    loop {
        tokio::time::sleep(interval).await;
        let now = Instant::now();
        let keys = {
            let mut warm_keys = match state.warm_keys.lock() {
                Ok(guard) => guard,
                Err(_) => {
                    warn!("dynamic upstream warm key map lock poisoned");
                    continue;
                }
            };
            warm_keys.retain(|_, item| now.duration_since(item.last_seen) <= active_window);
            warm_keys
                .iter()
                .map(|(key, item)| (key.clone(), item.clone()))
                .collect::<Vec<_>>()
        };
        for (warm_key, item) in keys {
            if item.warming {
                continue;
            }
            if item.failures >= 3 && item.failures % 3 != 0 {
                if let Ok(mut warm_keys) = state.warm_keys.lock() {
                    if let Some(current) = warm_keys.get_mut(&warm_key) {
                        current.failures = current.failures.saturating_add(1);
                    }
                }
                continue;
            }

            let should_start = match state.warm_keys.lock() {
                Ok(mut warm_keys) => {
                    if let Some(current) = warm_keys.get_mut(&warm_key) {
                        if current.warming {
                            false
                        } else {
                            current.warming = true;
                            true
                        }
                    } else {
                        false
                    }
                }
                Err(_) => {
                    warn!("dynamic upstream warm key map lock poisoned");
                    false
                }
            };
            if !should_start {
                continue;
            }

            let selection = match if state.http_lane_pool.enabled() {
                // Lane-enabled deployments use an active lease even for the
                // legacy keep-warm Client. This prevents the proxy reaper from
                // orphaning an in-flight warm connection while retaining the
                // exact legacy path when the emergency flag is disabled.
                state.client_for_proxy(item.proxy_url.as_deref())
            } else {
                state.legacy_client_for_proxy(item.proxy_url.as_deref())
            } {
                Ok(selection) => selection,
                Err(err) => {
                    warn!(
                        "dynamic upstream warm client failed: {}",
                        safe_edge_error(&err)
                    );
                    if let Ok(mut warm_keys) = state.warm_keys.lock() {
                        if let Some(current) = warm_keys.get_mut(&warm_key) {
                            current.warming = false;
                        }
                    }
                    continue;
                }
            };
            let warm_url = item.warm_url.clone();
            let state_for_update = state.clone();
            tokio::spawn(async move {
                let ProxyClientSelection { client, lease } = selection;
                let _proxy_lease = lease;
                let result = send_upstream_keep_warm(client, warm_url, true).await;
                let Ok(mut warm_keys) = state_for_update.warm_keys.lock() else {
                    return;
                };
                if let Some(current) = warm_keys.get_mut(&warm_key) {
                    current.warming = false;
                    match result {
                        Ok(()) => current.failures = 0,
                        Err(err) => {
                            current.failures = current.failures.saturating_add(1);
                            warn!(
                                "dynamic upstream keep-warm request failed: {}",
                                safe_edge_error(&err)
                            );
                        }
                    }
                }
            });
        }
    }
}

async fn run_edge_resource_reaper(state: AppState) {
    let interval = Duration::from_secs(60);
    loop {
        tokio::time::sleep(interval).await;
        let now = Instant::now();
        state.http_lane_pool.reap();

        if let (Ok(mut queues), Ok(mut last_used)) = (
            state.relay_domains.lock(),
            state.relay_queue_last_used.lock(),
        ) {
            let idle = Duration::from_secs(state.cfg.relay_domain_idle_secs.max(60));
            let stale = last_used
                .iter()
                .filter_map(|(key, used)| {
                    let idle = now.duration_since(*used) >= idle;
                    let idle_permits = queues.get(key).is_some_and(|domain| {
                        domain.available_permits() >= state.cfg.per_account_workers.max(1)
                    });
                    (idle && idle_permits).then_some(key.clone())
                })
                .collect::<Vec<_>>();
            for key in stale {
                queues.remove(&key);
                last_used.remove(&key);
            }
        }

        if let Ok(mut clients) = state.clients_by_proxy.lock() {
            let idle = Duration::from_secs(state.cfg.proxy_client_idle_secs.max(60));
            let stale = clients
                .iter()
                .filter_map(|(key, entry)| {
                    let inactive = entry.active.load(Ordering::Acquire) == 0;
                    let last_used = entry
                        .last_used
                        .lock()
                        .map(|used| now.duration_since(*used) >= idle)
                        .unwrap_or(false);
                    (inactive && last_used).then_some(key.clone())
                })
                .collect::<Vec<_>>();
            for key in stale {
                clients.remove(&key);
            }
        }

        let idle = Duration::from_secs(state.cfg.ws_idle_ttl_secs.max(60));
        let stale_ws = {
            let last_used = state.ws_idle_last_used.lock().await;
            last_used
                .iter()
                .filter_map(|(key, used)| {
                    (now.duration_since(*used) >= idle).then_some(key.clone())
                })
                .collect::<Vec<_>>()
        };
        if !stale_ws.is_empty() {
            let mut pools = state.ws_idle.lock().await;
            let mut last_used = state.ws_idle_last_used.lock().await;
            for key in stale_ws {
                pools.remove(&key);
                last_used.remove(&key);
            }
        }
    }
}

async fn healthz() -> &'static str {
    "ok"
}

async fn readyz(State(state): State<AppState>) -> Response {
    if state.metrics_is_draining() {
        return text_response(StatusCode::SERVICE_UNAVAILABLE, "draining");
    }
    let url = format!("{}/readyz", state.cfg.control_base_url);
    let control_ready = state
        .client
        .get(url)
        .timeout(Duration::from_millis(
            state.cfg.prepare_timeout_ms.min(1000),
        ))
        .send()
        .await
        .is_ok_and(|response| response.status().is_success());
    if !control_ready {
        return text_response(StatusCode::SERVICE_UNAVAILABLE, "control plane unavailable");
    }
    text_response(StatusCode::OK, "ready")
}

async fn metrics(State(state): State<AppState>) -> Response {
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "text/plain; version=0.0.4")
        .body(Body::from(state.metrics.render(&state)))
        .unwrap_or_else(|_| text_response(StatusCode::INTERNAL_SERVER_ERROR, "metrics unavailable"))
}

async fn drain(State(state): State<AppState>, headers: HeaderMap) -> Response {
    let provided = headers
        .get(EDGE_SECRET_HEADER)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default();
    if !constant_time_eq(provided.as_bytes(), state.cfg.internal_secret.as_bytes()) {
        return text_response(StatusCode::UNAUTHORIZED, "unauthorized");
    }
    state.draining.store(true, Ordering::Release);
    text_response(StatusCode::OK, "draining")
}

fn constant_time_eq(left: &[u8], right: &[u8]) -> bool {
    let mut difference = left.len() ^ right.len();
    let max_len = left.len().max(right.len());
    for index in 0..max_len {
        let left_byte = left.get(index).copied().unwrap_or_default();
        let right_byte = right.get(index).copied().unwrap_or_default();
        difference |= usize::from(left_byte ^ right_byte);
    }
    difference == 0
}

async fn handle_openai_edge(
    State(state): State<AppState>,
    ws: Option<WebSocketUpgrade>,
    req: Request,
) -> Response {
    if request_header_bytes(req.headers()) > state.cfg.max_header_bytes {
        state
            .metrics
            .rejected_requests
            .fetch_add(1, Ordering::Relaxed);
        return text_response(
            StatusCode::REQUEST_HEADER_FIELDS_TOO_LARGE,
            "request headers too large",
        );
    }
    if state.metrics_is_draining() {
        state
            .metrics
            .drain_rejections
            .fetch_add(1, Ordering::Relaxed);
        state
            .metrics
            .rejected_requests
            .fetch_add(1, Ordering::Relaxed);
        return text_response(StatusCode::SERVICE_UNAVAILABLE, "edge draining");
    }
    let _active_request = state.metrics.begin_request();
    let edge_request_id = Uuid::new_v4().to_string();
    let start = Instant::now();
    let (parts, body) = req.into_parts();
    let method = parts.method.clone();
    let uri = parts.uri.clone();
    let headers = parts.headers.clone();

    if is_websocket_upgrade(&headers) {
        let Some(ws) = ws else {
            return text_response(StatusCode::BAD_REQUEST, "invalid websocket upgrade");
        };
        return handle_openai_ws_edge(state, ws, method, uri, headers).await;
    }

    // Bound request-body residency before reading up to MAX_BODY_BYTES. This is
    // a non-blocking local permit, so admitted requests add no wait or control
    // plane round trip before first token.
    let ingress_permit = match state.ingress_permits.clone().try_acquire_owned() {
        Ok(permit) => permit,
        Err(_) => return overload_response(),
    };
    let content_length = headers
        .get(header::CONTENT_LENGTH)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.parse::<usize>().ok());
    if content_length.is_some_and(|length| length > MAX_BODY_BYTES) {
        return text_response(StatusCode::PAYLOAD_TOO_LARGE, "request body too large");
    }
    let mut ingress_body_permits = Vec::new();
    let body_bytes = if let Some(length) = content_length {
        if length > 0 {
            match state
                .ingress_body_bytes
                .clone()
                .try_acquire_many_owned(length as u32)
            {
                Ok(permit) => ingress_body_permits.push(permit),
                Err(_) => return overload_response(),
            }
        }
        match to_bytes(body, MAX_BODY_BYTES).await {
            Ok(bytes) => bytes,
            Err(err) => {
                state
                    .metrics
                    .prepare_failures
                    .fetch_add(1, Ordering::Relaxed);
                error!("read client body failed: {}", safe_edge_error(&err));
                return text_response(StatusCode::BAD_REQUEST, "failed to read request body");
            }
        }
    } else {
        let mut stream = body.into_data_stream();
        let mut buffered = Vec::new();
        while let Some(next) = stream.next().await {
            let chunk = match next {
                Ok(chunk) => chunk,
                Err(err) => {
                    state
                        .metrics
                        .prepare_failures
                        .fetch_add(1, Ordering::Relaxed);
                    error!("read chunked client body failed: {}", safe_edge_error(&err));
                    return text_response(StatusCode::BAD_REQUEST, "failed to read request body");
                }
            };
            if buffered.len().saturating_add(chunk.len()) > MAX_BODY_BYTES {
                return text_response(StatusCode::PAYLOAD_TOO_LARGE, "request body too large");
            }
            if !chunk.is_empty() {
                let permit = match state
                    .ingress_body_bytes
                    .clone()
                    .try_acquire_many_owned(chunk.len() as u32)
                {
                    Ok(permit) => permit,
                    Err(_) => return overload_response(),
                };
                ingress_body_permits.push(permit);
                buffered.extend_from_slice(&chunk);
            }
        }
        Bytes::from(buffered)
    };
    let _ingress_body_permits = ingress_body_permits;
    let raw_prepare_body = openai_prepare_raw_body(&state, &body_bytes);
    let (stream, prepare_body) = if raw_prepare_body.is_some() {
        let stream = serde_json::from_slice::<StreamOnlyRequest>(&body_bytes)
            .ok()
            .and_then(|request| request.stream);
        (stream, Value::Null)
    } else {
        let body_json = if body_bytes.is_empty() {
            Value::Null
        } else {
            match serde_json::from_slice::<Value>(&body_bytes) {
                Ok(value) => value,
                Err(_) => Value::String(String::from_utf8_lossy(&body_bytes).into_owned()),
            }
        };
        let stream = body_json.get("stream").and_then(Value::as_bool);
        (stream, body_json)
    };

    let prepare = PrepareRequest {
        edge_request_id: edge_request_id.clone(),
        edge_node_id: state.cfg.edge_node_id.clone(),
        edge_instance_id: state.edge_instance_id.as_ref().clone(),
        method: method.to_string(),
        path: uri.path().to_string(),
        raw_query: uri.query().map(ToOwned::to_owned),
        headers: header_map_to_strings(&headers),
        stream,
        body: prepare_body,
        body_raw_base64: raw_prepare_body,
        client_ip: client_ip_from_headers(&headers),
    };

    let prepare_started_at = Instant::now();
    let (plan, prepare_ms) = match call_prepare(&state, &prepare).await {
        Ok(plan) => (plan, prepare_started_at.elapsed().as_millis() as i64),
        Err(err) => {
            drop(ingress_permit);
            warn!(
                "prepare failed; falling back to Go: {}",
                safe_edge_error(&err)
            );
            if let Err(queue_err) = enqueue_settlement_retry(
                &state,
                SettlementRetryJob::Abort(AbortRequest {
                    edge_request_id: edge_request_id.clone(),
                    lease_id: None,
                    account_id: None,
                    reason: "prepare_failed".to_string(),
                    failure_class: "prepare_failed".to_string(),
                    client_disconnected: false,
                    relay_attempted: false,
                    fallback_to_go: true,
                }),
            ) {
                error!(
                    "prepare cancellation could not be queued: {}",
                    safe_edge_error(&queue_err)
                );
            }
            let mut timing = EdgeTiming {
                prepare_ms: Some(prepare_started_at.elapsed().as_millis() as i64),
                ..EdgeTiming::default()
            };
            timing.fallback_reason = Some("prepare_failed".to_string());
            return fallback_to_go(
                state,
                method,
                uri,
                headers,
                Body::from(body_bytes),
                "prepare_failed",
                timing,
            )
            .await;
        }
    };
    let timing = EdgeTiming {
        prepare_ms: Some(prepare_ms),
        ..EdgeTiming::default()
    };

    if plan.action != "relay" {
        drop(ingress_permit);
        state
            .metrics
            .fallback_requests
            .fetch_add(1, Ordering::Relaxed);
        if let Some(reason) = &plan.reason {
            info!("edge fallback_to_go reason={}", safe_edge_error(&reason));
        }
        return fallback_to_go(
            state,
            method,
            uri,
            headers,
            Body::from(body_bytes),
            plan.reason.as_deref().unwrap_or("prepare_fallback_go"),
            timing,
        )
        .await;
    }

    let relay_lease_id = plan.lease_id.clone();
    state.metrics.relay_requests.fetch_add(1, Ordering::Relaxed);
    let relay_account_id = plan.account_id;
    let timing_shared = Arc::new(Mutex::new(timing.clone()));
    match relay_upstream(
        state.clone(),
        plan,
        start,
        timing,
        Some(timing_shared.clone()),
        true,
        Some(ingress_permit),
    )
    .await
    {
        Ok(resp) => {
            if let Some(lease_id) = relay_lease_id.clone() {
                spawn_payload_commit(
                    state.clone(),
                    CommitRequest {
                        edge_request_id: edge_request_id.clone(),
                        lease_id: Some(lease_id),
                        account_id: relay_account_id,
                    },
                );
            }
            resp
        }
        Err(err) => {
            error!(
                "relay failed before response commit: {}",
                safe_edge_error(&err)
            );
            let reason = relay_error_fallback_reason(&err);
            let local_capacity = relay_error_is_local_capacity(&err);
            if let Err(callback_err) = call_abort(
                &state,
                AbortRequest {
                    edge_request_id,
                    lease_id: relay_lease_id,
                    account_id: relay_account_id,
                    reason: format!("{reason}: {err}"),
                    failure_class: if local_capacity {
                        "local_capacity_rejected"
                    } else {
                        "abort_failed"
                    }
                    .to_string(),
                    client_disconnected: false,
                    relay_attempted: !local_capacity,
                    fallback_to_go: !local_capacity,
                },
            )
            .await
            {
                error!(
                    "relay failure abort could not be delivered or queued: {}",
                    safe_edge_error(&callback_err)
                );
            }
            if local_capacity {
                state
                    .metrics
                    .rejected_requests
                    .fetch_add(1, Ordering::Relaxed);
                return overload_response();
            }
            let mut fallback_timing = edge_timing_snapshot(&timing_shared);
            fallback_timing.fallback_reason = Some(reason.to_string());
            fallback_to_go(
                state,
                method,
                uri,
                headers,
                Body::from(body_bytes),
                reason,
                fallback_timing,
            )
            .await
        }
    }
}

fn request_header_bytes(headers: &HeaderMap) -> usize {
    headers.iter().fold(0usize, |total, (name, value)| {
        total
            .saturating_add(name.as_str().len())
            .saturating_add(value.as_bytes().len())
            .saturating_add(4)
    })
}

async fn call_prepare(state: &AppState, req: &PrepareRequest) -> anyhow::Result<EdgePlan> {
    let url = format!(
        "{}/internal/edge/openai/prepare",
        state.cfg.control_base_url
    );
    let resp = state
        .client
        .post(url)
        .header(EDGE_SECRET_HEADER, &state.cfg.internal_secret)
        .timeout(std::time::Duration::from_millis(
            state.cfg.prepare_timeout_ms,
        ))
        .json(req)
        .send()
        .await?;
    let status = resp.status();
    let content_type = resp
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .unwrap_or("")
        .to_string();
    let body = resp.bytes().await?;
    if !status.is_success() {
        anyhow::bail!(
            "prepare status {status}; content_type={content_type}; body={}",
            response_body_preview(&body)
        );
    }
    decode_edge_plan(&body).map_err(|err| {
        anyhow::anyhow!(
            "prepare decode failed: {err}; status={status}; content_type={content_type}; body={}",
            response_body_preview(&body)
        )
    })
}

fn decode_edge_plan(body: &[u8]) -> serde_json::Result<EdgePlan> {
    match serde_json::from_slice::<EdgePlan>(body) {
        Ok(plan) => Ok(plan),
        Err(plan_err) => match serde_json::from_slice::<EdgePlanEnvelope>(body) {
            Ok(EdgePlanEnvelope { plan: Some(plan) }) => Ok(plan),
            Ok(EdgePlanEnvelope { plan: None }) => Err(plan_err),
            Err(_) => Err(plan_err),
        },
    }
}

fn response_body_preview(body: &[u8]) -> String {
    const MAX_PREVIEW: usize = 2048;
    let mut preview = String::from_utf8_lossy(&body[..body.len().min(MAX_PREVIEW)]).into_owned();
    preview = redact_json_secret_field(preview, "authorization");
    preview = redact_json_secret_field(preview, "api-key");
    preview = redact_json_secret_field(preview, "x-api-key");
    preview = redact_json_secret_field(preview, "openai-api-key");
    preview = redact_json_secret_field(preview, "api_key");
    preview = redact_json_secret_field(preview, "access_token");
    preview = redact_json_secret_field(preview, "refresh_token");
    preview = redact_json_secret_field(preview, "cookie");
    preview = redact_json_secret_field(preview, "set-cookie");
    preview = redact_json_secret_field(preview, "upstream_url");
    preview = redact_json_secret_field(preview, "proxy_url");
    let lower = preview.to_ascii_lowercase();
    if lower.contains("http://")
        || lower.contains("https://")
        || lower.contains("ws://")
        || lower.contains("wss://")
        || lower.contains("<!doctype")
        || lower.contains("<html")
        || (lower.contains("authorization")
            && (lower.contains("bearer") || lower.contains("basic")))
        || lower.contains("api-key")
        || lower.contains("api_key")
        || lower.contains("access_token")
        || lower.contains("refresh_token")
        || lower.contains("set-cookie")
        || lower.contains("cookie")
        || lower.contains("cloudflare")
        || lower.contains("chatgpt")
        || lower.contains("openai")
        || lower.contains("anthropic")
        || lower.contains("claude")
        || lower.contains("grok")
        || lower.contains("x.ai")
    {
        return "[redacted upstream error]".to_string();
    }
    if body.len() > MAX_PREVIEW {
        preview.push_str("...");
    }
    preview
}

fn safe_edge_error<E: Display>(error: E) -> String {
    response_body_preview(error.to_string().as_bytes())
}

fn redact_json_secret_field(mut text: String, key: &str) -> String {
    let needle = format!("\"{key}\":\"");
    let mut search_from = 0usize;
    loop {
        let lower = text.to_ascii_lowercase();
        let Some(relative_start) = lower[search_from..].find(&needle) else {
            break;
        };
        let value_start = search_from + relative_start + needle.len();
        let Some(relative_end) = text[value_start..].find('"') else {
            break;
        };
        let value_end = value_start + relative_end;
        text.replace_range(value_start..value_end, "[redacted]");
        search_from = value_start + "[redacted]".len();
    }
    text
}

async fn handle_openai_ws_edge(
    state: AppState,
    ws: WebSocketUpgrade,
    method: Method,
    uri: Uri,
    headers: HeaderMap,
) -> Response {
    ws.on_upgrade(move |socket| async move {
        let _stream_guard = state.metrics.begin_stream();
        if let Err(err) = relay_ws_session(state, socket, method, uri, headers).await {
            warn!(
                "edge ws session ended with error: {}",
                safe_edge_error(&err)
            );
        }
    })
}

async fn relay_ws_session(
    state: AppState,
    mut client_socket: WebSocket,
    method: Method,
    uri: Uri,
    headers: HeaderMap,
) -> anyhow::Result<()> {
    let started_at = Instant::now();
    let edge_request_id = Uuid::new_v4().to_string();
    let Some(first_msg) = client_socket.recv().await else {
        anyhow::bail!("missing first websocket message");
    };
    let first_msg = first_msg?;
    let first_text = match axum_ws_message_to_text(&first_msg) {
        Some(text) => text,
        None => anyhow::bail!("unsupported first websocket message"),
    };
    let first_json: Value = serde_json::from_str(&first_text)?;
    let prepare = PrepareRequest {
        edge_request_id: edge_request_id.clone(),
        edge_node_id: state.cfg.edge_node_id.clone(),
        edge_instance_id: state.edge_instance_id.as_ref().clone(),
        method: method.to_string(),
        path: uri.path().to_string(),
        raw_query: uri.query().map(ToOwned::to_owned),
        headers: header_map_to_strings(&headers),
        stream: Some(true),
        body: first_json,
        body_raw_base64: Some(b64_encode(first_text.as_bytes())),
        client_ip: client_ip_from_headers(&headers),
    };

    let ingress_permit = state
        .ingress_permits
        .clone()
        .try_acquire_owned()
        .map_err(|_| anyhow::anyhow!("edge ingress capacity exhausted"))?;

    let mut plan = match call_prepare(&state, &prepare).await {
        Ok(plan) => plan,
        Err(err) => {
            drop(ingress_permit);
            warn!(
                "ws prepare failed; proxying to Go: {}",
                safe_edge_error(&err)
            );
            if let Err(queue_err) = enqueue_settlement_retry(
                &state,
                SettlementRetryJob::Abort(AbortRequest {
                    edge_request_id: edge_request_id.clone(),
                    lease_id: None,
                    account_id: None,
                    reason: "ws_prepare_failed".to_string(),
                    failure_class: "prepare_failed".to_string(),
                    client_disconnected: false,
                    relay_attempted: false,
                    fallback_to_go: true,
                }),
            ) {
                error!(
                    "ws prepare cancellation could not be queued: {}",
                    safe_edge_error(&queue_err)
                );
            }
            return proxy_ws_to_go(state, client_socket, method, uri, headers, first_msg).await;
        }
    };
    if plan.action != "relay" {
        drop(ingress_permit);
        if let Some(reason) = &plan.reason {
            info!("edge ws fallback_to_go reason={}", safe_edge_error(&reason));
        }
        return proxy_ws_to_go(state, client_socket, method, uri, headers, first_msg).await;
    }
    let _lease_renewal_guard = LeaseRenewalGuard::start(&state, &plan);
    let mut guard = LeaseAbortGuard::new(
        state.clone(),
        plan.edge_request_id.clone(),
        plan.lease_id.clone(),
        plan.account_id,
        "ws_session_dropped",
        true,
    );
    // Setup failures are upstream/plan failures, not downstream disconnects.
    guard.set_client_disconnected(false);
    if plan.transport.as_deref() != Some("ws_v2") {
        let abort_result = call_abort(
            &state,
            AbortRequest {
                edge_request_id: edge_request_id.clone(),
                lease_id: plan.lease_id.clone(),
                account_id: plan.account_id,
                reason: "unsupported_ws_transport".to_string(),
                failure_class: "prepare_failed".to_string(),
                client_disconnected: false,
                relay_attempted: false,
                fallback_to_go: false,
            },
        )
        .await;
        if let Err(callback_err) = abort_result {
            error!(
                "unsupported ws transport abort could not be delivered or queued: {callback_err}"
            );
        } else {
            // `call_abort` returns Ok both for a direct acknowledgement and
            // for a successfully queued retry. If queueing also failed, keep
            // the guard alive so its Drop path gets one more delivery chance.
            guard.mark_done();
        }
        anyhow::bail!("unsupported ws transport");
    }
    if plan
        .proxy_url
        .as_deref()
        .is_some_and(|v| !v.trim().is_empty())
    {
        let abort_result = call_abort(
            &state,
            AbortRequest {
                edge_request_id: edge_request_id.clone(),
                lease_id: plan.lease_id.clone(),
                account_id: plan.account_id,
                reason: "ws_proxy_not_supported".to_string(),
                failure_class: "prepare_failed".to_string(),
                client_disconnected: false,
                relay_attempted: false,
                fallback_to_go: false,
            },
        )
        .await;
        if let Err(callback_err) = abort_result {
            error!(
                "invalid ws plan abort could not be delivered or queued: {}",
                safe_edge_error(&callback_err)
            );
        } else {
            guard.mark_done();
        }
        anyhow::bail!("ws proxy is not supported by edge yet");
    }

    // Setup failures are upstream failures, not downstream disconnects. Once
    // the first upstream message is sent, an unexpected task drop is treated
    // as a client disconnect unless the normal completion path says otherwise.
    let idle_conn = state.take_ws_idle(&plan).await;
    let (upstream_socket, upstream_request_id) = if let Some(conn) = idle_conn {
        (conn.socket, conn.request_id)
    } else {
        connect_ws_for_plan(&plan).await?
    };
    state.ensure_ws_idle(plan.clone()).await;
    let (mut upstream_write, mut upstream_read) = upstream_socket.split();
    let first_upstream_msg = edge_plan_ws_first_message(&plan, first_msg)?;
    let mut last_request_body = tungstenite_message_json(&first_upstream_msg);
    let mut wrote_client_response_for_turn = false;
    upstream_write.send(first_upstream_msg).await?;
    drop(ingress_permit);
    guard.mark_relay_attempted();
    guard.set_client_disconnected(true);

    let lease_id = plan.lease_id.clone();
    let account_id = plan.account_id;
    let edge_request_id_complete = plan.edge_request_id.clone();
    let mut success = true;
    let mut client_disconnected = false;
    let mut error_message: Option<String> = None;
    let mut summary = ChatStreamSummary {
        request_id: upstream_request_id,
        ..Default::default()
    };
    let mut prompt_cache_creation_optimization_model =
        plan.prompt_cache_creation_optimization_model.clone();
    let mut cache_creation_policy_applied_for_turn =
        plan.prompt_cache_creation_optimization_applied;
    let mut failure_state = OpenAIWSFailureState::default();

    loop {
        tokio::select! {
            next = client_socket.recv() => {
                match next {
                    Some(Ok(msg)) => {
                        if matches!(msg, AxumWsMessage::Close(_)) {
                            success = false;
                            client_disconnected = true;
                            let _ = upstream_write.send(TungsteniteMessage::Close(None)).await;
                            break;
                        }
                        let upstream_msg = match axum_to_tungstenite_message(msg) {
                            Ok(msg) => msg,
                            Err(err) => {
                                success = false;
                                client_disconnected = true;
                                error_message = Some(err.to_string());
                                break;
                            }
                        };
                        let (upstream_msg, turn_policy_applied) = apply_openai_ws_request_policies_tracked(
                            upstream_msg,
                            plan.prompt_cache_creation_optimization_mode.as_deref(),
                            &mut prompt_cache_creation_optimization_model,
                            plan.max_reasoning_effort.as_deref(),
                            &plan.reasoning_effort_mappings,
                        );
                        if let Some(applied) = turn_policy_applied {
                            cache_creation_policy_applied_for_turn = applied;
                        }
                        if tungstenite_message_is_response_create(&upstream_msg) {
                            last_request_body = tungstenite_message_json(&upstream_msg);
                            wrote_client_response_for_turn = false;
                        }
                        if let Err(err) = upstream_write.send(upstream_msg).await {
                            success = false;
                            failure_state.mark_other_failure();
                            error_message = Some(err.to_string());
                            break;
                        }
                    }
                    Some(Err(err)) => {
                        success = false;
                        client_disconnected = true;
                        error_message = Some(err.to_string());
                        break;
                    }
                    None => {
                        success = false;
                        client_disconnected = true;
                        break;
                    }
                }
            }
            next = upstream_read.next() => {
                match next {
                    Some(Ok(msg)) => {
                        if matches!(msg, TungsteniteMessage::Close(_)) {
                            let _ = client_socket.send(AxumWsMessage::Close(None)).await;
                            break;
                        }
                        if !wrote_client_response_for_turn {
                            if let (Some(rejected_body), Some(request_body)) = (
                                openai_ws_explicit_rejected_field_error(&msg),
                                last_request_body.clone(),
                            ) {
                                let retry_decision = call_retry(
                                    &state,
                                    RetryRequest {
                                        edge_request_id: plan.edge_request_id.clone(),
                                        lease_id: plan.lease_id.clone(),
                                        account_id: plan.account_id,
                                        upstream_status_code: Some(StatusCode::BAD_REQUEST.as_u16()),
                                        upstream_request_id: None,
                                        error_type: Some("responses_rejected_field".to_string()),
                                        error_message: Some("Upstream rejected a supported request field".to_string()),
                                        request_body: Some(request_body.clone()),
                                        response_body: Some(rejected_body),
                                        wrote_client_response: false,
                                    },
                                )
                                .await;
                                if let Ok(decision) = retry_decision {
                                    if decision.action == "relay" {
                                        if let Some(next_plan) = decision.plan {
                                            let same_lease = next_plan.lease_id == plan.lease_id;
                                            let same_account = next_plan.account_id == plan.account_id;
                                            let supported_transport = next_plan.transport.as_deref() == Some("ws_v2");
                                            let proxy_free = next_plan
                                                .proxy_url
                                                .as_deref()
                                                .is_none_or(|value| value.trim().is_empty());
                                            if same_lease && same_account && supported_transport && proxy_free {
                                                let next_original = AxumWsMessage::Text(request_body.to_string());
                                                if let Ok(next_message) = edge_plan_ws_first_message(&next_plan, next_original) {
                                                    let next_idle = state.take_ws_idle(&next_plan).await;
                                                    let next_connection = if let Some(conn) = next_idle {
                                                        Ok((conn.socket, conn.request_id))
                                                    } else {
                                                        connect_ws_for_plan(&next_plan).await
                                                    };
                                                    if let Ok((next_socket, _)) = next_connection {
                                                        let (mut next_write, next_read) = next_socket.split();
                                                        if next_write.send(next_message.clone()).await.is_ok() {
                                                            state.ensure_ws_idle(next_plan.clone()).await;
                                                            upstream_write = next_write;
                                                            upstream_read = next_read;
                                                            last_request_body = tungstenite_message_json(&next_message);
                                                            prompt_cache_creation_optimization_model =
                                                                next_plan.prompt_cache_creation_optimization_model.clone();
                                                            cache_creation_policy_applied_for_turn =
                                                                next_plan.prompt_cache_creation_optimization_applied;
                                                            failure_state = OpenAIWSFailureState::default();
                                                            plan = next_plan;
                                                            continue;
                                                        }
                                                    }
                                                }
                                            }
                                        }
                                    }
                                }
                            }
                        }
                        let message_failed = failure_state.observe_upstream_message(
                            &msg,
                            cache_creation_policy_applied_for_turn,
                        );
                        summary.observe_ws_message(&msg);
                        if message_failed && !summary.failed {
                            summary.failed = true;
                            summary.failed_terminal_event_type = Some("error".to_string());
                            summary.terminal_event_type = Some("error".to_string());
                        }
                        if summary.failed {
                            success = false;
                            error_message = Some("Upstream request failed".to_string());
                        }
                        let client_msg = match tungstenite_to_axum_message(sanitize_openai_ws_message(msg)) {
                            Ok(msg) => msg,
                            Err(err) => {
                                success = false;
                                error_message = Some(err.to_string());
                                break;
                            }
                        };
                        let is_client_payload = matches!(client_msg, AxumWsMessage::Text(_) | AxumWsMessage::Binary(_));
                        if let Err(err) = client_socket.send(client_msg).await {
                            success = false;
                            client_disconnected = true;
                            error_message = Some(err.to_string());
                            break;
                        }
                        if is_client_payload {
                            wrote_client_response_for_turn = true;
                        }
                    }
                    Some(Err(err)) => {
                        success = false;
                        failure_state.mark_other_failure();
                        error_message = Some(err.to_string());
                        break;
                    }
                    None => break,
                }
            }
        }
    }

    if client_disconnected {
        success = false;
        if error_message.is_none() {
            error_message = Some("Client disconnected".to_string());
        }
    } else if !summary.completed_successfully(Some("responses")) {
        success = false;
        if error_message.is_none() {
            error_message = Some("Upstream stream ended before completion".to_string());
        }
    }

    guard.set_client_disconnected(client_disconnected);
    call_complete(
        &state,
        CompleteRequest {
            edge_request_id: edge_request_id_complete,
            lease_id,
            account_id,
            success,
            failure_class: classify_stream_failure(success, client_disconnected, &summary),
            client_disconnected,
            request_id: summary.request_id.clone(),
            response_id: summary.response_id.clone(),
            model: summary.model.clone(),
            upstream_model: summary.upstream_model.clone(),
            usage: summary.usage.clone(),
            duration_ms: started_at.elapsed().as_millis() as i64,
            upstream_header_ms: None,
            upstream_first_byte_ms: None,
            first_token_ms: None,
            real_first_token_ms: None,
            guard_sample_at_unix_ns: None,
            first_client_flush_ms: None,
            edge_prepare_ms: None,
            edge_queue_wait_ms: None,
            edge_relay_start_ms: None,
            edge_fallback_reason: None,
            edge_retry_count: 0,
            error_type: if success {
                None
            } else if failure_state.is_cache_policy_only() {
                Some("cache_creation_optimization_unsupported".to_string())
            } else {
                Some("ws_error".to_string())
            },
            error_message,
            upstream_status_code: None,
            terminal_event_type: summary.terminal_event_type(Some("responses")),
            cyber_blocked: summary.cyber_blocked,
        },
    )
    .await?;
    guard.mark_done();
    Ok(())
}

async fn proxy_ws_to_go(
    state: AppState,
    client_socket: WebSocket,
    _method: Method,
    uri: Uri,
    headers: HeaderMap,
    first_msg: AxumWsMessage,
) -> anyhow::Result<()> {
    let url = format!(
        "{}{}",
        state.cfg.go_base_url.trim_end_matches('/'),
        uri.path_and_query()
            .map(|pq| pq.as_str())
            .unwrap_or(uri.path())
    )
    .replace("http://", "ws://")
    .replace("https://", "wss://");
    let mut req = url.into_client_request()?;
    for (name, value) in headers.iter() {
        if should_forward_header(name.as_str()) {
            if let Ok(value) = HeaderValue::from_bytes(value.as_bytes()) {
                req.headers_mut().insert(name.clone(), value);
            }
        }
    }
    let (upstream_socket, _) =
        connect_async_tls_with_config(req, Some(WebSocketConfig::default()), false, None).await?;
    let (mut upstream_write, mut upstream_read) = upstream_socket.split();
    upstream_write
        .send(axum_to_tungstenite_message(first_msg)?)
        .await?;
    let (mut client_write, mut client_read) = client_socket.split();
    loop {
        tokio::select! {
            next = client_read.next() => {
                match next {
                    Some(Ok(msg)) => upstream_write.send(axum_to_tungstenite_message(msg)?).await?,
                    Some(Err(err)) => anyhow::bail!(err),
                    None => break,
                }
            }
            next = upstream_read.next() => {
                match next {
                    Some(Ok(msg)) => client_write.send(tungstenite_to_axum_message(msg)?).await?,
                    Some(Err(err)) => anyhow::bail!(err),
                    None => break,
                }
            }
        }
    }
    Ok(())
}

async fn connect_ws_for_plan(plan: &EdgePlan) -> anyhow::Result<(WsStream, Option<String>)> {
    let upstream_url = plan
        .upstream_url
        .clone()
        .ok_or_else(|| anyhow::anyhow!("missing upstream ws url"))?;
    let mut upstream_req = upstream_url.into_client_request()?;
    if let Some(headers) = &plan.headers {
        for (k, v) in headers {
            let name = HeaderName::from_bytes(k.as_bytes())?;
            let value = HeaderValue::from_str(v)?;
            upstream_req.headers_mut().insert(name, value);
        }
    }
    let (socket, response) =
        connect_async_tls_with_config(upstream_req, Some(WebSocketConfig::default()), false, None)
            .await?;
    let request_id = response
        .headers()
        .get("x-request-id")
        .and_then(|v| v.to_str().ok())
        .map(ToOwned::to_owned);
    Ok((socket, request_id))
}

fn axum_ws_message_to_text(msg: &AxumWsMessage) -> Option<String> {
    match msg {
        AxumWsMessage::Text(text) => Some(text.clone()),
        AxumWsMessage::Binary(bytes) => String::from_utf8(bytes.clone()).ok(),
        _ => None,
    }
}

fn axum_to_tungstenite_message(msg: AxumWsMessage) -> anyhow::Result<TungsteniteMessage> {
    Ok(match msg {
        AxumWsMessage::Text(text) => TungsteniteMessage::Text(text),
        AxumWsMessage::Binary(bytes) => TungsteniteMessage::Binary(bytes),
        AxumWsMessage::Ping(bytes) => TungsteniteMessage::Ping(bytes),
        AxumWsMessage::Pong(bytes) => TungsteniteMessage::Pong(bytes),
        AxumWsMessage::Close(_) => TungsteniteMessage::Close(None),
    })
}

fn tungstenite_to_axum_message(msg: TungsteniteMessage) -> anyhow::Result<AxumWsMessage> {
    Ok(match msg {
        TungsteniteMessage::Text(text) => AxumWsMessage::Text(text),
        TungsteniteMessage::Binary(bytes) => AxumWsMessage::Binary(bytes),
        TungsteniteMessage::Ping(bytes) => AxumWsMessage::Ping(bytes),
        TungsteniteMessage::Pong(bytes) => AxumWsMessage::Pong(bytes),
        TungsteniteMessage::Close(_) => AxumWsMessage::Close(None),
        TungsteniteMessage::Frame(_) => anyhow::bail!("raw websocket frames are unsupported"),
    })
}

fn edge_plan_ws_first_message(
    plan: &EdgePlan,
    original: AxumWsMessage,
) -> anyhow::Result<TungsteniteMessage> {
    if let Some(raw) = plan
        .body_raw_base64
        .as_deref()
        .map(str::trim)
        .filter(|raw| !raw.is_empty())
    {
        return Ok(TungsteniteMessage::Text(String::from_utf8(b64_decode(
            raw,
        )?)?));
    }
    if let Some(body) = plan.body.clone() {
        return Ok(TungsteniteMessage::Text(body.to_string()));
    }
    axum_to_tungstenite_message(original)
}

const OPENAI_PROMPT_CACHE_EXPLICIT_MIN_STATIC_BYTES: usize = 4 * 1024;
const OPENAI_PROMPT_CACHE_CREATION_OPTIMIZATION_TTL: &str = "30m";

#[cfg(test)]
fn apply_openai_prompt_cache_creation_optimization_ws_message(
    msg: TungsteniteMessage,
    mode: Option<&str>,
    session_model: &mut Option<String>,
) -> TungsteniteMessage {
    apply_openai_prompt_cache_creation_optimization_ws_message_tracked(msg, mode, session_model).0
}

// The optional bool is present only for response.create frames and reports
// whether this exact turn was rewritten. This keeps account enablement separate
// from per-turn application when a WS session changes models.
#[cfg(test)]
fn apply_openai_prompt_cache_creation_optimization_ws_message_tracked(
    msg: TungsteniteMessage,
    mode: Option<&str>,
    session_model: &mut Option<String>,
) -> (TungsteniteMessage, Option<bool>) {
    apply_openai_ws_request_policies_tracked(msg, mode, session_model, None, &[])
}

fn apply_openai_ws_request_policies_tracked(
    msg: TungsteniteMessage,
    mode: Option<&str>,
    session_model: &mut Option<String>,
    max_reasoning_effort: Option<&str>,
    reasoning_effort_mappings: &[ReasoningEffortMapping],
) -> (TungsteniteMessage, Option<bool>) {
    let normalized_mode = mode.unwrap_or_default().trim().to_ascii_lowercase();
    let cache_policy_enabled = normalized_mode == "reduce" || normalized_mode == "suppress";
    let reasoning_policy_enabled = max_reasoning_effort
        .is_some_and(|value| !value.trim().is_empty())
        || !reasoning_effort_mappings.is_empty();
    if !cache_policy_enabled && !reasoning_policy_enabled {
        return (msg, None);
    }
    let (mut value, original) = match msg {
        TungsteniteMessage::Text(text) => {
            let Ok(value) = serde_json::from_str::<Value>(&text) else {
                return (TungsteniteMessage::Text(text), None);
            };
            (value, OpenAIWSJSONFrame::Text(text))
        }
        TungsteniteMessage::Binary(bytes) => {
            let Ok(value) = serde_json::from_slice::<Value>(&bytes) else {
                return (TungsteniteMessage::Binary(bytes), None);
            };
            (value, OpenAIWSJSONFrame::Binary(bytes))
        }
        other => return (other, None),
    };
    let event_type = value.get("type").and_then(Value::as_str);
    if event_type == Some("session.update") {
        if let Some(model) = value
            .pointer("/session/model")
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|model| !model.is_empty())
        {
            *session_model = Some(model.to_string());
        }
        return (original.into_message(), None);
    }
    if event_type != Some("response.create") {
        return (original.into_message(), None);
    }
    let model = value
        .get("model")
        .and_then(Value::as_str)
        .or(session_model.as_deref())
        .unwrap_or_default();
    // A reasoning-only policy still needs to clear the previous turn's cache
    // flag; otherwise a later settlement can inherit stale cache billing state.
    let mut cache_policy_applied = if cache_policy_enabled {
        None
    } else {
        Some(false)
    };
    let mut changed = false;
    if cache_policy_enabled {
        if !is_openai_gpt56_model(model) || is_openai_ws_image_generation_intent(&value) {
            cache_policy_applied = Some(false);
        } else {
            apply_openai_prompt_cache_creation_optimization_value(&mut value, &normalized_mode);
            cache_policy_applied = Some(true);
            changed = true;
        }
    }
    changed |= apply_openai_reasoning_effort_policy_value(
        &mut value,
        max_reasoning_effort,
        reasoning_effort_mappings,
    );
    if !changed {
        return (original.into_message(), cache_policy_applied);
    }
    original.with_updated_value(&value, cache_policy_applied)
}

enum OpenAIWSJSONFrame {
    Text(String),
    Binary(Vec<u8>),
}

impl OpenAIWSJSONFrame {
    fn into_message(self) -> TungsteniteMessage {
        match self {
            Self::Text(text) => TungsteniteMessage::Text(text),
            Self::Binary(bytes) => TungsteniteMessage::Binary(bytes),
        }
    }

    fn with_updated_value(
        self,
        value: &Value,
        policy_applied: Option<bool>,
    ) -> (TungsteniteMessage, Option<bool>) {
        match self {
            Self::Text(text) => match serde_json::to_string(value) {
                Ok(updated) => (TungsteniteMessage::Text(updated), policy_applied),
                Err(_) => (
                    TungsteniteMessage::Text(text),
                    policy_applied.map(|_| false),
                ),
            },
            Self::Binary(bytes) => match serde_json::to_vec(value) {
                Ok(updated) => (TungsteniteMessage::Binary(updated), policy_applied),
                Err(_) => (
                    TungsteniteMessage::Binary(bytes),
                    policy_applied.map(|_| false),
                ),
            },
        }
    }
}

fn normalize_openai_reasoning_effort(value: &str) -> Option<&'static str> {
    match value
        .trim()
        .to_ascii_lowercase()
        .replace(['-', '_', ' '], "")
        .as_str()
    {
        "minimal" => Some("minimal"),
        "low" => Some("low"),
        "medium" => Some("medium"),
        "high" => Some("high"),
        "xhigh" | "extrahigh" => Some("xhigh"),
        "max" => Some("max"),
        _ => None,
    }
}

fn openai_reasoning_effort_rank(value: &str) -> Option<u8> {
    match normalize_openai_reasoning_effort(value)? {
        "minimal" => Some(1),
        "low" => Some(2),
        "medium" => Some(3),
        "high" => Some(4),
        "xhigh" => Some(5),
        "max" => Some(6),
        _ => None,
    }
}

fn apply_openai_reasoning_effort_policy_value(
    value: &mut Value,
    max_effort: Option<&str>,
    mappings: &[ReasoningEffortMapping],
) -> bool {
    let max_rank = max_effort.and_then(openai_reasoning_effort_rank);
    let Some(request) = value.as_object_mut() else {
        return false;
    };
    let mut changed = false;
    for path in [["reasoning", "effort"], ["reasoning_effort", ""]] {
        let field = if path[1].is_empty() {
            request.get_mut(path[0])
        } else {
            request
                .get_mut(path[0])
                .and_then(Value::as_object_mut)
                .and_then(|nested| nested.get_mut(path[1]))
        };
        let Some(field) = field else { continue };
        let Some(original) = field.as_str().map(str::trim) else {
            continue;
        };
        let Some(canonical) = normalize_openai_reasoning_effort(original) else {
            continue;
        };
        let mut effective = mappings
            .iter()
            .find(|mapping| normalize_openai_reasoning_effort(&mapping.from) == Some(canonical))
            .and_then(|mapping| normalize_openai_reasoning_effort(&mapping.to))
            .unwrap_or(canonical);
        if let (Some(limit), Some(rank)) = (max_rank, openai_reasoning_effort_rank(effective)) {
            if rank > limit {
                effective = normalize_openai_reasoning_effort(max_effort.unwrap_or_default())
                    .unwrap_or(effective);
            }
        }
        if effective != original {
            *field = Value::String(effective.to_string());
            changed = true;
        }
    }
    changed
}

fn apply_openai_prompt_cache_creation_optimization_value(value: &mut Value, mode: &str) {
    let Some(request) = value.as_object_mut() else {
        return;
    };
    remove_openai_prompt_cache_breakpoints(request);
    request.remove("prompt_cache_retention");
    let prompt_cache_options = if mode == "suppress" {
        serde_json::json!({"mode": "explicit"})
    } else {
        serde_json::json!({
            "mode": "explicit",
            "ttl": OPENAI_PROMPT_CACHE_CREATION_OPTIMIZATION_TTL
        })
    };
    request.insert("prompt_cache_options".to_string(), prompt_cache_options);
    if mode == "reduce" {
        insert_openai_responses_stable_prefix_breakpoint(request);
    }
}

fn remove_openai_prompt_cache_breakpoints(request: &mut serde_json::Map<String, Value>) {
    request.remove("prompt_cache_breakpoint");
    for field in ["input", "messages", "instructions"] {
        let Some(items) = request.get_mut(field).and_then(Value::as_array_mut) else {
            continue;
        };
        for raw_item in items {
            let Some(item) = raw_item.as_object_mut() else {
                continue;
            };
            item.remove("prompt_cache_breakpoint");
            match item.get_mut("content") {
                Some(Value::Object(content)) => {
                    content.remove("prompt_cache_breakpoint");
                }
                Some(Value::Array(content)) => {
                    for raw_part in content {
                        if let Some(part) = raw_part.as_object_mut() {
                            part.remove("prompt_cache_breakpoint");
                        }
                    }
                }
                _ => {}
            }
        }
    }
}

fn insert_openai_responses_stable_prefix_breakpoint(request: &mut serde_json::Map<String, Value>) {
    let mut stable_bytes = openai_prompt_cache_top_level_static_bytes(request);
    let Some(input) = request.get_mut("input").and_then(Value::as_array_mut) else {
        return;
    };
    let mut target: Option<(usize, OpenAIPromptCacheStableTarget)> = None;
    for (item_index, raw_item) in input.iter().enumerate() {
        let Some(item) = raw_item.as_object() else {
            break;
        };
        let role = item
            .get("role")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .trim()
            .to_ascii_lowercase();
        if role != "system" && role != "developer" {
            break;
        }
        let (content_target, size, safe) = openai_responses_stable_content_target(item);
        stable_bytes = stable_bytes.saturating_add(size);
        if let Some(content_target) = content_target {
            target = Some((item_index, content_target));
        }
        if !safe {
            break;
        }
    }
    if stable_bytes < OPENAI_PROMPT_CACHE_EXPLICIT_MIN_STATIC_BYTES {
        return;
    }
    let Some((item_index, content_target)) = target else {
        return;
    };
    let Some(item) = input.get_mut(item_index).and_then(Value::as_object_mut) else {
        return;
    };
    let part = match content_target {
        OpenAIPromptCacheStableTarget::Existing(content_index) => item
            .get_mut("content")
            .and_then(Value::as_array_mut)
            .and_then(|content| content.get_mut(content_index))
            .and_then(Value::as_object_mut),
        OpenAIPromptCacheStableTarget::String(text) => {
            item.insert(
                "content".to_string(),
                serde_json::json!([{"type": "input_text", "text": text}]),
            );
            item.get_mut("content")
                .and_then(Value::as_array_mut)
                .and_then(|content| content.first_mut())
                .and_then(Value::as_object_mut)
        }
    };
    let Some(part) = part else {
        return;
    };
    part.insert(
        "prompt_cache_breakpoint".to_string(),
        serde_json::json!({"mode": "explicit"}),
    );
}

enum OpenAIPromptCacheStableTarget {
    Existing(usize),
    String(String),
}

fn openai_responses_stable_content_target(
    item: &serde_json::Map<String, Value>,
) -> (Option<OpenAIPromptCacheStableTarget>, usize, bool) {
    let Some(content) = item.get("content") else {
        return (None, 0, false);
    };
    if let Some(text) = content.as_str() {
        if text.trim().is_empty() {
            return (None, 0, true);
        }
        let size = text.len();
        return (
            Some(OpenAIPromptCacheStableTarget::String(text.to_string())),
            size,
            true,
        );
    }
    let Some(parts) = content.as_array() else {
        return (None, 0, false);
    };
    let mut target = None;
    let mut size = 0usize;
    for (index, part) in parts.iter().enumerate() {
        let Some(part_type) = part.get("type").and_then(Value::as_str) else {
            return (target, size, false);
        };
        if !["input_text", "input_image", "input_file"]
            .iter()
            .any(|supported| part_type.eq_ignore_ascii_case(supported))
        {
            return (target, size, false);
        }
        size = size.saturating_add(serde_json::to_vec(part).map(|v| v.len()).unwrap_or(0));
        target = Some(OpenAIPromptCacheStableTarget::Existing(index));
    }
    (target, size, true)
}

fn openai_prompt_cache_top_level_static_bytes(request: &serde_json::Map<String, Value>) -> usize {
    let mut total = request
        .get("instructions")
        .and_then(Value::as_str)
        .map(str::len)
        .unwrap_or(0);
    for field in ["tools", "functions", "response_format", "text"] {
        if let Some(value) = request.get(field) {
            total = total.saturating_add(
                serde_json::to_vec(value)
                    .map(|encoded| encoded.len())
                    .unwrap_or(0),
            );
        }
    }
    total
}

fn is_openai_ws_image_generation_intent(value: &Value) -> bool {
    openai_json_tools_contain_image_generation(value.get("tools"))
        || value
            .get("input")
            .and_then(Value::as_array)
            .is_some_and(|input| input.iter().any(openai_json_is_explicit_image_tool_call))
        || openai_json_tool_choice_selects_image_generation(value.get("tool_choice"))
}

fn openai_json_tools_contain_image_generation(value: Option<&Value>) -> bool {
    value
        .and_then(Value::as_array)
        .is_some_and(|tools| tools.iter().any(openai_json_is_image_generation_tool))
}

fn openai_json_is_image_generation_tool(tool: &Value) -> bool {
    let kind = tool
        .get("type")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .trim();
    kind.eq_ignore_ascii_case("image_generation")
}

fn openai_json_is_explicit_image_tool_call(item: &Value) -> bool {
    let kind = item
        .get("type")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .trim();
    if !["function_call", "custom_tool_call", "tool_call"]
        .iter()
        .any(|candidate| kind.eq_ignore_ascii_case(candidate))
    {
        return false;
    }
    let namespace = item
        .get("namespace")
        .and_then(Value::as_str)
        .unwrap_or_default();
    let name = item
        .get("name")
        .and_then(Value::as_str)
        .or_else(|| item.pointer("/function/name").and_then(Value::as_str))
        .unwrap_or_default();
    openai_json_is_image_function_reference(namespace, name)
}

fn openai_json_is_image_function_reference(namespace: &str, name: &str) -> bool {
    let namespace = namespace.trim();
    let name = name.trim();
    (namespace.eq_ignore_ascii_case("image_gen") && name.eq_ignore_ascii_case("imagegen"))
        || name.eq_ignore_ascii_case("image_gen.imagegen")
        || name.eq_ignore_ascii_case("image_gen__imagegen")
}

fn openai_json_tool_choice_selects_image_generation(value: Option<&Value>) -> bool {
    let Some(value) = value else {
        return false;
    };
    if let Some(choice) = value.as_str() {
        return choice.trim().eq_ignore_ascii_case("image_generation");
    }
    let Some(choice) = value.as_object() else {
        return false;
    };
    let kind = choice
        .get("type")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .trim();
    if kind.eq_ignore_ascii_case("image_generation") {
        return true;
    }
    if kind.eq_ignore_ascii_case("namespace")
        && ["name", "namespace"].iter().any(|field| {
            choice
                .get(*field)
                .and_then(Value::as_str)
                .is_some_and(|name| name.trim().eq_ignore_ascii_case("image_gen"))
        })
    {
        return true;
    }
    let namespace = choice
        .get("namespace")
        .and_then(Value::as_str)
        .unwrap_or_default();
    let name = choice
        .get("name")
        .and_then(Value::as_str)
        .unwrap_or_default();
    if openai_json_is_image_function_reference(namespace, name) {
        return true;
    }
    if openai_json_tool_choice_selects_image_generation(choice.get("tool")) {
        return true;
    }
    choice
        .get("function")
        .and_then(Value::as_object)
        .is_some_and(|function| {
            let namespace = function
                .get("namespace")
                .and_then(Value::as_str)
                .unwrap_or_default();
            let name = function
                .get("name")
                .and_then(Value::as_str)
                .unwrap_or_default();
            name.trim().eq_ignore_ascii_case("image_generation")
                || openai_json_is_image_function_reference(namespace, name)
        })
}

fn is_openai_gpt56_model(model: &str) -> bool {
    let mut normalized = model
        .trim()
        .rsplit('/')
        .next()
        .unwrap_or_default()
        .replace('_', "-")
        .split_whitespace()
        .collect::<Vec<_>>()
        .join("-")
        .to_ascii_lowercase();
    while normalized.contains("--") {
        normalized = normalized.replace("--", "-");
    }
    if let Some(suffix) = normalized.strip_prefix("gpt5") {
        normalized = format!("gpt-5{suffix}");
    }
    for (from, to) in [
        ("gpt-5.6sol", "gpt-5.6-sol"),
        ("gpt-5.6terra", "gpt-5.6-terra"),
        ("gpt-5.6luna", "gpt-5.6-luna"),
    ] {
        normalized = normalized.replace(from, to);
    }
    if normalized == "gpt-5.6" {
        return true;
    }
    for prefix in ["gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"] {
        if normalized == prefix || normalized.starts_with(&format!("{prefix}-")) {
            return true;
        }
    }
    let Some(suffix) = normalized.strip_prefix("gpt-5.6-") else {
        return false;
    };
    matches!(
        suffix,
        "max" | "none" | "minimal" | "low" | "medium" | "high" | "xhigh"
    ) || is_openai_codex_date_suffix(suffix)
}

fn is_openai_codex_date_suffix(suffix: &str) -> bool {
    let parts = suffix.split('-').collect::<Vec<_>>();
    parts.len() == 3
        && parts[0].len() == 4
        && parts[1].len() == 2
        && parts[2].len() == 2
        && parts
            .iter()
            .all(|part| part.bytes().all(|byte| byte.is_ascii_digit()))
}

fn openai_cache_creation_policy_unsupported(status: StatusCode, error_body: &str) -> bool {
    status.is_client_error() && openai_cache_creation_policy_error_text(error_body)
}

fn openai_ws_cache_creation_policy_unsupported(message: &TungsteniteMessage) -> bool {
    let bytes = match message {
        TungsteniteMessage::Text(text) => text.as_bytes(),
        TungsteniteMessage::Binary(bytes) => bytes.as_slice(),
        _ => return false,
    };
    let Ok(value) = serde_json::from_slice::<Value>(bytes) else {
        return false;
    };
    let event_type = value
        .get("type")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase();
    let is_error_event = event_type == "error"
        || event_type == "response.failed"
        || value.get("error").is_some_and(|error| !error.is_null())
        || value
            .pointer("/response/error")
            .is_some_and(|error| !error.is_null());
    is_error_event
        && std::str::from_utf8(bytes)
            .ok()
            .is_some_and(openai_cache_creation_policy_error_text)
}

#[derive(Default)]
struct OpenAIWSFailureState {
    cache_policy_compatibility_failure: bool,
    other_failure: bool,
}

impl OpenAIWSFailureState {
    fn observe_upstream_message(
        &mut self,
        message: &TungsteniteMessage,
        cache_policy_applied: bool,
    ) -> bool {
        if !openai_ws_message_is_failure(message) {
            return false;
        }
        if cache_policy_applied && openai_ws_cache_creation_policy_unsupported(message) {
            self.cache_policy_compatibility_failure = true;
        } else {
            self.other_failure = true;
        }
        true
    }

    fn mark_other_failure(&mut self) {
        self.other_failure = true;
    }

    fn is_cache_policy_only(&self) -> bool {
        self.cache_policy_compatibility_failure && !self.other_failure
    }
}

fn openai_ws_message_is_failure(message: &TungsteniteMessage) -> bool {
    let value = match message {
        TungsteniteMessage::Text(text) => serde_json::from_str::<Value>(text).ok(),
        // Binary frames are never passed through by the sanitizer, so they
        // must not leave the session eligible for a successful completion.
        TungsteniteMessage::Binary(_) => return true,
        _ => return false,
    };
    let Some(value) = value else {
        return true;
    };
    matches!(json_event_type(&value), Some("error" | "response.failed"))
        || json_is_unsafe_upstream_diagnostic(&value)
}

fn tungstenite_message_json(message: &TungsteniteMessage) -> Option<Value> {
    match message {
        TungsteniteMessage::Text(text) => serde_json::from_str::<Value>(text).ok(),
        TungsteniteMessage::Binary(bytes) => serde_json::from_slice::<Value>(bytes).ok(),
        _ => None,
    }
}

fn tungstenite_message_is_response_create(message: &TungsteniteMessage) -> bool {
    tungstenite_message_json(message)
        .as_ref()
        .and_then(json_event_type)
        == Some("response.create")
}

fn openai_ws_explicit_rejected_field_error(message: &TungsteniteMessage) -> Option<Value> {
    let value = tungstenite_message_json(message)?;
    if !matches!(json_event_type(&value), Some("error" | "response.failed")) {
        return None;
    }
    let error = value
        .get("error")
        .filter(|candidate| candidate.is_object())
        .or_else(|| value.pointer("/response/error"))?;
    let code = error
        .get("code")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase();
    let error_message = error
        .get("message")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase();
    let explicit = matches!(code.as_str(), "unknown_parameter" | "unsupported_parameter")
        || error_message.contains("unknown parameter")
        || error_message.contains("unsupported parameter");
    if !explicit {
        return None;
    }
    let param = error
        .get("param")
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|param| !param.is_empty())
        .map(str::to_ascii_lowercase)
        .or_else(|| openai_rejected_field_from_message(&error_message))?;
    if openai_rejected_field_supported(&param) {
        Some(value)
    } else {
        None
    }
}

fn openai_rejected_field_from_message(message: &str) -> Option<String> {
    if message.contains("max_output_tokens") {
        return Some("max_output_tokens".to_string());
    }
    let start = message.find("input[")?;
    let suffix = &message[start + "input[".len()..];
    let end = suffix.find(']')?;
    if end == 0 || !suffix[..end].bytes().all(|byte| byte.is_ascii_digit()) {
        return None;
    }
    let candidate = format!("input[{}].namespace", &suffix[..end]);
    suffix[end + 1..]
        .starts_with(".namespace")
        .then_some(candidate)
}

fn openai_rejected_field_supported(param: &str) -> bool {
    if param == "max_output_tokens" {
        return true;
    }
    let Some(index) = param
        .strip_prefix("input[")
        .and_then(|suffix| suffix.strip_suffix("].namespace"))
    else {
        return false;
    };
    !index.is_empty() && index.bytes().all(|byte| byte.is_ascii_digit())
}

fn openai_cache_creation_policy_error_text(error_body: &str) -> bool {
    openai_cache_creation_policy_error_candidates(error_body)
        .iter()
        .any(|candidate| openai_cache_creation_policy_error_candidate_unsupported(candidate))
}

fn openai_cache_creation_policy_error_candidates(error_body: &str) -> Vec<String> {
    let Ok(value) = serde_json::from_str::<Value>(error_body) else {
        return if error_body.len() <= 4 * 1024 {
            vec![error_body.to_string()]
        } else {
            Vec::new()
        };
    };
    let Some(root) = value.as_object() else {
        return value
            .as_str()
            .map(|message| vec![message.to_string()])
            .unwrap_or_default();
    };

    let mut candidates = Vec::with_capacity(5);
    append_openai_cache_creation_policy_error_object(&mut candidates, root);
    match root.get("detail") {
        Some(Value::String(detail)) => candidates.push(detail.clone()),
        Some(Value::Array(details)) => {
            append_openai_cache_creation_policy_error_list(&mut candidates, details)
        }
        _ => {}
    }
    if let Some(errors) = root.get("errors").and_then(Value::as_array) {
        append_openai_cache_creation_policy_error_list(&mut candidates, errors);
    }
    match root.get("error") {
        Some(Value::String(message)) => candidates.push(message.clone()),
        Some(Value::Object(error)) => {
            append_openai_cache_creation_policy_error_object(&mut candidates, error)
        }
        _ => {}
    }
    if let Some(response) = root.get("response").and_then(Value::as_object) {
        match response.get("error") {
            Some(Value::String(message)) => candidates.push(message.clone()),
            Some(Value::Object(error)) => {
                append_openai_cache_creation_policy_error_object(&mut candidates, error)
            }
            _ => {}
        }
    }
    candidates
}

fn append_openai_cache_creation_policy_error_object(
    candidates: &mut Vec<String>,
    object: &serde_json::Map<String, Value>,
) {
    let mut parts = ["message", "msg", "detail", "type", "code", "param"]
        .iter()
        .filter_map(|field| object.get(*field).and_then(Value::as_str))
        .filter(|value| !value.trim().is_empty())
        .collect::<Vec<_>>();
    if let Some(location) = object.get("loc").and_then(Value::as_array) {
        parts.extend(
            location
                .iter()
                .filter_map(Value::as_str)
                .filter(|value| !value.trim().is_empty()),
        );
    }
    if !parts.is_empty() {
        candidates.push(parts.join(" "));
    }
}

fn append_openai_cache_creation_policy_error_list(candidates: &mut Vec<String>, list: &[Value]) {
    for item in list {
        if let Some(object) = item.as_object() {
            append_openai_cache_creation_policy_error_object(candidates, object);
        }
    }
}

fn openai_cache_creation_policy_error_candidate_unsupported(candidate: &str) -> bool {
    const CONTEXT_BYTES: usize = 512;
    let text = candidate.to_ascii_lowercase().replace(['_', '-'], " ");
    let bytes = text.as_bytes();
    for field in ["prompt cache options", "prompt cache breakpoint"] {
        for (field_start, _) in text.match_indices(field) {
            let window_start = field_start.saturating_sub(CONTEXT_BYTES);
            let window_end = bytes.len().min(field_start + field.len() + CONTEXT_BYTES);
            let window = &bytes[window_start..window_end];
            if [
                "unsupported parameter",
                "unknown parameter",
                "unrecognized parameter",
                "invalid parameter",
                "unknown field",
                "unrecognized field",
                "unexpected field",
                "extra inputs are not permitted",
                "additional properties are not allowed",
                "not permitted",
                "not allowed",
                "not supported",
                "unsupported",
                "invalid value",
                "invalid field",
                "must be",
            ]
            .iter()
            .any(|marker| {
                let marker = marker.as_bytes();
                window.windows(marker.len()).any(|part| part == marker)
            }) {
                return true;
            }
        }
    }
    false
}

async fn relay_upstream(
    state: AppState,
    mut plan: EdgePlan,
    started_at: Instant,
    timing: EdgeTiming,
    timing_shared: Option<Arc<Mutex<EdgeTiming>>>,
    allow_initial_queue: bool,
    mut ingress_permit: Option<OwnedSemaphorePermit>,
) -> anyhow::Result<Response> {
    if allow_initial_queue {
        if let Some(domain_permit) = state.relay_permit_for_plan(&plan)? {
            let request_body = take_request_body_bytes(&mut plan)?;
            let queued_bytes = request_body.len().max(1);
            if queued_bytes > state.cfg.queue_max_bytes {
                let _domain_permit = domain_permit;
                let ingress_permit = ingress_permit
                    .take()
                    .ok_or_else(|| anyhow::anyhow!("missing edge ingress permit"))?;
                // This request bypasses the worker, so keep an abort guard
                // around the direct future until a response is committed.
                let lease_abort_guard = LeaseAbortGuard::new(
                    state.clone(),
                    plan.edge_request_id.clone(),
                    plan.lease_id.clone(),
                    plan.account_id,
                    "http_request_dropped_before_response",
                    true,
                );
                let result = relay_upstream_direct(
                    state,
                    plan,
                    request_body,
                    RelayAttemptContext {
                        started_at,
                        timing,
                        timing_shared,
                        relay_attempted_marker: Some(lease_abort_guard.relay_attempted_marker()),
                        ingress_permit: Some(ingress_permit),
                    },
                )
                .await;
                lease_abort_guard.mark_done();
                return result;
            }
            let queue_bytes_permit = try_reserve_relay_queue_bytes(
                state.relay_queue_bytes.clone(),
                state.cfg.queue_max_bytes,
                queued_bytes,
            )?;
            let (response_tx, response_rx) = oneshot::channel();
            let job = RelayJob {
                state: state.clone(),
                plan,
                request_body,
                started_at,
                enqueued_at: Instant::now(),
                timing,
                timing_shared,
                response_tx,
                _domain_permit: domain_permit,
                _queue_bytes_permit: queue_bytes_permit,
                ingress_permit: ingress_permit
                    .take()
                    .ok_or_else(|| anyhow::anyhow!("missing edge ingress permit"))?,
            };
            state
                .relay_tx
                .try_send(job)
                .map_err(|err| anyhow::anyhow!("edge relay queue full or closed: {err}"))?;
            return response_rx
                .await
                .map_err(|err| anyhow::anyhow!("edge relay worker dropped response: {err}"))?;
        }
    }
    let ingress_permit = ingress_permit;
    let request_body = take_request_body_bytes(&mut plan)?;
    let lease_abort_guard = if allow_initial_queue {
        Some(LeaseAbortGuard::new(
            state.clone(),
            plan.edge_request_id.clone(),
            plan.lease_id.clone(),
            plan.account_id,
            "http_request_dropped_before_response",
            true,
        ))
    } else {
        None
    };
    let result = relay_upstream_direct(
        state,
        plan,
        request_body,
        RelayAttemptContext {
            started_at,
            timing,
            timing_shared,
            relay_attempted_marker: lease_abort_guard
                .as_ref()
                .map(LeaseAbortGuard::relay_attempted_marker),
            ingress_permit,
        },
    )
    .await;
    if let Some(lease_abort_guard) = lease_abort_guard {
        lease_abort_guard.mark_done();
    }
    result
}

fn try_reserve_relay_queue_bytes(
    semaphore: Arc<Semaphore>,
    max_bytes: usize,
    queued_bytes: usize,
) -> anyhow::Result<OwnedSemaphorePermit> {
    if queued_bytes == 0 || queued_bytes > max_bytes || queued_bytes > u32::MAX as usize {
        anyhow::bail!("edge relay queue byte budget exhausted");
    }
    semaphore
        .try_acquire_many_owned(queued_bytes as u32)
        .map_err(|_| anyhow::anyhow!("edge relay queue byte budget exhausted"))
}

async fn run_relay_executor(state: AppState, mut receiver: mpsc::Receiver<RelayJob>) {
    let concurrency = Arc::new(Semaphore::new(state.cfg.global_workers.max(1)));
    while let Some(job) = receiver.recv().await {
        let permit = match concurrency.clone().acquire_owned().await {
            Ok(permit) => permit,
            Err(_) => return,
        };
        tokio::spawn(async move {
            let _permit = permit;
            let RelayJob {
                state: job_state,
                plan: job_plan,
                request_body: job_request_body,
                started_at: job_started_at,
                enqueued_at: job_enqueued_at,
                timing: mut job_timing,
                timing_shared: job_timing_shared,
                mut response_tx,
                _domain_permit,
                _queue_bytes_permit,
                ingress_permit,
            } = job;
            let _worker_guard = job_state.metrics.begin_relay_work();
            let lease_abort_guard = LeaseAbortGuard::new(
                job_state.clone(),
                job_plan.edge_request_id.clone(),
                job_plan.lease_id.clone(),
                job_plan.account_id,
                "http_request_dropped_before_response",
                true,
            );
            let relay_attempted_marker = lease_abort_guard.relay_attempted_marker();
            let retry_relay_attempted_marker = Arc::clone(&relay_attempted_marker);
            let queue_wait_ms = job_enqueued_at.elapsed().as_millis() as i64;
            job_timing.queue_wait_ms = Some(queue_wait_ms);
            update_edge_timing(job_timing_shared.as_ref(), |shared| {
                shared.queue_wait_ms = Some(queue_wait_ms);
                shared.retry_count = job_timing.retry_count;
            });
            let result_future = async move {
                if job_state.cfg.queue_wait_budget_ms > 0
                    && queue_wait_ms > job_state.cfg.queue_wait_budget_ms as i64
                {
                    retry_after_queue_wait_budget(
                        job_state,
                        job_plan,
                        queue_wait_ms,
                        RelayAttemptContext {
                            started_at: job_started_at,
                            timing: job_timing,
                            timing_shared: job_timing_shared,
                            relay_attempted_marker: Some(retry_relay_attempted_marker),
                            ingress_permit: Some(ingress_permit),
                        },
                    )
                    .await
                } else {
                    relay_upstream_direct(
                        job_state,
                        job_plan,
                        job_request_body,
                        RelayAttemptContext {
                            started_at: job_started_at,
                            timing: job_timing,
                            timing_shared: job_timing_shared,
                            relay_attempted_marker: Some(relay_attempted_marker),
                            ingress_permit: Some(ingress_permit),
                        },
                    )
                    .await
                }
            };
            tokio::pin!(result_future);
            tokio::select! {
                result = &mut result_future => {
                    if response_tx.send(result).is_ok() {
                        lease_abort_guard.mark_done();
                    }
                }
                _ = response_tx.closed() => {
                    // The caller has gone away. Dropping the relay future here
                    // releases any lane/transient permit it may be waiting on;
                    // LeaseAbortGuard below also settles the control-plane lease.
                }
            }
        });
    }
}

async fn relay_upstream_direct(
    state: AppState,
    plan: EdgePlan,
    request_body: Vec<u8>,
    context: RelayAttemptContext,
) -> anyhow::Result<Response> {
    let RelayAttemptContext {
        started_at,
        mut timing,
        timing_shared,
        relay_attempted_marker,
        ingress_permit,
    } = context;
    let lease_renewal_guard = LeaseRenewalGuard::start(&state, &plan);
    if timing.relay_start_ms.is_none() {
        timing.relay_start_ms = Some(started_at.elapsed().as_millis() as i64);
    }
    update_edge_timing(timing_shared.as_ref(), |shared| {
        shared.relay_start_ms = timing.relay_start_ms;
        shared.queue_wait_ms = timing.queue_wait_ms;
        shared.retry_count = timing.retry_count;
    });
    state.record_dynamic_warm_key(&plan);
    let upstream_url = plan
        .upstream_url
        .clone()
        .ok_or_else(|| anyhow::anyhow!("missing upstream_url"))?;
    if let Some(transport) = &plan.transport {
        if transport != "http2_sse" {
            anyhow::bail!("unsupported edge transport {transport}");
        }
    }

    if let Some(dialect) = &plan.response_dialect {
        if dialect != "chat_completions" && dialect != "responses" {
            anyhow::bail!("unsupported response dialect {dialect}");
        }
    }
    let low_latency_policy = low_latency_policy(plan.low_latency_mode.as_deref());
    if let Some(mode) = &plan.low_latency_mode {
        if !mode.is_empty() {
            info!("edge relay low_latency_mode={mode}");
        }
    }

    let edge_request_id = plan.edge_request_id.clone();
    let lease_id = plan.lease_id.clone();
    let account_id = plan.account_id;
    let edge_prepare_ms = timing.prepare_ms;
    let edge_queue_wait_ms = timing.queue_wait_ms;
    let edge_relay_start_ms = timing.relay_start_ms;
    let edge_fallback_reason = timing.fallback_reason.clone();
    let edge_retry_count = timing.retry_count;
    let complete_state = state.clone();
    let sse_comment_preflush = plan.sse_comment_preflush;
    let preamble_flush = plan.preamble_flush;
    let safe_token_placeholder = plan.safe_token_placeholder;
    let cache_creation_policy_applied = plan.prompt_cache_creation_optimization_applied;
    let first_token_timeout_placeholder =
        normalize_first_token_timeout_placeholder_ms(plan.first_token_timeout_placeholder_ms);
    let response_dialect = plan.response_dialect.clone();
    let send_state = state.clone();
    let send_url = upstream_url.clone();
    let send_proxy_url = plan.proxy_url.clone();
    let send_lane = plan.lane.clone();
    let send_account_id = plan.account_id;
    let send_account_type = plan.account_type.clone();
    let send_retry_count = timing.retry_count;
    let send_headers = plan.headers.clone();
    let header_start = Instant::now();
    let mut upstream_send = Box::pin(async move {
        // Client/lane selection is part of the header and placeholder race.
        // A resource-bounded proxy-capacity wait therefore cannot move an account's
        // configured first-token placeholder past its request-relative limit.
        let mut selected = send_state
            .upstream_client_for_plan(
                send_account_id,
                send_account_type.as_deref(),
                send_proxy_url.as_deref(),
                &send_url,
                send_lane.as_deref(),
            )
            .await?;
        let mut request = selected.client.post(&send_url);
        if let Some(headers) = &send_headers {
            for (name, value) in headers {
                request = request.header(name, value);
            }
        }
        request = request.header(
            header::ACCEPT_ENCODING,
            HeaderValue::from_static("identity"),
        );
        send_state.metrics.record_upstream_attempt(send_retry_count);
        let response_future = request.body(request_body).send();
        if let Some(marker) = &relay_attempted_marker {
            marker.store(true, Ordering::SeqCst);
        }
        let response = match response_future.await {
            Ok(response) => response,
            Err(error) => {
                send_state.metrics.record_upstream_send_error(&error);
                selected.guard.release();
                return Err(anyhow::Error::from(error));
            }
        };
        let mut upstream_client_guard = selected.guard;
        let version = response.version();
        upstream_client_guard.mark_headers(version);
        send_state
            .metrics
            .record_upstream_response(response.status(), version);
        Ok::<_, anyhow::Error>((response, upstream_client_guard, ingress_permit))
    });
    let (upstream, mut upstream_client_guard, ingress_permit) = if let Some(timeout) =
        first_token_timeout_placeholder
    {
        tokio::select! {
            result = &mut upstream_send => result?,
            // Match the Go request-header race and the pre-lane Edge path:
            // this account setting starts when the upstream request is sent,
            // so local admission or relay queue time cannot consume it.
            _ = tokio::time::sleep(timeout) => {
                let stream_guard = complete_state.metrics.begin_stream();
                // Construct this before building the body stream. Axum may
                // drop a response body after committing headers but before
                // polling it; keeping the completion guard outside the
                // generator ensures that cancellation still settles the
                // lease instead of waiting for its TTL.
                let complete_guard = ClientDisconnectCompleteGuard::new(
                    complete_state.clone(),
                    started_at,
                    pending_stream_complete_request(
                        edge_request_id.clone(),
                        lease_id.clone(),
                        account_id,
                        edge_prepare_ms,
                        edge_queue_wait_ms,
                        edge_relay_start_ms,
                        edge_fallback_reason.clone(),
                        edge_retry_count,
                    ),
                );
                let early_body_stream = stream! {
                    let _stream_guard = stream_guard;
                    let _lease_renewal_guard = lease_renewal_guard;
                    let guard = complete_guard;
                    let mut first_byte_ms: Option<i64> = None;
                    let first_flush_ms: Option<i64> =
                        Some(started_at.elapsed().as_millis() as i64);
                    let mut first_token_ms: Option<i64> = None;
                    let mut real_first_token_ms: Option<i64> = None;
                    let mut success = true;
                    let mut error_message: Option<String> = None;
                let mut upstream_status_code: Option<u16> = None;
                let mut upstream_header_ms: Option<i64> = None;
                let mut summary = ChatStreamSummary::with_pending(
                    complete_state.pools.take_sse_string(),
                    response_dialect.as_deref(),
                );
                let mut sanitizer = OpenAIStreamSanitizer::new(response_dialect.as_deref());
                // Older Go plans did not carry `preamble_flush`; their Edge
                // behavior was to forward Responses preamble events
                // immediately. The plan decoder defaults that field to true,
                // while an explicit false enables this gate.
                let mut preamble_gate = SsePreambleGate::new(
                    preamble_flush || response_dialect.as_deref() != Some("responses"),
                );
                    // Commit an account-requested SSE preflush before the
                    // timeout placeholder. This branch is reached only when
                    // the configured timeout wins the header race, so the
                    // comment and placeholder share the same response start.
                    if sse_comment_preflush {
                        // A local preflush is already client-visible, so it
                        // also releases any upstream preamble held by the
                        // account-level gate.
                        for output in preamble_gate.force() {
                            yield Ok::<Bytes, std::io::Error>(output);
                        }
                        yield Ok::<Bytes, std::io::Error>(Bytes::from_static(b":\n\n"));
                    }
                    let placeholder =
                        openai_stream_timeout_placeholder_frame(response_dialect.as_deref(), &summary);
                    for output in preamble_gate.force() {
                        yield Ok::<Bytes, std::io::Error>(output);
                    }
                    yield Ok::<Bytes, std::io::Error>(Bytes::from(placeholder));

                    let (upstream, mut upstream_client_guard, ingress_permit) = match upstream_send.await {
                        Ok(upstream) => upstream,
                        Err(err) => {
                            success = false;
                            error_message = Some(err.to_string());
                            guard.update_stream_snapshot(
                                &summary,
                                success,
                                error_message.clone(),
                                upstream_header_ms,
                                first_byte_ms,
                                first_token_ms,
                                real_first_token_ms,
                                first_flush_ms,
                                upstream_status_code,
                                true,
                            );
                            let frame = openai_stream_error_frame(
                                response_dialect.as_deref(),
                                summary.model.as_deref(),
                                "Upstream request failed",
                            );
                            yield Ok::<Bytes, std::io::Error>(Bytes::from(frame));
                            complete_state.pools.recycle_sse_string(std::mem::take(&mut summary.pending));
                            if call_complete(&complete_state, CompleteRequest {
                                edge_request_id: edge_request_id.clone(),
                                lease_id: lease_id.clone(),
                                account_id,
                                success,
                                failure_class: Some("upstream_error".to_string()),
                                client_disconnected: false,
                                request_id: summary.request_id.clone(),
                                response_id: summary.response_id.clone(),
                                model: summary.model.clone(),
                                upstream_model: summary.upstream_model.clone(),
                                usage: summary.usage.clone(),
                                duration_ms: started_at.elapsed().as_millis() as i64,
                                upstream_header_ms,
                                upstream_first_byte_ms: first_byte_ms,
                                first_token_ms,
                                real_first_token_ms,
                                guard_sample_at_unix_ns: None,
                                first_client_flush_ms: first_flush_ms,
                                edge_prepare_ms,
                                edge_queue_wait_ms,
                                edge_relay_start_ms,
                                edge_fallback_reason: edge_fallback_reason.clone(),
                                edge_retry_count,
                                error_type: Some("request_error".to_string()),
                                error_message,
                                upstream_status_code,
                                terminal_event_type: None,
                                cyber_blocked: false,
                            }).await.is_ok() {
                                guard.mark_done();
                            }
                            return;
                        }
                    };

                    // The response headers end the bounded pre-response phase.
                    // Until this point the body-owned future retains the ingress
                    // permit even when a timeout placeholder was already sent.
                    drop(ingress_permit);

                    upstream_header_ms = Some(header_start.elapsed().as_millis() as i64);
                    let status = upstream.status();
                    upstream_status_code = Some(status.as_u16());
                    let headers = upstream.headers().clone();
                    summary.request_id = headers
                        .get("x-request-id")
                        .and_then(|v| v.to_str().ok())
                        .map(ToOwned::to_owned);
                    if status.is_client_error() || status.is_server_error() {
                        success = false;
                        let error_body = upstream
                            .bytes()
                            .await
                            .map(|b| String::from_utf8_lossy(&b).into_owned())
                            .unwrap_or_default();
                        upstream_client_guard.release();
                        let cyber_blocked = json_text_is_cyber_policy(&error_body);
                        let message = "Upstream request failed";
                        error_message = Some(message.to_string());
                        let error_type = if cache_creation_policy_applied
                            && openai_cache_creation_policy_unsupported(status, &error_body)
                        {
                            "cache_creation_optimization_unsupported"
                        } else {
                            "upstream_error"
                        };
                        summary.cyber_blocked = cyber_blocked;
                        guard.update_stream_snapshot(
                            &summary,
                            success,
                            error_message.clone(),
                            upstream_header_ms,
                            first_byte_ms,
                            first_token_ms,
                            real_first_token_ms,
                            first_flush_ms,
                            upstream_status_code,
                            true,
                        );
                        let frame = openai_stream_error_frame(
                            response_dialect.as_deref(),
                            summary.model.as_deref(),
                            message,
                        );
                        yield Ok::<Bytes, std::io::Error>(Bytes::from(frame));
                        complete_state.pools.recycle_sse_string(std::mem::take(&mut summary.pending));
                        if call_complete(&complete_state, CompleteRequest {
                            edge_request_id: edge_request_id.clone(),
                            lease_id: lease_id.clone(),
                            account_id,
                            success,
                            failure_class: Some("upstream_error".to_string()),
                            client_disconnected: false,
                            request_id: summary.request_id.clone(),
                            response_id: summary.response_id.clone(),
                            model: summary.model.clone(),
                            upstream_model: summary.upstream_model.clone(),
                            usage: summary.usage.clone(),
                            duration_ms: started_at.elapsed().as_millis() as i64,
                            upstream_header_ms,
                            upstream_first_byte_ms: first_byte_ms,
                            first_token_ms,
                            real_first_token_ms,
                            guard_sample_at_unix_ns: None,
                            first_client_flush_ms: first_flush_ms,
                            edge_prepare_ms,
                            edge_queue_wait_ms,
                            edge_relay_start_ms,
                            edge_fallback_reason: edge_fallback_reason.clone(),
                            edge_retry_count,
                            error_type: Some(error_type.to_string()),
                            error_message,
                            upstream_status_code,
                            terminal_event_type: None,
                                cyber_blocked,
                        }).await.is_ok() {
                            guard.mark_done();
                        }
                        return;
                    }

                upstream_client_guard.mark_stream_open();
                let mut bytes_stream = upstream.bytes_stream();
                while let Some(next) = bytes_stream.next().await {
                    match next {
                        Ok(chunk) => {
                            if first_byte_ms.is_none() {
                                first_byte_ms = Some(header_start.elapsed().as_millis() as i64);
                            }
                            let observation = summary.observe(&chunk);
                            if real_first_token_ms.is_none() && observation.starts_real_output {
                                real_first_token_ms = Some(started_at.elapsed().as_millis() as i64);
                            }
                            if first_token_ms.is_none() && observation.starts_client_output {
                                first_token_ms = Some(started_at.elapsed().as_millis() as i64);
                            }
                            if summary.failed {
                                success = false;
                                error_message = Some("Upstream request failed".to_string());
                            }
                            guard.update_stream_snapshot(
                                &summary,
                                success,
                                error_message.clone(),
                                upstream_header_ms,
                                first_byte_ms,
                                first_token_ms,
                                real_first_token_ms,
                                first_flush_ms,
                                upstream_status_code,
                                summary.failed,
                            );
                            let sanitized = sanitizer.push(&chunk);
                            for output in preamble_gate.accept(
                                sanitized,
                                observation.starts_client_output,
                                summary.failed,
                            ) {
                                yield Ok::<Bytes, std::io::Error>(output);
                            }
                            if summary.completed_successfully(response_dialect.as_deref()) {
                                break;
                            }
                        }
                            Err(err) => {
                                if summary.completed_successfully(response_dialect.as_deref()) {
                                    break;
                                }
                                success = false;
                                error_message = Some(err.to_string());
                                guard.update_stream_snapshot(
                                    &summary,
                                    success,
                                    error_message.clone(),
                                    upstream_header_ms,
                                    first_byte_ms,
                                    first_token_ms,
                                    real_first_token_ms,
                                    first_flush_ms,
                                    upstream_status_code,
                                    true,
                                );
                                yield Err(std::io::Error::other(err.to_string()));
                                break;
                            }
                        }
                    }

                drop(bytes_stream);
                upstream_client_guard.release();
                let tail = sanitizer.finish();
                let tail_starts_output = !tail.is_empty();
                for output in preamble_gate.accept(tail, tail_starts_output, summary.failed) {
                    yield Ok::<Bytes, std::io::Error>(output);
                }
                // A response may legitimately complete without a text delta.
                // Never discard sanitized created/completed events merely
                // because account-level preamble flush was disabled.
                for output in preamble_gate.force() {
                    yield Ok::<Bytes, std::io::Error>(output);
                }
                if summary.failed {
                    success = false;
                    error_message = Some("Upstream request failed".to_string());
                } else if !summary.completed_successfully(response_dialect.as_deref()) {
                    success = false;
                    error_message = Some("Upstream stream ended before completion".to_string());
                }
                guard.update_stream_snapshot(
                    &summary,
                    success,
                    error_message.clone(),
                    upstream_header_ms,
                    first_byte_ms,
                    first_token_ms,
                    real_first_token_ms,
                    first_flush_ms,
                    upstream_status_code,
                    summary.failed || summary.terminal_event_type.is_none(),
                );
                let terminal_event_type = summary.terminal_event_type(response_dialect.as_deref());
                let request_id = summary.request_id.clone();
                    let response_id = summary.response_id.clone();
                    let model = summary.model.clone();
                    let upstream_model = summary.upstream_model.clone();
                    let usage = summary.usage.clone();
                    let cyber_blocked = summary.cyber_blocked;
                    complete_state.pools.recycle_sse_string(std::mem::take(&mut summary.pending));
                    if call_complete(&complete_state, CompleteRequest {
                        edge_request_id: edge_request_id.clone(),
                        lease_id: lease_id.clone(),
                        account_id,
                        success,
                        failure_class: classify_stream_failure(success, false, &summary),
                        client_disconnected: false,
                        request_id,
                        response_id,
                        model,
                        upstream_model,
                        usage,
                        duration_ms: started_at.elapsed().as_millis() as i64,
                        upstream_header_ms,
                        upstream_first_byte_ms: first_byte_ms,
                        first_token_ms,
                        real_first_token_ms,
                        guard_sample_at_unix_ns: None,
                        first_client_flush_ms: first_flush_ms,
                        edge_prepare_ms,
                        edge_queue_wait_ms,
                        edge_relay_start_ms,
                        edge_fallback_reason: edge_fallback_reason.clone(),
                        edge_retry_count,
                        error_type: if success { None } else { Some("stream_error".to_string()) },
                        error_message,
                        upstream_status_code,
                        terminal_event_type,
                        cyber_blocked,
                    }).await.is_ok() {
                        guard.mark_done();
                    }
            };

                let mut builder = Response::builder().status(StatusCode::OK);
                let headers = builder.headers_mut().expect("headers");
                headers.insert(header::CONTENT_TYPE, HeaderValue::from_static("text/event-stream"));
                headers.insert(
                    header::CACHE_CONTROL,
                    HeaderValue::from_static("no-cache, no-transform"),
                );
                headers.insert(HeaderName::from_static("x-accel-buffering"), HeaderValue::from_static("no"));
                return Ok(builder.body(Body::from_stream(early_body_stream))?);
            }
        }
    } else {
        upstream_send.await?
    };
    let upstream_header_ms = header_start.elapsed().as_millis() as i64;
    let status = upstream.status();
    let headers = upstream.headers().clone();
    if status.is_client_error() || status.is_server_error() {
        let upstream_request_id = headers
            .get("x-request-id")
            .and_then(|v| v.to_str().ok())
            .map(ToOwned::to_owned);
        let error_body = upstream
            .bytes()
            .await
            .map(|b| String::from_utf8_lossy(&b).into_owned())
            .unwrap_or_default();
        upstream_client_guard.release();
        if json_text_is_cyber_policy(&error_body) {
            if let Err(callback_err) = call_complete(
                &state,
                CompleteRequest {
                    edge_request_id: plan.edge_request_id.clone(),
                    lease_id: plan.lease_id.clone(),
                    account_id: plan.account_id,
                    success: false,
                    failure_class: Some("upstream_error".to_string()),
                    client_disconnected: false,
                    request_id: upstream_request_id,
                    response_id: None,
                    model: None,
                    upstream_model: None,
                    usage: Usage::default(),
                    duration_ms: started_at.elapsed().as_millis() as i64,
                    upstream_header_ms: Some(upstream_header_ms),
                    upstream_first_byte_ms: None,
                    first_token_ms: None,
                    real_first_token_ms: None,
                    guard_sample_at_unix_ns: None,
                    first_client_flush_ms: None,
                    edge_prepare_ms: timing.prepare_ms,
                    edge_queue_wait_ms: timing.queue_wait_ms,
                    edge_relay_start_ms: timing.relay_start_ms,
                    edge_fallback_reason: timing.fallback_reason.clone(),
                    edge_retry_count: timing.retry_count,
                    error_type: Some("safety_error".to_string()),
                    error_message: Some("Request blocked by safety policy".to_string()),
                    upstream_status_code: Some(status.as_u16()),
                    terminal_event_type: Some("response.failed".to_string()),
                    cyber_blocked: true,
                },
            )
            .await
            {
                error!(
                    "cyber completion could not be delivered or queued: {}",
                    safe_edge_error(&callback_err)
                );
            }
            return Ok(openai_error_response(
                StatusCode::FORBIDDEN,
                "safety_error",
                "Request blocked by safety policy",
            ));
        }
        let decision = call_retry(
            &state,
            RetryRequest {
                edge_request_id: plan.edge_request_id.clone(),
                lease_id: plan.lease_id.clone(),
                account_id: plan.account_id,
                upstream_status_code: Some(status.as_u16()),
                upstream_request_id,
                error_type: Some("upstream_error".to_string()),
                error_message: Some(error_body.clone()),
                request_body: None,
                response_body: if error_body.is_empty() {
                    None
                } else {
                    Some(Value::String(error_body))
                },
                wrote_client_response: false,
            },
        )
        .await?;

        if decision.action == "relay" {
            if let Some(next_plan) = decision.plan {
                let mut next_timing = timing.clone();
                next_timing.retry_count += 1;
                return Box::pin(relay_upstream(
                    state,
                    next_plan,
                    started_at,
                    next_timing,
                    timing_shared,
                    false,
                    ingress_permit,
                ))
                .await;
            }
            anyhow::bail!("retry decision missing relay plan");
        }
        if decision.action == "respond_error" {
            let mut reason = decision
                .reason
                .clone()
                .unwrap_or_else(|| "retry_respond_error".to_string());
            if decision.failure_recorded {
                reason = format!("retry_failure_already_recorded:{reason}");
            }
            if let Err(callback_err) = call_abort(
                &state,
                AbortRequest {
                    edge_request_id: plan.edge_request_id.clone(),
                    lease_id: plan.lease_id.clone(),
                    account_id: plan.account_id,
                    reason,
                    failure_class: "retry_exhausted".to_string(),
                    client_disconnected: false,
                    relay_attempted: true,
                    fallback_to_go: false,
                },
            )
            .await
            {
                error!(
                    "retry abort could not be delivered or queued: {}",
                    safe_edge_error(&callback_err)
                );
            }
            return Ok(openai_error_response(
                StatusCode::from_u16(decision.status_code.unwrap_or(400))
                    .unwrap_or(StatusCode::BAD_REQUEST),
                decision
                    .error_type
                    .as_deref()
                    .unwrap_or("invalid_request_error"),
                decision
                    .error_message
                    .as_deref()
                    .unwrap_or("Upstream pool rejected this request; check routing configuration"),
            ));
        }
        let reason = decision
            .reason
            .unwrap_or_else(|| "retry_fallback_go".to_string());
        if decision.failure_recorded {
            anyhow::bail!("retry_failure_already_recorded: {reason}");
        }
        anyhow::bail!("retry decision requested Go fallback: {reason}");
    }
    // A successful response header ends the bounded pre-response phase. The
    // lane/proxy guard below continues to cover the complete SSE body.
    drop(ingress_permit);
    upstream_client_guard.mark_stream_open();
    let mut bytes_stream = upstream.bytes_stream();

    let upstream_request_id = headers
        .get("x-request-id")
        .and_then(|v| v.to_str().ok())
        .map(ToOwned::to_owned);
    drop(plan);
    let stream_guard = complete_state.metrics.begin_stream();
    // See the early-placeholder path above. Constructing the guard outside
    // the async stream covers a response body that is dropped before its
    // first poll, when code inside `stream!` would never run.
    let complete_guard = ClientDisconnectCompleteGuard::new(
        complete_state.clone(),
        started_at,
        pending_stream_complete_request(
            edge_request_id.clone(),
            lease_id.clone(),
            account_id,
            edge_prepare_ms,
            edge_queue_wait_ms,
            edge_relay_start_ms,
            edge_fallback_reason.clone(),
            edge_retry_count,
        ),
    );
    let body_stream = stream! {
        let mut upstream_client_guard = upstream_client_guard;
        let _stream_guard = stream_guard;
        let _lease_renewal_guard = lease_renewal_guard;
        let guard = complete_guard;
        let mut first_byte_ms: Option<i64> = None;
        let mut first_flush_ms: Option<i64> = None;
        let mut first_token_ms: Option<i64> = None;
        let mut real_first_token_ms: Option<i64> = None;
        let mut safe_token_placeholder_sent = false;
        let mut first_token_timeout_placeholder_sent = false;
        let mut bootstrap_comment_sent = false;
        let mut success = true;
        let mut error_message: Option<String> = None;
        let mut summary = ChatStreamSummary::with_pending(
            complete_state.pools.take_sse_string(),
            response_dialect.as_deref(),
        );
        let mut sanitizer = OpenAIStreamSanitizer::new(response_dialect.as_deref());
        // Keep legacy Chat behavior unchanged. Only Responses preamble
        // events are held when the account disabled preamble flush; a missing
        // field is decoded as true for compatibility with older Go plans.
        let mut preamble_gate = SsePreambleGate::new(
            preamble_flush || response_dialect.as_deref() != Some("responses"),
        );
        summary.request_id = upstream_request_id;
        guard.update_stream_snapshot(
            &summary,
            success,
            error_message.clone(),
            Some(upstream_header_ms),
            first_byte_ms,
            first_token_ms,
            real_first_token_ms,
            first_flush_ms,
            Some(status.as_u16()),
            false,
        );

        if sse_comment_preflush
            || (low_latency_policy.enabled && low_latency_policy.barrier.is_none())
        {
            first_flush_ms = Some(started_at.elapsed().as_millis() as i64);
            bootstrap_comment_sent = true;
            for output in preamble_gate.force() {
                yield Ok::<Bytes, std::io::Error>(output);
            }
            yield Ok::<Bytes, std::io::Error>(Bytes::from_static(b":\n\n"));
        }

        let mut bootstrap_timer = if sse_comment_preflush {
            None
        } else {
            low_latency_policy
                .barrier
                .map(tokio::time::sleep)
                .map(Box::pin)
        };
        let mut first_token_timeout_timer = first_token_timeout_placeholder
            .map(|timeout| tokio::time::sleep(delay_until_elapsed(started_at, timeout)))
            .map(Box::pin);

        enum RelaySelectEvent {
            BootstrapComment,
            FirstTokenTimeoutPlaceholder,
            Upstream(Option<Result<Bytes, reqwest::Error>>),
        }

        loop {
            let event = if bootstrap_timer.is_some() || first_token_timeout_timer.is_some() {
                tokio::select! {
                    _ = wait_optional_sleep(&mut bootstrap_timer), if !bootstrap_comment_sent => {
                        RelaySelectEvent::BootstrapComment
                    }
                    _ = wait_optional_sleep(&mut first_token_timeout_timer), if !first_token_timeout_placeholder_sent && first_token_ms.is_none() => {
                        RelaySelectEvent::FirstTokenTimeoutPlaceholder
                    }
                    next = bytes_stream.next() => RelaySelectEvent::Upstream(next),
                }
            } else {
                RelaySelectEvent::Upstream(bytes_stream.next().await)
            };
            let next = match event {
                RelaySelectEvent::BootstrapComment => {
                    if first_flush_ms.is_none() {
                        first_flush_ms = Some(started_at.elapsed().as_millis() as i64);
                    }
                    bootstrap_comment_sent = true;
                    bootstrap_timer = None;
                    guard.update_stream_snapshot(
                        &summary,
                        success,
                        error_message.clone(),
                        Some(upstream_header_ms),
                        first_byte_ms,
                        first_token_ms,
                        real_first_token_ms,
                        first_flush_ms,
                        Some(status.as_u16()),
                        false,
                    );
                    for output in preamble_gate.force() {
                        yield Ok::<Bytes, std::io::Error>(output);
                    }
                    yield Ok::<Bytes, std::io::Error>(Bytes::from_static(b":\n\n"));
                    continue;
                }
                RelaySelectEvent::FirstTokenTimeoutPlaceholder => {
                    if first_flush_ms.is_none() {
                        first_flush_ms = Some(started_at.elapsed().as_millis() as i64);
                    }
                    first_token_timeout_placeholder_sent = true;
                    safe_token_placeholder_sent = true;
                    first_token_timeout_timer = None;
                    let placeholder =
                        openai_stream_timeout_placeholder_frame(response_dialect.as_deref(), &summary);
                    guard.update_stream_snapshot(
                        &summary,
                        success,
                        error_message.clone(),
                        Some(upstream_header_ms),
                        first_byte_ms,
                        first_token_ms,
                        real_first_token_ms,
                        first_flush_ms,
                        Some(status.as_u16()),
                        false,
                    );
                    for output in preamble_gate.force() {
                        yield Ok::<Bytes, std::io::Error>(output);
                    }
                    yield Ok::<Bytes, std::io::Error>(Bytes::from(placeholder));
                    continue;
                }
                RelaySelectEvent::Upstream(next) => next,
            };
            let Some(next) = next else {
                break;
            };
            match next {
                Ok(chunk) => {
                    if first_byte_ms.is_none() {
                        first_byte_ms = Some(header_start.elapsed().as_millis() as i64);
                    }
                    bootstrap_comment_sent = true;
                    bootstrap_timer = None;
                    let observation = summary.observe(&chunk);
                    if real_first_token_ms.is_none() && observation.starts_real_output {
                        real_first_token_ms = Some(started_at.elapsed().as_millis() as i64);
                    }
                    if first_token_ms.is_none() && observation.starts_client_output {
                        first_token_ms = Some(started_at.elapsed().as_millis() as i64);
                        first_token_timeout_timer = None;
                    }
                    if summary.failed {
                        success = false;
                        error_message = Some("Upstream request failed".to_string());
                    }
                    guard.update_stream_snapshot(
                        &summary,
                        success,
                        error_message.clone(),
                        Some(upstream_header_ms),
                        first_byte_ms,
                        first_token_ms,
                        real_first_token_ms,
                        first_flush_ms,
                        Some(status.as_u16()),
                        summary.failed,
                    );
                    if safe_token_placeholder && !safe_token_placeholder_sent {
                        if let Some(offset) = observation.response_created_boundary_offset {
                            safe_token_placeholder_sent = true;
                            first_token_timeout_placeholder_sent = true;
                            first_token_timeout_timer = None;
                            if first_flush_ms.is_none() {
                                first_flush_ms = Some(started_at.elapsed().as_millis() as i64);
                                guard.update_stream_snapshot(
                                    &summary,
                                    success,
                                    error_message.clone(),
                                    Some(upstream_header_ms),
                                    first_byte_ms,
                                    first_token_ms,
                                    real_first_token_ms,
                                    first_flush_ms,
                                    Some(status.as_u16()),
                                    summary.failed,
                                );
                            }
                            let offset = offset.min(chunk.len());
                            if offset > 0 {
                                let sanitized = sanitizer.push(&chunk.slice(..offset));
                                for output in preamble_gate.accept(sanitized, false, false) {
                                    yield Ok::<Bytes, std::io::Error>(output);
                                }
                            }
                            // The local placeholder is client-visible and
                            // therefore releases any buffered Responses
                            // preamble before it is emitted.
                            for output in preamble_gate.force() {
                                yield Ok::<Bytes, std::io::Error>(output);
                            }
                            let placeholder = openai_stream_timeout_placeholder_frame(
                                response_dialect.as_deref(),
                                &summary,
                            );
                            yield Ok::<Bytes, std::io::Error>(Bytes::from(placeholder));
                            if offset < chunk.len() {
                                let sanitized = sanitizer.push(&chunk.slice(offset..));
                                for output in preamble_gate.accept(
                                    sanitized,
                                    observation.starts_client_output,
                                    summary.failed,
                                ) {
                                    yield Ok::<Bytes, std::io::Error>(output);
                                }
                            }
                            if summary.completed_successfully(response_dialect.as_deref()) {
                                break;
                            }
                            continue;
                        }
                    }
                    // Native Chat Completions streams do not emit
                    // `response.created`. When the account opted into the
                    // safe placeholder, mirror the Go path at the first
                    // valid chat chunk instead. With the flag off this branch
                    // is skipped and the upstream bytes remain unchanged.
                    if safe_token_placeholder
                        && response_dialect.as_deref() == Some("chat_completions")
                        && !safe_token_placeholder_sent
                        && !summary.failed
                        && observation.starts_client_output
                    {
                        safe_token_placeholder_sent = true;
                        first_token_timeout_placeholder_sent = true;
                        first_token_timeout_timer = None;
                        if first_flush_ms.is_none() {
                            first_flush_ms = Some(started_at.elapsed().as_millis() as i64);
                            guard.update_stream_snapshot(
                                &summary,
                                success,
                                error_message.clone(),
                                Some(upstream_header_ms),
                                first_byte_ms,
                                first_token_ms,
                                real_first_token_ms,
                                first_flush_ms,
                                Some(status.as_u16()),
                                summary.failed,
                            );
                        }
                        let placeholder = openai_chat_safe_token_placeholder_frame(
                            summary.response_id.as_deref(),
                            summary.model.as_deref(),
                        );
                        for output in preamble_gate.force() {
                            yield Ok::<Bytes, std::io::Error>(output);
                        }
                        yield Ok::<Bytes, std::io::Error>(Bytes::from(placeholder));
                    }
                    let sanitized = sanitizer.push(&chunk);
                    for output in preamble_gate.accept(
                        sanitized,
                        observation.starts_client_output,
                        summary.failed,
                    ) {
                        if first_flush_ms.is_none() {
                            first_flush_ms = Some(started_at.elapsed().as_millis() as i64);
                            guard.update_stream_snapshot(
                                &summary,
                                success,
                                error_message.clone(),
                                Some(upstream_header_ms),
                                first_byte_ms,
                                first_token_ms,
                                real_first_token_ms,
                                first_flush_ms,
                                Some(status.as_u16()),
                                summary.failed,
                            );
                        }
                        yield Ok::<Bytes, std::io::Error>(output);
                    }
                    if summary.completed_successfully(response_dialect.as_deref()) {
                        break;
                    }
                }
                Err(err) => {
                    if summary.completed_successfully(response_dialect.as_deref()) {
                        break;
                    }
                    success = false;
                    error_message = Some(err.to_string());
                    guard.update_stream_snapshot(
                        &summary,
                        success,
                        error_message.clone(),
                        Some(upstream_header_ms),
                        first_byte_ms,
                        first_token_ms,
                        real_first_token_ms,
                        first_flush_ms,
                        Some(status.as_u16()),
                        true,
                    );
                    for output in preamble_gate.force() {
                        yield Ok::<Bytes, std::io::Error>(output);
                    }
                    yield Err(std::io::Error::other(err.to_string()));
                    break;
                }
            }
        }

        drop(bytes_stream);
        upstream_client_guard.release();
        let tail = sanitizer.finish();
        let tail_starts_output = !tail.is_empty();
        for output in preamble_gate.accept(tail, tail_starts_output, summary.failed) {
            if first_flush_ms.is_none() {
                first_flush_ms = Some(started_at.elapsed().as_millis() as i64);
                guard.update_stream_snapshot(
                    &summary,
                    success,
                    error_message.clone(),
                    Some(upstream_header_ms),
                    first_byte_ms,
                    first_token_ms,
                    real_first_token_ms,
                    first_flush_ms,
                    Some(status.as_u16()),
                    summary.failed,
                );
            }
            yield Ok::<Bytes, std::io::Error>(output);
        }
        // Preserve a tokenless but otherwise valid Responses stream. Without
        // this final drain, disabling preamble flush could drop every upstream
        // event when no output delta was produced.
        for output in preamble_gate.force() {
            if first_flush_ms.is_none() {
                first_flush_ms = Some(started_at.elapsed().as_millis() as i64);
                guard.update_stream_snapshot(
                    &summary,
                    success,
                    error_message.clone(),
                    Some(upstream_header_ms),
                    first_byte_ms,
                    first_token_ms,
                    real_first_token_ms,
                    first_flush_ms,
                    Some(status.as_u16()),
                    summary.failed,
                );
            }
            yield Ok::<Bytes, std::io::Error>(output);
        }
        if summary.failed {
            success = false;
            error_message = Some("Upstream request failed".to_string());
        } else if !summary.completed_successfully(response_dialect.as_deref()) {
            success = false;
            error_message = Some("Upstream stream ended before completion".to_string());
        }
        guard.update_stream_snapshot(
            &summary,
            success,
            error_message.clone(),
            Some(upstream_header_ms),
            first_byte_ms,
            first_token_ms,
            real_first_token_ms,
            first_flush_ms,
            Some(status.as_u16()),
            summary.failed || summary.terminal_event_type.is_none(),
        );
        let terminal_event_type = summary.terminal_event_type(response_dialect.as_deref());
        let request_id = summary.request_id.clone();
        let response_id = summary.response_id.clone();
        let model = summary.model.clone();
        let upstream_model = summary.upstream_model.clone();
        let usage = summary.usage.clone();
        let cyber_blocked = summary.cyber_blocked;
        complete_state.pools.recycle_sse_string(std::mem::take(&mut summary.pending));

        if call_complete(&complete_state, CompleteRequest {
            edge_request_id,
            lease_id,
            account_id,
            success,
            failure_class: classify_stream_failure(success, false, &summary),
            client_disconnected: false,
            request_id,
            response_id,
            model,
            upstream_model,
            usage,
            duration_ms: started_at.elapsed().as_millis() as i64,
            upstream_header_ms: Some(upstream_header_ms),
            upstream_first_byte_ms: first_byte_ms,
            first_token_ms,
            real_first_token_ms,
            guard_sample_at_unix_ns: None,
            first_client_flush_ms: first_flush_ms,
            edge_prepare_ms,
            edge_queue_wait_ms,
            edge_relay_start_ms,
            edge_fallback_reason,
            edge_retry_count,
            error_type: if success { None } else { Some("stream_error".to_string()) },
            error_message,
            upstream_status_code: Some(status.as_u16()),
            terminal_event_type,
            cyber_blocked,
        }).await.is_ok() {
            guard.mark_done();
        }
    };

    let mut builder = Response::builder().status(status.as_u16());
    write_direct_stream_response_headers(builder.headers_mut().expect("headers"), &headers);
    Ok(builder.body(Body::from_stream(body_stream))?)
}

async fn retry_after_queue_wait_budget(
    state: AppState,
    plan: EdgePlan,
    queue_wait_ms: i64,
    context: RelayAttemptContext,
) -> anyhow::Result<Response> {
    let RelayAttemptContext {
        started_at,
        timing,
        timing_shared,
        relay_attempted_marker,
        ingress_permit,
    } = context;
    let decision = call_retry(
        &state,
        RetryRequest {
            edge_request_id: plan.edge_request_id.clone(),
            lease_id: plan.lease_id.clone(),
            account_id: plan.account_id,
            upstream_status_code: None,
            upstream_request_id: None,
            error_type: Some("edge_queue_wait_timeout".to_string()),
            error_message: Some(format!(
                "edge relay queue wait exceeded budget: {queue_wait_ms}ms"
            )),
            request_body: None,
            response_body: None,
            wrote_client_response: false,
        },
    )
    .await?;
    if decision.action != "relay" {
        let reason = decision
            .reason
            .unwrap_or_else(|| "queue_wait_budget_fallback_go".to_string());
        anyhow::bail!("queue wait budget requested Go fallback: {reason}");
    }
    let Some(next_plan) = decision.plan else {
        anyhow::bail!("queue wait retry decision missing relay plan");
    };
    let mut next_timing = timing;
    next_timing.retry_count += 1;
    update_edge_timing(timing_shared.as_ref(), |shared| {
        shared.retry_count = next_timing.retry_count;
        shared.queue_wait_ms = next_timing.queue_wait_ms;
    });
    if let Some(relay_attempted_marker) = relay_attempted_marker {
        relay_attempted_marker.store(true, Ordering::SeqCst);
    }
    Box::pin(relay_upstream(
        state,
        next_plan,
        started_at,
        next_timing,
        timing_shared,
        false,
        ingress_permit,
    ))
    .await
}

fn low_latency_policy(mode: Option<&str>) -> LowLatencyPolicy {
    match mode
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase()
        .as_str()
    {
        "aggressive" => LowLatencyPolicy {
            enabled: true,
            barrier: None,
        },
        "smart" => LowLatencyPolicy {
            enabled: true,
            barrier: Some(Duration::from_millis(25)),
        },
        _ => LowLatencyPolicy::default(),
    }
}

fn normalize_first_token_timeout_placeholder_ms(ms: Option<u64>) -> Option<Duration> {
    match ms {
        Some(value @ 1..=3000) => Some(Duration::from_millis(value)),
        _ => None,
    }
}

fn delay_until_elapsed(started_at: Instant, timeout: Duration) -> Duration {
    timeout
        .checked_sub(started_at.elapsed())
        .unwrap_or(Duration::ZERO)
}

async fn wait_optional_sleep(timer: &mut Option<Pin<Box<tokio::time::Sleep>>>) {
    if let Some(timer) = timer.as_mut() {
        timer.as_mut().await;
    } else {
        pending::<()>().await;
    }
}

fn json_event_type(value: &Value) -> Option<&str> {
    value.get("type").and_then(Value::as_str)
}

fn openai_responses_safe_token_placeholder_frame(response_id: Option<&str>) -> String {
    let response_id = response_id
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .unwrap_or("resp_placeholder");
    let response_id_json =
        serde_json::to_string(response_id).unwrap_or_else(|_| "\"resp_placeholder\"".to_string());
    format!(
        "data: {{\"type\":\"response.output_text.delta\",\"delta\":\"\",\"response_id\":{},\"item_id\":\"msg_placeholder\",\"output_index\":0,\"content_index\":0}}\n\n",
        response_id_json
    )
}

fn openai_chat_safe_token_placeholder_frame(id: Option<&str>, model: Option<&str>) -> String {
    let id = id
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .unwrap_or("chatcmpl-placeholder");
    let id_json =
        serde_json::to_string(id).unwrap_or_else(|_| "\"chatcmpl-placeholder\"".to_string());
    let model = model.map(str::trim).filter(|v| !v.is_empty()).unwrap_or("");
    let model_json = serde_json::to_string(model).unwrap_or_else(|_| "\"\"".to_string());
    let created = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    format!(
        "data: {{\"id\":{},\"object\":\"chat.completion.chunk\",\"created\":{},\"model\":{},\"choices\":[{{\"index\":0,\"delta\":{{\"role\":\"assistant\",\"content\":\"\"}},\"finish_reason\":null}}]}}\n\n",
        id_json, created, model_json
    )
}

fn openai_stream_timeout_placeholder_frame(
    dialect: Option<&str>,
    summary: &ChatStreamSummary,
) -> String {
    if dialect == Some("chat_completions") {
        return openai_chat_safe_token_placeholder_frame(
            summary.response_id.as_deref(),
            summary.model.as_deref(),
        );
    }
    openai_responses_safe_token_placeholder_frame(summary.response_id.as_deref())
}

fn openai_stream_error_frame(dialect: Option<&str>, model: Option<&str>, message: &str) -> String {
    let _ = message;
    let message_json = "\"Upstream request failed\"";
    if dialect == Some("chat_completions") {
        return format!(
            "event: error\ndata: {{\"error\":{{\"type\":\"upstream_error\",\"message\":{}}}}}\n\n",
            message_json
        );
    }
    let model = model.map(str::trim).filter(|v| !v.is_empty()).unwrap_or("");
    let model_json = serde_json::to_string(model).unwrap_or_else(|_| "\"\"".to_string());
    format!(
        "event: response.failed\ndata: {{\"type\":\"response.failed\",\"response\":{{\"id\":\"resp_placeholder_failed\",\"object\":\"response\",\"model\":{},\"status\":\"failed\",\"output\":[],\"error\":{{\"code\":\"upstream_error\",\"message\":{}}}}}}}\n\n",
        model_json, message_json
    )
}

#[derive(Default)]
struct OpenAIStreamSanitizer {
    pending: Vec<u8>,
    chat_dialect: bool,
    event_type: Option<String>,
}

impl OpenAIStreamSanitizer {
    fn new(dialect: Option<&str>) -> Self {
        Self {
            pending: Vec::with_capacity(1024),
            chat_dialect: dialect == Some("chat_completions"),
            event_type: None,
        }
    }

    fn push(&mut self, chunk: &[u8]) -> Bytes {
        self.pending.extend_from_slice(chunk);
        self.drain_lines(false)
    }

    fn finish(&mut self) -> Bytes {
        self.drain_lines(true)
    }

    fn drain_lines(&mut self, flush_tail: bool) -> Bytes {
        let mut output = Vec::with_capacity(self.pending.len());
        while let Some(pos) = self.pending.iter().position(|byte| *byte == b'\n') {
            let line: Vec<u8> = self.pending.drain(..=pos).collect();
            output.extend_from_slice(&sanitize_openai_sse_line(
                &line,
                self.chat_dialect,
                &mut self.event_type,
            ));
        }
        if flush_tail && !self.pending.is_empty() {
            let line = std::mem::take(&mut self.pending);
            output.extend_from_slice(&sanitize_openai_sse_line(
                &line,
                self.chat_dialect,
                &mut self.event_type,
            ));
        }
        Bytes::from(output)
    }
}

/// Gates Responses preamble events until the first client-visible output when
/// the account has not enabled preamble flush. Chat Completions has no
/// Responses preamble, so its observations pass through immediately. The
/// bounded buffer prevents a malformed upstream from retaining unbounded
/// preamble data while still preserving the legacy fail-open behavior.
struct SsePreambleGate {
    released: bool,
    pending: Vec<Bytes>,
    pending_bytes: usize,
}

const SSE_PREAMBLE_BUFFER_MAX_BYTES: usize = 1024 * 1024;

impl SsePreambleGate {
    fn new(flush_preamble: bool) -> Self {
        Self {
            released: flush_preamble,
            pending: Vec::new(),
            pending_bytes: 0,
        }
    }

    /// Accept sanitized bytes and return chunks that are now safe to emit.
    /// `starts_client_output` comes from the dialect-aware stream summary;
    /// `force` is used for a local comment/placeholder or a sanitized error.
    fn accept(&mut self, bytes: Bytes, starts_client_output: bool, force: bool) -> Vec<Bytes> {
        if !self.released && !starts_client_output && !force {
            if !bytes.is_empty() {
                let exceeds_limit = self
                    .pending_bytes
                    .checked_add(bytes.len())
                    .is_none_or(|size| size > SSE_PREAMBLE_BUFFER_MAX_BYTES);
                if exceeds_limit {
                    // Do not let an unbounded sequence of preamble events pin
                    // memory. Releasing the buffered bytes preserves the
                    // stream rather than dropping upstream output.
                    self.released = true;
                    return self.release_with(bytes);
                }
                self.pending_bytes += bytes.len();
                self.pending.push(bytes);
            }
            return Vec::new();
        }
        self.released = true;
        self.release_with(bytes)
    }

    fn force(&mut self) -> Vec<Bytes> {
        self.accept(Bytes::new(), false, true)
    }

    fn release_with(&mut self, bytes: Bytes) -> Vec<Bytes> {
        let mut output = Vec::with_capacity(self.pending.len() + usize::from(!bytes.is_empty()));
        if !self.pending.is_empty() {
            let mut combined = Vec::with_capacity(self.pending_bytes);
            for pending in self.pending.drain(..) {
                combined.extend_from_slice(&pending);
            }
            self.pending_bytes = 0;
            output.push(Bytes::from(combined));
        }
        if !bytes.is_empty() {
            output.push(bytes);
        }
        output
    }
}

fn sanitize_openai_sse_line(
    line: &[u8],
    chat_dialect: bool,
    current_event_type: &mut Option<String>,
) -> Vec<u8> {
    let text = String::from_utf8_lossy(line);
    let without_newline = text.trim_end_matches(['\r', '\n']);
    let trimmed = without_newline.trim_start();
    if trimmed.is_empty() {
        *current_event_type = None;
        return line.to_vec();
    }
    if trimmed.starts_with(':') {
        let newline = if text.ends_with("\r\n") { "\r\n" } else { "\n" };
        return format!(":{newline}").into_bytes();
    }
    if let Some(data) = trimmed.strip_prefix("data:") {
        let payload = data.trim();
        if payload.is_empty() || payload == "[DONE]" {
            return line.to_vec();
        }
        let Ok(mut value) = serde_json::from_str::<Value>(payload) else {
            return safe_sse_error_line(chat_dialect);
        };
        let event_type = json_event_type(&value)
            .or(current_event_type.as_deref())
            .unwrap_or_default();
        let has_error = event_type == "error"
            || event_type == "response.failed"
            || json_is_unsafe_upstream_diagnostic(&value)
            || (chat_dialect && !value.get("choices").is_some_and(Value::is_array));
        if has_error {
            return safe_sse_error_line(chat_dialect);
        }
        let normalized_payload = if normalize_completed_image_generation_status(&mut value) {
            value.to_string()
        } else {
            payload.to_string()
        };
        let newline = if text.ends_with("\r\n") { "\r\n" } else { "\n" };
        return format!("data: {normalized_payload}{newline}").into_bytes();
    }
    if let Some(event) = trimmed.strip_prefix("event:") {
        let event = event.trim();
        if (event == "error" || event.starts_with("response."))
            && event
                .chars()
                .all(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '.' | '_' | '-'))
        {
            *current_event_type = Some(event.to_string());
            return format!("event: {event}\n").into_bytes();
        }
        *current_event_type = None;
        return Vec::new();
    }
    if trimmed.starts_with("id:") || trimmed.starts_with("retry:") {
        return Vec::new();
    }
    Vec::new()
}

fn safe_sse_error_line(chat_dialect: bool) -> Vec<u8> {
    if chat_dialect {
        b"data: {\"error\":{\"type\":\"upstream_error\",\"message\":\"Upstream request failed\"}}\n"
            .to_vec()
    } else {
        b"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"upstream_error\",\"message\":\"Upstream request failed\"}}}\n".to_vec()
    }
}

fn sanitize_openai_ws_message(msg: TungsteniteMessage) -> TungsteniteMessage {
    match msg {
        TungsteniteMessage::Text(text) => {
            let mut value = match serde_json::from_str::<Value>(&text) {
                Ok(value) => value,
                Err(_) => {
                    return TungsteniteMessage::Text(
                        r#"{"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}"#.to_string(),
                    )
                }
            };
            let image_status_normalized =
                normalize_completed_image_generation_status(&mut value);
            let event_type = json_event_type(&value).unwrap_or_default();
            let has_error = event_type == "error"
                || event_type == "response.failed"
                || json_is_unsafe_upstream_diagnostic(&value);
            if has_error {
                TungsteniteMessage::Text(
                    r#"{"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}"#.to_string(),
                )
            } else if image_status_normalized {
                TungsteniteMessage::Text(value.to_string())
            } else {
                TungsteniteMessage::Text(text)
            }
        }
        TungsteniteMessage::Binary(_) => TungsteniteMessage::Text(
            r#"{"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}"#.to_string(),
        ),
        other => other,
    }
}

fn normalize_completed_image_generation_status(value: &mut Value) -> bool {
    fn normalize_item(item: &mut Value) -> bool {
        if item.get("type").and_then(Value::as_str) != Some("image_generation_call") {
            return false;
        }
        let status = item
            .get("status")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let has_result = item.get("result").is_some_and(|result| {
            !result.is_null()
                && result
                    .as_str()
                    .map(|text| !text.trim().is_empty())
                    .unwrap_or(true)
        });
        if !has_result || !matches!(status, "generating" | "in_progress") {
            return false;
        }
        let Some(object) = item.as_object_mut() else {
            return false;
        };
        object.insert("status".to_string(), Value::String("completed".to_string()));
        true
    }

    let event_type = value
        .get("type")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_string();
    match event_type.as_str() {
        "response.output_item.done" => value.get_mut("item").is_some_and(normalize_item),
        "response.completed" | "response.done" => {
            let Some(items) = value
                .get_mut("response")
                .and_then(|response| response.get_mut("output"))
                .and_then(Value::as_array_mut)
            else {
                return false;
            };
            let mut changed = false;
            for item in items {
                changed |= normalize_item(item);
            }
            changed
        }
        _ => {
            if !matches!(
                value.get("status").and_then(Value::as_str),
                Some("completed" | "done")
            ) {
                return false;
            }
            let Some(items) = value.get_mut("output").and_then(Value::as_array_mut) else {
                return false;
            };
            let mut changed = false;
            for item in items {
                changed |= normalize_item(item);
            }
            changed
        }
    }
}

fn json_has_non_null_error(value: &Value) -> bool {
    value.get("error").is_some_and(|error| !error.is_null())
        || value
            .get("response")
            .and_then(|response| response.get("error"))
            .is_some_and(|error| !error.is_null())
}

fn json_is_unsafe_upstream_diagnostic(value: &Value) -> bool {
    if !value.is_object() || json_has_non_null_error(value) {
        return true;
    }
    for key in ["message", "detail", "reason"] {
        if value
            .get(key)
            .and_then(Value::as_str)
            .is_some_and(|text| !text.trim().is_empty())
        {
            return true;
        }
    }
    for key in [
        "provider",
        "upstream",
        "upstream_url",
        "base_url",
        "host",
        "hostname",
        "server",
        "traceback",
        "stack",
    ] {
        if value.get(key).is_some_and(|field| !field.is_null()) {
            return true;
        }
    }
    for status in [
        value.get("status").and_then(Value::as_str),
        value.pointer("/response/status").and_then(Value::as_str),
    ]
    .into_iter()
    .flatten()
    {
        if matches!(
            status.trim().to_ascii_lowercase().as_str(),
            "error" | "failed" | "failure"
        ) {
            return true;
        }
    }
    if value.get("success").and_then(Value::as_bool) == Some(false) {
        return true;
    }
    for envelope in ["data", "result"]
        .iter()
        .filter_map(|key| value.get(key).and_then(Value::as_object))
    {
        if envelope.get("error").is_some_and(|error| !error.is_null())
            || envelope
                .get("response")
                .and_then(Value::as_object)
                .and_then(|response| response.get("error"))
                .is_some_and(|error| !error.is_null())
        {
            return true;
        }
        for key in ["message", "detail", "reason"] {
            if envelope
                .get(key)
                .and_then(Value::as_str)
                .is_some_and(|text| !text.trim().is_empty())
            {
                return true;
            }
        }
        for key in [
            "provider",
            "upstream",
            "upstream_url",
            "base_url",
            "host",
            "hostname",
            "server",
            "traceback",
            "stack",
        ] {
            if envelope.get(key).is_some_and(|field| !field.is_null()) {
                return true;
            }
        }
        if envelope
            .get("status")
            .and_then(Value::as_str)
            .is_some_and(|status| {
                matches!(
                    status.trim().to_ascii_lowercase().as_str(),
                    "error" | "failed" | "failure"
                )
            })
            || envelope.get("success").and_then(Value::as_bool) == Some(false)
        {
            return true;
        }
    }
    false
}

fn json_is_cyber_policy(value: &Value) -> bool {
    [
        "/error/code",
        "/error/type",
        "/response/error/code",
        "/response/error/type",
    ]
    .iter()
    .filter_map(|path| value.pointer(path).and_then(Value::as_str))
    .any(|value| value.trim().eq_ignore_ascii_case("cyber_policy"))
        || ["/error/message", "/response/error/message"]
            .iter()
            .filter_map(|path| value.pointer(path).and_then(Value::as_str))
            .map(|value| value.trim().to_ascii_lowercase())
            .any(|value| {
                value.contains("high-risk cyber activity")
                    || value.contains("high risk cyber activity")
                    || value.contains("cyber_policy")
            })
}

fn json_text_is_cyber_policy(text: &str) -> bool {
    serde_json::from_str::<Value>(text)
        .ok()
        .as_ref()
        .is_some_and(json_is_cyber_policy)
}

fn json_starts_client_output(value: &Value) -> bool {
    let event_type = json_event_type(value).unwrap_or_default();
    !matches!(
        event_type,
        "response.created" | "response.in_progress" | "response.failed"
    )
}

fn json_starts_real_output(value: &Value) -> bool {
    match json_event_type(value).unwrap_or_default() {
        "response.output_text.delta"
        | "response.function_call_arguments.delta"
        | "response.custom_tool_call_input.delta"
        | "response.reasoning_summary_text.delta"
        | "response.reasoning_text.delta" => value
            .get("delta")
            .and_then(Value::as_str)
            .map(|delta| !delta.trim().is_empty())
            .unwrap_or(false),
        "response.output_item.added" => value
            .get("item")
            .and_then(Value::as_object)
            .and_then(|item| item.get("type"))
            .and_then(Value::as_str)
            .map(|item_type| item_type == "function_call" || item_type == "custom_tool_call")
            .unwrap_or(false),
        _ => false,
    }
}

async fn call_retry(state: &AppState, req: RetryRequest) -> anyhow::Result<RetryDecision> {
    let url = format!("{}/internal/edge/openai/retry", state.cfg.control_base_url);
    let resp = state
        .client
        .post(url)
        .header(EDGE_SECRET_HEADER, &state.cfg.internal_secret)
        .timeout(std::time::Duration::from_millis(
            state.cfg.complete_timeout_ms,
        ))
        .json(&req)
        .send()
        .await?;
    if !resp.status().is_success() {
        anyhow::bail!("retry status {}", resp.status());
    }
    Ok(resp.json::<RetryDecision>().await?)
}

async fn send_settlement_once<T: Serialize + ?Sized>(
    state: &AppState,
    path: &str,
    req: &T,
) -> anyhow::Result<()> {
    let url = format!("{}{}", state.cfg.control_base_url, path);
    let resp = state
        .client
        .post(url)
        .header(EDGE_SECRET_HEADER, &state.cfg.internal_secret)
        .timeout(std::time::Duration::from_millis(
            state.cfg.complete_timeout_ms,
        ))
        .json(&req)
        .send()
        .await?;
    if !resp.status().is_success() {
        anyhow::bail!("settlement status {}", resp.status());
    }
    Ok(())
}

fn spawn_payload_commit(state: AppState, request: CommitRequest) {
    if request.lease_id.as_deref().unwrap_or_default().is_empty() {
        return;
    }
    if let Err(err) = state.payload_commit_tx.try_send(request) {
        let request = err.into_inner();
        let overflow_permit = match state.payload_commit_overflow.clone().try_acquire_owned() {
            Ok(permit) => permit,
            Err(_) => {
                warn!(
                    "edge payload commit overflow saturated edge_request_id={}; deferring cleanup to settlement",
                    request.edge_request_id
                );
                return;
            }
        };
        warn!("edge payload commit queue full or closed; using direct retry");
        tokio::spawn(async move {
            let _overflow_permit = overflow_permit;
            let _worker_guard = state.metrics.begin_callback_work(true);
            let edge_request_id = request.edge_request_id.clone();
            if let Err(err) = retry_with_backoff(
                PAYLOAD_COMMIT_MAX_ATTEMPTS,
                Duration::from_millis(10),
                Duration::from_millis(250),
                || send_settlement_once(&state, "/internal/edge/openai/commit", &request),
            )
            .await
            {
                warn!(
                    "edge direct payload commit failed edge_request_id={edge_request_id}: {}",
                    safe_edge_error(&err)
                );
            }
        });
    }
}

async fn send_renew_once(state: &AppState, request: &RenewRequest) -> anyhow::Result<()> {
    send_settlement_once(state, "/internal/edge/openai/renew", request).await
}

async fn run_payload_commit_queue(state: AppState, mut receiver: mpsc::Receiver<CommitRequest>) {
    let semaphore = Arc::new(Semaphore::new(PAYLOAD_COMMIT_CONCURRENCY));
    while let Some(request) = receiver.recv().await {
        let permit = match semaphore.clone().acquire_owned().await {
            Ok(permit) => permit,
            Err(_) => return,
        };
        let commit_state = state.clone();
        tokio::spawn(async move {
            let _permit = permit;
            let _worker_guard = commit_state.metrics.begin_callback_work(true);
            let edge_request_id = request.edge_request_id.clone();
            if let Err(err) = retry_with_backoff(
                PAYLOAD_COMMIT_MAX_ATTEMPTS,
                Duration::from_millis(10),
                Duration::from_millis(250),
                || send_settlement_once(&commit_state, "/internal/edge/openai/commit", &request),
            )
            .await
            {
                warn!(
                    "edge payload commit failed edge_request_id={edge_request_id}: {}",
                    safe_edge_error(&err)
                );
            }
        });
    }
}

async fn send_recover_once(state: &AppState, req: &RecoverRequest) -> anyhow::Result<RecoverAck> {
    let url = format!(
        "{}/internal/edge/openai/recover",
        state.cfg.control_base_url
    );
    let resp = state
        .client
        .post(url)
        .header(EDGE_SECRET_HEADER, &state.cfg.internal_secret)
        .timeout(Duration::from_millis(state.cfg.complete_timeout_ms))
        .json(req)
        .send()
        .await?;
    if !resp.status().is_success() {
        anyhow::bail!("recover status {}", resp.status());
    }
    let ack = resp.json::<RecoverAck>().await?;
    if !ack.ok {
        anyhow::bail!("recover was not acknowledged");
    }
    Ok(ack)
}

async fn retry_with_backoff<F, Fut>(
    max_attempts: usize,
    initial_delay: Duration,
    max_delay: Duration,
    mut operation: F,
) -> anyhow::Result<usize>
where
    F: FnMut() -> Fut,
    Fut: Future<Output = anyhow::Result<()>>,
{
    let max_attempts = max_attempts.max(1);
    let mut delay = initial_delay;
    let mut last_error = None;
    for attempt in 1..=max_attempts {
        match operation().await {
            Ok(()) => return Ok(attempt),
            Err(err) => last_error = Some(err),
        }
        if attempt < max_attempts {
            tokio::time::sleep(delay).await;
            delay = delay.saturating_mul(2).min(max_delay);
        }
    }
    Err(last_error.unwrap_or_else(|| anyhow::anyhow!("settlement retry failed")))
}

async fn send_settlement_job_once(
    state: &AppState,
    job: &SettlementRetryJob,
) -> anyhow::Result<()> {
    match job {
        SettlementRetryJob::Complete(request) => {
            send_settlement_once(state, "/internal/edge/openai/complete", request).await
        }
        SettlementRetryJob::Abort(request) => {
            send_settlement_once(state, "/internal/edge/openai/abort", request).await
        }
    }
}

fn enqueue_settlement_retry(state: &AppState, job: SettlementRetryJob) -> anyhow::Result<()> {
    state
        .settlement_retry_tx
        .try_send(job)
        .map_err(|err| anyhow::anyhow!("settlement retry queue unavailable: {err}"))
}

async fn run_settlement_retry_job(state: AppState, job: SettlementRetryJob) {
    let kind = job.kind();
    let edge_request_id = job.edge_request_id().to_string();
    let retry_state = state.clone();
    let retry_job = job.clone();
    match retry_with_backoff(
        SETTLEMENT_RETRY_MAX_ATTEMPTS,
        SETTLEMENT_RETRY_INITIAL_DELAY,
        SETTLEMENT_RETRY_MAX_DELAY,
        move || {
            let state = retry_state.clone();
            let job = retry_job.clone();
            async move { send_settlement_job_once(&state, &job).await }
        },
    )
    .await
    {
        Ok(1) => debug!(
            "{kind} callback delivered from bounded queue edge_request_id={edge_request_id}"
        ),
        Ok(attempts) => warn!(
            "{kind} callback recovered after {attempts} queued attempts edge_request_id={edge_request_id}"
        ),
        Err(err) => error!(
            "{kind} callback retries exhausted edge_request_id={edge_request_id}: {err}"
        ),
    }
}

async fn run_settlement_retry_queue(
    state: AppState,
    mut receiver: mpsc::Receiver<SettlementRetryJob>,
) {
    let semaphore = Arc::new(Semaphore::new(SETTLEMENT_RETRY_CONCURRENCY));
    while let Some(job) = receiver.recv().await {
        let permit = match semaphore.clone().acquire_owned().await {
            Ok(permit) => permit,
            Err(_) => return,
        };
        let retry_state = state.clone();
        tokio::spawn(async move {
            let _permit = permit;
            let _worker_guard = retry_state.metrics.begin_callback_work(false);
            run_settlement_retry_job(retry_state, job).await;
        });
    }
}

async fn recover_previous_edge_leases(state: AppState) {
    let Some(edge_node_id) = state.cfg.edge_node_id.clone() else {
        return;
    };
    let request = RecoverRequest {
        edge_node_id,
        edge_instance_id: state.edge_instance_id.as_ref().clone(),
    };
    let mut attempts = 0usize;
    let mut delay = SETTLEMENT_RETRY_INITIAL_DELAY;
    loop {
        attempts = attempts.saturating_add(1);
        match send_recover_once(&state, &request).await {
            Ok(ack) => {
                info!(
                    "edge lease recovery completed released={} attempts={attempts}",
                    ack.released
                );
                return;
            }
            Err(err) => {
                if attempts == 1 || attempts.is_multiple_of(10) {
                    warn!(
                        "edge lease recovery waiting for control plane attempts={attempts}: {err}"
                    );
                }
            }
        }
        tokio::time::sleep(delay).await;
        delay = delay.saturating_mul(2).min(EDGE_RECOVERY_MAX_DELAY);
    }
}

async fn call_complete(state: &AppState, mut req: CompleteRequest) -> anyhow::Result<()> {
    stamp_complete_guard_sample(&mut req);
    match send_settlement_once(state, "/internal/edge/openai/complete", &req).await {
        Ok(()) => Ok(()),
        Err(err) => {
            warn!(
                "complete callback failed; queued for retry edge_request_id={}: {err}",
                req.edge_request_id
            );
            enqueue_settlement_retry(state, SettlementRetryJob::Complete(Box::new(req)))
        }
    }
}

fn stamp_complete_guard_sample(req: &mut CompleteRequest) {
    if req.real_first_token_ms.is_none() || req.guard_sample_at_unix_ns.is_some() {
        return;
    }
    req.guard_sample_at_unix_ns = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .ok()
        .and_then(|duration| i64::try_from(duration.as_nanos()).ok());
}

async fn call_abort(state: &AppState, req: AbortRequest) -> anyhow::Result<()> {
    match send_settlement_once(state, "/internal/edge/openai/abort", &req).await {
        Ok(()) => Ok(()),
        Err(err) => {
            warn!(
                "abort callback failed; queued for retry edge_request_id={}: {err}",
                req.edge_request_id
            );
            enqueue_settlement_retry(state, SettlementRetryJob::Abort(req))
        }
    }
}

async fn fallback_to_go(
    state: AppState,
    method: Method,
    uri: Uri,
    headers: HeaderMap,
    body: Body,
    reason: &str,
    timing: EdgeTiming,
) -> Response {
    let url = format!(
        "{}{}",
        state.cfg.go_base_url.trim_end_matches('/'),
        uri.path_and_query()
            .map(|pq| pq.as_str())
            .unwrap_or(uri.path())
    );
    let body_bytes = match to_bytes(body, MAX_BODY_BYTES).await {
        Ok(bytes) => bytes,
        Err(_) => return text_response(StatusCode::BAD_REQUEST, "Invalid request body"),
    };
    let mut req = state.client.request(method, url);
    for (name, value) in headers.iter() {
        if should_forward_header(name.as_str()) {
            req = req.header(name.as_str(), value.as_bytes());
        }
    }
    req = req.header(EDGE_FALLBACK_HEADER, "1");
    let reason = reason.trim();
    if !reason.is_empty() {
        req = req.header(EDGE_FALLBACK_REASON_HEADER, reason);
    }
    if let Some(value) = timing.prepare_ms {
        req = req.header(EDGE_PREPARE_MS_HEADER, value.to_string());
    }
    if let Some(value) = timing.queue_wait_ms {
        req = req.header(EDGE_QUEUE_WAIT_MS_HEADER, value.to_string());
    }
    if let Some(value) = timing.relay_start_ms {
        req = req.header(EDGE_RELAY_START_MS_HEADER, value.to_string());
    }
    if timing.retry_count > 0 {
        req = req.header(EDGE_RETRY_COUNT_HEADER, timing.retry_count.to_string());
    }
    let upstream = match req.body(body_bytes).send().await {
        Ok(resp) => resp,
        Err(_) => return text_response(StatusCode::BAD_GATEWAY, "Gateway request failed"),
    };
    let status = upstream.status();
    let headers = upstream.headers().clone();
    let stream_guard = state.metrics.begin_stream();
    let stream = stream! {
        let _stream_guard = stream_guard;
        let mut bytes = upstream.bytes_stream();
        while let Some(item) = bytes.next().await {
            yield item.map_err(|err| std::io::Error::other(err.to_string()));
        }
    };
    let mut builder = Response::builder().status(status.as_u16());
    copy_response_headers(builder.headers_mut().expect("headers"), &headers);
    builder
        .body(Body::from_stream(stream))
        .unwrap_or_else(|_| text_response(StatusCode::BAD_GATEWAY, "Gateway request failed"))
}

fn header_map_to_strings(headers: &HeaderMap) -> HashMap<String, String> {
    headers
        .iter()
        .filter_map(|(k, v)| {
            v.to_str()
                .ok()
                .map(|s| (k.as_str().to_string(), s.to_string()))
        })
        .collect()
}

fn client_ip_from_headers(headers: &HeaderMap) -> Option<String> {
    for name in ["cf-connecting-ip", "x-real-ip", "x-forwarded-for"] {
        let Some(value) = headers.get(name).and_then(|v| v.to_str().ok()) else {
            continue;
        };
        let ip = value
            .split(',')
            .next()
            .map(str::trim)
            .filter(|v| !v.is_empty());
        if let Some(ip) = ip {
            return Some(ip.to_string());
        }
    }
    None
}

fn openai_prepare_raw_body(state: &AppState, body: &[u8]) -> Option<String> {
    if body.is_empty() {
        return None;
    }
    if state.cfg.large_payload_passthrough && body.len() >= state.cfg.large_payload_threshold_bytes
    {
        return Some(b64_encode(body));
    }
    None
}

fn take_request_body_bytes(plan: &mut EdgePlan) -> anyhow::Result<Vec<u8>> {
    if let Some(raw) = plan.body_raw_base64.take() {
        let raw = raw.trim();
        if !raw.is_empty() {
            plan.body = None;
            return b64_decode(raw);
        }
    }
    let body = plan.body.take().unwrap_or(Value::Null);
    Ok(serde_json::to_vec(&body)?)
}

fn relay_queue_key(plan: &EdgePlan) -> Option<String> {
    let account_id = plan.account_id?;
    let upstream_url = plan.upstream_url.as_deref()?;
    let host = reqwest::Url::parse(upstream_url)
        .ok()
        .and_then(|url| url.host_str().map(ToOwned::to_owned))
        .unwrap_or_else(|| upstream_url.to_string());
    let proxy = plan.proxy_url.as_deref().unwrap_or("").trim();
    let lane = normalize_relay_lane(plan.lane.as_deref());
    Some(format!("{account_id}|{proxy}|{host}|{lane}"))
}

fn normalize_relay_lane(lane: Option<&str>) -> &'static str {
    match lane
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase()
        .as_str()
    {
        "priority" => "priority",
        "bulk" => "bulk",
        _ => "normal",
    }
}

fn upstream_origin_warm_url(upstream_url: Option<&str>) -> Option<String> {
    let upstream_url = upstream_url.map(str::trim).filter(|v| !v.is_empty())?;
    let parsed = reqwest::Url::parse(upstream_url).ok()?;
    let scheme = parsed.scheme();
    let host = parsed.host_str()?;
    let mut out = format!("{scheme}://{host}");
    if let Some(port) = parsed.port() {
        out.push(':');
        out.push_str(&port.to_string());
    }
    out.push('/');
    Some(out)
}

fn dynamic_warm_key(proxy_url: Option<&str>, warm_url: &str) -> DynamicWarmKey {
    let proxy = proxy_url
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .unwrap_or_default()
        .to_string();
    DynamicWarmKey {
        proxy,
        warm_url: warm_url.to_string(),
    }
}

fn ws_idle_key(plan: &EdgePlan) -> Option<String> {
    if plan.transport.as_deref() != Some("ws_v2") {
        return None;
    }
    let mut headers: Vec<_> = plan
        .headers
        .as_ref()
        .map(|headers| headers.iter().collect())
        .unwrap_or_default();
    headers.sort_by_key(|(key, _)| *key);
    let header_key = headers
        .into_iter()
        .map(|(k, v)| format!("{k}:{v}"))
        .collect::<Vec<_>>()
        .join("\n");
    Some(format!(
        "{}\n{}",
        plan.upstream_url.as_deref().unwrap_or_default(),
        header_key
    ))
}

fn copy_response_headers(dst: &mut HeaderMap, src: &reqwest::header::HeaderMap) {
    for (name, value) in src.iter() {
        if should_forward_response_header(name.as_str()) {
            if let (Ok(name), Ok(value)) = (
                HeaderName::from_bytes(name.as_str().as_bytes()),
                HeaderValue::from_bytes(value.as_bytes()),
            ) {
                dst.append(name, value);
            }
        }
    }
}

fn write_direct_stream_response_headers(dst: &mut HeaderMap, src: &reqwest::header::HeaderMap) {
    dst.insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("text/event-stream"),
    );
    dst.insert(
        header::CACHE_CONTROL,
        HeaderValue::from_static("no-cache, no-transform"),
    );
    dst.insert(
        HeaderName::from_static("x-accel-buffering"),
        HeaderValue::from_static("no"),
    );

    for (name, value) in src.iter() {
        if !is_safe_direct_stream_metric_header(name.as_str(), value) {
            continue;
        }
        if let (Ok(name), Ok(value)) = (
            HeaderName::from_bytes(name.as_str().as_bytes()),
            HeaderValue::from_bytes(value.as_bytes()),
        ) {
            dst.append(name, value);
        }
    }
}

fn is_safe_direct_stream_metric_header(name: &str, value: &reqwest::header::HeaderValue) -> bool {
    if !matches!(
        name.to_ascii_lowercase().as_str(),
        "x-ratelimit-limit-requests"
            | "x-ratelimit-limit-tokens"
            | "x-ratelimit-remaining-requests"
            | "x-ratelimit-remaining-tokens"
            | "x-ratelimit-reset-requests"
            | "x-ratelimit-reset-tokens"
            | "retry-after"
    ) {
        return false;
    }
    let Ok(value) = value.to_str() else {
        return false;
    };
    let value = value.trim();
    !value.is_empty()
        && value.len() <= 128
        && value.bytes().all(|byte| {
            byte.is_ascii_digit()
                || matches!(
                    byte,
                    b' ' | b'.' | b',' | b':' | b'+' | b'-' | b'=' | b'm' | b's'
                )
        })
}

fn should_forward_header(name: &str) -> bool {
    !matches!(
        name.to_ascii_lowercase().as_str(),
        "host"
            | "content-length"
            | "connection"
            | "keep-alive"
            | "proxy-authenticate"
            | "proxy-authorization"
            | "te"
            | "trailer"
            | "transfer-encoding"
            | "upgrade"
            | "x-sub2api-edge-secret"
    )
}

fn should_forward_response_header(name: &str) -> bool {
    matches!(
        name.to_ascii_lowercase().as_str(),
        "content-type"
            | "content-encoding"
            | "content-language"
            | "cache-control"
            | "etag"
            | "last-modified"
            | "expires"
            | "vary"
            | "date"
            | "x-request-id"
            | "x-ratelimit-limit-requests"
            | "x-ratelimit-limit-tokens"
            | "x-ratelimit-remaining-requests"
            | "x-ratelimit-remaining-tokens"
            | "x-ratelimit-reset-requests"
            | "x-ratelimit-reset-tokens"
            | "retry-after"
    )
}

fn is_websocket_upgrade(headers: &HeaderMap) -> bool {
    headers
        .get(header::UPGRADE)
        .and_then(|v| v.to_str().ok())
        .map(|v| v.eq_ignore_ascii_case("websocket"))
        .unwrap_or(false)
}

fn text_response(status: StatusCode, body: &str) -> Response {
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, "text/plain; charset=utf-8")
        .body(Body::from(body.to_string()))
        .unwrap()
}

fn overload_response() -> Response {
    Response::builder()
        .status(StatusCode::SERVICE_UNAVAILABLE)
        .header(header::CONTENT_TYPE, "text/plain; charset=utf-8")
        .header(header::RETRY_AFTER, "1")
        .body(Body::from("edge capacity exhausted"))
        .unwrap()
}

fn openai_error_response(status: StatusCode, error_type: &str, message: &str) -> Response {
    let body = serde_json::json!({
        "error": {
            "message": message,
            "type": error_type,
        }
    });
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(body.to_string()))
        .unwrap()
}

impl ChatStreamSummary {
    fn with_pending(pending: String, dialect: Option<&str>) -> Self {
        Self {
            pending,
            response_dialect: dialect.map(ToOwned::to_owned),
            ..Self::default()
        }
    }

    fn completed_successfully(&self, dialect: Option<&str>) -> bool {
        if self.failed || self.neutral_terminal_event_type.is_some() {
            return false;
        }
        matches!(
            (dialect, self.terminal_event_type.as_deref()),
            (
                Some("chat_completions"),
                Some("[DONE]" | "chat.finish_reason")
            ) | (
                Some("responses"),
                Some("response.completed" | "response.done")
            )
        )
    }

    fn terminal_event_type(&self, _dialect: Option<&str>) -> Option<String> {
        if self.failed {
            return self
                .failed_terminal_event_type
                .clone()
                .or_else(|| Some("error".to_string()));
        }
        if self.neutral_terminal_event_type.is_some() {
            return self.neutral_terminal_event_type.clone();
        }
        self.terminal_event_type.clone()
    }

    fn observe(&mut self, chunk: &[u8]) -> ChatStreamObservation {
        let text = String::from_utf8_lossy(chunk);
        let pending_before = self.pending.len();
        self.pending.push_str(&text);
        let mut observation = ChatStreamObservation::default();
        let mut consumed_total = 0usize;
        while let Some(pos) = self.pending.find('\n') {
            let mut line = self.pending[..pos].to_string();
            self.pending.drain(..=pos);
            consumed_total = consumed_total.saturating_add(pos + 1);
            if line.ends_with('\r') {
                line.pop();
            }
            let mut line_observation = self.observe_line(&line);
            if line_observation.saw_response_created {
                line_observation.response_created_boundary_offset = Some(
                    consumed_total
                        .saturating_sub(pending_before)
                        .min(chunk.len()),
                );
            }
            observation.merge(line_observation);
        }
        if self.pending.len() > 1024 * 1024 {
            self.pending.clear();
        }
        observation
    }

    fn observe_line(&mut self, line: &str) -> ChatStreamObservation {
        let trimmed = line.trim_start();
        if trimmed.is_empty() {
            if self.response_created_pending_boundary {
                self.response_created_pending_boundary = false;
                return ChatStreamObservation {
                    starts_client_output: false,
                    starts_real_output: false,
                    saw_response_created: true,
                    response_created_boundary_offset: None,
                };
            }
            return ChatStreamObservation::default();
        }
        let Some(data) = trimmed.strip_prefix("data:") else {
            return ChatStreamObservation::default();
        };
        let payload = data.trim();
        if payload.is_empty() {
            return ChatStreamObservation::default();
        }
        if payload == "[DONE]" {
            self.terminal_event_type = Some("[DONE]".to_string());
            return ChatStreamObservation::default();
        }
        let Ok(value) = serde_json::from_str::<Value>(payload) else {
            self.failed = true;
            if self.failed_terminal_event_type.is_none() {
                self.failed_terminal_event_type = Some("error".to_string());
            }
            if self.terminal_event_type.is_none() {
                self.terminal_event_type = Some("error".to_string());
            }
            return ChatStreamObservation::default();
        };
        let observation = ChatStreamObservation {
            starts_client_output: json_starts_client_output(&value),
            starts_real_output: json_starts_real_output(&value),
            saw_response_created: false,
            response_created_boundary_offset: None,
        };
        if json_event_type(&value) == Some("response.created") {
            self.response_created_pending_boundary = true;
        }
        self.observe_json(&value);
        observation
    }

    fn observe_ws_message(&mut self, msg: &TungsteniteMessage) {
        match msg {
            TungsteniteMessage::Text(text) => {
                if let Ok(value) = serde_json::from_str::<Value>(text) {
                    self.observe_json(&value);
                }
            }
            TungsteniteMessage::Binary(bytes) => {
                if let Ok(text) = std::str::from_utf8(bytes) {
                    if let Ok(value) = serde_json::from_str::<Value>(text) {
                        self.observe_json(&value);
                    }
                }
            }
            _ => {}
        }
    }

    fn observe_json(&mut self, value: &Value) {
        if json_is_cyber_policy(value) {
            self.cyber_blocked = true;
        }
        let event_type = json_event_type(value).unwrap_or_default();
        match event_type {
            "response.completed" | "response.done" => {
                self.terminal_event_type = Some(event_type.to_string());
            }
            "response.failed" | "error" => {
                self.failed = true;
                self.terminal_event_type = Some(event_type.to_string());
                if self.failed_terminal_event_type.is_none() {
                    self.failed_terminal_event_type = Some(event_type.to_string());
                }
            }
            "response.incomplete" | "response.cancelled" | "response.canceled" => {
                self.terminal_event_type = Some(event_type.to_string());
                if self.neutral_terminal_event_type.is_none() {
                    self.neutral_terminal_event_type = Some(event_type.to_string());
                }
            }
            _ => {}
        }
        let invalid_chat_payload = self.response_dialect.as_deref() == Some("chat_completions")
            && !value.get("choices").is_some_and(Value::is_array);
        if json_is_unsafe_upstream_diagnostic(value) || invalid_chat_payload {
            self.failed = true;
            if self.failed_terminal_event_type.is_none() {
                self.failed_terminal_event_type = Some("error".to_string());
            }
            if self.terminal_event_type.is_none() {
                self.terminal_event_type = Some("error".to_string());
            }
        }
        if value
            .get("choices")
            .and_then(Value::as_array)
            .is_some_and(|choices| {
                choices.iter().any(|choice| {
                    choice
                        .get("finish_reason")
                        .is_some_and(|reason| !reason.is_null() && reason.as_str() != Some(""))
                })
            })
        {
            self.terminal_event_type = Some("chat.finish_reason".to_string());
        }
        let response = value.get("response").unwrap_or(value);
        if self.response_id.is_none() {
            self.response_id = response
                .get("id")
                .and_then(Value::as_str)
                .filter(|v| !v.is_empty())
                .map(ToOwned::to_owned);
        }
        if self.model.is_none() {
            self.model = response
                .get("model")
                .and_then(Value::as_str)
                .filter(|v| !v.is_empty())
                .map(ToOwned::to_owned);
            self.upstream_model = self.model.clone();
        }
        let Some(usage) = response.get("usage") else {
            return;
        };
        if let Some(prompt) = usage
            .get("prompt_tokens")
            .or_else(|| usage.get("input_tokens"))
            .and_then(Value::as_i64)
        {
            self.usage.input_tokens = prompt;
        }
        if let Some(completion) = usage
            .get("completion_tokens")
            .or_else(|| usage.get("output_tokens"))
            .and_then(Value::as_i64)
        {
            self.usage.output_tokens = completion;
        }
        self.usage.cache_read_input_tokens = first_positive_json_i64(
            self.usage.cache_read_input_tokens,
            usage,
            &[
                "input_tokens_details.cached_tokens",
                "prompt_tokens_details.cached_tokens",
                "cache_read_input_tokens",
                "cache_read_tokens",
                "cached_tokens",
            ],
        );
        self.usage.cache_creation_input_tokens = first_positive_json_i64(
            self.usage.cache_creation_input_tokens,
            usage,
            &[
                "input_tokens_details.cache_creation_input_tokens",
                "prompt_tokens_details.cache_creation_input_tokens",
                "input_tokens_details.cache_write_input_tokens",
                "prompt_tokens_details.cache_write_input_tokens",
                "input_tokens_details.cache_write_tokens",
                "prompt_tokens_details.cache_write_tokens",
                "input_tokens_details.cache_creation_tokens",
                "prompt_tokens_details.cache_creation_tokens",
                "cache_write_tokens",
                "cache_creation_input_tokens",
                "cache_write_input_tokens",
                "cache_creation_tokens",
            ],
        );
    }
}

fn first_positive_json_i64(current: i64, value: &Value, paths: &[&str]) -> i64 {
    if current > 0 {
        return current;
    }
    paths
        .iter()
        .filter_map(|path| value_get_path(value, path).and_then(Value::as_i64))
        .find(|tokens| *tokens > 0)
        .unwrap_or(0)
}

fn value_get_path<'a>(value: &'a Value, path: &str) -> Option<&'a Value> {
    path.split('.')
        .try_fold(value, |current, key| current.get(key))
}

fn is_zero(v: &i64) -> bool {
    *v == 0
}

impl AppState {
    async fn take_ws_idle(&self, plan: &EdgePlan) -> Option<WsIdleConn> {
        let key = ws_idle_key(plan)?;
        let mut pools = self.ws_idle.lock().await;
        let item = pools.get_mut(&key).and_then(Vec::pop);
        if item.is_some() {
            self.ws_idle_last_used
                .lock()
                .await
                .insert(key, Instant::now());
        }
        item
    }

    async fn ensure_ws_idle(&self, plan: EdgePlan) {
        if self.cfg.ws_idle_per_key == 0 {
            return;
        }
        if plan
            .proxy_url
            .as_deref()
            .is_some_and(|v| !v.trim().is_empty())
        {
            return;
        }
        let Some(key) = ws_idle_key(&plan) else {
            return;
        };
        {
            let pools = self.ws_idle.lock().await;
            if pools.get(&key).map_or(0, Vec::len) >= self.cfg.ws_idle_per_key {
                return;
            }
            if !pools.contains_key(&key) && pools.len() >= self.cfg.max_ws_idle_keys.max(1) {
                return;
            }
        }
        let state = self.clone();
        tokio::spawn(async move {
            match connect_ws_for_plan(&plan).await {
                Ok((socket, request_id)) => {
                    let mut pools = state.ws_idle.lock().await;
                    let pool = pools.entry(key.clone()).or_default();
                    if pool.len() < state.cfg.ws_idle_per_key {
                        pool.push(WsIdleConn { socket, request_id });
                        state
                            .ws_idle_last_used
                            .lock()
                            .await
                            .insert(key, Instant::now());
                    }
                }
                Err(err) => warn!("edge ws idle preconnect failed: {}", safe_edge_error(&err)),
            }
        });
    }

    fn relay_permit_for_plan(
        &self,
        plan: &EdgePlan,
    ) -> anyhow::Result<Option<OwnedSemaphorePermit>> {
        if self.cfg.queue_buffer_size == 0 || self.cfg.per_account_workers == 0 {
            return Ok(None);
        }
        let Some(key) = relay_queue_key(plan) else {
            return Ok(None);
        };

        let mut domains = self
            .relay_domains
            .lock()
            .map_err(|_| anyhow::anyhow!("relay domain map lock poisoned"))?;
        let domain = if let Some(domain) = domains.get(&key) {
            if let Ok(mut last_used) = self.relay_queue_last_used.lock() {
                last_used.insert(key.clone(), Instant::now());
            }
            domain.clone()
        } else {
            if domains.len() >= self.cfg.max_relay_domains.max(1) {
                return Err(anyhow::anyhow!("edge relay domain capacity exhausted"));
            }
            let domain = Arc::new(Semaphore::new(self.cfg.per_account_workers.max(1)));
            domains.insert(key.clone(), domain.clone());
            if let Ok(mut last_used) = self.relay_queue_last_used.lock() {
                last_used.insert(key, Instant::now());
            }
            domain
        };
        drop(domains);
        match domain.try_acquire_owned() {
            Ok(permit) => Ok(Some(permit)),
            Err(_) => Err(anyhow::anyhow!("edge relay domain capacity exhausted")),
        }
    }

    fn client_for_proxy(&self, proxy_url: Option<&str>) -> anyhow::Result<ProxyClientSelection> {
        let proxy_url = proxy_url.map(str::trim).filter(|v| !v.is_empty());
        let Some(proxy_url) = proxy_url else {
            return Ok(ProxyClientSelection {
                client: self.client.clone(),
                lease: None,
            });
        };

        let mut clients = self
            .clients_by_proxy
            .lock()
            .map_err(|_| anyhow::anyhow!("proxy client pool lock poisoned"))?;
        if let Some(entry) = clients.get(proxy_url) {
            return Ok(ProxyClientSelection::leased(Arc::clone(entry)));
        }
        if clients.len() >= self.cfg.max_proxy_clients.max(1) {
            let now = Instant::now();
            let idle = Duration::from_secs(self.cfg.proxy_client_idle_secs.max(60));
            let stale = clients
                .iter()
                .filter_map(|(key, entry)| {
                    let inactive = entry.active.load(Ordering::Acquire) == 0;
                    let last_used = entry
                        .last_used
                        .lock()
                        .map(|used| now.duration_since(*used) >= idle)
                        .unwrap_or(false);
                    (inactive && last_used).then_some(key.clone())
                })
                .collect::<Vec<_>>();
            for key in stale {
                clients.remove(&key);
            }
        }
        if clients.len() >= self.cfg.max_proxy_clients.max(1) {
            return Err(anyhow::anyhow!("edge proxy client capacity exhausted"));
        }

        let proxy = reqwest::Proxy::all(proxy_url)
            .map_err(|_| anyhow::anyhow!("invalid upstream proxy configuration"))?;
        let client = edge_http_client_builder(&self.cfg)
            .proxy(proxy)
            .build()
            .map_err(|_| anyhow::anyhow!("could not build upstream HTTP client"))?;
        let entry = Arc::new(ProxyClientEntry {
            client: client.clone(),
            active: AtomicU64::new(0),
            last_used: Mutex::new(Instant::now()),
        });
        clients.insert(proxy_url.to_string(), Arc::clone(&entry));
        Ok(ProxyClientSelection::leased(entry))
    }

    /// Emergency rollback path for the lane-pool feature flag. Keep this
    /// selection deliberately equivalent to the pre-lane implementation: a
    /// cached proxy client is reused while capacity is available, and a full
    /// cache returns the same local capacity error. The bounded transient
    /// proxy path is only reachable while lane pooling is enabled.
    fn legacy_client_for_proxy(
        &self,
        proxy_url: Option<&str>,
    ) -> anyhow::Result<ProxyClientSelection> {
        let proxy_url = proxy_url.map(str::trim).filter(|v| !v.is_empty());
        let Some(proxy_url) = proxy_url else {
            return Ok(ProxyClientSelection {
                client: self.client.clone(),
                lease: None,
            });
        };

        let mut clients = self
            .clients_by_proxy
            .lock()
            .map_err(|_| anyhow::anyhow!("proxy client pool lock poisoned"))?;
        if let Some(entry) = clients.get(proxy_url) {
            return Ok(ProxyClientSelection::legacy(entry));
        }
        if clients.len() >= self.cfg.max_proxy_clients.max(1) {
            return Err(anyhow::anyhow!("edge proxy client capacity exhausted"));
        }

        let proxy = reqwest::Proxy::all(proxy_url)
            .map_err(|_| anyhow::anyhow!("invalid upstream proxy configuration"))?;
        let client = edge_http_client_builder(&self.cfg)
            .proxy(proxy)
            .build()
            .map_err(|_| anyhow::anyhow!("could not build upstream HTTP client"))?;
        let entry = Arc::new(ProxyClientEntry {
            client: client.clone(),
            active: AtomicU64::new(0),
            last_used: Mutex::new(Instant::now()),
        });
        clients.insert(proxy_url.to_string(), Arc::clone(&entry));
        Ok(ProxyClientSelection::legacy(&entry))
    }

    async fn acquire_transient_proxy_guard(&self) -> anyhow::Result<TransientProxyGuard> {
        acquire_transient_proxy_guard(
            Arc::clone(&self.metrics),
            Arc::clone(&self.transient_proxy_permits),
        )
        .await
    }

    /// Resource-bounded legacy client selection used while lane pooling is
    /// enabled. Account types that are not lane-eligible still need the cached
    /// proxy lease/transient fallback; otherwise a long SSE can outlive a
    /// reaped map entry and repeated proxy churn can bypass the Client/FD cap.
    async fn bounded_legacy_client_for_proxy(
        &self,
        proxy_url: Option<&str>,
    ) -> anyhow::Result<SelectedUpstreamClient> {
        let proxy_url = proxy_url.map(str::trim).filter(|value| !value.is_empty());
        let Some(proxy_url) = proxy_url else {
            return Ok(SelectedUpstreamClient {
                client: self.client.clone(),
                guard: UpstreamClientGuard::Legacy,
            });
        };

        match self.client_for_proxy(Some(proxy_url)) {
            Ok(ProxyClientSelection { client, lease }) => {
                return Ok(SelectedUpstreamClient {
                    client,
                    guard: UpstreamClientGuard::LegacyProxy(lease),
                });
            }
            Err(error)
                if error
                    .to_string()
                    .contains("edge proxy client capacity exhausted") => {}
            Err(error) => return Err(error),
        }

        let transient_guard = self
            .acquire_transient_proxy_guard()
            .await
            .map_err(|error| {
                anyhow::anyhow!("edge transient proxy client capacity closed: {error}")
            })?;
        // A request-scoped client is already bounded by the semaphore. Keep
        // its own idle pool at one connection as well; inheriting the legacy
        // 128-connection setting would multiply the FD ceiling by the number
        // of transient permits.
        let client = build_standalone_client(Some(proxy_url), 1).map_err(|error| {
            anyhow::anyhow!("edge transient proxy client build failed: {error}")
        })?;
        Ok(SelectedUpstreamClient {
            client,
            guard: UpstreamClientGuard::Transient(transient_guard),
        })
    }

    async fn upstream_client_for_plan(
        &self,
        account_id: Option<i64>,
        account_type: Option<&str>,
        proxy_url: Option<&str>,
        upstream_url: &str,
        lane: Option<&str>,
    ) -> anyhow::Result<SelectedUpstreamClient> {
        // This is a process-start feature flag, so the emergency-off path is
        // intentionally decided before constructing any lane-pool key. It
        // must return to the old direct/proxy Client behavior, including its
        // old bounded cached-proxy capacity error, without transient clients.
        if !self.http_lane_pool.enabled() {
            let ProxyClientSelection { client, lease } = self.legacy_client_for_proxy(proxy_url)?;
            return Ok(SelectedUpstreamClient {
                client,
                guard: UpstreamClientGuard::LegacyProxy(lease),
            });
        }
        let request = LaneRequest {
            account_id,
            proxy_url,
            origin_url: upstream_url,
            lane,
        };
        let pool_eligible = self.http_lane_pool.enabled()
            && self
                .http_lane_pool
                .can_pool_for_account_type(request, account_type);
        if !pool_eligible {
            return self.bounded_legacy_client_for_proxy(proxy_url).await;
        }
        match self.http_lane_pool.acquire(request).await {
            Ok(selection) => {
                return Ok(SelectedUpstreamClient {
                    client: selection.client,
                    guard: UpstreamClientGuard::Lane(selection.guard),
                });
            }
            Err(error) if is_capacity_error(&error) => {
                self.http_lane_pool.record_legacy_fallback();
            }
            Err(error) if is_legacy_fallback_error(&error) => {
                self.http_lane_pool.record_legacy_fallback();
            }
            Err(_error) => {
                // A lane is an optimization layer. If its local registry or
                // client builder cannot serve this request, keep the Go-selected
                // account/plan and use the existing legacy client instead of
                // re-entering Go scheduling.
                self.http_lane_pool.record_legacy_fallback();
                debug!("upstream lane selection failed; using legacy client");
            }
        }

        self.bounded_legacy_client_for_proxy(proxy_url).await
    }

    fn record_dynamic_warm_key(&self, plan: &EdgePlan) {
        let Some(warm_url) = upstream_origin_warm_url(plan.upstream_url.as_deref()) else {
            return;
        };
        let proxy_url = plan
            .proxy_url
            .as_deref()
            .map(str::trim)
            .filter(|v| !v.is_empty())
            .map(ToOwned::to_owned);
        let key = dynamic_warm_key(proxy_url.as_deref(), &warm_url);
        let Ok(mut warm_keys) = self.warm_keys.lock() else {
            warn!("dynamic upstream warm key map lock poisoned");
            return;
        };
        if !warm_keys.contains_key(&key) && warm_keys.len() >= self.cfg.max_dynamic_warm_keys.max(1)
        {
            return;
        }
        warm_keys
            .entry(key)
            .and_modify(|item| item.last_seen = Instant::now())
            .or_insert(WarmKeyState {
                proxy_url,
                warm_url,
                last_seen: Instant::now(),
                failures: 0,
                warming: false,
            });
    }
}

impl EdgeConfig {
    fn from_env() -> anyhow::Result<Self> {
        let listen_addr = env::var("SUB2API_EDGE_LISTEN_ADDR")
            .unwrap_or_else(|_| "127.0.0.1:18080".to_string())
            .parse()?;
        let go_base_url = env::var("SUB2API_EDGE_GO_BASE_URL")
            .unwrap_or_else(|_| "http://127.0.0.1:8080".to_string());
        let control_base_url =
            env::var("SUB2API_EDGE_CONTROL_BASE_URL").unwrap_or_else(|_| go_base_url.clone());
        let internal_secret = env::var("SUB2API_EDGE_INTERNAL_SECRET")
            .map_err(|_| anyhow::anyhow!("SUB2API_EDGE_INTERNAL_SECRET is required"))?;
        if internal_secret.trim().is_empty() {
            anyhow::bail!("SUB2API_EDGE_INTERNAL_SECRET must not be empty");
        }
        Ok(Self {
            listen_addr,
            go_base_url,
            control_base_url,
            internal_secret,
            edge_node_id: stable_edge_node_id(),
            prepare_timeout_ms: env_u64("SUB2API_EDGE_PREPARE_TIMEOUT_MS", 1500),
            complete_timeout_ms: env_u64("SUB2API_EDGE_COMPLETE_TIMEOUT_MS", 1500),
            initial_pool_size: env_usize("SUB2API_EDGE_INITIAL_POOL_SIZE", 512),
            queue_buffer_size: env_usize("SUB2API_EDGE_QUEUE_BUFFER_SIZE", 512),
            queue_max_bytes: env_usize("SUB2API_EDGE_QUEUE_MAX_BYTES", 256 * 1024 * 1024),
            max_header_bytes: env_usize("SUB2API_EDGE_MAX_HEADER_BYTES", 64 * 1024)
                .clamp(8 * 1024, 1024 * 1024),
            ingress_body_max_bytes: env_usize(
                "SUB2API_EDGE_INGRESS_BODY_MAX_BYTES",
                2 * 1024 * 1024 * 1024,
            )
            .min(u32::MAX as usize),
            global_workers: env_usize("SUB2API_EDGE_GLOBAL_WORKERS", 512),
            per_account_workers: env_usize("SUB2API_EDGE_PER_ACCOUNT_WORKERS", 128),
            max_relay_domains: env_usize("SUB2API_EDGE_MAX_RELAY_DOMAINS", 4096),
            relay_domain_idle_secs: env_u64("SUB2API_EDGE_RELAY_DOMAIN_IDLE_SECS", 300),
            max_proxy_clients: env_usize("SUB2API_EDGE_MAX_PROXY_CLIENTS", 1024),
            proxy_client_idle_secs: env_u64("SUB2API_EDGE_PROXY_CLIENT_IDLE_SECS", 300),
            max_idle_conns_per_account: env_usize(
                "SUB2API_EDGE_MAX_IDLE_PER_ACCOUNT",
                env_usize("SUB2API_EDGE_MAX_IDLE_PER_HOST", 128),
            ),
            transient_proxy_max_active: env_usize("SUB2API_EDGE_TRANSIENT_PROXY_MAX_ACTIVE", 32)
                .clamp(1, 4096),
            queue_wait_budget_ms: env_u64("SUB2API_EDGE_QUEUE_WAIT_BUDGET_MS", 150),
            large_payload_passthrough: env_bool("SUB2API_EDGE_LARGE_PAYLOAD_PASSTHROUGH", true),
            large_payload_threshold_bytes: env_usize(
                "SUB2API_EDGE_LARGE_PAYLOAD_THRESHOLD_BYTES",
                256 * 1024,
            ),
            ws_idle_per_key: env_usize("SUB2API_EDGE_WS_IDLE_PER_KEY", 1),
            max_ws_idle_keys: env_usize("SUB2API_EDGE_MAX_WS_IDLE_KEYS", 1024),
            ws_idle_ttl_secs: env_u64("SUB2API_EDGE_WS_IDLE_TTL_SECS", 300),
            drain_timeout_secs: env_u64("SUB2API_EDGE_DRAIN_TIMEOUT_SECS", 30),
            // P3: 后台上游连接保活。显式配置 URL 后启用；默认关闭，避免代理环境
            // 在没有直连业务时凭空直连上游。
            upstream_warm_url: match env::var("SUB2API_EDGE_UPSTREAM_WARM_URL") {
                Ok(v) if v.trim().is_empty() => None, // 显式置空 = 关闭
                Ok(v) => Some(v),
                Err(_) => None,
            },
            upstream_warm_interval_secs: env_u64("SUB2API_EDGE_UPSTREAM_WARM_INTERVAL_SECS", 30),
            upstream_dynamic_warm_active_secs: env_u64(
                "SUB2API_EDGE_UPSTREAM_DYNAMIC_WARM_ACTIVE_SECS",
                300,
            ),
            max_dynamic_warm_keys: env_usize("SUB2API_EDGE_MAX_DYNAMIC_WARM_KEYS", 4096),
        })
    }
}

fn stable_edge_node_id() -> Option<String> {
    non_empty_env("SUB2API_EDGE_NODE_ID")
        .or_else(|| non_empty_env("HOSTNAME"))
        .or_else(|| {
            std::fs::read_to_string("/etc/hostname")
                .ok()
                .map(|value| value.trim().to_string())
                .filter(|value| !value.is_empty())
        })
}

fn non_empty_env(key: &str) -> Option<String> {
    env::var(key)
        .ok()
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())
}

fn env_u64(key: &str, default_value: u64) -> u64 {
    env::var(key)
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(default_value)
}

fn env_usize(key: &str, default_value: usize) -> usize {
    env::var(key)
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(default_value)
}

fn env_bool(key: &str, default_value: bool) -> bool {
    env::var(key)
        .ok()
        .map(|v| matches!(v.to_ascii_lowercase().as_str(), "1" | "true" | "yes" | "on"))
        .unwrap_or(default_value)
}

const B64_TABLE: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

fn b64_encode(input: &[u8]) -> String {
    let mut out = String::with_capacity(input.len().div_ceil(3) * 4);
    for chunk in input.chunks(3) {
        let b0 = chunk[0];
        let b1 = *chunk.get(1).unwrap_or(&0);
        let b2 = *chunk.get(2).unwrap_or(&0);
        out.push(B64_TABLE[(b0 >> 2) as usize] as char);
        out.push(B64_TABLE[(((b0 & 0x03) << 4) | (b1 >> 4)) as usize] as char);
        if chunk.len() > 1 {
            out.push(B64_TABLE[(((b1 & 0x0f) << 2) | (b2 >> 6)) as usize] as char);
        } else {
            out.push('=');
        }
        if chunk.len() > 2 {
            out.push(B64_TABLE[(b2 & 0x3f) as usize] as char);
        } else {
            out.push('=');
        }
    }
    out
}

fn b64_decode(input: &str) -> anyhow::Result<Vec<u8>> {
    let cleaned: Vec<u8> = input.bytes().filter(|b| !b.is_ascii_whitespace()).collect();
    if !cleaned.len().is_multiple_of(4) {
        anyhow::bail!("invalid base64 length");
    }
    let mut out = Vec::with_capacity(cleaned.len() / 4 * 3);
    for chunk in cleaned.chunks(4) {
        let v0 = b64_value(chunk[0])?;
        let v1 = b64_value(chunk[1])?;
        let pad2 = chunk[2] == b'=';
        let pad3 = chunk[3] == b'=';
        let v2 = if pad2 { 0 } else { b64_value(chunk[2])? };
        let v3 = if pad3 { 0 } else { b64_value(chunk[3])? };
        out.push((v0 << 2) | (v1 >> 4));
        if !pad2 {
            out.push(((v1 & 0x0f) << 4) | (v2 >> 2));
        }
        if !pad3 {
            out.push(((v2 & 0x03) << 6) | v3);
        }
    }
    Ok(out)
}

fn b64_value(byte: u8) -> anyhow::Result<u8> {
    match byte {
        b'A'..=b'Z' => Ok(byte - b'A'),
        b'a'..=b'z' => Ok(byte - b'a' + 26),
        b'0'..=b'9' => Ok(byte - b'0' + 52),
        b'+' => Ok(62),
        b'/' => Ok(63),
        _ => anyhow::bail!("invalid base64 byte"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn request_header_bytes_counts_names_values_and_wire_overhead() {
        let mut headers = HeaderMap::new();
        headers.insert("x-test", HeaderValue::from_static("value"));
        assert_eq!(
            request_header_bytes(&headers),
            "x-test".len() + "value".len() + 4
        );
    }

    #[test]
    fn proxy_client_lease_keeps_entry_active_and_refreshes_idle_time_on_drop() {
        let entry = Arc::new(ProxyClientEntry {
            client: Client::new(),
            active: AtomicU64::new(0),
            last_used: Mutex::new(Instant::now() - Duration::from_secs(3_600)),
        });
        let lease = ProxyClientLease::new(Arc::clone(&entry));
        assert_eq!(entry.active.load(Ordering::Acquire), 1);

        *entry.last_used.lock().unwrap() = Instant::now() - Duration::from_secs(3_600);
        drop(lease);

        assert_eq!(entry.active.load(Ordering::Acquire), 0);
        assert!(entry.last_used.lock().unwrap().elapsed() < Duration::from_secs(1));
    }

    #[test]
    fn legacy_proxy_selection_does_not_pin_cache_entry() {
        let entry = Arc::new(ProxyClientEntry {
            client: Client::new(),
            active: AtomicU64::new(0),
            last_used: Mutex::new(Instant::now() - Duration::from_secs(3_600)),
        });

        let legacy = ProxyClientSelection::legacy(&entry);
        assert!(legacy.lease.is_none());
        assert_eq!(entry.active.load(Ordering::Acquire), 0);
        assert!(entry.last_used.lock().unwrap().elapsed() < Duration::from_secs(1));
        drop(legacy);
        assert_eq!(entry.active.load(Ordering::Acquire), 0);

        let leased = ProxyClientSelection::leased(Arc::clone(&entry));
        assert!(leased.lease.is_some());
        assert_eq!(entry.active.load(Ordering::Acquire), 1);
        drop(leased);
        assert_eq!(entry.active.load(Ordering::Acquire), 0);
    }

    #[tokio::test]
    async fn transient_proxy_permit_wait_is_bounded_and_cancellable() {
        let metrics = Arc::new(EdgeMetrics::default());
        let permits = Arc::new(Semaphore::new(1));
        let first = acquire_transient_proxy_guard(Arc::clone(&metrics), Arc::clone(&permits))
            .await
            .expect("first transient proxy permit");
        assert_eq!(metrics.transient_proxy_active.load(Ordering::Relaxed), 1);
        assert_eq!(metrics.transient_proxy_waiters.load(Ordering::Relaxed), 0);

        let wait_metrics = Arc::clone(&metrics);
        let wait_permits = Arc::clone(&permits);
        let waiter =
            tokio::spawn(
                async move { acquire_transient_proxy_guard(wait_metrics, wait_permits).await },
            );
        tokio::time::timeout(Duration::from_millis(100), async {
            while metrics.transient_proxy_waiters.load(Ordering::Relaxed) != 1 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("second request enters bounded wait");
        waiter.abort();
        let _ = waiter.await;
        tokio::task::yield_now().await;
        assert_eq!(metrics.transient_proxy_waiters.load(Ordering::Relaxed), 0);
        assert_eq!(metrics.transient_proxy_active.load(Ordering::Relaxed), 1);

        drop(first);
        assert_eq!(metrics.transient_proxy_active.load(Ordering::Relaxed), 0);
        let next = acquire_transient_proxy_guard(Arc::clone(&metrics), Arc::clone(&permits))
            .await
            .expect("permit is reusable after cancellation");
        assert_eq!(metrics.transient_proxy_active.load(Ordering::Relaxed), 1);
        drop(next);
        assert_eq!(metrics.transient_proxy_active.load(Ordering::Relaxed), 0);
    }

    #[tokio::test]
    async fn transient_proxy_permit_waits_until_capacity_is_released() {
        let metrics = Arc::new(EdgeMetrics::default());
        let permits = Arc::new(Semaphore::new(1));
        let first = acquire_transient_proxy_guard(Arc::clone(&metrics), Arc::clone(&permits))
            .await
            .expect("first transient proxy permit");

        let wait_metrics = Arc::clone(&metrics);
        let wait_permits = Arc::clone(&permits);
        let mut waiter =
            tokio::spawn(
                async move { acquire_transient_proxy_guard(wait_metrics, wait_permits).await },
            );
        assert!(tokio::time::timeout(Duration::from_millis(5), &mut waiter)
            .await
            .is_err());
        assert_eq!(metrics.transient_proxy_waiters.load(Ordering::Relaxed), 1);
        assert_eq!(metrics.transient_proxy_active.load(Ordering::Relaxed), 1);
        drop(first);
        let second = tokio::time::timeout(Duration::from_millis(100), waiter)
            .await
            .expect("waiter resumes when capacity is released")
            .expect("waiter task")
            .expect("second transient proxy permit");
        assert_eq!(metrics.transient_proxy_waiters.load(Ordering::Relaxed), 0);
        assert_eq!(metrics.transient_proxy_active.load(Ordering::Relaxed), 1);
        drop(second);
        assert_eq!(metrics.transient_proxy_active.load(Ordering::Relaxed), 0);
    }

    #[tokio::test]
    async fn settlement_retry_recovers_after_transient_failures() {
        let attempts = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let operation_attempts = attempts.clone();
        let completed_attempt = retry_with_backoff(
            4,
            Duration::from_millis(1),
            Duration::from_millis(2),
            move || {
                let attempts = operation_attempts.clone();
                async move {
                    let attempt = attempts.fetch_add(1, Ordering::SeqCst) + 1;
                    if attempt < 3 {
                        anyhow::bail!("temporary control-plane failure");
                    }
                    Ok(())
                }
            },
        )
        .await
        .expect("retry should recover");

        assert_eq!(completed_attempt, 3);
        assert_eq!(attempts.load(Ordering::SeqCst), 3);
    }

    #[test]
    fn ws_completed_event_updates_usage_summary() {
        let mut summary = ChatStreamSummary::default();
        summary.observe_ws_message(&TungsteniteMessage::Text(
            serde_json::json!({
                "type": "response.completed",
                "response": {
                    "id": "resp_123",
                    "model": "gpt-4.1",
                    "usage": {
                        "input_tokens": 11,
                        "output_tokens": 7,
                        "input_tokens_details": {
                            "cached_tokens": 3,
                            "cache_creation_input_tokens": 5
                        }
                    }
                }
            })
            .to_string(),
        ));

        assert_eq!(summary.response_id.as_deref(), Some("resp_123"));
        assert_eq!(summary.model.as_deref(), Some("gpt-4.1"));
        assert_eq!(summary.usage.input_tokens, 11);
        assert_eq!(summary.usage.output_tokens, 7);
        assert_eq!(summary.usage.cache_read_input_tokens, 3);
        assert_eq!(summary.usage.cache_creation_input_tokens, 5);
        let usage_json = serde_json::to_value(&summary.usage).expect("serialize usage");
        assert_eq!(usage_json["cache_creation_input_tokens"], 5);
    }

    #[test]
    fn usage_summary_accepts_cache_creation_aliases_without_zero_overwrite() {
        let mut nested = ChatStreamSummary::default();
        nested.observe_ws_message(&TungsteniteMessage::Text(
            serde_json::json!({
                "usage": {
                    "input_tokens_details": {
                        "cache_creation_input_tokens": 0,
                        "cache_write_tokens": 9
                    },
                    "cache_creation_input_tokens": 24,
                    "cache_read_input_tokens": 31
                }
            })
            .to_string(),
        ));

        assert_eq!(nested.usage.cache_creation_input_tokens, 9);
        assert_eq!(nested.usage.cache_read_input_tokens, 31);

        nested.observe_ws_message(&TungsteniteMessage::Text(
            serde_json::json!({
                "usage": {
                    "input_tokens_details": {
                        "cache_creation_input_tokens": 0
                    },
                    "cache_creation_input_tokens": 0
                }
            })
            .to_string(),
        ));

        assert_eq!(nested.usage.cache_creation_input_tokens, 9);
    }

    #[test]
    fn stream_failure_class_distinguishes_http_error_from_disconnect() {
        let summary = ChatStreamSummary::default();

        assert_eq!(
            classify_stream_failure_with_status(false, false, &summary, Some(502)).as_deref(),
            Some("upstream_error")
        );
        assert_eq!(
            classify_stream_failure_with_status(false, false, &summary, Some(200)).as_deref(),
            Some("upstream_disconnect")
        );
        assert_eq!(
            classify_stream_failure_with_status(false, true, &summary, Some(502)).as_deref(),
            Some("client_cancelled")
        );
    }

    #[test]
    fn client_disconnect_completion_preserves_partial_usage_but_clears_ttft() {
        let mut request = pending_stream_complete_request(
            "edge-1".to_string(),
            Some("lease-1".to_string()),
            Some(42),
            Some(3),
            Some(4),
            Some(5),
            None,
            2,
        );
        request.success = true;
        request.usage = Usage {
            input_tokens: 11,
            output_tokens: 7,
            cache_creation_input_tokens: 0,
            cache_read_input_tokens: 3,
        };
        request.first_token_ms = Some(80);
        request.real_first_token_ms = Some(95);
        request.terminal_event_type = Some("response.incomplete".to_string());

        mark_complete_request_client_disconnected(&mut request, 120);

        assert!(!request.success);
        assert!(request.client_disconnected);
        assert_eq!(request.usage.input_tokens, 11);
        assert_eq!(request.usage.output_tokens, 7);
        assert_eq!(request.usage.cache_read_input_tokens, 3);
        assert_eq!(request.duration_ms, 120);
        assert_eq!(request.first_token_ms, None);
        assert_eq!(request.real_first_token_ms, None);
        assert_eq!(
            request.terminal_event_type.as_deref(),
            Some("response.incomplete")
        );
    }

    #[test]
    fn complete_guard_sample_timestamp_is_stable_across_retries() {
        let mut request = pending_stream_complete_request(
            "edge-1".to_string(),
            Some("lease-1".to_string()),
            Some(42),
            None,
            None,
            None,
            None,
            0,
        );

        stamp_complete_guard_sample(&mut request);
        assert_eq!(request.guard_sample_at_unix_ns, None);

        request.real_first_token_ms = Some(95);
        stamp_complete_guard_sample(&mut request);
        let recorded_at = request
            .guard_sample_at_unix_ns
            .expect("guard completion timestamp");

        stamp_complete_guard_sample(&mut request);
        assert_eq!(request.guard_sample_at_unix_ns, Some(recorded_at));
    }

    #[test]
    fn base64_round_trip_preserves_raw_body() {
        let raw = br#"{"model":"gpt-4.1","stream":true,"input":"hello"}"#;
        let encoded = b64_encode(raw);
        let decoded = b64_decode(&encoded).expect("decode raw body");
        assert_eq!(decoded, raw);
    }

    #[test]
    fn low_latency_policy_maps_smart_and_aggressive() {
        let aggressive = low_latency_policy(Some("aggressive"));
        assert!(aggressive.enabled);
        assert_eq!(aggressive.barrier, None);

        let smart = low_latency_policy(Some("smart"));
        assert!(smart.enabled);
        assert_eq!(smart.barrier, Some(Duration::from_millis(25)));

        let off = low_latency_policy(Some("off"));
        assert!(!off.enabled);
        assert_eq!(off.barrier, None);
    }

    #[test]
    fn stream_summary_distinguishes_preamble_from_client_output() {
        let mut summary = ChatStreamSummary::default();
        let created_data =
            summary.observe(b"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_123\",\"model\":\"gpt-4.1\"}}\n");
        assert!(!created_data.starts_client_output);
        assert!(!created_data.saw_response_created);

        let created = summary.observe(b"\n");
        assert!(!created.starts_client_output);
        assert!(created.saw_response_created);
        assert_eq!(summary.response_id.as_deref(), Some("resp_123"));
        assert_eq!(summary.model.as_deref(), Some("gpt-4.1"));

        assert!(
            summary
                .observe(b"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
                .starts_client_output
        );
    }

    #[test]
    fn sse_preamble_gate_buffers_until_client_output_when_disabled() {
        let mut gate = SsePreambleGate::new(false);
        assert!(gate
            .accept(Bytes::from_static(b"created\n\n"), false, false)
            .is_empty());

        let released = gate.accept(Bytes::from_static(b"delta\n\n"), true, false);
        assert_eq!(released.len(), 2);
        assert_eq!(released[0], Bytes::from_static(b"created\n\n"));
        assert_eq!(released[1], Bytes::from_static(b"delta\n\n"));
        assert!(gate.force().is_empty(), "released bytes must not repeat");
    }

    #[test]
    fn sse_preamble_gate_flushes_immediately_when_enabled() {
        let mut gate = SsePreambleGate::new(true);
        let output = gate.accept(Bytes::from_static(b"created\n\n"), false, false);
        assert_eq!(output, vec![Bytes::from_static(b"created\n\n")]);
    }

    #[test]
    fn sse_preamble_gate_force_releases_pending_without_marking_token() {
        let mut gate = SsePreambleGate::new(false);
        assert!(gate
            .accept(Bytes::from_static(b"created\n\n"), false, false)
            .is_empty());
        let output = gate.force();
        assert_eq!(output, vec![Bytes::from_static(b"created\n\n")]);
        // A subsequent empty accept remains released; callers can emit a
        // local placeholder separately without treating the preamble as a
        // real token event.
        assert!(gate.accept(Bytes::new(), false, false).is_empty());
    }

    #[test]
    fn sse_preamble_gate_preserves_tokenless_completed_stream_at_end() {
        let mut gate = SsePreambleGate::new(false);
        assert!(gate
            .accept(Bytes::from_static(b"created\n\n"), false, false)
            .is_empty());
        assert!(gate
            .accept(Bytes::from_static(b"completed\n\n"), false, false)
            .is_empty());

        let output = gate.force();
        assert_eq!(
            output,
            vec![Bytes::from_static(b"created\n\ncompleted\n\n")]
        );
        assert!(gate.force().is_empty(), "final drain must be idempotent");
    }

    #[test]
    fn sse_preamble_gate_fails_open_at_bounded_buffer_size() {
        let mut gate = SsePreambleGate::new(false);
        let oversized = Bytes::from(vec![b'x'; SSE_PREAMBLE_BUFFER_MAX_BYTES + 1]);
        let output = gate.accept(oversized.clone(), false, false);
        assert_eq!(output, vec![oversized]);
        assert_eq!(
            gate.accept(Bytes::from_static(b"next"), false, false),
            vec![Bytes::from_static(b"next")]
        );
    }

    #[test]
    fn chat_completion_delta_counts_as_client_output() {
        let mut summary = ChatStreamSummary::default();
        assert!(
            summary
                .observe(b"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
                .starts_client_output
        );
    }

    #[test]
    fn safe_token_placeholder_frame_is_empty_output_delta() {
        let frame = openai_responses_safe_token_placeholder_frame(Some("resp_123"));
        assert!(frame.starts_with("data: "));
        assert!(frame.ends_with("\n\n"));
        assert!(frame.contains("\"type\":\"response.output_text.delta\""));
        assert!(frame.contains("\"delta\":\"\""));
        assert!(frame.contains("\"response_id\":\"resp_123\""));
    }

    #[test]
    fn first_token_timeout_placeholder_ms_accepts_safe_range() {
        assert_eq!(
            normalize_first_token_timeout_placeholder_ms(Some(1)),
            Some(Duration::from_millis(1))
        );
        assert_eq!(
            normalize_first_token_timeout_placeholder_ms(Some(100)),
            Some(Duration::from_millis(100))
        );
        assert_eq!(
            normalize_first_token_timeout_placeholder_ms(Some(200)),
            Some(Duration::from_millis(200))
        );
        assert_eq!(
            normalize_first_token_timeout_placeholder_ms(Some(3000)),
            Some(Duration::from_millis(3000))
        );
        assert_eq!(normalize_first_token_timeout_placeholder_ms(Some(0)), None);
        assert_eq!(
            normalize_first_token_timeout_placeholder_ms(Some(3001)),
            None
        );
        assert_eq!(normalize_first_token_timeout_placeholder_ms(None), None);
    }

    #[test]
    fn constant_time_eq_matches_bytes_and_lengths() {
        assert!(constant_time_eq(b"secret", b"secret"));
        assert!(!constant_time_eq(b"secret", b"secrex"));
        assert!(!constant_time_eq(b"secret", b"secret-longer"));
        assert!(!constant_time_eq(b"", b"x"));
    }

    #[test]
    fn first_token_timeout_placeholder_uses_chat_chunk_for_chat_dialect() {
        let summary = ChatStreamSummary {
            response_id: Some("chatcmpl_123".to_string()),
            model: Some("gpt-4.1".to_string()),
            ..Default::default()
        };

        let frame = openai_stream_timeout_placeholder_frame(Some("chat_completions"), &summary);
        assert!(frame.starts_with("data: "));
        assert!(frame.ends_with("\n\n"));
        assert!(frame.contains("\"object\":\"chat.completion.chunk\""));
        assert!(frame.contains("\"role\":\"assistant\""));
        assert!(frame.contains("\"content\":\"\""));
        assert!(frame.contains("\"id\":\"chatcmpl_123\""));
    }

    #[test]
    fn openai_error_response_is_sanitized_json() {
        let response = openai_error_response(
            StatusCode::BAD_REQUEST,
            "invalid_request_error",
            "Upstream pool rejected this request; check routing configuration",
        );
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
        assert_eq!(
            response
                .headers()
                .get(header::CONTENT_TYPE)
                .unwrap()
                .to_str()
                .unwrap(),
            "application/json"
        );
    }

    #[test]
    fn sse_error_and_html_are_replaced_without_upstream_details() {
        let mut sanitizer = OpenAIStreamSanitizer::new(None);
        let output = sanitizer.push(
            br#"data: {"type":"response.failed","response":{"error":{"message":"<!DOCTYPE html> https://private.example"}}}

"#,
        );
        let output = String::from_utf8(output.to_vec()).unwrap();
        assert!(output.contains("Upstream request failed"));
        assert!(!output.contains("private.example"));
        assert!(!output.contains("DOCTYPE"));
    }

    #[test]
    fn chat_diagnostic_success_chunk_is_sanitized_and_never_marks_success() {
        let input = b"data: {\"message\":\"private-provider.example failed\"}\n\ndata: [DONE]\n\n";
        let mut sanitizer = OpenAIStreamSanitizer::new(Some("chat_completions"));
        let output = String::from_utf8(sanitizer.push(input).to_vec()).unwrap();
        assert!(output.contains("Upstream request failed"));
        assert!(!output.contains("private-provider.example"));

        let mut summary = ChatStreamSummary::with_pending(String::new(), Some("chat_completions"));
        summary.observe(input);
        assert!(summary.failed);
        assert!(!summary.completed_successfully(Some("chat_completions")));
    }

    #[test]
    fn sse_error_null_and_normal_delta_remain_unchanged() {
        let mut sanitizer = OpenAIStreamSanitizer::new(None);
        let input = br#"data: {"type":"response.in_progress","response":{"error":null}}
data: {"type":"response.output_text.delta","delta":"ok"}

"#;
        let output = sanitizer.push(input);
        assert_eq!(output.as_ref(), input);

        let mut summary = ChatStreamSummary::default();
        summary.observe(input);
        assert!(!summary.failed);
    }

    #[test]
    fn sse_completed_image_generation_status_is_normalized() {
        let mut sanitizer = OpenAIStreamSanitizer::new(None);
        let input = br#"data: {"type":"response.output_item.done","item":{"type":"image_generation_call","status":"generating","result":"final-image"}}

data: {"type":"response.completed","response":{"output":[{"type":"image_generation_call","status":"in_progress","result":"final-image"}]}}

"#;
        let output = String::from_utf8(sanitizer.push(input).to_vec()).unwrap();
        let statuses = output
            .lines()
            .filter_map(|line| line.strip_prefix("data: "))
            .map(|payload| serde_json::from_str::<Value>(payload).unwrap())
            .collect::<Vec<_>>();
        assert_eq!(statuses[0]["item"]["status"], "completed");
        assert_eq!(statuses[1]["response"]["output"][0]["status"], "completed");
    }

    #[test]
    fn sse_error_split_across_chunks_is_sanitized_once_complete() {
        let mut sanitizer = OpenAIStreamSanitizer::new(None);
        let first = sanitizer
            .push(br#"data: {"type":"response.failed","response":{"error":{"message":"private."#);
        assert!(first.is_empty());
        let second = sanitizer.push(b"example\"}}}\n\n");
        let output = String::from_utf8(second.to_vec()).unwrap();
        assert!(output.contains("Upstream request failed"));
        assert!(!output.contains("private.example"));
    }

    #[test]
    fn malformed_sse_cannot_become_success_after_done() {
        let input = b"data: {not-json}\n\ndata: [DONE]\n\n";
        let mut sanitizer = OpenAIStreamSanitizer::new(Some("chat_completions"));
        let output = String::from_utf8(sanitizer.push(input).to_vec()).unwrap();
        assert!(output.contains("Upstream request failed"));

        let mut summary = ChatStreamSummary::with_pending(String::new(), Some("chat_completions"));
        summary.observe(input);
        assert!(summary.failed);
        assert!(!summary.completed_successfully(Some("chat_completions")));
        assert_eq!(summary.terminal_event_type(None).as_deref(), Some("error"));
    }

    #[test]
    fn ws_error_and_binary_frames_are_replaced() {
        let error = sanitize_openai_ws_message(TungsteniteMessage::Text(
            r#"{"type":"error","error":{"message":"private.example"}}"#.to_string(),
        ));
        assert!(
            matches!(error, TungsteniteMessage::Text(text) if !text.contains("private.example") && text.contains("Upstream request failed"))
        );
        let binary = sanitize_openai_ws_message(TungsteniteMessage::Binary(vec![0xff]));
        assert!(
            matches!(binary, TungsteniteMessage::Text(text) if text.contains("Upstream request failed"))
        );

        let diagnostic = sanitize_openai_ws_message(TungsteniteMessage::Text(
            r#"{"provider":"private-provider.example","status":"failed"}"#.to_string(),
        ));
        assert!(
            matches!(diagnostic, TungsteniteMessage::Text(text) if !text.contains("private-provider.example") && text.contains("Upstream request failed"))
        );

        let nested = sanitize_openai_ws_message(TungsteniteMessage::Text(
            r#"{"data":{"error":{"message":"https://private-provider.example failed"}}}"#
                .to_string(),
        ));
        assert!(
            matches!(nested, TungsteniteMessage::Text(text) if !text.contains("private-provider.example") && text.contains("Upstream request failed"))
        );
        assert!(!json_is_unsafe_upstream_diagnostic(&serde_json::json!({
            "data": [{"embedding": [0.1, 0.2], "index": 0}]
        })));
    }

    #[test]
    fn ws_completed_image_generation_status_is_normalized() {
        let done = sanitize_openai_ws_message(TungsteniteMessage::Text(
            r#"{"type":"response.output_item.done","item":{"type":"image_generation_call","status":"generating","result":"final-image"}}"#.to_string(),
        ));
        let TungsteniteMessage::Text(done) = done else {
            panic!("expected text frame")
        };
        let done: Value = serde_json::from_str(&done).unwrap();
        assert_eq!(done["item"]["status"], "completed");

        let completed = sanitize_openai_ws_message(TungsteniteMessage::Text(
            r#"{"type":"response.completed","response":{"output":[{"type":"image_generation_call","status":"in_progress","result":"final-image"}]}}"#.to_string(),
        ));
        let TungsteniteMessage::Text(completed) = completed else {
            panic!("expected text frame")
        };
        let completed: Value = serde_json::from_str(&completed).unwrap();
        assert_eq!(completed["response"]["output"][0]["status"], "completed");
    }

    #[test]
    fn ws_image_generation_without_result_or_terminal_event_is_unchanged() {
        let mut without_result = serde_json::json!({
            "type": "response.output_item.done",
            "item": {"type": "image_generation_call", "status": "generating"}
        });
        assert!(!normalize_completed_image_generation_status(
            &mut without_result
        ));
        assert_eq!(without_result["item"]["status"], "generating");

        let mut non_terminal = serde_json::json!({
            "type": "response.in_progress",
            "output": [{"type": "image_generation_call", "status": "in_progress", "result": "partial"}]
        });
        assert!(!normalize_completed_image_generation_status(
            &mut non_terminal
        ));
        assert_eq!(non_terminal["output"][0]["status"], "in_progress");
    }

    #[test]
    fn ws_non_image_frames_preserve_original_text_bytes() {
        let original = r#"{ "type": "response.output_text.delta", "delta": "first token" }"#;
        let sanitized = sanitize_openai_ws_message(TungsteniteMessage::Text(original.to_string()));
        assert!(matches!(sanitized, TungsteniteMessage::Text(text) if text == original));
    }

    #[test]
    fn ws_cache_creation_reduce_marks_only_long_stable_prefix() {
        let stable = "stable developer policy ".repeat(260);
        let mut session_model = Some("gpt-5.6-sol".to_string());
        let input = serde_json::json!({
            "type": "response.create",
            "model": "gpt-5.6-sol",
            "prompt_cache_retention": "24h",
            "input": [
                {"role": "developer", "content": stable},
                {"role": "user", "content": [{
                    "type": "input_text",
                    "text": "dynamic",
                    "prompt_cache_breakpoint": {"mode": "explicit"}
                }]}
            ]
        });
        let output = apply_openai_prompt_cache_creation_optimization_ws_message(
            TungsteniteMessage::Text(input.to_string()),
            Some("reduce"),
            &mut session_model,
        );
        let TungsteniteMessage::Text(output) = output else {
            panic!("expected text frame");
        };
        let output: Value = serde_json::from_str(&output).expect("optimized response.create");
        assert_eq!(
            output
                .pointer("/prompt_cache_options/mode")
                .and_then(Value::as_str),
            Some("explicit")
        );
        assert_eq!(
            output
                .pointer("/prompt_cache_options/ttl")
                .and_then(Value::as_str),
            Some("30m")
        );
        assert!(output.get("prompt_cache_retention").is_none());
        assert_eq!(
            output
                .pointer("/input/0/content/0/prompt_cache_breakpoint/mode")
                .and_then(Value::as_str),
            Some("explicit")
        );
        assert!(output
            .pointer("/input/1/content/0/prompt_cache_breakpoint")
            .is_none());
    }

    #[test]
    fn ws_cache_creation_reduce_keeps_short_string_content_shape() {
        let mut session_model = Some("gpt-5.6-sol".to_string());
        let input = serde_json::json!({
            "type": "response.create",
            "model": "gpt-5.6-sol",
            "input": [{"role": "developer", "content": "short stable policy"}]
        });
        let output = apply_openai_prompt_cache_creation_optimization_ws_message(
            TungsteniteMessage::Text(input.to_string()),
            Some("reduce"),
            &mut session_model,
        );
        let TungsteniteMessage::Text(output) = output else {
            panic!("expected text frame");
        };
        let output: Value = serde_json::from_str(&output).expect("optimized response.create");
        assert!(output
            .pointer("/input/0/content")
            .is_some_and(Value::is_string));
        assert!(output
            .pointer("/input/0/content/prompt_cache_breakpoint")
            .is_none());
    }

    #[test]
    fn ws_cache_creation_suppress_never_adds_breakpoint() {
        let stable = "stable developer policy ".repeat(260);
        let mut session_model = Some("gpt-5.6-terra".to_string());
        let input = serde_json::json!({
            "type": "response.create",
            "model": "gpt-5.6-terra",
            "prompt_cache_retention": "24h",
            "prompt_cache_options": {"mode": "implicit", "ttl": "24h"},
            "input": [{
                "role": "developer",
                "content": stable,
                "prompt_cache_breakpoint": {"mode": "explicit"}
            }, {
                "type": "function_call",
                "arguments": {"prompt_cache_breakpoint": "business-value"},
                "prompt_cache_breakpoint": {"mode": "explicit"}
            }]
        });
        let output = apply_openai_prompt_cache_creation_optimization_ws_message(
            TungsteniteMessage::Text(input.to_string()),
            Some("suppress"),
            &mut session_model,
        );
        let TungsteniteMessage::Text(output) = output else {
            panic!("expected text frame");
        };
        let output: Value = serde_json::from_str(&output).expect("optimized response.create");
        assert!(output
            .pointer("/input/0/content")
            .is_some_and(Value::is_string));
        assert!(output.pointer("/input/0/prompt_cache_breakpoint").is_none());
        assert!(output.pointer("/input/1/prompt_cache_breakpoint").is_none());
        assert_eq!(
            output
                .pointer("/input/1/arguments/prompt_cache_breakpoint")
                .and_then(Value::as_str),
            Some("business-value")
        );
        assert_eq!(
            output
                .pointer("/prompt_cache_options/mode")
                .and_then(Value::as_str),
            Some("explicit")
        );
        assert!(output.pointer("/prompt_cache_options/ttl").is_none());
    }

    #[test]
    fn ws_cache_creation_disabled_and_other_models_are_exact_noops() {
        let input =
            r#"{"type":"response.create","model":"gpt-5.5","prompt_cache_retention":"24h"}"#;
        let mut disabled_session_model = Some("gpt-5.6-sol".to_string());
        let disabled = apply_openai_prompt_cache_creation_optimization_ws_message(
            TungsteniteMessage::Text(input.to_string()),
            None,
            &mut disabled_session_model,
        );
        assert!(matches!(disabled, TungsteniteMessage::Text(text) if text == input));

        let mut other_session_model = Some("gpt-5.6-sol".to_string());
        let other_model = apply_openai_prompt_cache_creation_optimization_ws_message(
            TungsteniteMessage::Text(input.to_string()),
            Some("suppress"),
            &mut other_session_model,
        );
        assert!(matches!(other_model, TungsteniteMessage::Text(text) if text == input));

        let image = r#"{"type":"response.create","model":"gpt-5.6-sol","tool_choice":{"type":"namespace","name":"image_gen"},"input":[{"type":"additional_tools","tools":[{"type":"image_generation"}]}]}"#;
        let mut image_session_model = Some("gpt-5.6-sol".to_string());
        let image_output = apply_openai_prompt_cache_creation_optimization_ws_message(
            TungsteniteMessage::Text(image.to_string()),
            Some("reduce"),
            &mut image_session_model,
        );
        assert!(matches!(image_output, TungsteniteMessage::Text(text) if text == image));
    }

    #[test]
    fn ws_cache_creation_passive_image_namespace_is_not_image_intent() {
        let input = r#"{"type":"response.create","model":"gpt-5.6-sol","tools":[{"type":"namespace","name":"image_gen"}],"input":[{"type":"additional_tools","tools":[{"type":"image_generation"}]},{"role":"user","content":"write code"}],"tool_choice":"auto","prompt_cache_retention":"24h"}"#;
        let mut session_model = Some("gpt-5.6-sol".to_string());
        let (output, applied) = apply_openai_prompt_cache_creation_optimization_ws_message_tracked(
            TungsteniteMessage::Text(input.to_string()),
            Some("suppress"),
            &mut session_model,
        );
        assert_eq!(applied, Some(true));
        let TungsteniteMessage::Text(output) = output else {
            panic!("expected text frame");
        };
        let output: Value = serde_json::from_str(&output).unwrap();
        assert!(output.get("prompt_cache_retention").is_none());
        assert!(output.pointer("/prompt_cache_options/ttl").is_none());
    }

    #[test]
    fn ws_cache_creation_explicit_image_function_call_remains_noop() {
        let input = r#"{"type":"response.create","model":"gpt-5.6-sol","input":[{"type":"function_call","name":"image_gen.imagegen"}],"prompt_cache_retention":"24h"}"#;
        let mut session_model = Some("gpt-5.6-sol".to_string());
        let (output, applied) = apply_openai_prompt_cache_creation_optimization_ws_message_tracked(
            TungsteniteMessage::Text(input.to_string()),
            Some("suppress"),
            &mut session_model,
        );
        assert_eq!(applied, Some(false));
        assert!(matches!(output, TungsteniteMessage::Text(text) if text == input));
    }

    #[test]
    fn ws_cache_creation_explicit_top_level_image_tool_choice_remains_noop() {
        for input in [
            r#"{"type":"response.create","model":"gpt-5.6-sol","tool_choice":{"type":"function","name":"image_gen.imagegen"},"prompt_cache_retention":"24h"}"#,
            r#"{"type":"response.create","model":"gpt-5.6-sol","tool_choice":{"namespace":"image_gen","name":"imagegen"},"prompt_cache_retention":"24h"}"#,
        ] {
            let mut session_model = Some("gpt-5.6-sol".to_string());
            let (output, applied) =
                apply_openai_prompt_cache_creation_optimization_ws_message_tracked(
                    TungsteniteMessage::Text(input.to_string()),
                    Some("suppress"),
                    &mut session_model,
                );
            assert_eq!(applied, Some(false));
            assert!(matches!(output, TungsteniteMessage::Text(text) if text == input));
        }
    }

    #[test]
    fn lease_abort_request_tracks_relay_attempt_state() {
        let before = lease_abort_request(
            "edge-1",
            Some("lease-1"),
            Some(42),
            "ws_session_dropped",
            true,
            false,
        );
        assert!(!before.relay_attempted);
        assert!(!before.fallback_to_go);

        let after = lease_abort_request(
            "edge-1",
            Some("lease-1"),
            Some(42),
            "ws_session_dropped",
            true,
            true,
        );
        assert!(after.relay_attempted);
        assert!(!after.fallback_to_go);
    }

    #[test]
    fn ws_cache_creation_applies_to_binary_json_without_changing_frame_type() {
        let input = br#"{"type":"response.create","model":"gpt-5.6-sol","prompt_cache_retention":"24h","input":[{"role":"user","content":"hello"}]}"#;
        let mut session_model = Some("gpt-5.6-sol".to_string());
        let (output, applied) = apply_openai_prompt_cache_creation_optimization_ws_message_tracked(
            TungsteniteMessage::Binary(input.to_vec()),
            Some("suppress"),
            &mut session_model,
        );

        assert_eq!(applied, Some(true));
        let TungsteniteMessage::Binary(output) = output else {
            panic!("expected binary frame");
        };
        let output: Value =
            serde_json::from_slice(&output).expect("optimized binary response.create");
        assert_eq!(
            output
                .pointer("/prompt_cache_options/mode")
                .and_then(Value::as_str),
            Some("explicit")
        );
        assert!(output.pointer("/prompt_cache_options/ttl").is_none());
        assert!(output.get("prompt_cache_retention").is_none());

        let mut disabled_model = Some("gpt-5.6-sol".to_string());
        let disabled = apply_openai_prompt_cache_creation_optimization_ws_message(
            TungsteniteMessage::Binary(input.to_vec()),
            None,
            &mut disabled_model,
        );
        assert!(matches!(disabled, TungsteniteMessage::Binary(bytes) if bytes == input));
    }

    #[test]
    fn ws_cache_creation_tracks_session_model_changes() {
        let mut session_model = Some("gpt-5.5".to_string());
        let update = r#"{"type":"session.update","session":{"model":"gpt-5.6-sol"}}"#;
        let update_output = apply_openai_prompt_cache_creation_optimization_ws_message(
            TungsteniteMessage::Text(update.to_string()),
            Some("suppress"),
            &mut session_model,
        );
        assert!(matches!(update_output, TungsteniteMessage::Text(text) if text == update));
        assert_eq!(session_model.as_deref(), Some("gpt-5.6-sol"));

        let create = r#"{"type":"response.create","prompt_cache_retention":"24h","input":[{"role":"user","content":"hello"}]}"#;
        let create_output = apply_openai_prompt_cache_creation_optimization_ws_message(
            TungsteniteMessage::Text(create.to_string()),
            Some("suppress"),
            &mut session_model,
        );
        let TungsteniteMessage::Text(create_output) = create_output else {
            panic!("expected text frame");
        };
        let create_output: Value =
            serde_json::from_str(&create_output).expect("optimized response.create");
        assert_eq!(
            create_output
                .pointer("/prompt_cache_options/mode")
                .and_then(Value::as_str),
            Some("explicit")
        );
        assert!(create_output.pointer("/prompt_cache_options/ttl").is_none());
        assert!(create_output.get("prompt_cache_retention").is_none());

        let update = r#"{"type":"session.update","session":{"model":"gpt-5.5"}}"#;
        let _ = apply_openai_prompt_cache_creation_optimization_ws_message(
            TungsteniteMessage::Text(update.to_string()),
            Some("suppress"),
            &mut session_model,
        );
        let non_target = r#"{"type":"response.create","prompt_cache_retention":"24h"}"#;
        let non_target_output = apply_openai_prompt_cache_creation_optimization_ws_message(
            TungsteniteMessage::Text(non_target.to_string()),
            Some("suppress"),
            &mut session_model,
        );
        assert!(matches!(non_target_output, TungsteniteMessage::Text(text) if text == non_target));
    }

    #[test]
    fn ws_cache_creation_model_aliases_match_go_normalization() {
        for model in [
            "gpt5.6",
            "gpt-5.6_sol",
            "gpt5.6terra",
            "provider/gpt-5.6terra-high",
            "openai/gpt 5.6 luna",
            "gpt--5.6--luna",
        ] {
            assert!(is_openai_gpt56_model(model), "model={model}");
        }
        assert!(!is_openai_gpt56_model("gpt-5.60"));
    }

    #[test]
    fn cache_creation_policy_unsupported_classifier_is_narrow() {
        assert!(openai_cache_creation_policy_unsupported(
            StatusCode::BAD_REQUEST,
            r#"{"error":{"message":"Unsupported parameter: prompt_cache_options"}}"#,
        ));
        assert!(openai_cache_creation_policy_unsupported(
            StatusCode::BAD_REQUEST,
            r#"{"error":{"message":"Unsupported parameter","param":"prompt_cache_options"}}"#,
        ));
        assert!(openai_cache_creation_policy_unsupported(
            StatusCode::UNPROCESSABLE_ENTITY,
            r#"{"detail":[{"type":"extra_forbidden","loc":["body","prompt_cache_options"],"msg":"Extra inputs are not permitted","input":{"mode":"explicit"}}]}"#,
        ));
        assert!(!openai_cache_creation_policy_unsupported(
            StatusCode::BAD_GATEWAY,
            r#"{"error":{"message":"Unsupported parameter: prompt_cache_options"}}"#,
        ));
        assert!(!openai_cache_creation_policy_unsupported(
            StatusCode::BAD_REQUEST,
            r#"{"error":{"message":"model is not allowed"}}"#,
        ));
        assert!(!openai_cache_creation_policy_unsupported(
            StatusCode::BAD_REQUEST,
            r#"{"error":{"message":"Unsupported model name","param":"model"},"request":{"prompt_cache_options":{"mode":"explicit"}}}"#,
        ));
        assert!(!openai_cache_creation_policy_unsupported(
            StatusCode::BAD_REQUEST,
            r#"{"error":{"message":"Invalid schema: property prompt_cache_breakpoint is malformed","param":"tools.0.parameters"}}"#,
        ));
        assert!(openai_ws_cache_creation_policy_unsupported(
            &TungsteniteMessage::Text(
                r#"{"type":"error","error":{"message":"Unknown field prompt_cache_breakpoint"}}"#
                    .to_string(),
            ),
        ));
        assert!(!openai_ws_cache_creation_policy_unsupported(
            &TungsteniteMessage::Text(
                r#"{"type":"response.output_text.delta","delta":"Unsupported parameter: prompt_cache_options"}"#
                    .to_string(),
            ),
        ));
    }

    #[test]
    fn local_capacity_errors_never_fallback_to_go() {
        for message in [
            "edge relay queue full or closed",
            "edge relay queue byte budget exhausted",
            "edge relay domain capacity exhausted",
            "edge proxy client capacity exhausted",
            "queue wait budget exceeded",
        ] {
            assert!(relay_error_is_local_capacity(&anyhow::anyhow!(message)));
        }
        assert!(!relay_error_is_local_capacity(&anyhow::anyhow!(
            "upstream connection reset"
        )));
        assert!(!relay_error_is_local_capacity(&anyhow::anyhow!(
            "edge transient proxy client build failed: invalid proxy"
        )));
    }

    #[test]
    fn transient_proxy_build_failures_keep_distinct_reason() {
        assert_eq!(
            relay_error_fallback_reason(&anyhow::anyhow!(
                "edge transient proxy client build failed: invalid proxy"
            )),
            "edge_transient_proxy_client_build_failed"
        );
        assert!(!relay_error_is_local_capacity(&anyhow::anyhow!(
            "edge transient proxy client build failed: invalid proxy"
        )));
        for message in [
            "invalid upstream proxy configuration",
            "could not build upstream HTTP client",
        ] {
            assert_eq!(
                relay_error_fallback_reason(&anyhow::anyhow!(message)),
                "edge_upstream_client_build_failed"
            );
            assert!(!relay_error_is_local_capacity(&anyhow::anyhow!(message)));
        }
    }

    #[test]
    fn ws_failure_state_keeps_policy_failure_without_hiding_later_real_errors() {
        let policy_error = TungsteniteMessage::Text(
            r#"{"type":"error","error":{"message":"Unsupported parameter","param":"prompt_cache_options"}}"#
                .to_string(),
        );
        let normal_delta = TungsteniteMessage::Text(
            r#"{"type":"response.output_text.delta","delta":"ok"}"#.to_string(),
        );
        let real_error = TungsteniteMessage::Text(
            r#"{"type":"error","error":{"message":"Invalid model","param":"model"}}"#.to_string(),
        );

        let mut state = OpenAIWSFailureState::default();
        assert!(state.observe_upstream_message(&policy_error, true));
        assert!(state.is_cache_policy_only());
        assert!(!state.observe_upstream_message(&normal_delta, false));
        assert!(
            state.is_cache_policy_only(),
            "a later turn must not erase the earlier policy compatibility failure"
        );
        assert!(state.observe_upstream_message(&real_error, false));
        assert!(
            !state.is_cache_policy_only(),
            "a genuine later upstream failure must not become health-neutral"
        );
    }

    #[test]
    fn ws_binary_and_malformed_text_are_failure_samples() {
        let mut state = OpenAIWSFailureState::default();
        assert!(state.observe_upstream_message(&TungsteniteMessage::Binary(vec![0xff]), false));
        assert!(!state.is_cache_policy_only());

        let mut malformed = OpenAIWSFailureState::default();
        assert!(malformed
            .observe_upstream_message(&TungsteniteMessage::Text("not-json".to_string()), false,));
        assert!(!malformed.is_cache_policy_only());
    }

    #[test]
    fn ws_explicit_rejected_field_detection_is_narrow() {
        let max_tokens = TungsteniteMessage::Text(
            r#"{"type":"error","error":{"code":"unsupported_parameter","param":"max_output_tokens","message":"Unsupported parameter: max_output_tokens"}}"#.to_string(),
        );
        assert!(openai_ws_explicit_rejected_field_error(&max_tokens).is_some());

        let namespace = TungsteniteMessage::Text(
            r#"{"type":"response.failed","response":{"error":{"code":"unknown_parameter","param":"input[12].namespace","message":"Unknown parameter: input[12].namespace"}}}"#.to_string(),
        );
        assert!(openai_ws_explicit_rejected_field_error(&namespace).is_some());

        let ambiguous = TungsteniteMessage::Text(
            r#"{"type":"error","error":{"code":"invalid_request_error","param":"max_output_tokens","message":"max_output_tokens must be positive"}}"#.to_string(),
        );
        assert!(openai_ws_explicit_rejected_field_error(&ambiguous).is_none());

        let other_field = TungsteniteMessage::Text(
            r#"{"type":"error","error":{"code":"unknown_parameter","param":"metadata.secret","message":"Unknown parameter: metadata.secret"}}"#.to_string(),
        );
        assert!(openai_ws_explicit_rejected_field_error(&other_field).is_none());
    }

    #[test]
    fn ws_cache_creation_tracks_application_per_response_create() {
        let mut session_model = Some("gpt-5.6-sol".to_string());
        let (_, applied) = apply_openai_prompt_cache_creation_optimization_ws_message_tracked(
            TungsteniteMessage::Text(
                r#"{"type":"response.create","input":[{"role":"user","content":"hello"}]}"#
                    .to_string(),
            ),
            Some("suppress"),
            &mut session_model,
        );
        assert_eq!(applied, Some(true));

        let (_, applied) = apply_openai_prompt_cache_creation_optimization_ws_message_tracked(
            TungsteniteMessage::Text(
                r#"{"type":"response.create","model":"gpt-5.5","input":[]}"#.to_string(),
            ),
            Some("suppress"),
            &mut session_model,
        );
        assert_eq!(applied, Some(false));

        let (_, applied) = apply_openai_prompt_cache_creation_optimization_ws_message_tracked(
            TungsteniteMessage::Text(
                r#"{"type":"session.update","session":{"model":"gpt-5.6-terra"}}"#.to_string(),
            ),
            Some("suppress"),
            &mut session_model,
        );
        assert_eq!(applied, None);
    }

    #[test]
    fn ws_first_message_always_prefers_control_plane_raw_body() {
        let raw = br#"{"source":"raw"}"#;
        let base_plan = serde_json::json!({
            "action": "relay",
            "edge_request_id": "edge-cache-test",
            "body": {"source": "body"},
            "body_raw_base64": b64_encode(raw)
        });
        let disabled: EdgePlan = serde_json::from_value(base_plan.clone()).expect("disabled plan");
        let disabled_message =
            edge_plan_ws_first_message(&disabled, AxumWsMessage::Text("client".to_string()))
                .expect("disabled first message");
        assert!(
            matches!(disabled_message, TungsteniteMessage::Text(text) if text == r#"{"source":"raw"}"#)
        );

        let mut enabled_value = base_plan;
        enabled_value["prompt_cache_creation_optimization_mode"] =
            Value::String("reduce".to_string());
        let enabled: EdgePlan = serde_json::from_value(enabled_value).expect("enabled plan");
        let enabled_message =
            edge_plan_ws_first_message(&enabled, AxumWsMessage::Text("client".to_string()))
                .expect("enabled first message");
        assert!(
            matches!(enabled_message, TungsteniteMessage::Text(text) if text == r#"{"source":"raw"}"#)
        );
    }

    #[test]
    fn stream_summary_marks_failed_events() {
        let mut summary = ChatStreamSummary::default();
        summary.observe(b"data: {\"type\":\"response.failed\"}\n\n");
        assert!(summary.failed);

        let mut cyber = ChatStreamSummary::default();
        cyber.observe(
			b"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"cyber_policy\"}}}\n\n",
		);
        assert!(cyber.failed);
        assert!(cyber.cyber_blocked);
    }

    #[test]
    fn cyber_policy_detection_accepts_code_and_message_without_exposing_body() {
        assert!(json_text_is_cyber_policy(
            r#"{"error":{"code":"cyber_policy","message":"private.example"}}"#
        ));
        assert!(json_text_is_cyber_policy(
            r#"{"response":{"error":{"message":"Flagged for high-risk cyber activity"}}}"#
        ));
        assert!(!json_text_is_cyber_policy(
            r#"{"error":{"message":"ordinary request failure"}}"#
        ));
    }

    #[test]
    fn stream_summary_requires_dialect_specific_success_terminal() {
        let mut done_only = ChatStreamSummary::default();
        done_only.observe(b"data: [DONE]\n\n");
        assert!(done_only.completed_successfully(Some("chat_completions")));
        assert!(!done_only.completed_successfully(Some("responses")));
        assert_eq!(
            done_only.terminal_event_type(Some("chat_completions")),
            Some("[DONE]".to_string())
        );
        assert_eq!(
            done_only.terminal_event_type(Some("responses")),
            Some("[DONE]".to_string())
        );

        let mut responses = ChatStreamSummary::default();
        responses.observe(b"data: {\"type\":\"response.completed\"}\n\n");
        assert!(responses.completed_successfully(Some("responses")));
        assert_eq!(
            responses.terminal_event_type(Some("responses")),
            Some("response.completed".to_string())
        );

        responses.observe(b"data: {\"type\":\"response.incomplete\"}\n\n");
        assert!(!responses.completed_successfully(Some("responses")));
        assert_eq!(
            responses.terminal_event_type(Some("responses")),
            Some("response.incomplete".to_string())
        );

        responses.observe(b"data: {\"type\":\"response.completed\"}\n\n");
        assert!(!responses.completed_successfully(Some("responses")));
        assert_eq!(
            responses.terminal_event_type(Some("responses")),
            Some("response.incomplete".to_string())
        );

        let mut failed_then_completed = ChatStreamSummary::default();
        failed_then_completed.observe(b"data: {\"type\":\"response.failed\"}\n\n");
        failed_then_completed.observe(b"data: {\"type\":\"response.completed\"}\n\n");
        assert!(!failed_then_completed.completed_successfully(Some("responses")));
        assert_eq!(
            failed_then_completed.terminal_event_type(Some("responses")),
            Some("response.failed".to_string())
        );
    }

    #[test]
    fn combined_created_and_completed_chunk_is_terminal() {
        let mut summary = ChatStreamSummary::default();
        let observation = summary.observe(
            b"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n",
        );

        assert!(observation.response_created_boundary_offset.is_some());
        assert!(summary.completed_successfully(Some("responses")));
    }

    #[test]
    fn first_token_detection_matches_go_stream_event_semantics() {
        assert!(!json_starts_client_output(&serde_json::json!({
            "type": "response.created"
        })));
        assert!(!json_starts_client_output(&serde_json::json!({
            "type": "response.in_progress"
        })));
        assert!(!json_starts_client_output(&serde_json::json!({
            "type": "response.failed"
        })));
        assert!(json_starts_client_output(&serde_json::json!({
            "type": "response.output_text.delta",
            "delta": ""
        })));
        assert!(json_starts_client_output(&serde_json::json!({
            "type": "response.output_item.added"
        })));
    }

    #[test]
    fn real_first_token_detection_requires_real_output() {
        assert!(!json_starts_real_output(&serde_json::json!({
            "type": "response.completed"
        })));
        assert!(!json_starts_real_output(&serde_json::json!({
            "type": "response.output_text.delta",
            "delta": ""
        })));
        assert!(json_starts_real_output(&serde_json::json!({
            "type": "response.output_text.delta",
            "delta": "hello"
        })));
        assert!(json_starts_real_output(&serde_json::json!({
            "type": "response.output_item.added",
            "item": { "type": "function_call" }
        })));
    }

    #[test]
    fn hop_by_hop_and_edge_secret_headers_are_not_forwarded() {
        for name in [
            "host",
            "connection",
            "keep-alive",
            "proxy-authenticate",
            "proxy-authorization",
            "te",
            "trailer",
            "transfer-encoding",
            "upgrade",
            "x-sub2api-edge-secret",
        ] {
            assert!(!should_forward_header(name), "{name} request header");
        }
        assert!(should_forward_header("authorization"));
        assert!(should_forward_header("accept"));

        for name in [
            "connection",
            "keep-alive",
            "proxy-authenticate",
            "proxy-authorization",
            "te",
            "trailer",
            "transfer-encoding",
            "upgrade",
        ] {
            assert!(
                !should_forward_response_header(name),
                "{name} response header"
            );
        }
        assert!(should_forward_response_header("content-type"));
        assert!(should_forward_response_header("x-request-id"));
    }

    #[test]
    fn direct_stream_response_headers_do_not_expose_upstream_identity() {
        let mut src = reqwest::header::HeaderMap::new();
        src.insert(
            header::CONTENT_TYPE,
            HeaderValue::from_static("text/event-stream; provider=private-upstream.example"),
        );
        src.insert(
            HeaderName::from_static("x-request-id"),
            HeaderValue::from_static("openai-private-request"),
        );
        src.insert(
            HeaderName::from_static("x-ratelimit-remaining-requests"),
            HeaderValue::from_static("42"),
        );
        src.insert(
            HeaderName::from_static("x-ratelimit-reset-requests"),
            HeaderValue::from_static("private-upstream.example"),
        );
        let mut dst = HeaderMap::new();

        write_direct_stream_response_headers(&mut dst, &src);

        assert_eq!(
            dst.get(header::CONTENT_TYPE)
                .and_then(|value| value.to_str().ok()),
            Some("text/event-stream")
        );
        assert!(dst.get("x-request-id").is_none());
        assert_eq!(
            dst.get("x-ratelimit-remaining-requests")
                .and_then(|value| value.to_str().ok()),
            Some("42")
        );
        assert!(dst.get("x-ratelimit-reset-requests").is_none());
    }

    #[test]
    fn take_request_body_prefers_raw_base64_and_clears_plan_body() {
        let mut plan = EdgePlan {
            action: "relay".to_string(),
            reason: None,
            edge_request_id: "edge-1".to_string(),
            lease_id: Some("lease-1".to_string()),
            lease_ttl_ms: Some(120_000),
            account_id: Some(42),
            account_type: None,
            transport: Some("http2_sse".to_string()),
            response_dialect: Some("responses".to_string()),
            upstream_url: Some("https://api.openai.com/v1/responses".to_string()),
            headers: None,
            body: Some(serde_json::json!({"rewritten": false})),
            body_raw_base64: Some(b64_encode(br#"{"rewritten":true}"#)),
            proxy_url: None,
            low_latency_mode: None,
            lane: None,
            sse_comment_preflush: false,
            preamble_flush: false,
            safe_token_placeholder: false,
            first_token_timeout_placeholder_ms: None,
            prompt_cache_creation_optimization_mode: None,
            prompt_cache_creation_optimization_model: None,
            prompt_cache_creation_optimization_applied: false,
            max_reasoning_effort: None,
            reasoning_effort_mappings: Vec::new(),
        };

        let first = edge_plan_ws_first_message(
            &plan,
            AxumWsMessage::Text(r#"{"rewritten":"original"}"#.to_string()),
        )
        .expect("ws first message");
        assert!(matches!(first, TungsteniteMessage::Text(text) if text == r#"{"rewritten":true}"#));

        let body = take_request_body_bytes(&mut plan).expect("request body");
        assert_eq!(body, br#"{"rewritten":true}"#);
        assert!(plan.body.is_none());
        assert!(plan.body_raw_base64.is_none());
    }

    #[test]
    fn buffer_pool_recycle_is_capped_at_initial_size() {
        let pools = BufferPools::prewarmed(1);
        let first = pools.take_sse_string();
        let second = pools.take_sse_string();

        pools.recycle_sse_string(first);
        pools.recycle_sse_string(second);

        let retained = pools.sse_strings.lock().expect("pool lock").len();
        assert_eq!(retained, 1);
    }

    #[test]
    fn buffer_pool_drops_oversized_idle_strings() {
        let pools = BufferPools::prewarmed(1);
        let _existing = pools.take_sse_string();
        let mut oversized = String::with_capacity(SSE_STRING_IDLE_MAX_CAPACITY + 1);
        oversized.push_str("data: {}\n\n");

        pools.recycle_sse_string(oversized);

        let retained = pools.sse_strings.lock().expect("pool lock").len();
        assert_eq!(retained, 0);
    }

    #[test]
    fn decode_edge_plan_accepts_plan_envelope() {
        let body = br#"{"plan":{"action":"fallback_go","reason":"edge_disabled","edge_request_id":"edge-1","account_type":"apikey"}}"#;
        let plan = decode_edge_plan(body).expect("plan envelope");
        assert_eq!(plan.action, "fallback_go");
        assert_eq!(plan.reason.as_deref(), Some("edge_disabled"));
        assert_eq!(plan.edge_request_id, "edge-1");
        assert_eq!(plan.account_type.as_deref(), Some("apikey"));
    }

    #[test]
    fn edge_plan_sse_comment_preflush_is_optional_and_decodes() {
        let legacy = br#"{"action":"relay","edge_request_id":"edge-legacy"}"#;
        let legacy_plan = decode_edge_plan(legacy).expect("legacy edge plan");
        assert!(!legacy_plan.sse_comment_preflush);
        assert!(legacy_plan.preamble_flush);

        let enabled =
            br#"{"action":"relay","edge_request_id":"edge-preflush","sse_comment_preflush":true}"#;
        let enabled_plan = decode_edge_plan(enabled).expect("preflush edge plan");
        assert!(enabled_plan.sse_comment_preflush);

        let buffered =
            br#"{"action":"relay","edge_request_id":"edge-buffered","preamble_flush":false}"#;
        let buffered_plan = decode_edge_plan(buffered).expect("buffered edge plan");
        assert!(!buffered_plan.preamble_flush);
    }

    #[test]
    fn response_body_preview_redacts_secret_headers() {
        let body = br#"{"headers":{"authorization":"Bearer secret","api-key":"sk-secret","x-api-key":"x-secret","cookie":"session=secret","upstream_url":"https://upstream.example","proxy_url":"http://proxy.example"}}"#;
        let preview = response_body_preview(body);
        assert_eq!(preview, "[redacted upstream error]");
        assert!(!preview.contains("secret"));
        assert!(!preview.contains("upstream.example"));
        assert!(!preview.contains("proxy.example"));
    }

    #[test]
    fn response_body_preview_redacts_upstream_urls_and_html() {
        let preview = response_body_preview(
            br#"<html><body>proxy https://private.example/internal OpenAI error</body></html>"#,
        );
        assert_eq!(preview, "[redacted upstream error]");
    }

    #[test]
    fn relay_queue_key_uses_account_proxy_and_host() {
        let plan = EdgePlan {
            action: "relay".to_string(),
            reason: None,
            edge_request_id: "edge-1".to_string(),
            lease_id: Some("lease-1".to_string()),
            lease_ttl_ms: Some(120_000),
            account_id: Some(42),
            account_type: None,
            transport: Some("http2_sse".to_string()),
            response_dialect: Some("chat_completions".to_string()),
            upstream_url: Some("https://api.openai.com/v1/chat/completions".to_string()),
            headers: None,
            body: None,
            body_raw_base64: None,
            proxy_url: Some("http://127.0.0.1:7890".to_string()),
            low_latency_mode: None,
            lane: Some("priority".to_string()),
            sse_comment_preflush: false,
            preamble_flush: false,
            safe_token_placeholder: false,
            first_token_timeout_placeholder_ms: None,
            prompt_cache_creation_optimization_mode: None,
            prompt_cache_creation_optimization_model: None,
            prompt_cache_creation_optimization_applied: false,
            max_reasoning_effort: None,
            reasoning_effort_mappings: Vec::new(),
        };

        assert_eq!(
            relay_queue_key(&plan).as_deref(),
            Some("42|http://127.0.0.1:7890|api.openai.com|priority")
        );
    }

    #[test]
    fn dynamic_warm_keys_keep_legacy_proxy_origin_sharing() {
        let warm_url = "https://api.openai.com/";
        let proxy = Some(" http://127.0.0.1:7890 ");
        let key = dynamic_warm_key(proxy, warm_url);
        assert_eq!(key, dynamic_warm_key(proxy, warm_url));
        assert_eq!(key.proxy, "http://127.0.0.1:7890");
        assert_eq!(key.warm_url, warm_url);
    }

    #[test]
    fn reasoning_policy_maps_then_caps_without_adding_omitted_fields() {
        let mappings = vec![ReasoningEffortMapping {
            from: "max".to_string(),
            to: "xhigh".to_string(),
        }];
        let mut value = serde_json::json!({"reasoning": {"effort": "max"}});
        assert!(apply_openai_reasoning_effort_policy_value(
            &mut value,
            Some("medium"),
            &mappings,
        ));
        assert_eq!(
            value.pointer("/reasoning/effort").and_then(Value::as_str),
            Some("medium")
        );

        let mut omitted = serde_json::json!({"model": "gpt-5.6"});
        assert!(!apply_openai_reasoning_effort_policy_value(
            &mut omitted,
            Some("low"),
            &[],
        ));
        assert!(omitted.get("reasoning_effort").is_none());
    }

    #[test]
    fn reasoning_only_ws_policy_clears_previous_cache_flag() {
        let payload = TungsteniteMessage::Text(
            r#"{"type":"response.create","model":"gpt-5.6","reasoning_effort":"high"}"#.to_string(),
        );
        let (updated, cache_applied) =
            apply_openai_ws_request_policies_tracked(payload, None, &mut None, Some("medium"), &[]);
        assert_eq!(cache_applied, Some(false));
        let TungsteniteMessage::Text(text) = updated else {
            panic!("expected text frame");
        };
        let value: Value = serde_json::from_str(&text).unwrap();
        assert_eq!(
            value.get("reasoning_effort").and_then(Value::as_str),
            Some("medium")
        );
    }

    #[test]
    fn ws_reasoning_policy_preserves_text_and_binary_frame_types() {
        let mappings = vec![ReasoningEffortMapping {
            from: "max".to_string(),
            to: "high".to_string(),
        }];
        let payload = br#"{"type":"response.create","model":"gpt-5.6","reasoning_effort":"max"}"#;

        for original in [
            TungsteniteMessage::Text(String::from_utf8(payload.to_vec()).unwrap()),
            TungsteniteMessage::Binary(payload.to_vec()),
        ] {
            let is_text = matches!(original, TungsteniteMessage::Text(_));
            let (updated, cache_applied) = apply_openai_ws_request_policies_tracked(
                original,
                Some("reduce"),
                &mut None,
                Some("medium"),
                &mappings,
            );
            assert_eq!(cache_applied, Some(true));
            let bytes = match updated {
                TungsteniteMessage::Text(text) => {
                    assert!(is_text);
                    text.into_bytes()
                }
                TungsteniteMessage::Binary(bytes) => {
                    assert!(!is_text);
                    bytes
                }
                _ => panic!("unexpected frame type"),
            };
            let value: Value = serde_json::from_slice(&bytes).unwrap();
            assert_eq!(
                value.get("reasoning_effort").and_then(Value::as_str),
                Some("medium")
            );
            assert_eq!(
                value
                    .pointer("/prompt_cache_options/ttl")
                    .and_then(Value::as_str),
                Some("30m")
            );
        }
    }

    #[test]
    fn relay_queue_byte_budget_is_bounded_and_released() {
        let semaphore = Arc::new(Semaphore::new(8));
        let permit =
            try_reserve_relay_queue_bytes(semaphore.clone(), 8, 6).expect("reserve queue bytes");
        assert!(try_reserve_relay_queue_bytes(semaphore.clone(), 8, 3).is_err());
        assert!(try_reserve_relay_queue_bytes(semaphore.clone(), 8, 9).is_err());
        drop(permit);
        assert!(try_reserve_relay_queue_bytes(semaphore, 8, 8).is_ok());
    }
}
