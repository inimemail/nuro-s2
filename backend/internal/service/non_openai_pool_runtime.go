package service

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// NonOpenAIPoolRuntime is deliberately separate from the OpenAI and Anthropic
// runtimes. It is used by OpenAI-compatible domestic providers and by the
// Gemini compatibility path, without changing either existing implementation.
type NonOpenAIPoolRuntime struct {
	deadlines           sync.Map // key: platform:accountID, value: time.Time
	states              sync.Map // key: platform:accountID, value: NonOpenAIPoolRuntimeState
	probes              sync.Map // key: platform:accountID, value: nonOpenAIPoolProbeLease
	consecutiveFailures sync.Map // key: platform:accountID, value: int
	probeFailures       sync.Map // key: platform:accountID, value: int
	nextProbe           atomic.Uint64
}

type nonOpenAIPoolProbeLease struct {
	StartedAt time.Time
	Token     uint64
}

type NonOpenAIPoolRuntimeState struct {
	Until          time.Time
	Cooling        bool
	Due            bool
	ProbeInFlight  bool
	StatusCode     int
	Reason         string
	CooldownSource string
}

func NewNonOpenAIPoolRuntime() *NonOpenAIPoolRuntime {
	return &NonOpenAIPoolRuntime{}
}

func nonOpenAIPoolPlatform(platform string) bool {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case PlatformGemini, PlatformAntigravity, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepSeek:
		return true
	default:
		return false
	}
}

func shouldNonOpenAIPoolFailoverStatus(statusCode int) bool {
	return statusCode == 0 || statusCode == http.StatusRequestTimeout || statusCode == http.StatusUnauthorized || statusCode == http.StatusPaymentRequired || statusCode == http.StatusForbidden || statusCode == http.StatusTooManyRequests || statusCode == 529 || statusCode >= 500
}

func nonOpenAIPoolKey(account *Account) string {
	if account == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(account.Platform)) + ":" + formatInt64(account.ID)
}

