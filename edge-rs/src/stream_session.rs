//! Internal Go -> Edge transport recovery. A reserved session can start exactly
//! once; reconnects only read its byte journal and never execute a new request.
use super::*;
use std::collections::VecDeque;
use std::sync::OnceLock;
use tokio::sync::Notify;

pub const ID_HEADER: &str = "x-sub2api-edge-stream-session";
pub const OFFSET_HEADER: &str = "x-sub2api-edge-stream-offset";
const PREFIX: &str = "/internal/edge/stream-session/";
const WINDOW: usize = 512 * 1024;
// Tiny upstream chunks need an independent metadata/allocation bound.
const MAX_CHUNKS: usize = 1024;
// At most 128 MiB of retained replay bytes, plus response headers/chunk metadata.
const MAX_SESSIONS: usize = 256;
// Must exceed Go's 35s silent-socket detection + 25s recovery budget. Hyper
// can finish/drop a reader while a proxy still holds the client socket open.
const GRACE: Duration = Duration::from_secs(75);

#[derive(Default)]
struct Journal {
    started: bool,
    awaiting_first_reader: bool,
    head: Option<(StatusCode, HeaderMap)>,
    chunks: VecDeque<Bytes>,
    base: u64,
    end: u64,
    done: bool,
    failed: bool,
    readers: usize,
    reader_epoch: u64,
    last_attached: Option<Instant>,
}

struct Session {
    binding: String,
    journal: Mutex<Journal>,
    changed: Notify,
    cancel: tokio::sync::watch::Sender<bool>,
    _permit: Option<OwnedSemaphorePermit>,
}

type Registry = Mutex<HashMap<String, Arc<Session>>>;
fn registry() -> &'static Registry {
    static REGISTRY: OnceLock<Registry> = OnceLock::new();
    REGISTRY.get_or_init(|| Mutex::new(HashMap::new()))
}

fn capacity() -> &'static Arc<Semaphore> {
    static CAPACITY: OnceLock<Arc<Semaphore>> = OnceLock::new();
    CAPACITY.get_or_init(|| Arc::new(Semaphore::new(MAX_SESSIONS)))
}

fn authorized(state: &AppState, headers: &HeaderMap) -> bool {
    let secret = headers
        .get(EDGE_SECRET_HEADER)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    !secret.is_empty() && constant_time_eq(secret.as_bytes(), state.cfg.internal_secret.as_bytes())
}

fn binding(headers: &HeaderMap) -> String {
    headers
        .get("x-sub2api-edge-stream-binding")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string()
}

pub async fn control(State(state): State<AppState>, req: Request) -> Response {
    if !authorized(&state, req.headers()) {
        return text_response(StatusCode::UNAUTHORIZED, "unauthorized");
    }
    let id = req.uri().path().strip_prefix(PREFIX).unwrap_or("");
    if Uuid::parse_str(id).is_err() || binding(req.headers()).len() != 64 {
        return text_response(StatusCode::BAD_REQUEST, "invalid session");
    }
    if req.method() == Method::PUT {
        let mut entries = registry().lock().unwrap();
        if let Some(session) = entries.get(id) {
            return text_response(
                if session.binding == binding(req.headers()) {
                    StatusCode::NO_CONTENT
                } else {
                    StatusCode::CONFLICT
                },
                "",
            );
        }
        if entries.len() >= MAX_SESSIONS || state.metrics_is_draining() {
            return text_response(
                StatusCode::SERVICE_UNAVAILABLE,
                "session capacity unavailable",
            );
        }
        let Ok(permit) = capacity().clone().try_acquire_owned() else {
            return text_response(
                StatusCode::SERVICE_UNAVAILABLE,
                "session capacity unavailable",
            );
        };
        let (cancel, _) = tokio::sync::watch::channel(false);
        let session = Arc::new(Session {
            binding: binding(req.headers()),
            journal: Mutex::new(Journal {
                last_attached: Some(Instant::now()),
                ..Default::default()
            }),
            changed: Notify::new(),
            cancel,
            _permit: Some(permit),
        });
        entries.insert(id.to_string(), session.clone());
        let id = id.to_string();
        // Retire reservations, detached executions, and completed journals.
        // The registry is never a license to restart an expired execution.
        tokio::spawn(async move {
            let mut cancelled = session.cancel.subscribe();
            loop {
                tokio::select! {
                    _ = wait_cancelled(&mut cancelled) => break,
                    _ = tokio::time::sleep(Duration::from_secs(1)) => {},
                }
                let expired = {
                    let journal = session.journal.lock().unwrap();
                    let expired = journal.readers == 0
                        && journal
                            .last_attached
                            .is_some_and(|at| at.elapsed() >= GRACE);
                    // Serialize expiry with attach/claim; never revoke a
                    // reader that reattached between checking and cancelling.
                    if expired {
                        session.cancel.send_replace(true);
                    }
                    expired
                };
                if expired {
                    registry().lock().unwrap().remove(&id);
                    session.changed.notify_waiters();
                    break;
                }
            }
        });
        return text_response(StatusCode::NO_CONTENT, "");
    }
    let session = registry().lock().unwrap().get(id).cloned();
    let Some(session) = session.filter(|s| s.binding == binding(req.headers())) else {
        return text_response(StatusCode::GONE, "session unavailable");
    };
    if req.method() == Method::DELETE {
        registry().lock().unwrap().remove(id);
        session.cancel.send_replace(true);
        session.changed.notify_waiters();
        // POST cannot create a session; a delayed POST now receives Gone.
        return text_response(StatusCode::NO_CONTENT, "");
    }
    if req.method() != Method::GET {
        return text_response(StatusCode::METHOD_NOT_ALLOWED, "unsupported method");
    }
    let Some(offset) = req
        .headers()
        .get(OFFSET_HEADER)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.parse::<u64>().ok())
    else {
        return text_response(StatusCode::BAD_REQUEST, "invalid offset");
    };
    attach(session, offset).await
}

