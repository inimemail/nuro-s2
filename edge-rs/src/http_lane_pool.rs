//! Small, bounded pools of independent HTTP clients for upstream SSE traffic.
//!
//! reqwest clones share one internal hyper pool. That is normally desirable,
//! but it also means every account using the same origin can end up sharing one
//! HTTP/2 connection and its peer stream limit. This module deliberately builds
//! independent clients only when the feature is enabled. The legacy path in
//! `main.rs` remains the complete emergency fallback when the flag is off.

use std::{
    collections::HashMap,
    error::Error as StdError,
    fmt,
    sync::{
        atomic::{AtomicBool, AtomicU64, AtomicU8, AtomicUsize, Ordering},
        Arc, Mutex,
    },
    time::{Duration, Instant},
};

use http::Version;
use reqwest::{Client, Url};
use ring::{
    hmac,
    rand::{SecureRandom, SystemRandom},
};
use tracing::debug;

const DEFAULT_MAX_KEYS: usize = 1024;
const DEFAULT_MAX_TOTAL_LANES: usize = 2048;
const DEFAULT_MAX_H2_LANES: usize = 4;
const DEFAULT_MAX_UNKNOWN_LANES: usize = 2;
const LANE_MAX_IDLE_PER_HOST: usize = 1;
const DEFAULT_HIGH_WATER_INFLIGHT: u64 = 24;
const DEFAULT_TARGET_INFLIGHT: u64 = 32;
const DEFAULT_PRESSURE_DURATION: Duration = Duration::from_millis(250);
const EXPANSION_COOLDOWN: Duration = Duration::from_secs(1);
const DEFAULT_IDLE_LANE_TTL: Duration = Duration::from_secs(300);
const DEFAULT_IDLE_POOL_TTL: Duration = Duration::from_secs(1200);

const HTTP_VERSION_UNKNOWN: u8 = 0;
const HTTP_VERSION_1: u8 = 1;
const HTTP_VERSION_2: u8 = 2;

/// Inputs used to select an upstream client lane.
#[derive(Clone, Copy)]
pub struct LaneRequest<'a> {
    pub account_id: Option<i64>,
    pub proxy_url: Option<&'a str>,
    pub origin_url: &'a str,
    pub lane: Option<&'a str>,
}

/// Runtime limits for the lane pool.
#[derive(Clone, Debug)]
pub struct HttpLanePoolConfig {
    pub enabled: bool,
    pub max_keys: usize,
    pub max_total_lanes: usize,
    pub max_idle_per_host: usize,
    pub target_inflight: u64,
    pub high_water_inflight: u64,
    pub pressure_duration: Duration,
    expansion_cooldown: Duration,
    pub max_unknown_lanes: usize,
    pub max_h2_lanes: usize,
    pub idle_lane_ttl: Duration,
    pub idle_pool_ttl: Duration,
}

impl HttpLanePoolConfig {
    pub fn from_env() -> Self {
        let high_water_inflight = env_u64(
            "SUB2API_EDGE_UPSTREAM_LANE_HIGH_WATER",
            DEFAULT_HIGH_WATER_INFLIGHT,
        )
        .clamp(1, 65_536);
        let target_inflight = env_u64(
            "SUB2API_EDGE_UPSTREAM_LANE_TARGET_INFLIGHT",
            DEFAULT_TARGET_INFLIGHT,
        )
        .clamp(1, 65_536)
        .max(high_water_inflight);
        let max_h2_lanes =
            env_usize("SUB2API_EDGE_UPSTREAM_LANE_MAX", DEFAULT_MAX_H2_LANES).clamp(1, 64);
        let max_unknown_lanes = env_usize(
            "SUB2API_EDGE_UPSTREAM_LANE_UNKNOWN_MAX",
            DEFAULT_MAX_UNKNOWN_LANES,
        )
        .clamp(1, 64)
        .min(max_h2_lanes);
        Self {
            enabled: env_bool("SUB2API_EDGE_UPSTREAM_LANE_POOL_ENABLED", true),
            max_keys: env_usize("SUB2API_EDGE_UPSTREAM_MAX_POOL_KEYS", DEFAULT_MAX_KEYS)
                .clamp(1, 65_536),
            max_total_lanes: env_usize(
                "SUB2API_EDGE_UPSTREAM_MAX_TOTAL_LANES",
                DEFAULT_MAX_TOTAL_LANES,
            )
            .clamp(1, 65_536),
            // A lane is already an independent pool and H2 multiplexes on one
            // connection. Retaining the legacy per-host idle allowance in
            // every lane would multiply the process-wide FD ceiling.
            max_idle_per_host: LANE_MAX_IDLE_PER_HOST,
            target_inflight,
            high_water_inflight,
            pressure_duration: Duration::from_millis(
                env_u64(
                    "SUB2API_EDGE_UPSTREAM_LANE_PRESSURE_MS",
                    DEFAULT_PRESSURE_DURATION.as_millis() as u64,
                )
                .clamp(1, 60_000),
            ),
            expansion_cooldown: EXPANSION_COOLDOWN,
            max_unknown_lanes,
            max_h2_lanes,
            idle_lane_ttl: Duration::from_secs(
                env_u64(
                    "SUB2API_EDGE_UPSTREAM_LANE_IDLE_SECS",
                    DEFAULT_IDLE_LANE_TTL.as_secs(),
                )
                .clamp(1, 86_400),
            ),
            idle_pool_ttl: Duration::from_secs(
                env_u64(
                    "SUB2API_EDGE_UPSTREAM_POOL_IDLE_SECS",
                    DEFAULT_IDLE_POOL_TTL.as_secs(),
                )
                .clamp(1, 604_800),
            ),
        }
    }
}

impl Default for HttpLanePoolConfig {
    fn default() -> Self {
        Self {
            enabled: true,
            max_keys: DEFAULT_MAX_KEYS,
            max_total_lanes: DEFAULT_MAX_TOTAL_LANES,
            max_idle_per_host: LANE_MAX_IDLE_PER_HOST,
            target_inflight: DEFAULT_TARGET_INFLIGHT,
            high_water_inflight: DEFAULT_HIGH_WATER_INFLIGHT,
            pressure_duration: DEFAULT_PRESSURE_DURATION,
            expansion_cooldown: EXPANSION_COOLDOWN,
            max_unknown_lanes: DEFAULT_MAX_UNKNOWN_LANES,
            max_h2_lanes: DEFAULT_MAX_H2_LANES,
            idle_lane_ttl: DEFAULT_IDLE_LANE_TTL,
            idle_pool_ttl: DEFAULT_IDLE_POOL_TTL,
        }
    }
}

/// A selected independent reqwest client and its lifecycle guard.
pub struct LaneSelection {
    pub client: Client,
    pub guard: LaneGuard,
}

enum AcquireStep {
    Ready(LaneSelection),
}

#[derive(Debug)]
struct LanePoolCapacityError;

impl fmt::Display for LanePoolCapacityError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("http lane pool hard capacity exhausted")
    }
}

impl StdError for LanePoolCapacityError {}

#[derive(Debug)]
struct LanePoolLegacyFallbackError;

impl fmt::Display for LanePoolLegacyFallbackError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("http lane pool protocol requires legacy client")
    }
}

impl StdError for LanePoolLegacyFallbackError {}

pub fn is_capacity_error(error: &anyhow::Error) -> bool {
    error.downcast_ref::<LanePoolCapacityError>().is_some()
}

pub fn is_legacy_fallback_error(error: &anyhow::Error) -> bool {
    error
        .downcast_ref::<LanePoolLegacyFallbackError>()
        .is_some()
}

/// RAII accounting for one request from before `send()` until the response
/// body is dropped. It is intentionally explicit so retry paths can release a
/// lane before asking Go for another plan.
pub struct LaneGuard {
    lane: Option<Arc<LaneState>>,
    group_h1_observed: Option<Arc<AtomicBool>>,
    group_h2_observed: Option<Arc<AtomicBool>>,
    group_last_used_ms: Option<Arc<AtomicU64>>,
    overflow_active: Option<Arc<AtomicU64>>,
    released: bool,
    headers_marked: bool,
    stream_marked: bool,
}

impl LaneGuard {
    /// Record the upstream protocol as soon as response headers are available.
    pub fn mark_headers(&mut self, version: Option<Version>) {
        if self.released || self.headers_marked {
            return;
        }
        self.headers_marked = true;
        if let Some(lane) = &self.lane {
            if let Some(version) = version {
                if is_http1(version) {
                    if let Some(h1_observed) = &self.group_h1_observed {
                        h1_observed.store(true, Ordering::Release);
                    }
                } else if version == Version::HTTP_2 {
                    if let Some(h2_observed) = &self.group_h2_observed {
                        h2_observed.store(true, Ordering::Release);
                    }
                }
                lane.record_version(version);
            }
            decrement(&lane.awaiting_headers);
            self.touch(lane);
        }
    }

    /// Mark the response body as an active upstream stream. This must happen
    /// before moving the guard into an Axum body stream.
    pub fn mark_stream_open(&mut self) {
        if self.released || self.stream_marked {
            return;
        }
        self.stream_marked = true;
        if let Some(lane) = &self.lane {
            lane.open_streams.fetch_add(1, Ordering::Relaxed);
            self.touch(lane);
        }
    }

