package service

import (
	"context"
	"errors"
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
	maxEntries       int
}

type openAIProxyStreamCircuitEntry struct {
	failureCount int
	windowStart  time.Time
	blockedUntil time.Time
	lastTouched  time.Time
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

func resolveOpenAIProxyStreamCircuitSettings(s *OpenAIGatewayService) openAIProxyStreamCircuitSettings {
	settings := openAIProxyStreamCircuitSettings{
		failureThreshold: defaultOpenAIProxyStreamFailureThreshold,
		failureWindow:    defaultOpenAIProxyStreamFailureWindow,
		quarantineTTL:    defaultOpenAIProxyStreamQuarantineTTL,
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
	}
	entry.failureCount++
	entry.lastTouched = now
	tripped := entry.failureCount >= c.settings.failureThreshold
	if tripped {
		entry.blockedUntil = now.Add(c.settings.quarantineTTL)
	}
	c.entries[proxyID] = entry
	c.publishLocked()
	return tripped, entry.blockedUntil
}

func (c *openAIProxyStreamCircuit) recordSuccess(proxyID int64) {
	if c == nil || !c.settings.enabled || proxyID <= 0 {
		return
	}
	c.mu.Lock()
	delete(c.entries, proxyID)
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
	proxyID, ok := openAIProxyStreamCircuitProxyID(account)
	if !ok {
		return
	}
	circuit := s.getOpenAIProxyStreamCircuit()
	if circuit == nil {
		return
	}
	if terminalSuccess {
		circuit.recordSuccess(proxyID)
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

// RecordOpenAIEdgeStreamOutcome is the control-plane boundary used by edge-rs
// settlement callbacks. The circuit remains local and observes only the
// explicitly classified upstream disconnect outcome; client/local outcomes
// never affect the proxy quarantine table.
func (s *OpenAIGatewayService) RecordOpenAIEdgeStreamOutcome(account *Account, failureClass string, clientOutputStarted bool, terminalSuccess bool, errorMessage string) {
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