pub async fn ingress(
    State(state): State<AppState>,
    ws: Option<WebSocketUpgrade>,
    mut req: Request,
) -> Response {
    let id = req
        .headers()
        .get(ID_HEADER)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string();
    if id.is_empty() {
        return handle_openai_edge(State(state), ws, req).await;
    }
    if !authorized(&state, req.headers()) || req.method() != Method::POST || ws.is_some() {
        return text_response(StatusCode::UNAUTHORIZED, "unauthorized session");
    }
    let session = registry().lock().unwrap().get(&id).cloned();
    let Some(session) = session.filter(|s| s.binding == binding(req.headers())) else {
        let mut response = text_response(StatusCode::GONE, "session unavailable");
        // Authenticated POST was rejected before claim/prepare. This is a
        // safe pre-execution fallback only on Go's first send, never after
        // an ambiguous disconnect or a recovery attempt.
        response
            .headers_mut()
            .insert(ID_HEADER, HeaderValue::from_static("absent"));
        return response;
    };
    let start = match claim(&session) {
        Ok(start) => start,
        Err(()) => return text_response(StatusCode::GONE, "session cancelled"),
    };
    if start {
        // Never expose internal capabilities in upstream or Go-fallback headers.
        req.headers_mut().remove(ID_HEADER);
        req.headers_mut().remove("x-sub2api-edge-stream-binding");
        req.headers_mut().remove(OFFSET_HEADER);
        req.headers_mut().remove(EDGE_SECRET_HEADER);
        req.extensions_mut().insert(ExecutionID(id));
        let producer = session.clone();
        tokio::spawn(async move {
            let mut cancelled = producer.cancel.subscribe();
            tokio::select! {
                biased;
                _ = wait_cancelled(&mut cancelled) => {},
                _ = produce(producer.clone(), handle_openai_edge(State(state), None, req)) => {},
            }
            let mut journal = producer.journal.lock().unwrap();
            if !journal.done {
                journal.failed = true;
                journal.done = true;
            }
            drop(journal);
            producer.changed.notify_waiters();
        });
    }
    attach_inner(session, 0, false).await
}

#[derive(Clone)]
pub struct ExecutionID(pub String);

fn claim(session: &Session) -> Result<bool, ()> {
    let mut journal = session.journal.lock().unwrap();
    if *session.cancel.borrow() {
        return Err(());
    }
    let start = !journal.started;
    journal.started = true;
    if start {
        journal.awaiting_first_reader = true;
    }
    Ok(start)
}

async fn wait_cancelled(cancelled: &mut tokio::sync::watch::Receiver<bool>) {
    while !*cancelled.borrow() {
        if cancelled.changed().await.is_err() {
            break;
        }
    }
}

