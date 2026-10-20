package service

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultOpenAIProxyStreamFailureThreshold  = 2
	defaultOpenAIProxyStreamFailureWindow     = time.Minute
	defaultOpenAIProxyStreamQuarantineTTL     = 10 * time.Minute
	defaultOpenAIProxyStreamCircuitMaxEntries = 4096
	defaultOpenAIProxyStreamFailureCollapse   = 3 * time.Second
	openAIAccountStreamFailureWindow          = 2 * time.Minute
	openAIAccountStreamFailureThreshold       = 2
)

// OpenAIStreamFailureClass distinguishes upstream failures from downstream
// cancellation and local admission failures. Only upstream_disconnect is
// eligible for proxy quarantine.
type OpenAIStreamFailureClass string

const (
	OpenAIStreamFailureNone                  OpenAIStreamFailureClass = ""
	OpenAIStreamFailureUpstreamDisconnect    OpenAIStreamFailureClass = "upstream_disconnect"
	OpenAIStreamFailureUpstreamError         OpenAIStreamFailureClass = "upstream_error"
	OpenAIStreamFailureClientCancelled       OpenAIStreamFailureClass = "client_cancelled"
	OpenAIStreamFailureLocalCapacityRejected OpenAIStreamFailureClass = "local_capacity_rejected"
	OpenAIStreamFailureQueueTimeout          OpenAIStreamFailureClass = "queue_timeout"
	OpenAIStreamFailurePrepareFailed         OpenAIStreamFailureClass = "prepare_failed"
	OpenAIStreamFailureRetryExhausted        OpenAIStreamFailureClass = "retry_exhausted"
	OpenAIStreamFailureCompleteFailed        OpenAIStreamFailureClass = "complete_failed"
	OpenAIStreamFailureAbortFailed           OpenAIStreamFailureClass = "abort_failed"
)

type openAIProxyStreamCircuitSettings struct {
	enabled          bool
	failureThreshold int
	failureWindow    time.Duration
	quarantineTTL    time.Duration
	collapseInterval time.Duration
	maxEntries       int
}

type openAIProxyStreamCircuitEntry struct {
	failureCount  int
	windowStart   time.Time
	blockedUntil  time.Time
	lastFailureAt time.Time
	lastTouched   time.Time
}

// openAIProxyStreamCircuit is deliberately process-local, bounded and
// ephemeral. Restarting a node clears observations and cannot affect sticky
// account state stored elsewhere.
type openAIProxyStreamCircuit struct {
	mu       sync.Mutex
	settings openAIProxyStreamCircuitSettings
	entries  map[int64]openAIProxyStreamCircuitEntry
	blocked  atomic.Pointer[openAIProxyStreamCircuitSnapshot]
}

type openAIProxyStreamCircuitSnapshot struct {
	blockedUntil map[int64]time.Time
}

type openAIAccountStreamDisconnectState struct {
	count int64
	start time.Time
}

func resolveOpenAIProxyStreamCircuitSettings(s *OpenAIGatewayService) openAIProxyStreamCircuitSettings {
	settings := openAIProxyStreamCircuitSettings{
		failureThreshold: defaultOpenAIProxyStreamFailureThreshold,
		failureWindow:    defaultOpenAIProxyStreamFailureWindow,
		quarantineTTL:    defaultOpenAIProxyStreamQuarantineTTL,
		collapseInterval: defaultOpenAIProxyStreamFailureCollapse,
		maxEntries:       defaultOpenAIProxyStreamCircuitMaxEntries,
	}
	if s == nil || s.cfg == nil {
		return settings
	}
	cfg := s.cfg.Gateway.OpenAIProxyStreamCircuit
	settings.enabled = cfg.Enabled
	if cfg.FailureThreshold > 0 {
		settings.failureThreshold = cfg.FailureThreshold
	}
	if cfg.WindowSeconds > 0 {
		settings.failureWindow = time.Duration(cfg.WindowSeconds) * time.Second
	}
	if cfg.TTLSeconds > 0 {
		settings.quarantineTTL = time.Duration(cfg.TTLSeconds) * time.Second
	}
	if cfg.MaxEntries > 0 {
		settings.maxEntries = cfg.MaxEntries
	}
	return settings
}