    /// Idempotently release all counters before a retry/fallback.
    pub fn release(&mut self) {
        if self.released {
            return;
        }
        self.released = true;
        let Some(lane) = &self.lane else {
            return;
        };
        if !self.headers_marked {
            self.headers_marked = true;
            decrement(&lane.awaiting_headers);
        }
        if self.stream_marked {
            self.stream_marked = false;
            decrement(&lane.open_streams);
        }
        if let Some(overflow_active) = self.overflow_active.take() {
            decrement(&overflow_active);
        }
        decrement(&lane.inflight);
        self.touch(lane);
    }

    fn touch(&self, lane: &LaneState) {
        let now = now_millis();
        lane.last_used_ms.store(now, Ordering::Relaxed);
        if let Some(last_used_ms) = &self.group_last_used_ms {
            last_used_ms.store(now, Ordering::Relaxed);
        }
    }
}

impl Drop for LaneGuard {
    fn drop(&mut self) {
        self.release();
    }
}

#[derive(Clone, Eq, Hash, PartialEq)]
struct LaneKey {
    account_id: i64,
    proxy: String,
    origin: String,
    lane: String,
}

struct LaneState {
    client: Client,
    inflight: AtomicU64,
    awaiting_headers: AtomicU64,
    open_streams: AtomicU64,
    version: AtomicU8,
    last_used_ms: AtomicU64,
}

impl LaneState {
    fn new(client: Client, now_ms: u64) -> Self {
        Self {
            client,
            inflight: AtomicU64::new(0),
            awaiting_headers: AtomicU64::new(0),
            open_streams: AtomicU64::new(0),
            version: AtomicU8::new(HTTP_VERSION_UNKNOWN),
            last_used_ms: AtomicU64::new(now_ms),
        }
    }

    fn touch(&self) {
        // The pool's epoch is process-local; an absolute timestamp is not
        // needed and would make the hot path call SystemTime.
        self.last_used_ms.store(now_millis(), Ordering::Relaxed);
    }

    fn record_version(&self, version: Version) {
        if is_http1(version) {
            self.version.store(HTTP_VERSION_1, Ordering::Release);
        } else if version == Version::HTTP_2 {
            let _ = self.version.compare_exchange(
                HTTP_VERSION_UNKNOWN,
                HTTP_VERSION_2,
                Ordering::AcqRel,
                Ordering::Acquire,
            );
        }
    }

    fn is_idle(&self) -> bool {
        self.inflight.load(Ordering::Acquire) == 0
            && self.awaiting_headers.load(Ordering::Acquire) == 0
            && self.open_streams.load(Ordering::Acquire) == 0
    }
}

struct LaneGroup {
    key: LaneKey,
    /// Monotonic identity for this group lifetime. Expansion tasks retain it
    /// so a task from a reaped group cannot act on a later group with the same
    /// key and a reused local schedule generation.
    group_epoch: u64,
    lanes: Vec<Arc<LaneState>>,
    pressure_since_ms: Option<u64>,
    last_expand_ms: Option<u64>,
    expansion_scheduled: bool,
    expansion_generation: u64,
    last_used_ms: Arc<AtomicU64>,
    h1_observed: Arc<AtomicBool>,
    h2_observed: Arc<AtomicBool>,
    overflow_active: Arc<AtomicU64>,
    round_robin: usize,
}

impl LaneGroup {
    fn begin_expansion_schedule(&mut self) -> u64 {
        self.expansion_generation = self.expansion_generation.wrapping_add(1);
        self.expansion_scheduled = true;
        self.expansion_generation
    }

    fn cancel_expansion_schedule(&mut self) {
        if self.expansion_scheduled {
            self.expansion_generation = self.expansion_generation.wrapping_add(1);
            self.expansion_scheduled = false;
        }
    }

    fn expansion_schedule_matches(&self, generation: u64) -> bool {
        self.expansion_scheduled && self.expansion_generation == generation
    }

    fn has_h1(&self) -> bool {
        self.h1_observed.load(Ordering::Acquire)
    }

    fn has_h2(&self) -> bool {
        self.h2_observed.load(Ordering::Acquire)
    }

    fn protocol_max_lanes(&self, config: &HttpLanePoolConfig) -> usize {
        if self.has_h1() {
            self.lanes.len().max(1)
        } else if self.has_h2() {
            config.max_h2_lanes
        } else {
            config.max_unknown_lanes
        }
    }

    fn is_under_pressure(&self, config: &HttpLanePoolConfig) -> bool {
        !self.has_h1()
            && self
                .lanes
                .iter()
                .all(|lane| lane.inflight.load(Ordering::Relaxed) >= config.high_water_inflight)
            && self
                .lanes
                .iter()
                .any(|lane| lane.awaiting_headers.load(Ordering::Relaxed) > 0)
    }

    fn has_sustained_pressure(&self, config: &HttpLanePoolConfig, now_ms: u64) -> bool {
        self.is_under_pressure(config)
            && self.pressure_since_ms.is_some_and(|pressure_since_ms| {
                now_ms.saturating_sub(pressure_since_ms)
                    >= config.pressure_duration.as_millis().min(u64::MAX as u128) as u64
            })
    }

    fn choose_lane(&mut self) -> Arc<LaneState> {
        let min_inflight = self
            .lanes
            .iter()
            .map(|lane| lane.inflight.load(Ordering::Relaxed))
            .min()
            .unwrap_or(0);
        let len = self.lanes.len();
        for offset in 0..len {
            let index = (self.round_robin + offset) % len;
            if self.lanes[index].inflight.load(Ordering::Relaxed) == min_inflight {
                self.round_robin = (index + 1) % len;
                return Arc::clone(&self.lanes[index]);
            }
        }
        Arc::clone(&self.lanes[0])
    }
}

#[derive(Default, Clone, Debug)]
pub struct HttpLanePoolSnapshot {
    pub enabled: bool,
    pub keys: usize,
    pub lanes: usize,
    pub inflight: u64,
    pub awaiting_headers: u64,
    pub open_streams: u64,
    pub unknown_lanes: usize,
    pub http1_lanes: usize,
    pub http2_lanes: usize,
    pub pools_under_pressure: usize,
    pub pools_at_cap_pressure: usize,
    pub pools_overflowing: usize,
    pub overflow_active: u64,
    pub overflow_total: u64,
    pub expansions_total: u64,
    pub shrinks_total: u64,
    pub expansion_failures_total: u64,
    pub capacity_exhaustions_total: u64,
    pub legacy_fallbacks_total: u64,
    pub expansion_waiters: u64,
    pub expansion_delay_micros_total: u64,
    pub expansion_delay_count: u64,
}

/// Bounded, adaptive independent-client pool.
pub struct HttpLanePool {
    config: HttpLanePoolConfig,
    client_factory: Arc<ClientFactory>,
    groups: Mutex<HashMap<LaneKey, LaneGroup>>,
    proxy_fingerprint_key: hmac::Key,
    next_group_epoch: AtomicU64,
    total_lanes: AtomicUsize,
    expansions_total: AtomicU64,
    shrinks_total: AtomicU64,
    expansion_failures_total: AtomicU64,
    capacity_exhaustions_total: AtomicU64,
    legacy_fallbacks_total: AtomicU64,
    overflow_total: AtomicU64,
    expansion_waiters: AtomicU64,
    expansion_delay_micros_total: AtomicU64,
    expansion_delay_count: AtomicU64,
}

type ClientFactory = dyn Fn(&str, usize) -> anyhow::Result<Client> + Send + Sync;

impl HttpLanePool {
    pub fn new(config: HttpLanePoolConfig) -> Arc<Self> {
        Arc::new(Self::with_client_factory(config, Arc::new(build_client)))
    }

    fn with_client_factory(config: HttpLanePoolConfig, client_factory: Arc<ClientFactory>) -> Self {
        let mut fingerprint_key = [0_u8; 32];
        SystemRandom::new()
            .fill(&mut fingerprint_key)
            .expect("system randomness is required for proxy fingerprints");
        Self {
            config,
            client_factory,
            groups: Mutex::new(HashMap::new()),
            proxy_fingerprint_key: hmac::Key::new(hmac::HMAC_SHA256, &fingerprint_key),
            next_group_epoch: AtomicU64::new(1),
            total_lanes: AtomicUsize::new(0),
            expansions_total: AtomicU64::new(0),
            shrinks_total: AtomicU64::new(0),
            expansion_failures_total: AtomicU64::new(0),
            capacity_exhaustions_total: AtomicU64::new(0),
            legacy_fallbacks_total: AtomicU64::new(0),
            overflow_total: AtomicU64::new(0),
            expansion_waiters: AtomicU64::new(0),
            expansion_delay_micros_total: AtomicU64::new(0),
            expansion_delay_count: AtomicU64::new(0),
        }
    }

    pub fn enabled(&self) -> bool {
        self.config.enabled
    }

    /// Pooling is only safe when the control plane supplied an account key.
    /// Callers should use the existing shared client when this returns false.
    pub fn can_pool(&self, request: LaneRequest<'_>) -> bool {
        self.config.enabled && request.account_id.is_some_and(|account_id| account_id > 0)
    }

    /// The lane pool is deliberately limited to raw API-key upstream plans.
    /// OAuth/translated plans have different connection and auth semantics and
    /// must retain the legacy client path unless the control plane explicitly
    /// identifies the account as an API key.
    pub fn can_pool_for_account_type(
        &self,
        request: LaneRequest<'_>,
        account_type: Option<&str>,
    ) -> bool {
        self.can_pool(request)
            && account_type.is_some_and(|value| value.trim().eq_ignore_ascii_case("apikey"))
    }