func formatInt64(value int64) string {
	// Avoid fmt allocation in this hot path while keeping the key unambiguous.
	if value == 0 {
		return "0"
	}
	buf := [20]byte{}
	negative := value < 0
	if negative {
		value = -value
	}
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func (r *NonOpenAIPoolRuntime) shouldSkip(ctx context.Context, settings NonOpenAIPoolSettings, account *Account) bool {
	_ = ctx
	if r == nil || account == nil || !account.IsPoolMode() || !nonOpenAIPoolPlatform(account.Platform) {
		return false
	}
	if !settings.Enabled {
		r.clear(account)
		return false
	}
	if !account.IsPoolSoftCooldownEnabled() {
		r.clear(account)
		return false
	}
	key := nonOpenAIPoolKey(account)
	platformSettings, hasPlatformSettings := settings.Platforms[strings.ToLower(strings.TrimSpace(account.Platform))]
	probeTimeout := settings.ProbeTimeoutSeconds
	if hasPlatformSettings && platformSettings.ProbeTimeoutSeconds > 0 {
		probeTimeout = platformSettings.ProbeTimeoutSeconds
	}
	if existing, loaded := r.probes.Load(key); loaded {
		lease, valid := existing.(nonOpenAIPoolProbeLease)
		if valid && (probeTimeout <= 0 || time.Since(lease.StartedAt) < time.Duration(probeTimeout)*time.Second) {
			// The selected request may pass through more than one eligibility check.
			// Keep its lease idempotent while excluding all other request copies.
			if account.nonOpenAIPoolProbeToken == lease.Token {
				return false
			}
			state := r.state(account.ID, account.Platform)
			state.Cooling = true
			state.Due = true
			state.ProbeInFlight = true
			r.states.Store(key, state)
			return true
		}
		if !r.probes.CompareAndDelete(key, existing) {
			return true
		}
	}
	value, ok := r.deadlines.Load(key)
	if !ok {
		return false
	}
	until, ok := value.(time.Time)
	if !ok || until.IsZero() {
		r.deadlines.Delete(key)
		return false
	}
	if time.Now().Before(until) {
		state := r.state(account.ID, account.Platform)
		state.Until = until
		state.Cooling = true
		state.Due = false
		state.ProbeInFlight = false
		r.states.Store(key, state)
		return true
	}
	if hasPlatformSettings && !platformSettings.RecoveryProbeEnabled {
		r.clear(account)
		return false
	}
	// Exactly one request is allowed to act as the recovery probe. The probe
	// itself is the normal downstream request, so no extra retry/reconnect is added.
	lease := nonOpenAIPoolProbeLease{StartedAt: time.Now(), Token: r.nextProbe.Add(1)}
	if _, loaded := r.probes.LoadOrStore(key, lease); loaded {
		return true
	}
	account.nonOpenAIPoolProbeToken = lease.Token
	state := r.state(account.ID, account.Platform)
	state.Until = until
	state.Cooling = true
	state.Due = true
	state.ProbeInFlight = true
	r.states.Store(key, state)
	return false
}

// candidateBlocked is the side-effect-free scheduler precheck. An expired
// cooldown without a probe lease remains eligible so the request that is
// actually selected can claim the recovery probe in shouldSkip.
func (r *NonOpenAIPoolRuntime) candidateBlocked(settings NonOpenAIPoolSettings, account *Account) bool {
	if r == nil || account == nil || !account.IsPoolMode() || !nonOpenAIPoolPlatform(account.Platform) {
		return false
	}
	if !settings.Enabled || !account.IsPoolSoftCooldownEnabled() {
		r.clear(account)
		return false
	}
	state := r.stateForAccount(account)
	if !state.Cooling {
		return false
	}
	if account.nonOpenAIPoolProbeToken != 0 {
		if value, ok := r.probes.Load(nonOpenAIPoolKey(account)); ok {
			if lease, valid := value.(nonOpenAIPoolProbeLease); valid && lease.Token == account.nonOpenAIPoolProbeToken {
				return false
			}
		}
	}
	return !state.Due || state.ProbeInFlight
}

func (r *NonOpenAIPoolRuntime) stateForAccount(account *Account) NonOpenAIPoolRuntimeState {
	if account == nil || !nonOpenAIPoolPlatform(account.Platform) {
		return NonOpenAIPoolRuntimeState{}
	}
	return r.state(account.ID, account.Platform)
}

func (r *NonOpenAIPoolRuntime) markFailure(ctx context.Context, settings NonOpenAIPoolSettings, account *Account, statusCode int, reason, source string) {
	_ = ctx
	if r == nil || account == nil || !account.IsPoolMode() || !nonOpenAIPoolPlatform(account.Platform) {
		return
	}
	if !settings.Enabled {
		r.clear(account)
		return
	}
	if !account.IsPoolSoftCooldownEnabled() {
		r.clear(account)
		return
	}
	seconds := settings.DefaultCooldownSeconds
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusPaymentRequired || statusCode == http.StatusForbidden || statusCode == http.StatusTooManyRequests:
		seconds = settings.AuthCooldownSeconds
	case statusCode == 0:
		seconds = settings.TransportCooldownSeconds
	case statusCode == http.StatusBadGateway || statusCode == 529 || statusCode >= 500:
		seconds = settings.ServerErrorCooldownSeconds
	}
	platformSettings, hasPlatformSettings := settings.Platforms[strings.ToLower(strings.TrimSpace(account.Platform))]
	if hasPlatformSettings {
		if platformSettings.SoftCooldownMaxSeconds > 0 && seconds > platformSettings.SoftCooldownMaxSeconds {
			seconds = platformSettings.SoftCooldownMaxSeconds
		}
	}
	if settings.MaxCooldownSeconds > 0 && seconds > settings.MaxCooldownSeconds {
		seconds = settings.MaxCooldownSeconds
	}
	reason = sanitizeUpstreamErrorMessage(strings.TrimSpace(reason))
	key := nonOpenAIPoolKey(account)
	probeFailure := false
	if currentProbe, ok := r.probes.Load(key); ok {
		lease, valid := currentProbe.(nonOpenAIPoolProbeLease)
		if valid && account.nonOpenAIPoolProbeToken != 0 && lease.Token == account.nonOpenAIPoolProbeToken {
			probeFailure = true
		} else {
			// A normal request that started before cooldown, or an expired probe,
			// must not replace a newer in-flight recovery probe.
			return
		}
	}
	if !probeFailure {
		if value, ok := r.deadlines.Load(key); ok {
			if activeUntil, valid := value.(time.Time); valid && time.Now().Before(activeUntil) {
				state := r.state(account.ID, account.Platform)
				state.Until = activeUntil
				state.Cooling = true
				state.StatusCode = statusCode
				state.Reason = truncateString(strings.TrimSpace(reason), 256)
				state.CooldownSource = truncateString(strings.TrimSpace(source), 64)
				r.states.Store(key, state)
				return
			}
		}
	}
	if !probeFailure {
		threshold := account.GetPoolSoftCooldownErrorThreshold()
		failureCount := incrementNonOpenAIPoolCounter(&r.consecutiveFailures, key)
		if threshold > 1 && failureCount < threshold {
			return
		}
		r.consecutiveFailures.Delete(key)
	}
	probeFailureCount := 0
	if probeFailure {
		probeFailureCount = incrementNonOpenAIPoolCounter(&r.probeFailures, key)
	}
	if probeFailureCount > 1 {
		for i := 1; i < probeFailureCount && seconds < settings.ProbeMaxBackoffSeconds; i++ {
			seconds *= 2
		}
		if settings.ProbeMaxBackoffSeconds > 0 && seconds > settings.ProbeMaxBackoffSeconds {
			seconds = settings.ProbeMaxBackoffSeconds
		}
	}
	if hasPlatformSettings && platformSettings.SoftCooldownMaxSeconds > 0 && seconds > platformSettings.SoftCooldownMaxSeconds {
		seconds = platformSettings.SoftCooldownMaxSeconds
	}
	if settings.MaxCooldownSeconds > 0 && seconds > settings.MaxCooldownSeconds {
		seconds = settings.MaxCooldownSeconds
	}
	if seconds <= 0 {
		// A failed recovery probe with no effective backoff must not leave its
		// lease (and the expired deadline) installed. Otherwise every subsequent
		// candidate can be reported as ProbeInFlight until the lease timeout.
		if probeFailure {
			r.clear(account)
		}
		return
	}
	account.nonOpenAIPoolProbeToken = 0
	r.probes.Delete(key)
	until := time.Now().Add(time.Duration(seconds) * time.Second)
	r.deadlines.Store(key, until)
	r.states.Store(key, NonOpenAIPoolRuntimeState{Until: until, Cooling: true, StatusCode: statusCode, Reason: truncateString(strings.TrimSpace(reason), 256), CooldownSource: truncateString(strings.TrimSpace(source), 64)})
}

