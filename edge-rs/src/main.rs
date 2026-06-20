use std::{
    collections::HashMap,
    env,
    net::SocketAddr,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc, Mutex,
    },
    time::{Duration, Instant},
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
    started_at: Instant,
    enqueued_at: Instant,
    timing: EdgeTiming,
    timing_shared: Option<Arc<Mutex<EdgeTiming>>>,
    retry_depth: u8,
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
}

impl BufferPools {
    fn prewarmed(initial_pool_size: usize) -> Self {
        let mut strings = Vec::with_capacity(initial_pool_size);
        for _ in 0..initial_pool_size {
            strings.push(String::with_capacity(8192));
        }
        Self {
            sse_strings: Mutex::new(strings),
        }
    }

    fn take_sse_string(&self) -> String {
        self.sse_strings
            .lock()
            .ok()
            .and_then(|mut pool| pool.pop())
            .unwrap_or_else(|| String::with_capacity(8192))
    }

    fn recycle_sse_string(&self, mut value: String) {
        if value.capacity() > 1024 * 1024 {
            return;
        }
        value.clear();
        if let Ok(mut pool) = self.sse_strings.lock() {
            pool.push(value);
        }
    }
}

#[derive(Debug, Deserialize)]
struct RetryDecision {
    action: String,
    reason: Option<String>,
    plan: Option<EdgePlan>,
    retry_max_depth: Option<u8>,
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

#[derive(Debug, Serialize)]
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
    first_client_flush_ms: Option<i64>,
    edge_prepare_ms: Option<i64>,
    edge_queue_wait_ms: Option<i64>,
    edge_relay_start_ms: Option<i64>,
    edge_fallback_reason: Option<String>,
    edge_retry_count: i64,
    error_type: Option<String>,
    error_message: Option<String>,
    upstream_status_code: Option<u16>,
}

#[derive(Clone, Debug, Default, Serialize)]
struct Usage {
    input_tokens: i64,
    output_tokens: i64,
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
}

#[derive(Clone, Copy, Debug, Default)]
struct ChatStreamObservation {
    starts_client_output: bool,
    saw_response_created: bool,
    response_created_boundary_offset: Option<usize>,
}