func newOpenAIProxyStreamCircuit(settings openAIProxyStreamCircuitSettings) *openAIProxyStreamCircuit {
	if settings.failureThreshold <= 0 {
		settings.failureThreshold = defaultOpenAIProxyStreamFailureThreshold
	}
	if settings.failureWindow <= 0 {
		settings.failureWindow = defaultOpenAIProxyStreamFailureWindow
	}
	if settings.quarantineTTL <= 0 {
		settings.quarantineTTL = defaultOpenAIProxyStreamQuarantineTTL
	}
	if settings.maxEntries <= 0 {
		settings.maxEntries = defaultOpenAIProxyStreamCircuitMaxEntries
	}
	if settings.collapseInterval < 0 {
		settings.collapseInterval = 0
	}
	circuit := &openAIProxyStreamCircuit{settings: settings, entries: make(map[int64]openAIProxyStreamCircuitEntry)}
	circuit.blocked.Store(&openAIProxyStreamCircuitSnapshot{blockedUntil: map[int64]time.Time{}})
	return circuit
}

func (c *openAIProxyStreamCircuit) publishLocked() {
	blocked := make(map[int64]time.Time)
	for proxyID, entry := range c.entries {
		if !entry.blockedUntil.IsZero() {
			blocked[proxyID] = entry.blockedUntil
		}
	}
	c.blocked.Store(&openAIProxyStreamCircuitSnapshot{blockedUntil: blocked})
}

func (s *OpenAIGatewayService) getOpenAIProxyStreamCircuit() *openAIProxyStreamCircuit {
	if s == nil {
		return nil
	}
	s.openaiProxyStreamCircuitOnce.Do(func() {
		s.openaiProxyStreamCircuit = newOpenAIProxyStreamCircuit(resolveOpenAIProxyStreamCircuitSettings(s))
	})
	if s.openaiProxyStreamCircuit == nil || !s.openaiProxyStreamCircuit.settings.enabled {
		return nil
	}
	return s.openaiProxyStreamCircuit
}

func (c *openAIProxyStreamCircuit) recordFailure(proxyID int64, now time.Time) (bool, time.Time) {
	if c == nil || !c.settings.enabled || proxyID <= 0 {
		return false, time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, exists := c.entries[proxyID]
	if exists && now.Before(entry.blockedUntil) {
		entry.lastTouched = now
		c.entries[proxyID] = entry
		return false, entry.blockedUntil
	}
	if !exists {
		c.ensureCapacityLocked(now)
	}
	if entry.windowStart.IsZero() || now.Before(entry.windowStart) || now.Sub(entry.windowStart) > c.settings.failureWindow {
		entry.failureCount = 0
		entry.windowStart = now
		entry.blockedUntil = time.Time{}
		entry.lastFailureAt = time.Time{}
	}
	if c.settings.collapseInterval > 0 && !entry.lastFailureAt.IsZero() {
		elapsed := now.Sub(entry.lastFailureAt)
		if elapsed >= 0 && elapsed < c.settings.collapseInterval {
			entry.lastTouched = now
			c.entries[proxyID] = entry
			return false, time.Time{}
		}
	}
	entry.failureCount++
	entry.lastFailureAt = now
	entry.lastTouched = now
	tripped := entry.failureCount >= c.settings.failureThreshold
	if tripped {
		entry.blockedUntil = now.Add(c.settings.quarantineTTL)
	}
	c.entries[proxyID] = entry
	c.publishLocked()
	return tripped, entry.blockedUntil
}

// recordTerminal breaks a pre-trip disconnect streak, but an already-open
// circuit keeps its bounded quarantine TTL. Otherwise a slower in-flight
// success could immediately undo a quarantine triggered by concurrent calls.
// This state is circuit-local and never reports a scheduler health sample.
func (c *openAIProxyStreamCircuit) recordTerminal(proxyID int64, now time.Time) {
	if c == nil || !c.settings.enabled || proxyID <= 0 {
		return
	}
	c.mu.Lock()
	if entry, ok := c.entries[proxyID]; !ok || !now.Before(entry.blockedUntil) {
		delete(c.entries, proxyID)
	} else {
		entry.failureCount = 0
		entry.windowStart = time.Time{}
		entry.lastFailureAt = time.Time{}
		entry.lastTouched = now
		c.entries[proxyID] = entry
	}
	c.publishLocked()
	c.mu.Unlock()
}

func (c *openAIProxyStreamCircuit) isBlocked(proxyID int64, now time.Time) bool {
	if c == nil || !c.settings.enabled || proxyID <= 0 {
		return false
	}
	snapshot := c.blocked.Load()
	if snapshot == nil {
		return false
	}
	blockedUntil, ok := snapshot.blockedUntil[proxyID]
	return ok && now.Before(blockedUntil)
}

func (c *openAIProxyStreamCircuit) ensureCapacityLocked(now time.Time) {
	if len(c.entries) < c.settings.maxEntries {
		return
	}
	for proxyID, entry := range c.entries {
		stale := entry.blockedUntil.IsZero() && now.Sub(entry.lastTouched) > c.settings.failureWindow
		expired := !entry.blockedUntil.IsZero() && !now.Before(entry.blockedUntil)
		if stale || expired {
			delete(c.entries, proxyID)
		}
	}
	if len(c.entries) < c.settings.maxEntries {
		return
	}
	var oldestProxyID int64
	var oldest time.Time
	for proxyID, entry := range c.entries {
		if oldestProxyID == 0 || entry.lastTouched.Before(oldest) {
			oldestProxyID = proxyID
			oldest = entry.lastTouched
		}
	}
	if oldestProxyID > 0 {
		delete(c.entries, oldestProxyID)
	}
	c.publishLocked()
}

func openAIProxyStreamCircuitProxyID(account *Account) (int64, bool) {
	if account == nil || account.Platform != PlatformOpenAI || account.ProxyID == nil || *account.ProxyID <= 0 {
		return 0, false
	}
	return *account.ProxyID, true
}

func (s *OpenAIGatewayService) recordOpenAIProxyStreamOutcome(account *Account, class OpenAIStreamFailureClass, _ bool, terminalSuccess bool, streamErr error) {
	if account != nil && account.Platform == PlatformOpenAI {
		if !terminalSuccess && class == OpenAIStreamFailureUpstreamDisconnect &&
			(streamErr == nil || (!errors.Is(streamErr, context.Canceled) && !errors.Is(streamErr, context.DeadlineExceeded))) {
			s.recordOpenAIAccountStreamDisconnect(account)
		}
	}
	proxyID, ok := openAIProxyStreamCircuitProxyID(account)
	if !ok {
		return
	}
	circuit := s.getOpenAIProxyStreamCircuit()
	if circuit == nil {
		return
	}
	if terminalSuccess {
		circuit.recordTerminal(proxyID, time.Now())
		return
	}
	if class == OpenAIStreamFailureUpstreamError {
		circuit.recordTerminal(proxyID, time.Now())
		return
	}
	if class != OpenAIStreamFailureUpstreamDisconnect {
		return
	}
	if streamErr != nil && (errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded)) {
		return
	}
	circuit.recordFailure(proxyID, time.Now())
}