    pub fn record_legacy_fallback(&self) {
        self.legacy_fallbacks_total.fetch_add(1, Ordering::Relaxed);
    }

    /// Select the least-inflight lane, expanding only after sustained pressure.
    pub async fn acquire(
        self: &Arc<Self>,
        request: LaneRequest<'_>,
    ) -> anyhow::Result<LaneSelection> {
        if !self.config.enabled {
            anyhow::bail!("http lane pool disabled")
        }
        let account_id = request
            .account_id
            .filter(|account_id| *account_id > 0)
            .ok_or_else(|| anyhow::anyhow!("http lane pool requires a positive account id"))?;
        let key = LaneKey {
            account_id,
            proxy: canonical_proxy(request.proxy_url)?,
            origin: normalize_origin(request.origin_url)?,
            lane: normalize_lane(request.lane),
        };
        match self.acquire_step(&key)? {
            AcquireStep::Ready(selection) => Ok(selection),
        }
    }

    fn acquire_step(self: &Arc<Self>, key: &LaneKey) -> anyhow::Result<AcquireStep> {
        let now = now_millis();
        let mut groups = self
            .groups
            .lock()
            .map_err(|_| anyhow::anyhow!("http lane pool lock poisoned"))?;

        // Keep the check and insertion under one mutex. This prevents two
        // concurrent first requests from replacing an active group.
        if !groups.contains_key(key) {
            let max_keys = self.config.max_keys.max(1);
            if groups.len() >= max_keys {
                self.reap_idle_groups_for_key_capacity(&mut groups);
                if groups.len() >= max_keys {
                    self.capacity_exhaustions_total
                        .fetch_add(1, Ordering::Relaxed);
                    return Err(LanePoolCapacityError.into());
                }
            }
            let max_total_lanes = self.config.max_total_lanes.max(1);
            if self.total_lanes.load(Ordering::Acquire) >= max_total_lanes {
                self.reap_idle_lanes_for_capacity(&mut groups);
            }
            if self.total_lanes.load(Ordering::Acquire) >= max_total_lanes {
                self.capacity_exhaustions_total
                    .fetch_add(1, Ordering::Relaxed);
                return Err(LanePoolCapacityError.into());
            }
            let client = (self.client_factory)(&key.proxy, self.config.max_idle_per_host)?;
            let lane = Arc::new(LaneState::new(client, now));
            let group_epoch = self.next_group_epoch.fetch_add(1, Ordering::Relaxed);
            self.total_lanes.fetch_add(1, Ordering::Relaxed);
            groups.insert(
                key.clone(),
                LaneGroup {
                    key: key.clone(),
                    group_epoch,
                    lanes: vec![lane],
                    pressure_since_ms: None,
                    last_expand_ms: None,
                    expansion_scheduled: false,
                    expansion_generation: 0,
                    last_used_ms: Arc::new(AtomicU64::new(now)),
                    h1_observed: Arc::new(AtomicBool::new(false)),
                    h2_observed: Arc::new(AtomicBool::new(false)),
                    overflow_active: Arc::new(AtomicU64::new(0)),
                    round_robin: 0,
                },
            );
            debug!(
                account_id = key.account_id,
                origin = %key.origin,
                lane = %key.lane,
                proxy_fp = %self.proxy_fingerprint(&key.proxy),
                "created upstream HTTP lane"
            );
        }

        let should_reap_for_expansion = self.total_lanes.load(Ordering::Acquire)
            >= self.config.max_total_lanes.max(1)
            && groups
                .get(key)
                .is_some_and(|group| group.is_under_pressure(&self.config));
        if should_reap_for_expansion {
            self.reap_idle_lanes_for_capacity(&mut groups);
        }

        let group = groups
            .get_mut(key)
            .expect("lane group inserted or already present");
        if group.has_h1() {
            return Err(LanePoolLegacyFallbackError.into());
        }
        let under_pressure = group.is_under_pressure(&self.config);
        if under_pressure {
            let pressure_since = *group.pressure_since_ms.get_or_insert(now);
            let can_expand = group.lanes.len() < group.protocol_max_lanes(&self.config)
                && self.total_lanes.load(Ordering::Acquire) < self.config.max_total_lanes.max(1);
            if can_expand {
                let pressure_wait = self
                    .config
                    .pressure_duration
                    .as_millis()
                    .saturating_sub(now.saturating_sub(pressure_since) as u128);
                let cooldown_wait = group
                    .last_expand_ms
                    .map(|last_expand_ms| {
                        self.config
                            .expansion_cooldown
                            .as_millis()
                            .saturating_sub(now.saturating_sub(last_expand_ms) as u128)
                    })
                    .unwrap_or(0);
                let wait_ms = pressure_wait.max(cooldown_wait);
                if wait_ms > 0 {
                    // Expansion is scheduled off the request path. Waiting here
                    // would add the pressure timer/cooldown directly to TTFT and
                    // could fire a configured first-token placeholder before the
                    // request has even been sent.
                    if !group.expansion_scheduled {
                        let generation = group.begin_expansion_schedule();
                        self.schedule_expansion(
                            key.clone(),
                            group.group_epoch,
                            Duration::from_millis(wait_ms as u64),
                            generation,
                        );
                    }
                } else {
                    self.expand_group_locked(group, now);
                }
            }
        } else {
            group.pressure_since_ms = None;
            group.cancel_expansion_schedule();
        }

        let protocol_max = group.protocol_max_lanes(&self.config);
        // `high_water_inflight` starts adaptive expansion. `target_inflight`
        // is the softer per-lane capacity used for overflow accounting, so a
        // pool can be under pressure while it is still adding lanes without
        // falsely reporting an overflow signal to the autoscaler.
        let target = self
            .config
            .target_inflight
            .max(self.config.high_water_inflight);
        let all_at_target = group
            .lanes
            .iter()
            .all(|lane| lane.inflight.load(Ordering::Relaxed) >= target);
        let at_global_cap =
            self.total_lanes.load(Ordering::Acquire) >= self.config.max_total_lanes.max(1);
        let overflow = all_at_target && (group.lanes.len() >= protocol_max || at_global_cap);
        let lane = group.choose_lane();
        lane.inflight.fetch_add(1, Ordering::Relaxed);
        lane.awaiting_headers.fetch_add(1, Ordering::Relaxed);
        lane.touch();
        group.last_used_ms.store(now, Ordering::Relaxed);
        let overflow_active = if overflow {
            self.overflow_total.fetch_add(1, Ordering::Relaxed);
            group.overflow_active.fetch_add(1, Ordering::Relaxed);
            Some(Arc::clone(&group.overflow_active))
        } else {
            None
        };

        // The request itself may be the first one that is waiting for
        // response headers. Observe that transition after accounting it, but
        // defer the actual check to a background task so a caller that receives
        // headers immediately can clear the heuristic before a new lane is
        // built. This also means a single queued request does not depend on a
        // later acquire to start expansion.
        if group.is_under_pressure(&self.config) {
            let pressure_since = *group.pressure_since_ms.get_or_insert(now);
            let can_expand = group.lanes.len() < group.protocol_max_lanes(&self.config)
                && self.total_lanes.load(Ordering::Acquire) < self.config.max_total_lanes.max(1);
            if can_expand && !group.expansion_scheduled {
                let pressure_wait = self
                    .config
                    .pressure_duration
                    .as_millis()
                    .saturating_sub(now.saturating_sub(pressure_since) as u128);
                let cooldown_wait = group
                    .last_expand_ms
                    .map(|last_expand_ms| {
                        self.config
                            .expansion_cooldown
                            .as_millis()
                            .saturating_sub(now.saturating_sub(last_expand_ms) as u128)
                    })
                    .unwrap_or(0);
                let generation = group.begin_expansion_schedule();
                self.schedule_expansion(
                    key.clone(),
                    group.group_epoch,
                    Duration::from_millis(pressure_wait.max(cooldown_wait) as u64),
                    generation,
                );
            }
        }
        Ok(AcquireStep::Ready(LaneSelection {
            client: lane.client.clone(),
            guard: LaneGuard {
                lane: Some(lane),
                group_h1_observed: Some(Arc::clone(&group.h1_observed)),
                group_h2_observed: Some(Arc::clone(&group.h2_observed)),
                group_last_used_ms: Some(Arc::clone(&group.last_used_ms)),
                overflow_active,
                released: false,
                headers_marked: false,
                stream_marked: false,
            },
        }))
    }

    fn schedule_expansion(
        self: &Arc<Self>,
        key: LaneKey,
        group_epoch: u64,
        delay: Duration,
        generation: u64,
    ) {
        let pool = Arc::clone(self);
        let wait_guard = LaneExpansionDelayGuard::new(self);
        tokio::spawn(async move {
            tokio::time::sleep(delay).await;
            drop(wait_guard);
            pool.expand_after_pressure(&key, group_epoch, generation);
        });
    }