func (r *NonOpenAIPoolRuntime) markSuccess(account *Account) {
	if r == nil || account == nil || !account.IsPoolMode() || !nonOpenAIPoolPlatform(account.Platform) {
		return
	}
	key := nonOpenAIPoolKey(account)
	r.consecutiveFailures.Delete(key)
	if account.nonOpenAIPoolProbeToken == 0 {
		return
	}
	value, ok := r.probes.Load(key)
	lease, valid := value.(nonOpenAIPoolProbeLease)
	if !ok || !valid || lease.Token != account.nonOpenAIPoolProbeToken || !r.probes.CompareAndDelete(key, value) {
		return
	}
	account.nonOpenAIPoolProbeToken = 0
	r.deadlines.Delete(key)
	r.probeFailures.Delete(key)
	r.states.Delete(key)
}

// releaseProbe rolls back a probe lease when scheduler admission fails after
// the candidate was selected. Without this rollback a failed Redis slot claim
// could leave the account marked as probing until the lease timeout.
func (r *NonOpenAIPoolRuntime) releaseProbe(account *Account) {
	if r == nil || account == nil || account.nonOpenAIPoolProbeToken == 0 {
		return
	}
	key := nonOpenAIPoolKey(account)
	value, ok := r.probes.Load(key)
	lease, valid := value.(nonOpenAIPoolProbeLease)
	if !ok || !valid || lease.Token != account.nonOpenAIPoolProbeToken || !r.probes.CompareAndDelete(key, value) {
		return
	}
	account.nonOpenAIPoolProbeToken = 0
	state := r.state(account.ID, account.Platform)
	if state.ProbeInFlight {
		state.ProbeInFlight = false
		r.states.Store(key, state)
	}
}