impl ChatStreamObservation {
    fn merge(&mut self, other: ChatStreamObservation) {
        self.starts_client_output |= other.starts_client_output;
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
        let send = client.get(&warm_url).timeout(Duration::from_secs(10)).send();
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
                    if let Some(current) =
                        warm_keys.get_mut(&dynamic_warm_key(item.proxy_url.as_deref(), &item.warm_url))
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
                    .header(header::ACCEPT_ENCODING, HeaderValue::from_static("identity"))
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
        0,
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
    let first_upstream_msg = if let Some(body) = plan.body.clone() {
        TungsteniteMessage::Text(body.to_string())
    } else {
        axum_to_tungstenite_message(first_msg)?
    };
    upstream_write.send(first_upstream_msg).await?;

    let lease_id = plan.lease_id.clone();
    let account_id = plan.account_id;
    let edge_request_id_complete = plan.edge_request_id.clone();
    let mut success = true;
    let mut client_disconnected = false;
    let mut error_message: Option<String> = None;
    let mut summary = ChatStreamSummary::default();
    summary.request_id = upstream_request_id;

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
                        upstream_write.send(axum_to_tungstenite_message(msg)?).await?;
                    }
                    Some(Err(err)) => {
                        success = false;
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
                        summary.observe_ws_message(&msg);
                        client_socket.send(tungstenite_to_axum_message(msg)?).await?;
                    }
                    Some(Err(err)) => {
                        success = false;
                        error_message = Some(err.to_string());
                        break;
                    }
                    None => break,
                }
            }
        }
    }

    if client_disconnected {
        call_abort(
            &state,
            AbortRequest {
                edge_request_id: edge_request_id_complete,
                lease_id,
                account_id,
                reason: "client_disconnect".to_string(),
                client_disconnected: true,
            },
        )
        .await?;
        guard.mark_done();
        return Ok(());
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
            first_client_flush_ms: None,
            edge_prepare_ms: None,
            edge_queue_wait_ms: None,
            edge_relay_start_ms: None,
            edge_fallback_reason: None,
            edge_retry_count: 0,
            error_type: if success {
                None
            } else {
                Some("ws_error".to_string())
            },
            error_message,
            upstream_status_code: None,
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

async fn relay_upstream(
    state: AppState,
    plan: EdgePlan,
    started_at: Instant,
    timing: EdgeTiming,
    timing_shared: Option<Arc<Mutex<EdgeTiming>>>,
    retry_depth: u8,
) -> anyhow::Result<Response> {
    if retry_depth == 0 {
        if let Some(sender) = state.relay_queue_for_plan(&plan)? {
            let (response_tx, response_rx) = oneshot::channel();
            let job = RelayJob {
                state: state.clone(),
                plan,
                started_at,
                enqueued_at: Instant::now(),
                timing,
                timing_shared,
                retry_depth,
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
    relay_upstream_direct(state, plan, started_at, timing, timing_shared, retry_depth).await
}

async fn relay_upstream_direct(
    state: AppState,
    plan: EdgePlan,
    started_at: Instant,
    mut timing: EdgeTiming,
    timing_shared: Option<Arc<Mutex<EdgeTiming>>>,
    retry_depth: u8,
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
    let body = request_body_bytes(&plan)?;
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
    req = req.header(header::ACCEPT_ENCODING, HeaderValue::from_static("identity"));
    let header_start = Instant::now();
    let upstream = req.body(body).send().await?;
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
            let retry_max_depth = decision.retry_max_depth.unwrap_or(5).max(1);
            if retry_depth >= retry_max_depth {
                anyhow::bail!("edge retry depth exceeded");
            }
            if let Some(next_plan) = decision.plan {
                let mut next_timing = timing.clone();
                next_timing.retry_count += 1;
                return Box::pin(relay_upstream(
                    state,
                    next_plan,
                    started_at,
                    next_timing,
                    timing_shared,
                    retry_depth + 1,
                ))
                .await;
            }
            anyhow::bail!("retry decision missing relay plan");
        }
        let reason = decision
            .reason
            .unwrap_or_else(|| "retry_fallback_go".to_string());
        anyhow::bail!("retry decision requested Go fallback: {reason}");
    }
    let mut bytes_stream = upstream.bytes_stream();

    let edge_request_id = plan.edge_request_id.clone();
    let lease_id = plan.lease_id.clone();
    let account_id = plan.account_id;
    let edge_prepare_ms = timing.prepare_ms;
    let edge_queue_wait_ms = timing.queue_wait_ms;
    let edge_relay_start_ms = timing.relay_start_ms;
    let edge_fallback_reason = timing.fallback_reason.clone();
    let edge_retry_count = timing.retry_count;
    let complete_state = state.clone();
    let upstream_request_id = headers
        .get("x-request-id")
        .and_then(|v| v.to_str().ok())
        .map(ToOwned::to_owned);
    let body_stream = stream! {
        let guard = LeaseAbortGuard::new(
            complete_state.clone(),
            edge_request_id.clone(),
            lease_id.clone(),
            account_id,
            "stream_dropped_before_complete",
            true,
        );
        let mut first_byte_ms: Option<i64> = None;
        let mut first_flush_ms: Option<i64> = None;
        let mut first_token_ms: Option<i64> = None;
        let mut safe_token_placeholder_sent = false;
        let mut bootstrap_comment_sent = false;
        let mut success = true;
        let mut error_message: Option<String> = None;
        let mut summary = ChatStreamSummary::with_pending(complete_state.pools.take_sse_string());
        summary.request_id = upstream_request_id;

        if low_latency_policy.enabled && low_latency_policy.barrier.is_none() {
            first_flush_ms = Some(started_at.elapsed().as_millis() as i64);
            bootstrap_comment_sent = true;
            yield Ok::<Bytes, std::io::Error>(Bytes::from_static(b":\n\n"));
        }

        let mut bootstrap_timer = low_latency_policy
            .barrier
            .map(tokio::time::sleep)
            .map(Box::pin);

        loop {
            let next = if let Some(timer) = bootstrap_timer.as_mut() {
                tokio::select! {
                    _ = timer.as_mut(), if !bootstrap_comment_sent => {
                        if first_flush_ms.is_none() {
                            first_flush_ms = Some(started_at.elapsed().as_millis() as i64);
                        }
                        bootstrap_comment_sent = true;
                        yield Ok::<Bytes, std::io::Error>(Bytes::from_static(b":\n\n"));
                        continue;
                    }
                    next = bytes_stream.next() => next,
                }
            } else {
                bytes_stream.next().await
            };
            let Some(next) = next else {
                break;
            };
            match next {
                Ok(chunk) => {
                    if first_byte_ms.is_none() {
                        first_byte_ms = Some(header_start.elapsed().as_millis() as i64);
                    }
                    if first_flush_ms.is_none() {
                        first_flush_ms = Some(started_at.elapsed().as_millis() as i64);
                    }
                    bootstrap_comment_sent = true;
                    let observation = summary.observe(&chunk);
                    if first_token_ms.is_none() && observation.starts_client_output {
                        first_token_ms = Some(started_at.elapsed().as_millis() as i64);
                    }
                    if plan.safe_token_placeholder && !safe_token_placeholder_sent {
                        if let Some(offset) = observation.response_created_boundary_offset {
                            safe_token_placeholder_sent = true;
                            if first_token_ms.is_none() {
                                first_token_ms = Some(started_at.elapsed().as_millis() as i64);
                            }
                            if first_flush_ms.is_none() {
                                first_flush_ms = Some(started_at.elapsed().as_millis() as i64);
                            }
                            let offset = offset.min(chunk.len());
                            if offset > 0 {
                                yield Ok::<Bytes, std::io::Error>(chunk.slice(..offset));
                            }
                            let placeholder =
                                openai_responses_safe_token_placeholder_frame(summary.response_id.as_deref());
                            yield Ok::<Bytes, std::io::Error>(Bytes::from(placeholder));
                            if offset < chunk.len() {
                                yield Ok::<Bytes, std::io::Error>(chunk.slice(offset..));
                            }
                            continue;
                        }
                    }
                    yield Ok::<Bytes, std::io::Error>(chunk);
                }
                Err(err) => {
                    success = false;
                    error_message = Some(err.to_string());
                    yield Err(std::io::Error::new(std::io::ErrorKind::Other, err.to_string()));
                    break;
                }
            }
        }

        let request_id = summary.request_id.clone();
        let response_id = summary.response_id.clone();
        let model = summary.model.clone();
        let upstream_model = summary.upstream_model.clone();
        let usage = summary.usage.clone();
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
            first_client_flush_ms: first_flush_ms,
            edge_prepare_ms,
            edge_queue_wait_ms,
            edge_relay_start_ms,
            edge_fallback_reason,
            edge_retry_count,
            error_type: if success { None } else { Some("stream_error".to_string()) },
            error_message,
            upstream_status_code: Some(status.as_u16()),
        }).await.is_ok() {
            guard.mark_done();
        }
    };

    let mut builder = Response::builder().status(status.as_u16());
    copy_response_headers(builder.headers_mut().expect("headers"), &headers);
    Ok(builder.body(Body::from_stream(body_stream))?)
}