async fn produce(session: Arc<Session>, response: impl Future<Output = Response>) {
    let response = response.await;
    let (parts, body) = response.into_parts();
    {
        let mut journal = session.journal.lock().unwrap();
        journal.head = Some((parts.status, parts.headers));
    }
    session.changed.notify_waiters();
    let mut source = body.into_data_stream();
    while let Some(chunk) = source.next().await {
        let Ok(chunk) = chunk else {
            session.journal.lock().unwrap().failed = true;
            break;
        };
        // Backpressure while attached: don't evict bytes the live reader has
        // not consumed. A detached producer retains only the recovery window.
        for bytes in chunk.chunks(32 * 1024) {
            loop {
                let notified = session.changed.notified();
                tokio::pin!(notified);
                notified.as_mut().enable();
                let appended = {
                    let mut journal = session.journal.lock().unwrap();
                    if (journal.readers == 0 && !journal.awaiting_first_reader)
                        || (journal.end - journal.base + bytes.len() as u64 <= WINDOW as u64
                            && journal.chunks.len() < MAX_CHUNKS)
                    {
                        while journal.end - journal.base + bytes.len() as u64 > WINDOW as u64
                            || journal.chunks.len() >= MAX_CHUNKS
                        {
                            if let Some(old) = journal.chunks.pop_front() {
                                journal.base += old.len() as u64;
                            } else {
                                break;
                            }
                        }
                        journal.chunks.push_back(Bytes::copy_from_slice(bytes));
                        journal.end += bytes.len() as u64;
                        true
                    } else {
                        false
                    }
                };
                if appended {
                    session.changed.notify_waiters();
                    break;
                }
                notified.await;
            }
        }
    }
    session.journal.lock().unwrap().done = true;
    session.changed.notify_waiters();
}

struct Reader(Arc<Session>, u64);
impl Drop for Reader {
    fn drop(&mut self) {
        let mut journal = self.0.journal.lock().unwrap();
        if journal.reader_epoch == self.1 {
            journal.readers = 0;
            journal.last_attached = Some(Instant::now());
        }
        drop(journal);
        self.0.changed.notify_waiters();
    }
}

async fn attach(session: Arc<Session>, offset: u64) -> Response {
    attach_inner(session, offset, true).await
}

async fn attach_inner(session: Arc<Session>, mut offset: u64, takeover: bool) -> Response {
    let epoch = {
        let mut journal = session.journal.lock().unwrap();
        if !journal.started {
            return text_response(StatusCode::TOO_EARLY, "session not started");
        }
        if journal.readers > 0 && !takeover {
            return text_response(StatusCode::CONFLICT, "session already attached");
        }
        journal.readers = 1;
        journal.awaiting_first_reader = false;
        journal.reader_epoch += 1;
        journal.reader_epoch
    };
    session.changed.notify_waiters();
    let reader = Reader(session.clone(), epoch);
    let head = loop {
        let notified = session.changed.notified();
        tokio::pin!(notified);
        notified.as_mut().enable();
        {
            let journal = session.journal.lock().unwrap();
            if journal.reader_epoch != epoch
                || *session.cancel.borrow()
                || offset < journal.base
                || offset > journal.end
            {
                return text_response(StatusCode::GONE, "session cursor unavailable");
            }
            if journal.done && offset == journal.end {
                return text_response(
                    if journal.failed {
                        StatusCode::GONE
                    } else {
                        StatusCode::NO_CONTENT
                    },
                    "",
                );
            }
            if let Some(head) = &journal.head {
                break head.clone();
            }
            if journal.done {
                return text_response(StatusCode::GONE, "session failed");
            }
            if takeover {
                // A live execution waiting for upstream headers is not a
                // failed transport. Let Go poll without imposing a generation
                // deadline or competing with the original header waiter.
                let mut pending = text_response(StatusCode::ACCEPTED, "");
                pending
                    .headers_mut()
                    .insert(ID_HEADER, HeaderValue::from_static("pending"));
                return pending;
            }
        }
        notified.await;
    };
    let body = stream! {
        let _reader = reader;
        loop {
            let notified = session.changed.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            let (chunk, done, failed, pruned) = {
                let mut journal = session.journal.lock().unwrap();
                if journal.reader_epoch != epoch || *session.cancel.borrow() || offset < journal.base {
                    (None, true, true, false)
                } else {
                let mut cursor = journal.base;
                let mut found = None;
                for chunk in &journal.chunks {
                    if cursor + chunk.len() as u64 > offset {
                        found = Some(chunk.slice((offset - cursor) as usize..)); break;
                    }
                    cursor += chunk.len() as u64;
                }
                // Retain a replay tail, freeing older acknowledged data when
                // the window fills. Yielded bytes are not yet acknowledged;
                // keep at least half the journal for transport recovery.
                let mut pruned = false;
                while journal.end - journal.base > (WINDOW / 2) as u64 || journal.chunks.len() > MAX_CHUNKS / 2 {
                    let Some(front) = journal.chunks.front() else { break; };
                    if journal.base + front.len() as u64 > offset { break; }
                    let old = journal.chunks.pop_front().unwrap();
                    journal.base += old.len() as u64;
                    pruned = true;
                }
                (found, journal.done, journal.failed || *session.cancel.borrow(), pruned)
                }
            };
            if pruned { session.changed.notify_waiters(); }
            if let Some(chunk) = chunk {
                offset += chunk.len() as u64;
                yield Ok::<Bytes, std::io::Error>(chunk);
            } else if done || failed {
                if failed { yield Err(std::io::Error::other("edge session interrupted")); }
                break;
            } else { notified.await; }
        }
    };
    let mut response = Response::new(Body::from_stream(body));
    *response.status_mut() = head.0;
    *response.headers_mut() = head.1;
    response.headers_mut().remove(header::CONTENT_LENGTH);
    response
        .headers_mut()
        .insert(ID_HEADER, HeaderValue::from_static("v1"));
    response.headers_mut().insert(
        OFFSET_HEADER,
        HeaderValue::from_str(&offset.to_string()).unwrap(),
    );
    response
}