func incrementNonOpenAIPoolCounter(counter *sync.Map, key string) int {
	if counter == nil || key == "" {
		return 0
	}
	for {
		current, loaded := counter.Load(key)
		if !loaded {
			if _, stored := counter.LoadOrStore(key, 1); !stored {
				return 1
			}
			continue
		}
		value, valid := current.(int)
		if !valid || value < 0 {
			if counter.CompareAndSwap(key, current, 1) {
				return 1
			}
			continue
		}
		if counter.CompareAndSwap(key, current, value+1) {
			return value + 1
		}
	}
}

func copyNonOpenAIPoolProbeToken(source, target *Account) {
	if source == nil || target == nil || source.ID != target.ID || source.Platform != target.Platform {
		return
	}
	target.nonOpenAIPoolProbeToken = source.nonOpenAIPoolProbeToken
}

func (r *NonOpenAIPoolRuntime) clear(account *Account) {
	if r == nil || account == nil {
		return
	}
	account.nonOpenAIPoolProbeToken = 0
	key := nonOpenAIPoolKey(account)
	r.deadlines.Delete(key)
	r.probes.Delete(key)
	r.consecutiveFailures.Delete(key)
	r.probeFailures.Delete(key)
	r.states.Delete(key)
}

func (r *NonOpenAIPoolRuntime) clearAccountID(accountID int64) {
	if r == nil {
		return
	}
	suffix := ":" + formatInt64(accountID)
	for _, platform := range []string{PlatformGemini, PlatformAntigravity, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepSeek} {
		key := strings.ToLower(platform) + suffix
		r.deadlines.Delete(key)
		r.probes.Delete(key)
		r.consecutiveFailures.Delete(key)
		r.probeFailures.Delete(key)
		r.states.Delete(key)
	}
}

func (r *NonOpenAIPoolRuntime) clearAll() {
	if r == nil {
		return
	}
	for _, stateMap := range []*sync.Map{&r.deadlines, &r.states, &r.probes, &r.consecutiveFailures, &r.probeFailures} {
		stateMap.Range(func(key, _ any) bool {
			stateMap.Delete(key)
			return true
		})
	}
}

func (r *NonOpenAIPoolRuntime) state(accountID int64, platform string) NonOpenAIPoolRuntimeState {
	if r == nil {
		return NonOpenAIPoolRuntimeState{}
	}
	key := strings.ToLower(strings.TrimSpace(platform)) + ":" + formatInt64(accountID)
	if value, ok := r.states.Load(key); ok {
		if state, ok := value.(NonOpenAIPoolRuntimeState); ok {
			if !state.Until.IsZero() && time.Now().After(state.Until) {
				state.Due = true
			}
			return state
		}
	}
	return NonOpenAIPoolRuntimeState{}
}

func nonOpenAIPoolSettings(ctx context.Context, settingService *SettingService) NonOpenAIPoolSettings {
	if settingService == nil {
		return DefaultNonOpenAIPoolSettings()
	}
	// Cached settings are immutable after publication. Reuse the snapshot on
	// scheduler hot paths instead of cloning the platform map per candidate.
	return settingService.getGatewayForwardingSettingsCached(ctx).nonOpenAIPool
}
