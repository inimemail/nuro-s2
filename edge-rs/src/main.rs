use std::{
    collections::HashMap,
    env,
    future::pending,
    net::SocketAddr,
    pin::Pin,
    sync::{
        atomic::{AtomicBool, Ordering},
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
use reqwest::{Client, ClientBuilder};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio::net::TcpStream;
use tokio::sync::{mpsc, oneshot};
use tokio_tungstenite::{
    connect_async_tls_with_config,
    tungstenite::{
        client::IntoClientRequest,
        protocol::{Message as TungsteniteMessage, WebSocketConfig},
    },
    MaybeTlsStream, WebSocketStream,
};
use tracing::{error, info, warn};
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

#[derive(Clone)]
struct AppState {
    cfg: Arc<EdgeConfig>,
    client: Client,
    clients_by_proxy: Arc<Mutex<HashMap<String, Client>>>,
    relay_queues: Arc<Mutex<HashMap<String, mpsc::Sender<RelayJob>>>>,
    warm_keys: Arc<Mutex<HashMap<String, WarmKeyState>>>,
    ws_idle: Arc<tokio::sync::Mutex<HashMap<String, Vec<WsIdleConn>>>>,
    pools: Arc<BufferPools>,
}

#[derive(Clone, Debug)]
struct EdgeConfig {
    listen_addr: SocketAddr,
    go_base_url: String,
    control_base_url: String,
    internal_secret: String,
    prepare_timeout_ms: u64,
    complete_timeout_ms: u64,
    initial_pool_size: usize,
    queue_buffer_size: usize,
    per_account_workers: usize,
    max_idle_conns_per_account: usize,
    queue_wait_budget_ms: u64,
    large_payload_passthrough: bool,
    large_payload_threshold_bytes: usize,
    ws_idle_per_key: usize,
    upstream_warm_url: Option<String>,
    upstream_warm_interval_secs: u64,
    upstream_dynamic_warm_active_secs: u64,
}

#[derive(Debug, Serialize)]
struct PrepareRequest {
    edge_request_id: String,
    method: String,
    path: String,
    raw_query: Option<String>,
    headers: HashMap<String, String>,
    body: Value,
    body_raw_base64: Option<String>,
    client_ip: Option<String>,
    stream: Option<bool>,
}

#[derive(Clone, Debug, Deserialize)]
struct EdgePlan {
    action: String,
    reason: Option<String>,
    edge_request_id: String,
    lease_id: Option<String>,
    account_id: Option<i64>,
    transport: Option<String>,
    response_dialect: Option<String>,
    upstream_url: Option<String>,
    headers: Option<HashMap<String, String>>,
    body: Option<Value>,
    body_raw_base64: Option<String>,
    proxy_url: Option<String>,
    low_latency_mode: Option<String>,
    lane: Option<String>,
    #[serde(default)]
    safe_token_placeholder: bool,
    first_token_timeout_placeholder_ms: Option<u64>,
    prompt_cache_creation_optimization_mode: Option<String>,
    prompt_cache_creation_optimization_model: Option<String>,
    #[serde(default)]
    prompt_cache_creation_optimization_applied: bool,
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
}

#[derive(Clone, Debug)]
struct WarmKeyState {
    proxy_url: Option<String>,
    warm_url: String,
    last_seen: Instant,
    failures: u32,
}

#[derive(Clone, Debug, Default)]
struct EdgeTiming {
    prepare_ms: Option<i64>,
    queue_wait_ms: Option<i64>,
    relay_start_ms: Option<i64>,
    fallback_reason: Option<String>,
    retry_count: i64,
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
    "relay_error_before_commit"
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
    response_body: Option<Value>,
    wrote_client_response: bool,
}

#[derive(Clone, Debug, Serialize)]
struct CompleteRequest {
    edge_request_id: String,
    lease_id: Option<String>,
    account_id: Option<i64>,
    success: bool,
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

#[derive(Debug, Serialize)]
struct AbortRequest {
    edge_request_id: String,
    lease_id: Option<String>,
    account_id: Option<i64>,
    reason: String,
    client_disconnected: bool,
}

struct LeaseAbortGuard {
    state: AppState,
    edge_request_id: String,
    lease_id: Option<String>,
    account_id: Option<i64>,
    reason: &'static str,
    client_disconnected: bool,
    done: Arc<AtomicBool>,
}

struct ClientDisconnectCompleteGuard {
    state: AppState,
    started_at: Instant,
    request: Mutex<CompleteRequest>,
    definitive_failure: AtomicBool,
    done: AtomicBool,
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
        let state = self.state.clone();
        tokio::spawn(async move {
            let _ = call_complete(&state, request).await;
        });
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
            done: Arc::new(AtomicBool::new(false)),
        }
    }

    fn mark_done(&self) {
        self.done.store(true, Ordering::SeqCst);
    }
}

impl Drop for LeaseAbortGuard {
    fn drop(&mut self) {
        if self.done.load(Ordering::SeqCst) {
            return;
        }
        let state = self.state.clone();
        let req = AbortRequest {
            edge_request_id: self.edge_request_id.clone(),
            lease_id: self.lease_id.clone(),
            account_id: self.account_id,
            reason: self.reason.to_string(),
            client_disconnected: self.client_disconnected,
        };
        tokio::spawn(async move {
            let _ = call_abort(&state, req).await;
        });
    }
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(env::var("RUST_LOG").unwrap_or_else(|_| "warn".to_string()))
        .init();

    let cfg = Arc::new(EdgeConfig::from_env()?);
    let client = edge_http_client_builder(&cfg).build()?;
    let state = AppState {
        cfg: cfg.clone(),
        client,
        clients_by_proxy: Arc::new(Mutex::new(HashMap::new())),
        relay_queues: Arc::new(Mutex::new(HashMap::new())),
        warm_keys: Arc::new(Mutex::new(HashMap::new())),
        ws_idle: Arc::new(tokio::sync::Mutex::new(HashMap::new())),
        pools: Arc::new(BufferPools::prewarmed(cfg.initial_pool_size)),
    };
    let warm_client = state.client.clone();
    let dynamic_warm_state = state.clone();
    let app = Router::new()
        .route("/healthz", any(healthz))
        .route("/*path", any(handle_openai_edge))
        .with_state(state);

    info!("sub2api-edge-rs listening on {}", cfg.listen_addr);

    // P3: 启动后台上游连接保活（仅当显式配置了 warm url）。这里使用直连 client，
    // 代理环境不要默认开启，避免保活出口不同于真实业务出口。
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
        .with_graceful_shutdown(async {
            let _ = tokio::signal::ctrl_c().await;
        })
        .await?;
    Ok(())
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

// run_upstream_keep_warm 周期性地向上游 origin 发一个轻量请求，保持 reqwest
// 的 TCP+TLS+h2 连接常驻连接池，使真实请求能复用热连接、省一次握手 RTT。
// 复用同一个 client（与真实请求共享连接池），不带鉴权；上游返回 401/405 均可接受，
// 目的只在于保活连接。全程不触碰任何业务路径。
async fn run_upstream_keep_warm(client: Client, warm_url: String, interval_secs: u64) {
    let interval = Duration::from_secs(interval_secs);
    loop {
        let send = client
            .get(&warm_url)
            .timeout(Duration::from_secs(10))
            .send();
        match send.await {
            Ok(resp) => {
                let _ = resp.status();
            }
            Err(err) => {
                warn!("upstream keep-warm request failed: {err}");
            }
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
            warm_keys.values().cloned().collect::<Vec<_>>()
        };
        for item in keys {
            if item.failures >= 3 && item.failures % 3 != 0 {
                if let Ok(mut warm_keys) = state.warm_keys.lock() {
                    if let Some(current) = warm_keys
                        .get_mut(&dynamic_warm_key(item.proxy_url.as_deref(), &item.warm_url))
                    {
                        current.failures = current.failures.saturating_add(1);
                    }
                }
                continue;
            }
            let client = match state.client_for_proxy(item.proxy_url.as_deref()) {
                Ok(client) => client,
                Err(err) => {
                    warn!("dynamic upstream warm client failed: {err}");
                    continue;
                }
            };
            let warm_url = item.warm_url.clone();
            let warm_key = dynamic_warm_key(item.proxy_url.as_deref(), &warm_url);
            let state_for_update = state.clone();
            tokio::spawn(async move {
                let result = client
                    .get(&warm_url)
                    .header(
                        header::ACCEPT_ENCODING,
                        HeaderValue::from_static("identity"),
                    )
                    .timeout(Duration::from_secs(10))
                    .send()
                    .await;
                let Ok(mut warm_keys) = state_for_update.warm_keys.lock() else {
                    return;
                };
                if let Some(current) = warm_keys.get_mut(&warm_key) {
                    match result {
                        Ok(resp) => {
                            let _ = resp.status();
                            current.failures = 0;
                        }
                        Err(err) => {
                            current.failures = current.failures.saturating_add(1);
                            warn!("dynamic upstream keep-warm request failed: {err}");
                        }
                    }
                }
            });
        }
    }
}

async fn healthz() -> &'static str {
    "ok"
}

async fn handle_openai_edge(
    State(state): State<AppState>,
    ws: Option<WebSocketUpgrade>,
    req: Request,
) -> Response {
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

    let body_bytes = match to_bytes(body, MAX_BODY_BYTES).await {
        Ok(bytes) => bytes,
        Err(err) => {
            error!("read client body failed: {err}");
            return text_response(StatusCode::BAD_REQUEST, "failed to read request body");
        }
    };
    let body_json = if body_bytes.is_empty() {
        Value::Null
    } else {
        match serde_json::from_slice::<Value>(&body_bytes) {
            Ok(v) => v,
            Err(_) => Value::String(String::from_utf8_lossy(&body_bytes).into_owned()),
        }
    };
    let raw_prepare_body = openai_prepare_raw_body(&state, &body_bytes);
    let prepare_body = if raw_prepare_body.is_some() {
        Value::Null
    } else {
        body_json.clone()
    };

    let prepare = PrepareRequest {
        edge_request_id: edge_request_id.clone(),
        method: method.to_string(),
        path: uri.path().to_string(),
        raw_query: uri.query().map(ToOwned::to_owned),
        headers: header_map_to_strings(&headers),
        stream: body_json.get("stream").and_then(|v| v.as_bool()),
        body: prepare_body,
        body_raw_base64: raw_prepare_body,
        client_ip: client_ip_from_headers(&headers),
    };

    let prepare_started_at = Instant::now();
    let (plan, prepare_ms) = match call_prepare(&state, &prepare).await {
        Ok(plan) => (plan, prepare_started_at.elapsed().as_millis() as i64),
        Err(err) => {
            warn!("prepare failed; falling back to Go: {err}");
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
        if let Some(reason) = &plan.reason {
            info!("edge fallback_to_go reason={reason}");
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
    let relay_account_id = plan.account_id;
    let timing_shared = Arc::new(Mutex::new(timing.clone()));
    match relay_upstream(
        state.clone(),
        plan,
        start,
        timing,
        Some(timing_shared.clone()),
        true,
    )
    .await
    {
        Ok(resp) => resp,
        Err(err) => {
            error!("relay failed before response commit: {err}");
            let reason = relay_error_fallback_reason(&err);
            let _ = call_abort(
                &state,
                AbortRequest {
                    edge_request_id,
                    lease_id: relay_lease_id,
                    account_id: relay_account_id,
                    reason: format!("{reason}: {err}"),
                    client_disconnected: false,
                },
            )
            .await;
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
    if body.len() > MAX_PREVIEW {
        preview.push_str("...");
    }
    preview
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
        if let Err(err) = relay_ws_session(state, socket, method, uri, headers).await {
            warn!("edge ws session ended with error: {err}");
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
        method: method.to_string(),
        path: uri.path().to_string(),
        raw_query: uri.query().map(ToOwned::to_owned),
        headers: header_map_to_strings(&headers),
        stream: Some(true),
        body: first_json,
        body_raw_base64: Some(b64_encode(first_text.as_bytes())),
        client_ip: client_ip_from_headers(&headers),
    };

    let plan = match call_prepare(&state, &prepare).await {
        Ok(plan) => plan,
        Err(err) => {
            warn!("ws prepare failed; proxying to Go: {err}");
            return proxy_ws_to_go(state, client_socket, method, uri, headers, first_msg).await;
        }
    };
    if plan.action != "relay" {
        if let Some(reason) = &plan.reason {
            info!("edge ws fallback_to_go reason={reason}");
        }
        return proxy_ws_to_go(state, client_socket, method, uri, headers, first_msg).await;
    }
    let guard = LeaseAbortGuard::new(
        state.clone(),
        plan.edge_request_id.clone(),
        plan.lease_id.clone(),
        plan.account_id,
        "ws_session_dropped",
        true,
    );
    if plan.transport.as_deref() != Some("ws_v2") {
        let _ = call_abort(
            &state,
            AbortRequest {
                edge_request_id,
                lease_id: plan.lease_id.clone(),
                account_id: plan.account_id,
                reason: "unsupported_ws_transport".to_string(),
                client_disconnected: false,
            },
        )
        .await;
        anyhow::bail!("unsupported ws transport");
    }
    if plan
        .proxy_url
        .as_deref()
        .is_some_and(|v| !v.trim().is_empty())
    {
        let _ = call_abort(
            &state,
            AbortRequest {
                edge_request_id,
                lease_id: plan.lease_id.clone(),
                account_id: plan.account_id,
                reason: "ws_proxy_not_supported".to_string(),
                client_disconnected: false,
            },
        )
        .await;
        anyhow::bail!("ws proxy is not supported by edge yet");
    }

    let idle_conn = state.take_ws_idle(&plan).await;
    let (upstream_socket, upstream_request_id) = if let Some(conn) = idle_conn {
        (conn.socket, conn.request_id)
    } else {
        connect_ws_for_plan(&plan).await?
    };
    state.ensure_ws_idle(plan.clone()).await;
    let (mut upstream_write, mut upstream_read) = upstream_socket.split();
    let first_upstream_msg = edge_plan_ws_first_message(&plan, first_msg)?;
    upstream_write.send(first_upstream_msg).await?;

    let lease_id = plan.lease_id.clone();
    let account_id = plan.account_id;
    let edge_request_id_complete = plan.edge_request_id.clone();
    let mut success = true;
    let mut client_disconnected = false;
    let mut error_message: Option<String> = None;
    let mut summary = ChatStreamSummary::default();
    summary.request_id = upstream_request_id;
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
                        let (upstream_msg, turn_policy_applied) = apply_openai_prompt_cache_creation_optimization_ws_message_tracked(
                            upstream_msg,
                            plan.prompt_cache_creation_optimization_mode.as_deref(),
                            &mut prompt_cache_creation_optimization_model,
                        );
                        if let Some(applied) = turn_policy_applied {
                            cache_creation_policy_applied_for_turn = applied;
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
                        if let Err(err) = client_socket.send(client_msg).await {
                            success = false;
                            client_disconnected = true;
                            error_message = Some(err.to_string());
                            break;
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

    call_complete(
        &state,
        CompleteRequest {
            edge_request_id: edge_request_id_complete,
            lease_id,
            account_id,
            success,
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
    if plan.prompt_cache_creation_optimization_mode.is_some() {
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
    }
    if let Some(body) = plan.body.clone() {
        return Ok(TungsteniteMessage::Text(body.to_string()));
    }
    axum_to_tungstenite_message(original)
}

const OPENAI_PROMPT_CACHE_EXPLICIT_MIN_STATIC_BYTES: usize = 4 * 1024;
const OPENAI_PROMPT_CACHE_CREATION_OPTIMIZATION_TTL: &str = "24h";

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
fn apply_openai_prompt_cache_creation_optimization_ws_message_tracked(
    msg: TungsteniteMessage,
    mode: Option<&str>,
    session_model: &mut Option<String>,
) -> (TungsteniteMessage, Option<bool>) {
    let normalized_mode = mode.unwrap_or_default().trim().to_ascii_lowercase();
    if normalized_mode != "reduce" && normalized_mode != "suppress" {
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
    if !is_openai_gpt56_model(model) || is_openai_ws_image_generation_intent(&value) {
        return (original.into_message(), Some(false));
    }

    apply_openai_prompt_cache_creation_optimization_value(&mut value, &normalized_mode);
    original.with_updated_value(&value)
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

    fn with_updated_value(self, value: &Value) -> (TungsteniteMessage, Option<bool>) {
        match self {
            Self::Text(text) => match serde_json::to_string(value) {
                Ok(updated) => (TungsteniteMessage::Text(updated), Some(true)),
                Err(_) => (TungsteniteMessage::Text(text), Some(false)),
            },
            Self::Binary(bytes) => match serde_json::to_vec(value) {
                Ok(updated) => (TungsteniteMessage::Binary(updated), Some(true)),
                Err(_) => (TungsteniteMessage::Binary(bytes), Some(false)),
            },
        }
    }
}

fn apply_openai_prompt_cache_creation_optimization_value(value: &mut Value, mode: &str) {
    let Some(request) = value.as_object_mut() else {
        return;
    };
    remove_openai_prompt_cache_breakpoints(request);
    request.remove("prompt_cache_retention");
    request.insert(
        "prompt_cache_options".to_string(),
        serde_json::json!({"mode": "explicit", "ttl": OPENAI_PROMPT_CACHE_CREATION_OPTIMIZATION_TTL}),
    );
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
            .is_some_and(|input| {
                input.iter().any(|item| {
                    item.get("type").and_then(Value::as_str) == Some("additional_tools")
                        && openai_json_tools_contain_image_generation(item.get("tools"))
                })
            })
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
        || (kind.eq_ignore_ascii_case("namespace")
            && tool
                .get("name")
                .and_then(Value::as_str)
                .is_some_and(|name| name.trim().eq_ignore_ascii_case("image_gen")))
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
    if openai_json_tool_choice_selects_image_generation(choice.get("tool")) {
        return true;
    }
    choice
        .get("function")
        .and_then(Value::as_object)
        .and_then(|function| function.get("name"))
        .and_then(Value::as_str)
        .is_some_and(|name| name.trim().eq_ignore_ascii_case("image_generation"))
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
) -> anyhow::Result<Response> {
    if allow_initial_queue {
        if let Some(sender) = state.relay_queue_for_plan(&plan)? {
            let request_body = take_request_body_bytes(&mut plan)?;
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
            };
            sender
                .try_send(job)
                .map_err(|err| anyhow::anyhow!("edge relay queue full or closed: {err}"))?;
            return response_rx
                .await
                .map_err(|err| anyhow::anyhow!("edge relay worker dropped response: {err}"))?;
        }
    }
    let request_body = take_request_body_bytes(&mut plan)?;
    relay_upstream_direct(state, plan, request_body, started_at, timing, timing_shared).await
}

async fn relay_upstream_direct(
    state: AppState,
    plan: EdgePlan,
    request_body: Vec<u8>,
    started_at: Instant,
    mut timing: EdgeTiming,
    timing_shared: Option<Arc<Mutex<EdgeTiming>>>,
) -> anyhow::Result<Response> {
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

    let upstream_client = state.client_for_proxy(plan.proxy_url.as_deref())?;
    let mut req = upstream_client.post(upstream_url);
    if let Some(headers) = &plan.headers {
        for (k, v) in headers {
            req = req.header(k, v);
        }
    }
    req = req.header(
        header::ACCEPT_ENCODING,
        HeaderValue::from_static("identity"),
    );
    let edge_request_id = plan.edge_request_id.clone();
    let lease_id = plan.lease_id.clone();
    let account_id = plan.account_id;
    let edge_prepare_ms = timing.prepare_ms;
    let edge_queue_wait_ms = timing.queue_wait_ms;
    let edge_relay_start_ms = timing.relay_start_ms;
    let edge_fallback_reason = timing.fallback_reason.clone();
    let edge_retry_count = timing.retry_count;
    let complete_state = state.clone();
    let safe_token_placeholder = plan.safe_token_placeholder;
    let cache_creation_policy_applied = plan.prompt_cache_creation_optimization_applied;
    let first_token_timeout_placeholder =
        normalize_first_token_timeout_placeholder_ms(plan.first_token_timeout_placeholder_ms);
    let response_dialect = plan.response_dialect.clone();
    let header_start = Instant::now();
    let mut upstream_send = Box::pin(req.body(request_body).send());
    let upstream = if let Some(timeout) = first_token_timeout_placeholder {
        tokio::select! {
            result = &mut upstream_send => result?,
            _ = tokio::time::sleep(timeout) => {
                let early_body_stream = stream! {
                    let guard = ClientDisconnectCompleteGuard::new(
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
                    let mut first_byte_ms: Option<i64> = None;
                    let first_flush_ms: Option<i64> = Some(started_at.elapsed().as_millis() as i64);
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
                    let placeholder =
                        openai_stream_timeout_placeholder_frame(response_dialect.as_deref(), &summary);
                    yield Ok::<Bytes, std::io::Error>(Bytes::from(placeholder));

                    let upstream = match upstream_send.await {
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
                            &message,
                        );
                        yield Ok::<Bytes, std::io::Error>(Bytes::from(frame));
                        complete_state.pools.recycle_sse_string(std::mem::take(&mut summary.pending));
                        if call_complete(&complete_state, CompleteRequest {
                            edge_request_id: edge_request_id.clone(),
                            lease_id: lease_id.clone(),
                            account_id,
                            success,
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
                            if !sanitized.is_empty() {
                                yield Ok::<Bytes, std::io::Error>(sanitized);
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
                                yield Err(std::io::Error::new(std::io::ErrorKind::Other, err.to_string()));
                                break;
                            }
                        }
                    }

                let tail = sanitizer.finish();
                if !tail.is_empty() {
                    yield Ok::<Bytes, std::io::Error>(tail);
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
                headers.insert(header::CACHE_CONTROL, HeaderValue::from_static("no-cache"));
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
        if json_text_is_cyber_policy(&error_body) {
            let _ = call_complete(
                &state,
                CompleteRequest {
                    edge_request_id: plan.edge_request_id.clone(),
                    lease_id: plan.lease_id.clone(),
                    account_id: plan.account_id,
                    success: false,
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
            .await;
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
            let _ = call_abort(
                &state,
                AbortRequest {
                    edge_request_id: plan.edge_request_id.clone(),
                    lease_id: plan.lease_id.clone(),
                    account_id: plan.account_id,
                    reason,
                    client_disconnected: false,
                },
            )
            .await;
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
    let mut bytes_stream = upstream.bytes_stream();

    let upstream_request_id = headers
        .get("x-request-id")
        .and_then(|v| v.to_str().ok())
        .map(ToOwned::to_owned);
    drop(plan);
    let body_stream = stream! {
        let guard = ClientDisconnectCompleteGuard::new(
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

        if low_latency_policy.enabled && low_latency_policy.barrier.is_none() {
            first_flush_ms = Some(started_at.elapsed().as_millis() as i64);
            bootstrap_comment_sent = true;
            yield Ok::<Bytes, std::io::Error>(Bytes::from_static(b":\n\n"));
        }

        let mut bootstrap_timer = low_latency_policy
            .barrier
            .map(tokio::time::sleep)
            .map(Box::pin);
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
                            if first_token_ms.is_none() {
                                first_token_ms = Some(started_at.elapsed().as_millis() as i64);
                                first_token_timeout_timer = None;
                            }
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
                                if !sanitized.is_empty() {
                                    yield Ok::<Bytes, std::io::Error>(sanitized);
                                }
                            }
                            let placeholder =
                                openai_responses_safe_token_placeholder_frame(summary.response_id.as_deref());
                            yield Ok::<Bytes, std::io::Error>(Bytes::from(placeholder));
                            if offset < chunk.len() {
                                let sanitized = sanitizer.push(&chunk.slice(offset..));
                                if !sanitized.is_empty() {
                                    yield Ok::<Bytes, std::io::Error>(sanitized);
                                }
                            }
                            continue;
                        }
                    }
                    let sanitized = sanitizer.push(&chunk);
                    if !sanitized.is_empty() {
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
                        yield Ok::<Bytes, std::io::Error>(sanitized);
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
                    yield Err(std::io::Error::new(std::io::ErrorKind::Other, err.to_string()));
                    break;
                }
            }
        }

        let tail = sanitizer.finish();
        if !tail.is_empty() {
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
            yield Ok::<Bytes, std::io::Error>(tail);
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
    started_at: Instant,
    timing: EdgeTiming,
    timing_shared: Option<Arc<Mutex<EdgeTiming>>>,
    queue_wait_ms: i64,
) -> anyhow::Result<Response> {
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
    Box::pin(relay_upstream(
        state,
        next_plan,
        started_at,
        next_timing,
        timing_shared,
        false,
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
            barrier: Some(Duration::from_millis(200)),
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
        loop {
            let Some(pos) = self.pending.iter().position(|byte| *byte == b'\n') else {
                break;
            };
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
        let Ok(value) = serde_json::from_str::<Value>(payload) else {
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
        let newline = if text.ends_with("\r\n") { "\r\n" } else { "\n" };
        return format!("data: {payload}{newline}").into_bytes();
    }
    if trimmed.starts_with("event:") {
        let event = trimmed["event:".len()..].trim();
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
            let value = serde_json::from_str::<Value>(&text).ok();
            let has_error = value.as_ref().is_some_and(|value| {
                let event_type = json_event_type(value).unwrap_or_default();
                event_type == "error"
                    || event_type == "response.failed"
                    || json_is_unsafe_upstream_diagnostic(value)
            });
            if has_error || value.is_none() {
                TungsteniteMessage::Text(
                    r#"{"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}"#.to_string(),
                )
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

async fn call_complete(state: &AppState, req: CompleteRequest) -> anyhow::Result<()> {
    let url = format!(
        "{}/internal/edge/openai/complete",
        state.cfg.control_base_url
    );
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
        anyhow::bail!("complete status {}", resp.status());
    }
    Ok(())
}

async fn call_abort(state: &AppState, req: AbortRequest) -> anyhow::Result<()> {
    let url = format!("{}/internal/edge/openai/abort", state.cfg.control_base_url);
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
        anyhow::bail!("abort status {}", resp.status());
    }
    Ok(())
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
    let stream = upstream.bytes_stream().map(|item| {
        item.map_err(|err| std::io::Error::new(std::io::ErrorKind::Other, err.to_string()))
    });
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

fn dynamic_warm_key(proxy_url: Option<&str>, warm_url: &str) -> String {
    let proxy = proxy_url
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .unwrap_or("-");
    format!("{proxy}|{warm_url}")
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
    headers.sort_by(|(ak, _), (bk, _)| ak.cmp(bk));
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
    dst.insert(header::CACHE_CONTROL, HeaderValue::from_static("no-cache"));
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
        match (dialect, self.terminal_event_type.as_deref()) {
            (Some("chat_completions"), Some("[DONE]" | "chat.finish_reason")) => true,
            (Some("responses"), Some("response.completed" | "response.done")) => true,
            _ => false,
        }
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
        let response = value.get("response").unwrap_or(&value);
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
        pools.get_mut(&key).and_then(Vec::pop)
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
        }
        let state = self.clone();
        tokio::spawn(async move {
            match connect_ws_for_plan(&plan).await {
                Ok((socket, request_id)) => {
                    let mut pools = state.ws_idle.lock().await;
                    let pool = pools.entry(key).or_default();
                    if pool.len() < state.cfg.ws_idle_per_key {
                        pool.push(WsIdleConn { socket, request_id });
                    }
                }
                Err(err) => warn!("edge ws idle preconnect failed: {err}"),
            }
        });
    }

    fn relay_queue_for_plan(
        &self,
        plan: &EdgePlan,
    ) -> anyhow::Result<Option<mpsc::Sender<RelayJob>>> {
        if self.cfg.queue_buffer_size == 0 || self.cfg.per_account_workers == 0 {
            return Ok(None);
        }
        let Some(key) = relay_queue_key(plan) else {
            return Ok(None);
        };

        let mut queues = self
            .relay_queues
            .lock()
            .map_err(|_| anyhow::anyhow!("relay queue map lock poisoned"))?;
        if let Some(sender) = queues.get(&key) {
            return Ok(Some(sender.clone()));
        }

        let (sender, receiver) = mpsc::channel::<RelayJob>(self.cfg.queue_buffer_size);
        let receiver = Arc::new(tokio::sync::Mutex::new(receiver));
        for worker_id in 0..self.cfg.per_account_workers {
            let rx = receiver.clone();
            let queue_key = key.clone();
            tokio::spawn(async move {
                loop {
                    let job = {
                        let mut guard = rx.lock().await;
                        guard.recv().await
                    };
                    let Some(job) = job else {
                        break;
                    };
                    let mut timing = job.timing;
                    let queue_wait_ms = job.enqueued_at.elapsed().as_millis() as i64;
                    timing.queue_wait_ms = Some(queue_wait_ms);
                    update_edge_timing(job.timing_shared.as_ref(), |shared| {
                        shared.queue_wait_ms = Some(queue_wait_ms);
                        shared.retry_count = timing.retry_count;
                    });
                    if job.state.cfg.queue_wait_budget_ms > 0
                        && queue_wait_ms > job.state.cfg.queue_wait_budget_ms as i64
                    {
                        let response_tx = job.response_tx;
                        let result = retry_after_queue_wait_budget(
                            job.state,
                            job.plan,
                            job.started_at,
                            timing,
                            job.timing_shared,
                            queue_wait_ms,
                        )
                        .await;
                        let _ = response_tx.send(result);
                        continue;
                    }
                    let result = relay_upstream_direct(
                        job.state,
                        job.plan,
                        job.request_body,
                        job.started_at,
                        timing,
                        job.timing_shared,
                    )
                    .await;
                    let _ = job.response_tx.send(result);
                }
                info!("edge relay worker stopped key={queue_key} worker={worker_id}");
            });
        }
        queues.insert(key, sender.clone());
        Ok(Some(sender))
    }

    fn client_for_proxy(&self, proxy_url: Option<&str>) -> anyhow::Result<Client> {
        let proxy_url = proxy_url.map(str::trim).filter(|v| !v.is_empty());
        let Some(proxy_url) = proxy_url else {
            return Ok(self.client.clone());
        };

        let mut clients = self
            .clients_by_proxy
            .lock()
            .map_err(|_| anyhow::anyhow!("proxy client pool lock poisoned"))?;
        if let Some(client) = clients.get(proxy_url) {
            return Ok(client.clone());
        }

        let client = edge_http_client_builder(&self.cfg)
            .proxy(reqwest::Proxy::all(proxy_url)?)
            .build()?;
        clients.insert(proxy_url.to_string(), client.clone());
        Ok(client)
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
        warm_keys
            .entry(key)
            .and_modify(|item| item.last_seen = Instant::now())
            .or_insert(WarmKeyState {
                proxy_url,
                warm_url,
                last_seen: Instant::now(),
                failures: 0,
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
        Ok(Self {
            listen_addr,
            go_base_url,
            control_base_url,
            internal_secret,
            prepare_timeout_ms: env_u64("SUB2API_EDGE_PREPARE_TIMEOUT_MS", 1500),
            complete_timeout_ms: env_u64("SUB2API_EDGE_COMPLETE_TIMEOUT_MS", 1500),
            initial_pool_size: env_usize("SUB2API_EDGE_INITIAL_POOL_SIZE", 10000),
            queue_buffer_size: env_usize("SUB2API_EDGE_QUEUE_BUFFER_SIZE", 2000),
            per_account_workers: env_usize("SUB2API_EDGE_PER_ACCOUNT_WORKERS", 32),
            max_idle_conns_per_account: env_usize(
                "SUB2API_EDGE_MAX_IDLE_PER_ACCOUNT",
                env_usize("SUB2API_EDGE_MAX_IDLE_PER_HOST", 64),
            ),
            queue_wait_budget_ms: env_u64("SUB2API_EDGE_QUEUE_WAIT_BUDGET_MS", 150),
            large_payload_passthrough: env_bool("SUB2API_EDGE_LARGE_PAYLOAD_PASSTHROUGH", true),
            large_payload_threshold_bytes: env_usize(
                "SUB2API_EDGE_LARGE_PAYLOAD_THRESHOLD_BYTES",
                256 * 1024,
            ),
            ws_idle_per_key: env_usize("SUB2API_EDGE_WS_IDLE_PER_KEY", 1),
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
        })
    }
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
    if cleaned.len() % 4 != 0 {
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
        assert_eq!(smart.barrier, Some(Duration::from_millis(200)));

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
    fn first_token_timeout_placeholder_uses_chat_chunk_for_chat_dialect() {
        let mut summary = ChatStreamSummary::default();
        summary.response_id = Some("chatcmpl_123".to_string());
        summary.model = Some("gpt-4.1".to_string());

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
            Some("24h")
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
        assert_eq!(
            output
                .pointer("/prompt_cache_options/ttl")
                .and_then(Value::as_str),
            Some("24h")
        );
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
    fn ws_first_message_uses_raw_body_only_for_cache_optimization() {
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
            matches!(disabled_message, TungsteniteMessage::Text(text) if text == r#"{"source":"body"}"#)
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
            account_id: Some(42),
            transport: Some("http2_sse".to_string()),
            response_dialect: Some("responses".to_string()),
            upstream_url: Some("https://api.openai.com/v1/responses".to_string()),
            headers: None,
            body: Some(serde_json::json!({"rewritten": false})),
            body_raw_base64: Some(b64_encode(br#"{"rewritten":true}"#)),
            proxy_url: None,
            low_latency_mode: None,
            lane: None,
            safe_token_placeholder: false,
            first_token_timeout_placeholder_ms: None,
            prompt_cache_creation_optimization_mode: None,
            prompt_cache_creation_optimization_model: None,
            prompt_cache_creation_optimization_applied: false,
        };

        let first = edge_plan_ws_first_message(
            &plan,
            AxumWsMessage::Text(r#"{"rewritten":"original"}"#.to_string()),
        )
        .expect("ws first message");
        assert!(
            matches!(first, TungsteniteMessage::Text(text) if text == r#"{"rewritten":false}"#)
        );

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
        let body = br#"{"plan":{"action":"fallback_go","reason":"edge_disabled","edge_request_id":"edge-1"}}"#;
        let plan = decode_edge_plan(body).expect("plan envelope");
        assert_eq!(plan.action, "fallback_go");
        assert_eq!(plan.reason.as_deref(), Some("edge_disabled"));
        assert_eq!(plan.edge_request_id, "edge-1");
    }

    #[test]
    fn response_body_preview_redacts_secret_headers() {
        let body = br#"{"headers":{"authorization":"Bearer secret","api-key":"sk-secret","x-api-key":"x-secret"}}"#;
        let preview = response_body_preview(body);
        assert!(preview.contains("\"authorization\":\"[redacted]\""));
        assert!(preview.contains("\"api-key\":\"[redacted]\""));
        assert!(preview.contains("\"x-api-key\":\"[redacted]\""));
        assert!(!preview.contains("secret"));
    }

    #[test]
    fn relay_queue_key_uses_account_proxy_and_host() {
        let plan = EdgePlan {
            action: "relay".to_string(),
            reason: None,
            edge_request_id: "edge-1".to_string(),
            lease_id: Some("lease-1".to_string()),
            account_id: Some(42),
            transport: Some("http2_sse".to_string()),
            response_dialect: Some("chat_completions".to_string()),
            upstream_url: Some("https://api.openai.com/v1/chat/completions".to_string()),
            headers: None,
            body: None,
            body_raw_base64: None,
            proxy_url: Some("http://127.0.0.1:7890".to_string()),
            low_latency_mode: None,
            lane: Some("priority".to_string()),
            safe_token_placeholder: false,
            first_token_timeout_placeholder_ms: None,
            prompt_cache_creation_optimization_mode: None,
            prompt_cache_creation_optimization_model: None,
            prompt_cache_creation_optimization_applied: false,
        };

        assert_eq!(
            relay_queue_key(&plan).as_deref(),
            Some("42|http://127.0.0.1:7890|api.openai.com|priority")
        );
    }
}