#[cfg(test)]
mod tests {
    use super::*;

    fn session() -> Arc<Session> {
        let (cancel, _) = tokio::sync::watch::channel(false);
        Arc::new(Session {
            binding: "test".to_string(),
            journal: Mutex::new(Journal::default()),
            changed: Notify::new(),
            cancel,
            _permit: None,
        })
    }

    #[tokio::test]
    async fn execution_claim_is_atomic_and_cannot_restart_after_cancel() {
        let s = session();
        let mut tasks = Vec::new();
        for _ in 0..32 {
            let s = s.clone();
            tasks.push(tokio::spawn(async move { claim(&s).unwrap() }));
        }
        let mut executions = 0;
        for task in tasks {
            executions += usize::from(task.await.unwrap());
        }
        assert_eq!(executions, 1);
        s.cancel.send_replace(true);
        assert!(claim(&s).is_err());
    }

    #[tokio::test]
    async fn detached_reader_resumes_exact_bytes_including_partial_tool_frame() {
        let s = session();
        claim(&s).unwrap();
        let bytes = Bytes::from_static(b"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"pwd\"}\n\ndata: {\"type\":\"response.completed\"}\n\n");
        let copy = bytes.clone();
        produce(s.clone(), async { Response::new(Body::from(copy)) }).await;
        let response = attach(s.clone(), 0).await;
        assert_eq!(response.status(), StatusCode::OK);
        drop(response); // headers/response lost without restarting producer
        let split = 47;
        let response = attach(s.clone(), split).await;
        assert_eq!(response.headers()[OFFSET_HEADER], split.to_string());
        let suffix = to_bytes(response.into_body(), WINDOW).await.unwrap();
        assert_eq!(suffix, bytes.slice(split as usize..));
        assert_eq!(
            attach(s.clone(), bytes.len() as u64).await.status(),
            StatusCode::NO_CONTENT
        );
        assert!(!claim(&s).unwrap());
    }

    #[tokio::test]
    async fn attached_stream_larger_than_window_is_continuous_and_bounded() {
        let s = session();
        claim(&s).unwrap();
        let bytes = Bytes::from(vec![b'x'; WINDOW * 5]);
        let copy = bytes.clone();
        let producer = s.clone();
        // Make the reader active before publishing headers.
        let reader = tokio::spawn({
            let s = s.clone();
            async move { attach_inner(s, 0, false).await }
        });
        tokio::task::yield_now().await;
        let task = tokio::spawn(async move {
            produce(producer, async { Response::new(Body::from(copy)) }).await
        });
        let result = tokio::time::timeout(Duration::from_secs(2), async {
            let response = reader.await.unwrap();
            to_bytes(response.into_body(), WINDOW * 6).await.unwrap()
        })
        .await
        .unwrap();
        task.await.unwrap();
        assert_eq!(result, bytes);
        let journal = s.journal.lock().unwrap();
        assert!(journal.end - journal.base <= WINDOW as u64);
        assert_eq!(journal.readers, 0);
    }