    fn expand_group_locked(&self, group: &mut LaneGroup, now: u64) {
        group.cancel_expansion_schedule();
        match (self.client_factory)(&group.key.proxy, self.config.max_idle_per_host) {
            Ok(client) => {
                group.lanes.push(Arc::new(LaneState::new(client, now)));
                self.total_lanes.fetch_add(1, Ordering::Relaxed);
                self.expansions_total.fetch_add(1, Ordering::Relaxed);
                group.last_expand_ms = Some(now);
                group.pressure_since_ms = None;
                debug!(
                    account_id = group.key.account_id,
                    origin = %group.key.origin,
                    lane = %group.key.lane,
                    lanes = group.lanes.len(),
                    target_inflight = self.config.target_inflight,
                    proxy_fp = %self.proxy_fingerprint(&group.key.proxy),
                    "expanded upstream HTTP lane group"
                );
            }
            Err(_) => {
                self.expansion_failures_total
                    .fetch_add(1, Ordering::Relaxed);
                group.last_expand_ms = Some(now);
                group.pressure_since_ms = None;
                debug!(
                    account_id = group.key.account_id,
                    origin = %group.key.origin,
                    lane = %group.key.lane,
                    proxy_fp = %self.proxy_fingerprint(&group.key.proxy),
                    "could not expand upstream HTTP lane group; using existing lane"
                );
            }
        }
    }

    fn expand_after_pressure(self: &Arc<Self>, key: &LaneKey, group_epoch: u64, generation: u64) {
        let now = now_millis();
        let mut reschedule = None;
        {
            let Ok(mut groups) = self.groups.lock() else {
                return;
            };
            let Some(group) = groups.get_mut(key) else {
                return;
            };
            if group.group_epoch != group_epoch {
                return;
            }
            if !group.expansion_schedule_matches(generation) {
                return;
            }
            group.cancel_expansion_schedule();
            if group.has_h1() || !group.is_under_pressure(&self.config) {
                group.pressure_since_ms = None;
            } else {
                let pressure_since = *group.pressure_since_ms.get_or_insert(now);
                let can_expand = group.lanes.len() < group.protocol_max_lanes(&self.config)
                    && self.total_lanes.load(Ordering::Acquire)
                        < self.config.max_total_lanes.max(1);
                if can_expand {
                    let pressure_wait = self
                        .config
                        .pressure_duration
                        .as_millis()
                        .saturating_sub(now.saturating_sub(pressure_since) as u128);
                    let cooldown_wait = group
                        .last_expand_ms
                        .map(|last_expand_ms| {
                            self.config
                                .expansion_cooldown
                                .as_millis()
                                .saturating_sub(now.saturating_sub(last_expand_ms) as u128)
                        })
                        .unwrap_or(0);
                    let wait_ms = pressure_wait.max(cooldown_wait);
                    if wait_ms > 0 {
                        let next_generation = group.begin_expansion_schedule();
                        reschedule = Some((
                            Duration::from_millis(wait_ms as u64),
                            group.group_epoch,
                            next_generation,
                        ));
                    } else {
                        self.expand_group_locked(group, now);
                    }
                }
            }
        }
        if let Some((delay, group_epoch, next_generation)) = reschedule {
            self.schedule_expansion(key.clone(), group_epoch, delay, next_generation);
        }
    }

    /// Remove idle extra lanes and then idle groups. Called by the existing
    /// one-minute resource reaper; no network request is issued here.
    pub fn reap(&self) {
        if !self.config.enabled {
            return;
        }
        let now = now_millis();
        let mut groups = match self.groups.lock() {
            Ok(groups) => groups,
            Err(_) => return,
        };
        let mut remove_keys = Vec::new();
        for (key, group) in groups.iter_mut() {
            while group.lanes.len() > 1 {
                let Some(index) = group.lanes.iter().position(|lane| {
                    lane.is_idle()
                        && now.saturating_sub(lane.last_used_ms.load(Ordering::Relaxed))
                            >= self.config.idle_lane_ttl.as_millis() as u64
                }) else {
                    break;
                };
                group.lanes.remove(index);
                self.total_lanes.fetch_sub(1, Ordering::Relaxed);
                self.shrinks_total.fetch_add(1, Ordering::Relaxed);
            }
            // Once HTTP/1 is observed this group can never serve another lane
            // request: acquire() deliberately returns to reqwest's legacy
            // multi-connection pool. Do not retain that unusable Client/key for
            // the full pool TTL and let many H1 accounts crowd out H2 groups.
            let idle_ttl = if group.has_h1() {
                self.config.idle_lane_ttl
            } else {
                self.config.idle_pool_ttl
            };
            let group_idle = group.lanes.iter().all(|lane| lane.is_idle())
                && now.saturating_sub(group.last_used_ms.load(Ordering::Relaxed))
                    >= idle_ttl.as_millis() as u64;
            if group_idle {
                remove_keys.push(key.clone());
            }
        }
        for key in remove_keys {
            if let Some(group) = groups.remove(&key) {
                let removed = group.lanes.len();
                self.total_lanes.fetch_sub(removed, Ordering::Relaxed);
                self.shrinks_total
                    .fetch_add(removed as u64, Ordering::Relaxed);
            }
        }
    }

    fn reap_idle_groups_for_key_capacity(&self, groups: &mut HashMap<LaneKey, LaneGroup>) {
        let now = now_millis();
        while groups.len() >= self.config.max_keys.max(1) && !groups.is_empty() {
            let Some(key) = groups
                .iter()
                .filter(|(_, group)| {
                    group.lanes.iter().all(|lane| lane.is_idle())
                        && (group.has_h1()
                            || now.saturating_sub(group.last_used_ms.load(Ordering::Relaxed))
                                >= self.config.idle_pool_ttl.as_millis() as u64)
                })
                .min_by_key(|(_, group)| group.last_used_ms.load(Ordering::Relaxed))
                .map(|(key, _)| key.clone())
            else {
                break;
            };
            if let Some(group) = groups.remove(&key) {
                let removed = group.lanes.len();
                self.total_lanes.fetch_sub(removed, Ordering::Relaxed);
                self.shrinks_total
                    .fetch_add(removed as u64, Ordering::Relaxed);
            }
        }
    }

    fn reap_idle_lanes_for_capacity(&self, groups: &mut HashMap<LaneKey, LaneGroup>) {
        let now = now_millis();
        while self.total_lanes.load(Ordering::Acquire) >= self.config.max_total_lanes.max(1) {
            let candidate = groups
                .iter()
                .flat_map(|(key, group)| {
                    group
                        .lanes
                        .iter()
                        .enumerate()
                        .filter(|(_, lane)| {
                            // Keep one lane for every live group, but there
                            // is no permanent "base" index: an expanded
                            // lane may be the only active one.
                            group.lanes.len() > 1
                                && lane.is_idle()
                                && now.saturating_sub(lane.last_used_ms.load(Ordering::Relaxed))
                                    >= self.config.idle_lane_ttl.as_millis() as u64
                        })
                        .map(move |(index, lane)| {
                            (
                                key.clone(),
                                index,
                                lane.last_used_ms.load(Ordering::Relaxed),
                            )
                        })
                })
                .min_by_key(|(_, _, last_used_ms)| *last_used_ms);
            if let Some((key, index, _)) = candidate {
                if let Some(group) = groups.get_mut(&key) {
                    group.lanes.remove(index);
                    self.total_lanes.fetch_sub(1, Ordering::Relaxed);
                    self.shrinks_total.fetch_add(1, Ordering::Relaxed);
                    continue;
                }
            }

            let candidate = groups
                .iter()
                .filter(|(_, group)| {
                    group.lanes.iter().all(|lane| lane.is_idle())
                        && (group.has_h1()
                            || now.saturating_sub(group.last_used_ms.load(Ordering::Relaxed))
                                >= self.config.idle_pool_ttl.as_millis() as u64)
                })
                .min_by_key(|(_, group)| group.last_used_ms.load(Ordering::Relaxed))
                .map(|(key, _)| key.clone());
            let Some(key) = candidate else {
                break;
            };
            if let Some(group) = groups.remove(&key) {
                let removed = group.lanes.len();
                self.total_lanes.fetch_sub(removed, Ordering::Relaxed);
                self.shrinks_total
                    .fetch_add(removed as u64, Ordering::Relaxed);
            }
        }
    }

    pub fn snapshot(&self) -> HttpLanePoolSnapshot {
        self.snapshot_at(now_millis())
    }

    fn snapshot_at(&self, now: u64) -> HttpLanePoolSnapshot {
        let mut snapshot = HttpLanePoolSnapshot {
            enabled: self.config.enabled,
            expansions_total: self.expansions_total.load(Ordering::Relaxed),
            shrinks_total: self.shrinks_total.load(Ordering::Relaxed),
            expansion_failures_total: self.expansion_failures_total.load(Ordering::Relaxed),
            capacity_exhaustions_total: self.capacity_exhaustions_total.load(Ordering::Relaxed),
            legacy_fallbacks_total: self.legacy_fallbacks_total.load(Ordering::Relaxed),
            overflow_total: self.overflow_total.load(Ordering::Relaxed),
            expansion_waiters: self.expansion_waiters.load(Ordering::Relaxed),
            expansion_delay_micros_total: self.expansion_delay_micros_total.load(Ordering::Relaxed),
            expansion_delay_count: self.expansion_delay_count.load(Ordering::Relaxed),
            ..Default::default()
        };
        let Ok(groups) = self.groups.lock() else {
            return snapshot;
        };
        snapshot.keys = groups.len();
        let at_global_lane_cap =
            self.total_lanes.load(Ordering::Acquire) >= self.config.max_total_lanes.max(1);
        for group in groups.values() {
            snapshot.lanes += group.lanes.len();
            if group.is_under_pressure(&self.config) {
                snapshot.pools_under_pressure += 1;
            }
            let at_protocol_lane_cap = group.lanes.len() >= group.protocol_max_lanes(&self.config);
            if (at_protocol_lane_cap || at_global_lane_cap)
                && group.has_sustained_pressure(&self.config, now)
            {
                snapshot.pools_at_cap_pressure += 1;
            }
            let overflow_active = group.overflow_active.load(Ordering::Relaxed);
            snapshot.overflow_active += overflow_active;
            if !group.has_h1() && overflow_active > 0 {
                snapshot.pools_overflowing += 1;
            }
            for lane in &group.lanes {
                snapshot.inflight += lane.inflight.load(Ordering::Relaxed);
                snapshot.awaiting_headers += lane.awaiting_headers.load(Ordering::Relaxed);
                snapshot.open_streams += lane.open_streams.load(Ordering::Relaxed);
                match lane.version.load(Ordering::Relaxed) {
                    HTTP_VERSION_1 => snapshot.http1_lanes += 1,
                    HTTP_VERSION_2 => snapshot.http2_lanes += 1,
                    _ => snapshot.unknown_lanes += 1,
                }
            }
        }
        snapshot
    }