async fn retry_after_queue_wait_budget(
    state: AppState,
    plan: EdgePlan,
    started_at: Instant,
    timing: EdgeTiming,
    timing_shared: Option<Arc<Mutex<EdgeTiming>>>,
    retry_depth: u8,
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
    let retry_max_depth = decision.retry_max_depth.unwrap_or(5).max(1);
    if retry_depth >= retry_max_depth {
        anyhow::bail!("edge queue wait retry depth exceeded");
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
        retry_depth + 1,
    ))
    .await
}

fn low_latency_policy(mode: Option<&str>) -> LowLatencyPolicy {
    match mode.unwrap_or_default().trim().to_ascii_lowercase().as_str() {
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

fn json_event_type(value: &Value) -> Option<&str> {
    value.get("type").and_then(Value::as_str)
}

fn openai_responses_safe_token_placeholder_frame(response_id: Option<&str>) -> String {
    let response_id = response_id
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .unwrap_or("resp_placeholder");
    let response_id_json = serde_json::to_string(response_id)
        .unwrap_or_else(|_| "\"resp_placeholder\"".to_string());
    format!(
        "data: {{\"type\":\"response.output_text.delta\",\"delta\":\"\",\"response_id\":{},\"item_id\":\"msg_placeholder\",\"output_index\":0,\"content_index\":0}}\n\n",
        response_id_json
    )
}

fn json_starts_client_output(value: &Value) -> bool {
    let event_type = json_event_type(value).unwrap_or_default();
    !matches!(
        event_type,
        "response.created" | "response.in_progress" | "response.failed"
    )
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
        Err(err) => {
            return text_response(
                StatusCode::BAD_REQUEST,
                &format!("read fallback body failed: {err}"),
            )
        }
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
        Err(err) => {
            return text_response(
                StatusCode::BAD_GATEWAY,
                &format!("fallback go request failed: {err}"),
            )
        }
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
        .unwrap_or_else(|err| {
            text_response(
                StatusCode::BAD_GATEWAY,
                &format!("build fallback response failed: {err}"),
            )
        })
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

fn request_body_bytes(plan: &EdgePlan) -> anyhow::Result<Vec<u8>> {
    if let Some(raw) = plan
        .body_raw_base64
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
    {
        return b64_decode(raw);
    }
    Ok(serde_json::to_vec(
        &plan.body.clone().unwrap_or(Value::Null),
    )?)
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
    match lane.unwrap_or_default().trim().to_ascii_lowercase().as_str() {
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
    !matches!(
        name.to_ascii_lowercase().as_str(),
        "content-length"
            | "connection"
            | "keep-alive"
            | "proxy-authenticate"
            | "proxy-authorization"
            | "te"
            | "trailer"
            | "transfer-encoding"
            | "upgrade"
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

impl ChatStreamSummary {
    fn with_pending(pending: String) -> Self {
        Self {
            pending,
            ..Self::default()
        }
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
                line_observation.response_created_boundary_offset =
                    Some(consumed_total.saturating_sub(pending_before).min(chunk.len()));
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
        if payload.is_empty() || payload == "[DONE]" {
            return ChatStreamObservation::default();
        }
        let Ok(value) = serde_json::from_str::<Value>(payload) else {
            return ChatStreamObservation::default();
        };
        let observation = ChatStreamObservation {
            starts_client_output: json_starts_client_output(&value),
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
        if let Some(cached) = usage
            .get("prompt_tokens_details")
            .or_else(|| usage.get("input_tokens_details"))
            .and_then(|v| v.get("cached_tokens"))
            .and_then(Value::as_i64)
        {
            self.usage.cache_read_input_tokens = cached;
        }
    }
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
                            job.retry_depth,
                            queue_wait_ms,
                        )
                        .await;
                        let _ = response_tx.send(result);
                        continue;
                    }
                    let result = relay_upstream_direct(
                        job.state,
                        job.plan,
                        job.started_at,
                        timing,
                        job.timing_shared,
                        job.retry_depth,
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
                            "cached_tokens": 3
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

        assert!(summary.observe(
            b"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
        )
        .starts_client_output);
    }

    #[test]
    fn chat_completion_delta_counts_as_client_output() {
        let mut summary = ChatStreamSummary::default();
        assert!(summary.observe(
            b"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"
        )
        .starts_client_output);
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
    fn request_body_prefers_raw_base64() {
        let plan = EdgePlan {
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
        };

        let body = request_body_bytes(&plan).expect("request body");
        assert_eq!(body, br#"{"rewritten":true}"#);
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
        };

        assert_eq!(
            relay_queue_key(&plan).as_deref(),
            Some("42|http://127.0.0.1:7890|api.openai.com|priority")
        );
    }
}