    #[tokio::test]
    async fn expired_cursor_is_rejected_and_cancel_wakes_header_waiter() {
        let s = session();
        claim(&s).unwrap();
        s.journal.lock().unwrap().awaiting_first_reader = false;
        produce(s.clone(), async {
            Response::new(Body::from(vec![b'x'; WINDOW * 2]))
        })
        .await;
        assert_eq!(attach(s.clone(), 0).await.status(), StatusCode::GONE);
        let waiting = session();
        claim(&waiting).unwrap();
        let task = tokio::spawn({
            let s = waiting.clone();
            async move { attach_inner(s, 0, false).await }
        });
        tokio::task::yield_now().await;
        waiting.cancel.send_replace(true);
        waiting.changed.notify_waiters();
        assert_eq!(
            tokio::time::timeout(Duration::from_secs(1), task)
                .await
                .unwrap()
                .unwrap()
                .status(),
            StatusCode::GONE
        );
        assert_eq!(waiting.journal.lock().unwrap().readers, 0);
    }

    #[tokio::test]
    async fn idle_body_waits_for_data_without_busy_loop() {
        let s = session();
        claim(&s).unwrap();
        s.journal.lock().unwrap().head = Some((StatusCode::OK, HeaderMap::new()));
        let response = attach(s.clone(), 0).await;
        let task = tokio::spawn(async move { to_bytes(response.into_body(), WINDOW).await });
        tokio::time::sleep(Duration::from_millis(20)).await;
        assert!(!task.is_finished());
        s.journal.lock().unwrap().done = true;
        s.changed.notify_waiters();
        assert!(tokio::time::timeout(Duration::from_secs(1), task)
            .await
            .unwrap()
            .unwrap()
            .unwrap()
            .is_empty());
    }

    #[tokio::test]
    async fn reconnect_takes_over_stale_reader_without_stopping_execution() {
        let s = session();
        claim(&s).unwrap();
        produce(s.clone(), async { Response::new(Body::from("abcdef")) }).await;
        let original = attach_inner(s.clone(), 0, false).await;
        let resumed = attach(s.clone(), 3).await;
        drop(original);
        assert_eq!(
            s.journal.lock().unwrap().readers,
            1,
            "old reader cannot detach its replacement"
        );
        assert_eq!(to_bytes(resumed.into_body(), WINDOW).await.unwrap(), "def");
        assert!(!claim(&s).unwrap());
    }

    #[tokio::test]
    async fn recovery_reports_pending_without_restart_or_generation_timeout() {
        let s = session();
        claim(&s).unwrap();
        let response = attach(s.clone(), 0).await;
        assert_eq!(response.status(), StatusCode::ACCEPTED);
        assert_eq!(response.headers()[ID_HEADER], "pending");
        assert!(!claim(&s).unwrap());
        assert_eq!(s.journal.lock().unwrap().readers, 0);
    }

    #[tokio::test]
    async fn tiny_chunks_bound_metadata_and_keep_attached_stream_complete() {
        let s = session();
        claim(&s).unwrap();
        let reader = tokio::spawn({
            let s = s.clone();
            async move { attach_inner(s, 0, false).await }
        });
        tokio::task::yield_now().await;
        let producer = s.clone();
        let task = tokio::spawn(async move {
            produce(producer, async {
                Response::new(Body::from_stream(futures_util::stream::iter(
                    (0..MAX_CHUNKS * 4).map(|_| Ok::<_, std::io::Error>(Bytes::from_static(b"x"))),
                )))
            })
            .await;
        });
        let result = tokio::time::timeout(Duration::from_secs(2), async {
            to_bytes(reader.await.unwrap().into_body(), WINDOW)
                .await
                .unwrap()
        })
        .await
        .unwrap();
        task.await.unwrap();
        assert_eq!(result.len(), MAX_CHUNKS * 4);
        assert!(s.journal.lock().unwrap().chunks.len() <= MAX_CHUNKS);

        let detached = session();
        claim(&detached).unwrap();
        detached.journal.lock().unwrap().awaiting_first_reader = false;
        produce(detached.clone(), async {
            Response::new(Body::from_stream(futures_util::stream::iter(
                (0..MAX_CHUNKS * 4).map(|_| Ok::<_, std::io::Error>(Bytes::from_static(b"x"))),
            )))
        })
        .await;
        assert_eq!(detached.journal.lock().unwrap().chunks.len(), MAX_CHUNKS);
        assert_eq!(attach(detached, 0).await.status(), StatusCode::GONE);
    }
}