// recordOpenAIAccountStreamDisconnect temporarily removes a repeatedly
// disconnecting OpenAI account from scheduling without changing its persisted
// enabled state. The Redis counter makes the fixed failure window consistent across
// gateway replicas; the shared account cooldown is already consumed by the
// normal scheduler admission path.
func (s *OpenAIGatewayService) recordOpenAIAccountStreamDisconnect(account *Account) {
	if s == nil || account == nil || account.Platform != PlatformOpenAI || account.ID <= 0 {
		return
	}
	count := int64(1)
	if s.openaiAccountHealthRedis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		key := "openai:account_stream_disconnect:" + strconv.FormatInt(account.ID, 10)
		countValue, err := s.openaiAccountHealthRedis.Eval(ctx,
			"local n=redis.call('INCR',KEYS[1]); if n==1 then redis.call('EXPIRE',KEYS[1],ARGV[1]) end; return n",
			[]string{key}, int(openAIAccountStreamFailureWindow.Seconds())).Int64()
		if err == nil {
			count = countValue
		} else {
			slog.Warn("openai.account_stream_disconnect_counter_failed", "account_id", account.ID, "error", err)
			count = s.nextLocalOpenAIAccountStreamDisconnectCount(account.ID, time.Now())
		}
	} else {
		count = s.nextLocalOpenAIAccountStreamDisconnectCount(account.ID, time.Now())
		s.pruneLocalOpenAIAccountStreamDisconnectCounts(time.Now())
	}
	// A single disconnect is recorded as a health failure but does not block
	// the account. Repeated disconnects in the shared failure window use the established runtime block
	// duration and automatic expiry instead of introducing a second cooldown
	// ladder that can keep accounts sidelined for increasingly long periods.
	if count == openAIAccountStreamFailureThreshold {
		s.blockOpenAIAccountTransportFailure(account, "upstream_stream_disconnect", "upstream stream disconnected repeatedly")
	}
}

// Reuse the existing short runtime block and status presentation. Repository
// updates and the Redis cooldown are monotonic, so a transport fault cannot
// shorten a longer rate limit, operator restriction or existing cooldown.
func (s *OpenAIGatewayService) blockOpenAIAccountTransportFailure(account *Account, source, reason string) {
	if s == nil || account == nil || account.ID <= 0 || account.Platform != PlatformOpenAI {
		return
	}
	until := time.Now().Add(openAIStopSchedulingBridgeCooldown)
	s.BlockAccountScheduling(account, until, source)
	if s.accountRepo != nil {
		ctx, cancel := context.WithTimeout(context.Background(), openAIAccountStateUpdateTimeout)
		defer cancel()
		if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
			slog.Warn("openai.account_transport_unschedulable_write_failed", "account_id", account.ID, "error", err)
		}
	}
}