    fn proxy_fingerprint(&self, proxy: &str) -> String {
        let digest = hmac::sign(&self.proxy_fingerprint_key, proxy.as_bytes());
        digest.as_ref()[..16]
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect()
    }
}

struct LaneExpansionDelayGuard {
    pool: Arc<HttpLanePool>,
    started_at: Instant,
}

impl LaneExpansionDelayGuard {
    fn new(pool: &Arc<HttpLanePool>) -> Self {
        pool.expansion_waiters.fetch_add(1, Ordering::Relaxed);
        Self {
            pool: Arc::clone(pool),
            started_at: Instant::now(),
        }
    }
}

impl Drop for LaneExpansionDelayGuard {
    fn drop(&mut self) {
        self.pool.expansion_waiters.fetch_sub(1, Ordering::Relaxed);
        self.pool.expansion_delay_micros_total.fetch_add(
            self.started_at.elapsed().as_micros().min(u64::MAX as u128) as u64,
            Ordering::Relaxed,
        );
        self.pool
            .expansion_delay_count
            .fetch_add(1, Ordering::Relaxed);
    }
}

pub fn build_standalone_client(
    proxy_url: Option<&str>,
    max_idle_per_host: usize,
) -> anyhow::Result<Client> {
    let proxy = canonical_proxy(proxy_url)?;
    build_client(&proxy, max_idle_per_host)
}

fn build_client(proxy: &str, max_idle_per_host: usize) -> anyhow::Result<Client> {
    let mut builder = Client::builder()
        .tcp_nodelay(true)
        .http2_adaptive_window(true)
        .http2_keep_alive_interval(Duration::from_secs(20))
        .http2_keep_alive_timeout(Duration::from_secs(5))
        .http2_keep_alive_while_idle(true)
        .pool_idle_timeout(Duration::from_secs(300))
        .pool_max_idle_per_host(max_idle_per_host.max(1));
    if !proxy.is_empty() {
        let proxy = reqwest::Proxy::all(proxy)
            .map_err(|_| anyhow::anyhow!("invalid upstream proxy configuration"))?;
        builder = builder.proxy(proxy);
    }
    builder
        .build()
        .map_err(|_| anyhow::anyhow!("could not build upstream HTTP client"))
}

fn canonical_proxy(proxy: Option<&str>) -> anyhow::Result<String> {
    let Some(proxy) = proxy.map(str::trim).filter(|proxy| !proxy.is_empty()) else {
        return Ok(String::new());
    };
    Url::parse(proxy)
        .map(|url| url.to_string())
        .map_err(|_| anyhow::anyhow!("invalid upstream proxy URL"))
}

fn normalize_origin(origin: &str) -> anyhow::Result<String> {
    let url = Url::parse(origin.trim())?;
    let scheme = url.scheme();
    let host = url
        .host_str()
        .ok_or_else(|| anyhow::anyhow!("upstream origin has no host"))?;
    let host = if host.contains(':') && !host.starts_with('[') {
        format!("[{host}]")
    } else {
        host.to_string()
    };
    let mut normalized = format!("{scheme}://{host}");
    if let Some(port) = url.port_or_known_default() {
        normalized.push(':');
        normalized.push_str(&port.to_string());
    }
    Ok(normalized)
}

fn normalize_lane(lane: Option<&str>) -> String {
    match lane
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase()
        .as_str()
    {
        "priority" => "priority".to_string(),
        "bulk" => "bulk".to_string(),
        _ => "normal".to_string(),
    }
}

fn is_http1(version: Version) -> bool {
    version == Version::HTTP_11 || version == Version::HTTP_10 || version == Version::HTTP_09
}

fn decrement(value: &AtomicU64) {
    let _ = value.fetch_update(Ordering::AcqRel, Ordering::Acquire, |current| {
        current.checked_sub(1)
    });
}

fn now_millis() -> u64 {
    static EPOCH: std::sync::OnceLock<Instant> = std::sync::OnceLock::new();
    EPOCH
        .get_or_init(Instant::now)
        .elapsed()
        .as_millis()
        .min(u64::MAX as u128) as u64
}

fn env_usize(key: &str, default_value: usize) -> usize {
    std::env::var(key)
        .ok()
        .and_then(|value| value.parse().ok())
        .unwrap_or(default_value)
}

fn env_u64(key: &str, default_value: u64) -> u64 {
    std::env::var(key)
        .ok()
        .and_then(|value| value.parse().ok())
        .unwrap_or(default_value)
}