func (s *OpenAIGatewayService) nextLocalOpenAIAccountStreamDisconnectCount(accountID int64, now time.Time) int64 {
	for {
		value, loaded := s.openaiAccountStreamDisconnectCount.Load(accountID)
		state, _ := value.(openAIAccountStreamDisconnectState)
		if !loaded || state.start.IsZero() || now.Before(state.start) || now.Sub(state.start) >= openAIAccountStreamFailureWindow {
			state = openAIAccountStreamDisconnectState{count: 1, start: now}
		} else {
			state.count++
		}
		if loaded {
			if s.openaiAccountStreamDisconnectCount.CompareAndSwap(accountID, value, state) {
				return state.count
			}
		} else if _, loaded = s.openaiAccountStreamDisconnectCount.LoadOrStore(accountID, state); !loaded {
			return state.count
		}
	}
}

func (s *OpenAIGatewayService) pruneLocalOpenAIAccountStreamDisconnectCounts(now time.Time) {
	if s == nil {
		return
	}
	nowNano := now.UnixNano()
	lastSweep := s.openaiAccountStreamDisconnectCleanupAt.Load()
	if lastSweep > 0 && nowNano-lastSweep < int64(time.Minute) {
		return
	}
	if !s.openaiAccountStreamDisconnectCleanupAt.CompareAndSwap(lastSweep, nowNano) {
		return
	}
	s.openaiAccountStreamDisconnectCount.Range(func(key, value any) bool {
		state, ok := value.(openAIAccountStreamDisconnectState)
		if !ok || state.start.IsZero() || now.Sub(state.start) > openAIAccountStreamFailureWindow {
			s.openaiAccountStreamDisconnectCount.Delete(key)
		}
		return true
	})
}

// RecordOpenAIAccountStreamDisconnect records a confirmed upstream disconnect
// from transports whose terminal result is not available to the normal HTTP
// settlement callback (notably failed WebSocket turns).
func (s *OpenAIGatewayService) RecordOpenAIAccountStreamDisconnect(account *Account, requestedModel string) {
	if s == nil || account == nil {
		return
	}
	s.recordOpenAIProxyStreamOutcome(account, OpenAIStreamFailureUpstreamDisconnect, true, false, errors.New("websocket upstream stream disconnected"))
	s.ReportOpenAIAccountScheduleResultForRequest(account, requestedModel, false, nil)
}

func (s *OpenAIGatewayService) clearOpenAIAccountStreamFailures(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	// Redis holds a shared fixed-window failure count. Do not clear it on
	// success: a success on one replica cannot safely reset another replica's
	// observation without a Redis write on every successful request. The key
	// expires naturally after the short failure window.
	s.openaiAccountStreamDisconnectCount.Delete(accountID)
}

// RecordOpenAIEdgeStreamOutcome is the control-plane boundary used by edge-rs
// settlement callbacks. The circuit remains local and observes only the
// explicitly classified upstream disconnect outcome; client/local outcomes
// never affect the proxy quarantine table.
func (s *OpenAIGatewayService) RecordOpenAIEdgeStreamOutcome(account *Account, failureClass string, clientOutputStarted bool, terminalSuccess bool, errorMessage string) {
	if !terminalSuccess && strings.EqualFold(strings.TrimSpace(failureClass), "upstream_no_progress") {
		// This is the final configured hard deadline, not the learned semantic
		// heuristic. A confirmed hang may be avoided immediately; health itself
		// is reported once by the normal completion path.
		s.blockOpenAIAccountTransportFailure(account, "upstream_no_progress", "upstream response made no effective progress")
		return
	}
	class := OpenAIStreamFailureClass(strings.TrimSpace(failureClass))
	var streamErr error
	if strings.TrimSpace(errorMessage) != "" {
		streamErr = errors.New("edge stream failure")
	}
	s.recordOpenAIProxyStreamOutcome(account, class, clientOutputStarted, terminalSuccess, streamErr)
}

func (s *OpenAIGatewayService) isOpenAIProxyStreamQuarantined(account *Account) bool {
	proxyID, ok := openAIProxyStreamCircuitProxyID(account)
	if !ok {
		return false
	}
	circuit := s.getOpenAIProxyStreamCircuit()
	return circuit != nil && circuit.isBlocked(proxyID, time.Now())
}