fn env_bool(key: &str, default_value: bool) -> bool {
    std::env::var(key)
        .ok()
        .map(|value| {
            matches!(
                value.to_ascii_lowercase().as_str(),
                "1" | "true" | "yes" | "on"
            )
        })
        .unwrap_or(default_value)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn assert_send<T: Send>(_: T) {}

    async fn wait_for_lanes(pool: &Arc<HttpLanePool>, expected: usize) {
        tokio::time::timeout(Duration::from_millis(250), async {
            loop {
                if pool.snapshot().lanes >= expected {
                    return;
                }
                tokio::time::sleep(Duration::from_millis(1)).await;
            }
        })
        .await
        .expect("lane expansion completed");
    }

    #[test]
    fn canonical_keys_separate_proxy_origin_and_lane() {
        let proxy = canonical_proxy(Some("HTTP://User:Pass@Example.COM:80")).unwrap();
        assert!(proxy.starts_with("http://User:Pass@example.com"));
        assert_eq!(
            canonical_proxy(Some("http://example.com:80")).unwrap(),
            canonical_proxy(Some("http://example.com")).unwrap()
        );
        assert_eq!(
            normalize_origin("HTTPS://Example.COM:443/v1/responses").unwrap(),
            "https://example.com:443"
        );
        assert_eq!(
            normalize_origin("https://example.com/v1/responses").unwrap(),
            "https://example.com:443"
        );
        assert_eq!(
            normalize_origin("https://[2001:db8::1]/v1/responses").unwrap(),
            "https://[2001:db8::1]:443"
        );
        assert_eq!(normalize_lane(Some("PRIORITY")), "priority");
        assert_eq!(normalize_lane(Some("unknown")), "normal");
    }

    #[test]
    fn proxy_fingerprint_is_process_keyed_stable_and_redacted() {
        let pool = HttpLanePool::new(HttpLanePoolConfig::default());
        let proxy = "http://user:secret@example.com:8080";
        let fingerprint = pool.proxy_fingerprint(proxy);
        assert_eq!(fingerprint.len(), 32);
        assert!(fingerprint.bytes().all(|byte| byte.is_ascii_hexdigit()));
        assert_eq!(fingerprint, pool.proxy_fingerprint(proxy));
        assert_ne!(
            fingerprint,
            pool.proxy_fingerprint("http://user:other@example.com:8080")
        );
        assert!(!fingerprint.contains("secret"));
        assert!(!fingerprint.contains("example.com"));
    }

    #[test]
    fn disabled_pool_is_explicitly_disabled() {
        let pool = HttpLanePool::new(HttpLanePoolConfig {
            enabled: false,
            max_keys: 1,
            max_total_lanes: 1,
            max_idle_per_host: 1,
            ..Default::default()
        });
        assert!(!pool.enabled());
        assert!(!pool.snapshot().enabled);
    }

    #[test]
    fn default_pool_is_enabled_for_first_deployment() {
        let config = HttpLanePoolConfig::default();
        assert!(config.enabled);
        assert_eq!(config.max_idle_per_host, 1);
    }

    #[test]
    fn only_explicit_api_key_plans_enter_the_pool() {
        let pool = HttpLanePool::new(HttpLanePoolConfig::default());
        let request = LaneRequest {
            account_id: Some(1),
            proxy_url: None,
            origin_url: "https://example.com/v1/responses",
            lane: None,
        };
        assert!(pool.can_pool_for_account_type(request, Some("apikey")));
        assert!(pool.can_pool_for_account_type(request, Some("APIKEY")));
        assert!(pool.can_pool_for_account_type(request, Some(" apikey ")));
        assert!(!pool.can_pool_for_account_type(request, Some("oauth")));
        assert!(!pool.can_pool_for_account_type(request, None));
    }

    #[test]
    fn non_positive_account_ids_never_enter_the_pool() {
        let pool = HttpLanePool::new(HttpLanePoolConfig::default());
        for account_id in [Some(0), Some(-1), None] {
            assert!(!pool.can_pool(LaneRequest {
                account_id,
                proxy_url: None,
                origin_url: "https://example.com/v1/responses",
                lane: None,
            }));
        }
    }

    #[tokio::test]
    async fn acquire_rejects_non_positive_account_ids() {
        let pool = HttpLanePool::new(HttpLanePoolConfig::default());
        for account_id in [Some(0), Some(-1), None] {
            let error = match pool
                .acquire(LaneRequest {
                    account_id,
                    proxy_url: None,
                    origin_url: "https://example.com/v1/responses",
                    lane: None,
                })
                .await
            {
                Ok(_) => panic!("invalid account IDs must not create a lane"),
                Err(error) => error,
            };
            assert!(error.to_string().contains("positive account id"));
        }
        assert_eq!(pool.snapshot().keys, 0);
        assert_eq!(pool.snapshot().lanes, 0);
    }

    #[test]
    fn acquire_future_is_send() {
        let pool = HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            ..Default::default()
        });
        assert_send(pool.acquire(LaneRequest {
            account_id: Some(1),
            proxy_url: None,
            origin_url: "https://example.com/v1/responses",
            lane: None,
        }));
    }

    #[tokio::test]
    async fn guard_releases_header_and_stream_counters_once() {
        let pool = Arc::new(HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            max_keys: 2,
            max_total_lanes: 2,
            max_idle_per_host: 1,
            ..Default::default()
        }));
        let mut selection = pool
            .acquire(LaneRequest {
                account_id: Some(7),
                proxy_url: None,
                origin_url: "https://example.com/v1/responses",
                lane: Some("normal"),
            })
            .await
            .unwrap();
        assert_eq!(pool.snapshot().inflight, 1);
        assert_eq!(pool.snapshot().awaiting_headers, 1);
        selection.guard.mark_headers(Some(Version::HTTP_2));
        selection.guard.mark_stream_open();
        assert_eq!(pool.snapshot().awaiting_headers, 0);
        assert_eq!(pool.snapshot().open_streams, 1);
        selection.guard.release();
        selection.guard.release();
        assert_eq!(pool.snapshot().inflight, 0);
        assert_eq!(pool.snapshot().open_streams, 0);
    }

    #[tokio::test]
    async fn h1_group_falls_back_to_legacy_client() {
        let pool = Arc::new(HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            max_keys: 2,
            max_total_lanes: 4,
            max_idle_per_host: 1,
            high_water_inflight: 1,
            target_inflight: 1,
            pressure_duration: Duration::from_millis(1),
            ..Default::default()
        }));
        let mut first = pool
            .acquire(LaneRequest {
                account_id: Some(8),
                proxy_url: None,
                origin_url: "https://example.com/v1/responses",
                lane: None,
            })
            .await
            .unwrap();
        first.guard.mark_headers(Some(Version::HTTP_11));
        let error = match pool
            .acquire(LaneRequest {
                account_id: Some(8),
                proxy_url: None,
                origin_url: "https://example.com/v1/responses",
                lane: None,
            })
            .await
        {
            Ok(_) => panic!("HTTP/1 lane group must return to the legacy client"),
            Err(error) => error,
        };
        assert!(is_legacy_fallback_error(&error));
        assert_eq!(pool.snapshot().lanes, 1);
        first.guard.release();
    }

    #[tokio::test]
    async fn idle_h1_group_does_not_block_a_new_h2_pool_key() {
        let pool = HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            max_keys: 1,
            max_total_lanes: 1,
            idle_lane_ttl: Duration::from_secs(300),
            idle_pool_ttl: Duration::from_secs(1200),
            ..Default::default()
        });
        let mut h1 = pool
            .acquire(LaneRequest {
                account_id: Some(80),
                proxy_url: None,
                origin_url: "https://h1.example/v1/responses",
                lane: None,
            })
            .await
            .unwrap();
        h1.guard.mark_headers(Some(Version::HTTP_11));
        h1.guard.release();

        let h2_candidate = pool
            .acquire(LaneRequest {
                account_id: Some(81),
                proxy_url: None,
                origin_url: "https://h2.example/v1/responses",
                lane: None,
            })
            .await
            .expect("idle H1 key must be reclaimed under capacity pressure");
        let snapshot = pool.snapshot();
        assert_eq!(snapshot.keys, 1);
        assert_eq!(snapshot.lanes, 1);
        drop(h2_candidate);
    }

    #[tokio::test]
    async fn sustained_unknown_pressure_expands_to_second_lane() {
        let pool = Arc::new(HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            high_water_inflight: 1,
            target_inflight: 2,
            pressure_duration: Duration::from_millis(1),
            max_unknown_lanes: 2,
            max_h2_lanes: 4,
            ..Default::default()
        }));
        let first = pool
            .acquire(LaneRequest {
                account_id: Some(9),
                proxy_url: None,
                origin_url: "https://example.com/v1/responses",
                lane: None,
            })
            .await
            .unwrap();
        let second = pool
            .acquire(LaneRequest {
                account_id: Some(9),
                proxy_url: None,
                origin_url: "https://example.com/v1/responses",
                lane: None,
            })
            .await
            .unwrap();
        wait_for_lanes(&pool, 2).await;
        assert_eq!(pool.snapshot().lanes, 2);
        // Depending on scheduler timing, the pressure interval may elapse
        // before the next acquire and expand synchronously; lane creation is
        // the invariant, while the background-wait counter is diagnostic.
        assert_eq!(pool.snapshot().expansions_total, 1);
        drop(first);
        drop(second);
    }

    #[tokio::test]
    async fn scheduled_lane_expansion_releases_waiter_metric() {
        let pool = Arc::new(HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            high_water_inflight: 1,
            target_inflight: 1,
            pressure_duration: Duration::from_millis(1),
            max_unknown_lanes: 2,
            ..Default::default()
        }));
        let request = LaneRequest {
            account_id: Some(91),
            proxy_url: None,
            origin_url: "https://example.com/v1/responses",
            lane: None,
        };
        let first = pool.acquire(request).await.unwrap();
        let _second = pool.acquire(request).await.unwrap();
        tokio::time::timeout(Duration::from_millis(100), async {
            while pool.snapshot().expansion_waiters != 1 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("pressure expansion enters the background wait");
        tokio::time::timeout(Duration::from_millis(100), async {
            while pool.snapshot().expansion_waiters != 0 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("background pressure wait completes");
        assert_eq!(pool.snapshot().expansion_waiters, 0);
        assert_eq!(pool.snapshot().expansion_delay_count, 1);
        drop(first);
    }

    #[tokio::test]
    async fn stale_expansion_task_cannot_clear_current_schedule() {
        let pool = HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            high_water_inflight: 100,
            target_inflight: 100,
            max_unknown_lanes: 2,
            ..Default::default()
        });
        let request = LaneRequest {
            account_id: Some(92),
            proxy_url: None,
            origin_url: "https://example.com/v1/responses",
            lane: None,
        };
        let selection = pool.acquire(request).await.unwrap();
        let key = LaneKey {
            account_id: 92,
            proxy: canonical_proxy(None).unwrap(),
            origin: normalize_origin(request.origin_url).unwrap(),
            lane: normalize_lane(None),
        };

        let (group_epoch, stale_generation, current_generation) = {
            let mut groups = pool.groups.lock().unwrap();
            let group = groups.get_mut(&key).unwrap();
            let group_epoch = group.group_epoch;
            let stale_generation = group.begin_expansion_schedule();
            group.cancel_expansion_schedule();
            let current_generation = group.begin_expansion_schedule();
            (group_epoch, stale_generation, current_generation)
        };

        pool.expand_after_pressure(&key, group_epoch, stale_generation);
        {
            let groups = pool.groups.lock().unwrap();
            let group = groups.get(&key).unwrap();
            assert!(group.expansion_schedule_matches(current_generation));
            assert_eq!(group.lanes.len(), 1);
        }

        pool.expand_after_pressure(&key, group_epoch, current_generation);
        {
            let groups = pool.groups.lock().unwrap();
            let group = groups.get(&key).unwrap();
            assert!(!group.expansion_scheduled);
            assert_eq!(group.lanes.len(), 1);
        }
        drop(selection);
    }

    #[tokio::test]
    async fn expansion_task_from_reaped_group_cannot_touch_recreated_group() {
        let pool = HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            max_keys: 2,
            max_total_lanes: 2,
            idle_lane_ttl: Duration::ZERO,
            idle_pool_ttl: Duration::ZERO,
            ..Default::default()
        });
        let request = LaneRequest {
            account_id: Some(93),
            proxy_url: None,
            origin_url: "https://example.com/v1/responses",
            lane: None,
        };
        let selection = pool.acquire(request).await.unwrap();
        let key = LaneKey {
            account_id: 93,
            proxy: canonical_proxy(None).unwrap(),
            origin: normalize_origin(request.origin_url).unwrap(),
            lane: normalize_lane(None),
        };
        let (old_epoch, stale_generation) = {
            let mut groups = pool.groups.lock().unwrap();
            let group = groups.get_mut(&key).unwrap();
            (group.group_epoch, group.begin_expansion_schedule())
        };

        // Reaping is allowed to remove an idle group even if its delayed task
        // has not woken yet. Recreate the same key to exercise that lifetime
        // boundary explicitly.
        drop(selection);
        pool.reap();
        assert_eq!(pool.snapshot().keys, 0);
        let replacement = pool.acquire(request).await.unwrap();
        let new_epoch = pool
            .groups
            .lock()
            .unwrap()
            .get(&key)
            .expect("replacement group")
            .group_epoch;
        assert_ne!(old_epoch, new_epoch);

        pool.expand_after_pressure(&key, old_epoch, stale_generation);
        let groups = pool.groups.lock().unwrap();
        let group = groups.get(&key).expect("replacement group remains");
        assert_eq!(group.group_epoch, new_epoch);
        assert_eq!(group.lanes.len(), 1);
        assert!(!group.expansion_scheduled);
        drop(groups);
        drop(replacement);
    }

    #[tokio::test]
    async fn overflow_guard_tracks_active_lifecycle() {
        let pool = Arc::new(HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            high_water_inflight: 1,
            target_inflight: 1,
            max_unknown_lanes: 1,
            max_h2_lanes: 1,
            ..Default::default()
        }));
        let first = pool
            .acquire(LaneRequest {
                account_id: Some(10),
                proxy_url: None,
                origin_url: "https://example.com/v1/responses",
                lane: None,
            })
            .await
            .unwrap();
        let mut overflow = pool
            .acquire(LaneRequest {
                account_id: Some(10),
                proxy_url: None,
                origin_url: "https://example.com/v1/responses",
                lane: None,
            })
            .await
            .unwrap();
        assert_eq!(pool.snapshot().overflow_total, 1);
        assert_eq!(pool.snapshot().overflow_active, 1);
        assert_eq!(pool.snapshot().pools_overflowing, 1);
        overflow.guard.release();
        assert_eq!(pool.snapshot().overflow_active, 0);
        drop(first);
    }

    #[tokio::test]
    async fn sustained_pressure_at_lane_cap_is_distinct_from_soft_overflow() {
        let pool = HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            high_water_inflight: 1,
            target_inflight: 2,
            pressure_duration: Duration::from_millis(20),
            max_unknown_lanes: 1,
            max_h2_lanes: 1,
            ..Default::default()
        });
        let request = LaneRequest {
            account_id: Some(11),
            proxy_url: None,
            origin_url: "https://example.com/v1/responses",
            lane: None,
        };

        let mut selection = pool.acquire(request).await.unwrap();
        let pressure_since = pool
            .groups
            .lock()
            .unwrap()
            .values()
            .next()
            .and_then(|group| group.pressure_since_ms)
            .expect("pressure start");
        let initial = pool.snapshot_at(pressure_since + 19);
        assert_eq!(initial.pools_under_pressure, 1);
        assert_eq!(initial.pools_at_cap_pressure, 0);
        assert_eq!(initial.overflow_active, 0);

        let sustained = pool.snapshot_at(pressure_since + 20);
        assert_eq!(sustained.pools_at_cap_pressure, 1);
        assert_eq!(sustained.overflow_active, 0);

        selection.guard.mark_headers(Some(Version::HTTP_2));
        assert_eq!(pool.snapshot().pools_at_cap_pressure, 0);
    }

    #[tokio::test]
    async fn active_max_key_returns_capacity_error_without_registry_growth() {
        let pool = HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            max_keys: 1,
            max_total_lanes: 2,
            ..Default::default()
        });
        let first = pool
            .acquire(LaneRequest {
                account_id: Some(101),
                proxy_url: None,
                origin_url: "https://one.example/v1/responses",
                lane: None,
            })
            .await
            .unwrap();
        let error = match pool
            .acquire(LaneRequest {
                account_id: Some(102),
                proxy_url: None,
                origin_url: "https://two.example/v1/responses",
                lane: None,
            })
            .await
        {
            Ok(_) => panic!("active key cap must reject a new lane group"),
            Err(error) => error,
        };
        assert!(is_capacity_error(&error));
        assert_eq!(pool.snapshot().keys, 1);
        assert_eq!(pool.snapshot().capacity_exhaustions_total, 1);
        drop(first);
    }

    #[tokio::test]
    async fn total_lane_cap_rejects_active_key_but_reclaims_idle_key() {
        let pool = HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            max_keys: 2,
            max_total_lanes: 1,
            idle_lane_ttl: Duration::ZERO,
            idle_pool_ttl: Duration::ZERO,
            ..Default::default()
        });
        let first = pool
            .acquire(LaneRequest {
                account_id: Some(201),
                proxy_url: None,
                origin_url: "https://one.example/v1/responses",
                lane: None,
            })
            .await
            .unwrap();
        let request = LaneRequest {
            account_id: Some(202),
            proxy_url: None,
            origin_url: "https://two.example/v1/responses",
            lane: None,
        };
        let error = match pool.acquire(request).await {
            Ok(_) => panic!("active total lane cap must reject a new lane group"),
            Err(error) => error,
        };
        assert!(is_capacity_error(&error));
        drop(first);

        let second = pool.acquire(request).await.unwrap();
        let snapshot = pool.snapshot();
        assert_eq!(snapshot.keys, 1);
        assert_eq!(snapshot.lanes, 1);
        drop(second);
    }

    #[tokio::test]
    async fn pressured_key_reclaims_expired_idle_lane_before_expanding() {
        let pool = HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            max_keys: 2,
            max_total_lanes: 2,
            high_water_inflight: 1,
            target_inflight: 1,
            pressure_duration: Duration::ZERO,
            expansion_cooldown: Duration::ZERO,
            idle_lane_ttl: Duration::ZERO,
            idle_pool_ttl: Duration::ZERO,
            max_unknown_lanes: 2,
            ..Default::default()
        });
        let request_a = LaneRequest {
            account_id: Some(251),
            proxy_url: None,
            origin_url: "https://one.example/v1/responses",
            lane: None,
        };
        let request_b = LaneRequest {
            account_id: Some(252),
            proxy_url: None,
            origin_url: "https://two.example/v1/responses",
            lane: None,
        };
        let first = pool.acquire(request_a).await.unwrap();
        let idle = pool.acquire(request_b).await.unwrap();
        drop(idle);

        let expanded = pool.acquire(request_a).await.unwrap();
        let snapshot = pool.snapshot();
        assert_eq!(snapshot.keys, 1);
        assert_eq!(snapshot.lanes, 2);
        assert_eq!(snapshot.expansions_total, 1);
        drop(expanded);
        drop(first);
    }

    #[tokio::test]
    async fn total_cap_reclaims_idle_original_lane_when_expanded_lane_is_active() {
        let pool = HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            max_keys: 3,
            max_total_lanes: 2,
            high_water_inflight: 1,
            target_inflight: 1,
            pressure_duration: Duration::ZERO,
            expansion_cooldown: Duration::ZERO,
            idle_lane_ttl: Duration::ZERO,
            idle_pool_ttl: Duration::from_secs(3_600),
            max_unknown_lanes: 2,
            ..Default::default()
        });
        let request_a = LaneRequest {
            account_id: Some(261),
            proxy_url: None,
            origin_url: "https://one.example/v1/responses",
            lane: None,
        };
        let first = pool.acquire(request_a).await.unwrap();
        wait_for_lanes(&pool, 2).await;
        let expanded = pool.acquire(request_a).await.unwrap();
        drop(first); // The original lane is idle; the expanded lane stays active.

        let request_b = LaneRequest {
            account_id: Some(262),
            proxy_url: None,
            origin_url: "https://two.example/v1/responses",
            lane: None,
        };
        let replacement = pool
            .acquire(request_b)
            .await
            .expect("an idle lane at index zero is reclaimable");
        let snapshot = pool.snapshot();
        assert_eq!(snapshot.keys, 2);
        assert_eq!(snapshot.lanes, 2);
        drop(replacement);
        drop(expanded);
    }

    #[tokio::test]
    async fn guard_drop_releases_all_lifecycle_counters() {
        let pool = HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            high_water_inflight: 1,
            target_inflight: 1,
            max_unknown_lanes: 1,
            max_h2_lanes: 1,
            ..Default::default()
        });
        let request = LaneRequest {
            account_id: Some(301),
            proxy_url: None,
            origin_url: "https://example.com/v1/responses",
            lane: None,
        };
        let mut first = pool.acquire(request).await.unwrap();
        first.guard.mark_headers(Some(Version::HTTP_2));
        first.guard.mark_stream_open();
        let mut overflow = pool.acquire(request).await.unwrap();
        overflow.guard.mark_headers(Some(Version::HTTP_2));
        overflow.guard.mark_stream_open();
        let active = pool.snapshot();
        assert_eq!(active.inflight, 2);
        assert_eq!(active.open_streams, 2);
        assert_eq!(active.overflow_active, 1);

        drop(overflow);
        drop(first);
        let released = pool.snapshot();
        assert_eq!(released.inflight, 0);
        assert_eq!(released.awaiting_headers, 0);
        assert_eq!(released.open_streams, 0);
        assert_eq!(released.overflow_active, 0);
    }

    #[tokio::test]
    async fn stable_open_h2_streams_without_header_wait_do_not_expand() {
        let pool = HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            high_water_inflight: 24,
            target_inflight: 32,
            pressure_duration: Duration::from_millis(5),
            max_unknown_lanes: 2,
            max_h2_lanes: 4,
            ..Default::default()
        });
        let request = LaneRequest {
            account_id: Some(401),
            proxy_url: None,
            origin_url: "https://example.com/v1/responses",
            lane: None,
        };
        let mut streams = Vec::new();
        for _ in 0..24 {
            let mut selection = pool.acquire(request).await.unwrap();
            selection.guard.mark_headers(Some(Version::HTTP_2));
            selection.guard.mark_stream_open();
            streams.push(selection);
        }
        let mut next = tokio::time::timeout(Duration::from_millis(20), pool.acquire(request))
            .await
            .expect("a stable open stream must not enter the pressure timer")
            .unwrap();
        next.guard.mark_headers(Some(Version::HTTP_2));
        next.guard.mark_stream_open();
        let snapshot = pool.snapshot();
        assert_eq!(snapshot.lanes, 1);
        assert_eq!(snapshot.awaiting_headers, 0);
        assert_eq!(snapshot.pools_under_pressure, 0);
        drop(next);
        drop(streams);
    }

    #[tokio::test]
    async fn unknown_protocol_caps_at_two_lanes_then_overflows() {
        let pool = HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            high_water_inflight: 1,
            target_inflight: 1,
            pressure_duration: Duration::from_millis(1),
            expansion_cooldown: Duration::ZERO,
            max_unknown_lanes: 2,
            max_h2_lanes: 4,
            ..Default::default()
        });
        let request = LaneRequest {
            account_id: Some(501),
            proxy_url: None,
            origin_url: "https://example.com/v1/responses",
            lane: None,
        };
        let first = pool.acquire(request).await.unwrap();
        let second = pool.acquire(request).await.unwrap();
        wait_for_lanes(&pool, 2).await;
        assert_eq!(pool.snapshot().lanes, 2);
        let third = pool.acquire(request).await.unwrap();
        let overflow = pool.acquire(request).await.unwrap();
        let snapshot = pool.snapshot();
        assert_eq!(snapshot.lanes, 2);
        assert_eq!(snapshot.unknown_lanes, 2);
        // Depending on whether the background expansion runs between the
        // second and third acquire, either the third request or both the
        // third and fourth request can legitimately be over the two-lane
        // soft capacity. The signal is a count, not a boolean.
        assert!(snapshot.overflow_active >= 1);
        drop(overflow);
        drop(third);
        drop(second);
        drop(first);
    }

    #[tokio::test]
    async fn h2_protocol_expands_to_four_lanes_and_never_five() {
        let pool = HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            high_water_inflight: 1,
            target_inflight: 1,
            pressure_duration: Duration::from_millis(1),
            expansion_cooldown: Duration::ZERO,
            max_unknown_lanes: 2,
            max_h2_lanes: 4,
            ..Default::default()
        });
        let request = LaneRequest {
            account_id: Some(601),
            proxy_url: None,
            origin_url: "https://example.com/v1/responses",
            lane: None,
        };
        let mut held = Vec::new();
        let mut first = pool.acquire(request).await.unwrap();
        first.guard.mark_headers(Some(Version::HTTP_2));
        held.push(first);

        // Leave one request awaiting headers to establish sustained pressure.
        held.push(pool.acquire(request).await.unwrap());
        while pool.snapshot().lanes < 4 {
            let mut expanded = pool.acquire(request).await.unwrap();
            expanded.guard.mark_headers(Some(Version::HTTP_2));
            held.push(expanded);
        }
        let overflow = pool.acquire(request).await.unwrap();
        let snapshot = pool.snapshot();
        assert_eq!(snapshot.lanes, 4);
        assert_eq!(snapshot.http2_lanes, 4);
        assert_eq!(snapshot.overflow_active, 1);
        drop(overflow);
        drop(held);
    }

    #[tokio::test]
    async fn concurrent_pressure_builds_only_one_unknown_lane() {
        let pool = Arc::new(HttpLanePool::new(HttpLanePoolConfig {
            enabled: true,
            high_water_inflight: 1,
            target_inflight: 1,
            pressure_duration: Duration::from_millis(2),
            expansion_cooldown: Duration::ZERO,
            max_unknown_lanes: 2,
            max_h2_lanes: 4,
            ..Default::default()
        }));
        let first = pool
            .acquire(LaneRequest {
                account_id: Some(701),
                proxy_url: None,
                origin_url: "https://example.com/v1/responses",
                lane: None,
            })
            .await
            .unwrap();
        let mut tasks = Vec::new();
        for _ in 0..8 {
            let pool = Arc::clone(&pool);
            tasks.push(tokio::spawn(async move {
                pool.acquire(LaneRequest {
                    account_id: Some(701),
                    proxy_url: None,
                    origin_url: "https://example.com/v1/responses",
                    lane: None,
                })
                .await
                .unwrap()
            }));
        }
        let mut selections = Vec::new();
        for task in tasks {
            selections.push(task.await.unwrap());
        }
        wait_for_lanes(&pool, 2).await;
        assert_eq!(pool.snapshot().lanes, 2);
        assert_eq!(pool.snapshot().expansions_total, 1);
        drop(selections);
        drop(first);
    }

    #[tokio::test]
    async fn independent_lane_unblocks_real_h2_stream_limit() {
        let listener = match tokio::net::TcpListener::bind("127.0.0.1:0").await {
            Ok(listener) => listener,
            Err(error) if error.kind() == std::io::ErrorKind::PermissionDenied => {
                eprintln!("skipping loopback H2 integration test: {error}");
                return;
            }
            Err(error) => panic!("bind h2 test server: {error}"),
        };
        let address = listener.local_addr().expect("h2 test address");
        let connections = Arc::new(AtomicUsize::new(0));
        let server_connections = Arc::clone(&connections);
        let (release_tx, release_rx) = tokio::sync::watch::channel(false);
        let server = tokio::spawn(async move {
            loop {
                let Ok((socket, _)) = listener.accept().await else {
                    return;
                };
                server_connections.fetch_add(1, Ordering::Relaxed);
                let release_rx = release_rx.clone();
                tokio::spawn(async move {
                    let mut builder = h2::server::Builder::new();
                    builder.max_concurrent_streams(1);
                    let Ok(mut connection) = builder.handshake(socket).await else {
                        return;
                    };
                    while let Some(Ok((_request, mut respond))) = connection.accept().await {
                        let mut release_rx = release_rx.clone();
                        tokio::spawn(async move {
                            let response = http::Response::builder()
                                .status(http::StatusCode::OK)
                                .body(())
                                .expect("h2 response");
                            let Ok(mut body) = respond.send_response(response, false) else {
                                return;
                            };
                            while !*release_rx.borrow() {
                                if release_rx.changed().await.is_err() {
                                    return;
                                }
                            }
                            let _ = body.send_data(bytes::Bytes::new(), true);
                        });
                    }
                });
            }
        });

        let factory: Arc<ClientFactory> = Arc::new(|proxy, max_idle_per_host| {
            assert!(proxy.is_empty());
            Client::builder()
                .http2_prior_knowledge()
                .pool_max_idle_per_host(max_idle_per_host.max(1))
                .build()
                .map_err(anyhow::Error::from)
        });
        let pool = Arc::new(HttpLanePool::with_client_factory(
            HttpLanePoolConfig {
                enabled: true,
                max_keys: 1,
                max_total_lanes: 2,
                high_water_inflight: 1,
                target_inflight: 1,
                pressure_duration: Duration::from_millis(10),
                expansion_cooldown: Duration::ZERO,
                max_unknown_lanes: 2,
                max_h2_lanes: 2,
                ..Default::default()
            },
            factory,
        ));
        let url = format!("http://{address}/stream");
        let request = LaneRequest {
            account_id: Some(801),
            proxy_url: None,
            origin_url: &url,
            lane: None,
        };

        let mut first = pool.acquire(request).await.unwrap();
        let response_one =
            tokio::time::timeout(Duration::from_secs(1), first.client.post(&url).send())
                .await
                .expect("first H2 response headers")
                .unwrap();
        assert_eq!(response_one.version(), Version::HTTP_2);
        first.guard.mark_headers(Some(response_one.version()));
        first.guard.mark_stream_open();

        let second = pool.acquire(request).await.unwrap();
        let second_url = url.clone();
        let mut second_task = tokio::spawn(async move {
            let response = second.client.post(second_url).send().await;
            (response, second.guard)
        });
        assert!(
            tokio::time::timeout(Duration::from_millis(50), &mut second_task)
                .await
                .is_err(),
            "the second request on one H2 connection must wait for stream capacity"
        );

        // Expansion is deliberately scheduled off the request path after the
        // configured pressure interval. Wait for that background task before
        // asserting that a new request is placed on the independent lane.
        wait_for_lanes(&pool, 2).await;
        let mut third = pool.acquire(request).await.unwrap();
        let response_three =
            tokio::time::timeout(Duration::from_secs(1), third.client.post(&url).send())
                .await
                .expect("second lane obtains H2 response headers")
                .unwrap();
        assert_eq!(response_three.version(), Version::HTTP_2);
        third.guard.mark_headers(Some(response_three.version()));
        third.guard.mark_stream_open();
        assert_eq!(connections.load(Ordering::Relaxed), 2);
        assert_eq!(pool.snapshot().lanes, 2);

        release_tx.send(true).expect("release H2 response bodies");
        response_one.bytes().await.unwrap();
        first.guard.release();
        let (response_two, mut second_guard) =
            tokio::time::timeout(Duration::from_secs(1), second_task)
                .await
                .expect("queued H2 request resumes")
                .unwrap();
        let response_two = response_two.unwrap();
        assert_eq!(response_two.version(), Version::HTTP_2);
        second_guard.mark_headers(Some(response_two.version()));
        second_guard.mark_stream_open();
        response_two.bytes().await.unwrap();
        second_guard.release();
        response_three.bytes().await.unwrap();
        third.guard.release();
        let released = pool.snapshot();
        assert_eq!(released.inflight, 0);
        assert_eq!(released.awaiting_headers, 0);
        assert_eq!(released.open_streams, 0);
        server.abort();
    }
}
