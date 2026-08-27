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
	deadlines           sync.Map // key: platform:kind:accountID, value: time.Time
	states              sync.Map // key: platform:kind:accountID, value: NonOpenAIPoolRuntimeState
	probes              sync.Map // key: platform:kind:accountID, value: nonOpenAIPoolProbeLease
	consecutiveFailures sync.Map // key: platform:kind:accountID, value: int
	probeFailures       sync.Map // key: platform:kind:accountID, value: int
	nextProbe           atomic.Uint64
	probeRunnersMu      sync.RWMutex
	probeRunners        map[string]NonOpenAIPoolProbeFunc
	settingsProvider    func() NonOpenAIPoolSettings
}

type nonOpenAIPoolProbeLease struct {
	StartedAt time.Time
	Token     uint64
	Deadline  time.Time
}

type NonOpenAIPoolProbeResult struct {
	Success    bool
	StatusCode int
	Reason     string
	Source     string
}

type NonOpenAIPoolProbeFunc func(ctx context.Context, accountID int64, platform, kind, model string) NonOpenAIPoolProbeResult

type NonOpenAIPoolRuntimeState struct {
	Until           time.Time
	Cooling         bool
	Due             bool
	ProbeInFlight   bool
	StatusCode      int
	Reason          string
	CooldownSource  string
	ProbeModel      string
	ProbeKind       string
	LastProbeStatus int
	LastProbeReason string
}

const (
	NonOpenAIPoolRequestKindText  = "text"
	NonOpenAIPoolRequestKindImage = "image"
)

type nonOpenAIPoolRequestKindContextKey struct{}

// WithNonOpenAIPoolRequestKind marks normal text or image/media traffic.
func WithNonOpenAIPoolRequestKind(ctx context.Context, kind string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if kind != NonOpenAIPoolRequestKindImage {
		kind = NonOpenAIPoolRequestKindText
	}
	return context.WithValue(ctx, nonOpenAIPoolRequestKindContextKey{}, kind)
}

func nonOpenAIPoolRequestKindFromContext(ctx context.Context) string {
	kind, _ := nonOpenAIPoolRequestKindFromContextExplicit(ctx)
	return kind
}

func nonOpenAIPoolRequestKindFromContextExplicit(ctx context.Context) (string, bool) {
	if ctx != nil {
		if kind, ok := ctx.Value(nonOpenAIPoolRequestKindContextKey{}).(string); ok {
			if kind == NonOpenAIPoolRequestKindImage {
				return kind, true
			}
			return NonOpenAIPoolRequestKindText, true
		}
		if OpenAIImageGenerationIntentFromContext(ctx) || OpenAIImagesEndpointFromContext(ctx) {
			return NonOpenAIPoolRequestKindImage, true
		}
	}
	return NonOpenAIPoolRequestKindText, false
}

func nonOpenAIPoolRequestKindForAccount(ctx context.Context, account *Account) string {
	if account != nil && nonOpenAIPoolPlatformSupportsImageBucket(account.Platform) &&
		nonOpenAIPoolRequestKindFromContext(ctx) == NonOpenAIPoolRequestKindImage {
		return NonOpenAIPoolRequestKindImage
	}
	return NonOpenAIPoolRequestKindText
}

func withNonOpenAIPoolModelKind(ctx context.Context, account *Account, requestedModel string) context.Context {
	if account == nil || !nonOpenAIPoolPlatformSupportsImageBucket(account.Platform) {
		return ctx
	}
	if nonOpenAIPoolRequestKindFromContext(ctx) == NonOpenAIPoolRequestKindImage {
		return ctx
	}
	model := strings.TrimSpace(requestedModel)
	if account != nil {
		if mapped := strings.TrimSpace(account.GetMappedModel(model)); mapped != "" {
			model = mapped
		}
	}
	if isImageGenerationModel(model) {
		return WithNonOpenAIPoolRequestKind(ctx, NonOpenAIPoolRequestKindImage)
	}
	return ctx
}

func NewNonOpenAIPoolRuntime() *NonOpenAIPoolRuntime {
	return &NonOpenAIPoolRuntime{probeRunners: make(map[string]NonOpenAIPoolProbeFunc)}
}

func (r *NonOpenAIPoolRuntime) currentSettings(fallback NonOpenAIPoolSettings) NonOpenAIPoolSettings {
	if r != nil && r.settingsProvider != nil {
		return r.settingsProvider()
	}
	return fallback
}

func nonOpenAIPoolPlatform(platform string) bool {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case PlatformGemini, PlatformAntigravity, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepSeek:
		return true
	default:
		return false
	}
}

func nonOpenAIPoolPlatformSupportsImageBucket(platform string) bool {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case PlatformGemini, PlatformAntigravity, PlatformGrok:
		return true
	default:
		return false
	}
}

func shouldNonOpenAIPoolFailoverStatus(statusCode int) bool {
	return statusCode == 0 || statusCode == http.StatusRequestTimeout || statusCode == http.StatusUnauthorized || statusCode == http.StatusPaymentRequired || statusCode == http.StatusForbidden || statusCode == http.StatusTooManyRequests || statusCode == 529 || statusCode >= 500
}

func nonOpenAIPoolKey(account *Account, kinds ...string) string {
	if account == nil {
		return ""
	}
	kind := NonOpenAIPoolRequestKindText
	if len(kinds) > 0 && kinds[0] == NonOpenAIPoolRequestKindImage {
		kind = NonOpenAIPoolRequestKindImage
	}
	return strings.ToLower(strings.TrimSpace(account.Platform)) + ":" + kind + ":" + formatInt64(account.ID)
}

func nonOpenAIPoolBucketSettings(settings NonOpenAIPoolSettings, account *Account, kind string) (NonOpenAIPoolBucketSettings, bool) {
	if account == nil {
		return NonOpenAIPoolBucketSettings{}, false
	}
	platformSettings, ok := settings.Platforms[strings.ToLower(strings.TrimSpace(account.Platform))]
	if !ok {
		return NonOpenAIPoolBucketSettings{RecoveryProbeEnabled: true, SoftCooldownMaxSeconds: settings.MaxCooldownSeconds, ProbeTimeoutSeconds: settings.ProbeTimeoutSeconds}, false
	}
	if kind == NonOpenAIPoolRequestKindImage && nonOpenAIPoolPlatformSupportsImageBucket(account.Platform) {
		return platformSettings.Image, true
	}
	return NonOpenAIPoolBucketSettings{RecoveryProbeEnabled: platformSettings.RecoveryProbeEnabled, RecoveryProbeModel: platformSettings.RecoveryProbeModel, SoftCooldownMaxSeconds: platformSettings.SoftCooldownMaxSeconds, ProbeTimeoutSeconds: platformSettings.ProbeTimeoutSeconds}, true
}

func (r *NonOpenAIPoolRuntime) registerProbeRunner(platform string, runner NonOpenAIPoolProbeFunc) {
	if r == nil || runner == nil {
		return
	}
	r.probeRunnersMu.Lock()
	if r.probeRunners == nil {
		r.probeRunners = make(map[string]NonOpenAIPoolProbeFunc)
	}
	r.probeRunners[strings.ToLower(strings.TrimSpace(platform))] = runner
	r.probeRunnersMu.Unlock()
}

func (r *NonOpenAIPoolRuntime) probeRunner(platform string) NonOpenAIPoolProbeFunc {
	if r == nil {
		return nil
	}
	r.probeRunnersMu.RLock()
	runner := r.probeRunners[strings.ToLower(strings.TrimSpace(platform))]
	r.probeRunnersMu.RUnlock()
	return runner
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
	kind := nonOpenAIPoolRequestKindForAccount(ctx, account)
	account.nonOpenAIPoolRequestKind = kind
	key := nonOpenAIPoolKey(account, kind)
	bucket, hasPlatformSettings := nonOpenAIPoolBucketSettings(settings, account, kind)
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
		state := r.state(account.ID, account.Platform, kind)
		state.Until = until
		state.Cooling = true
		state.Due = false
		state.ProbeInFlight = false
		r.states.Store(key, state)
		return true
	}
	if hasPlatformSettings && !bucket.RecoveryProbeEnabled {
		r.clearKind(account, kind)
		return false
	}
	state := r.state(account.ID, account.Platform, kind)
	state.Until = until
	state.Cooling = true
	state.Due = true
	r.states.Store(key, state)
	r.maybeStartRecoveryProbe(settings, account, kind, until)
	return true
}

func (r *NonOpenAIPoolRuntime) candidateBlocked(settings NonOpenAIPoolSettings, account *Account) bool {
	return r.candidateBlockedForKind(settings, account, NonOpenAIPoolRequestKindText)
}

func (r *NonOpenAIPoolRuntime) candidateBlockedForKind(settings NonOpenAIPoolSettings, account *Account, kind string) bool {
	if r == nil || account == nil || !account.IsPoolMode() || !nonOpenAIPoolPlatform(account.Platform) {
		return false
	}
	if !settings.Enabled || !account.IsPoolSoftCooldownEnabled() {
		r.clear(account)
		return false
	}
	if kind == NonOpenAIPoolRequestKindImage && !nonOpenAIPoolPlatformSupportsImageBucket(account.Platform) {
		kind = NonOpenAIPoolRequestKindText
	}
	key := nonOpenAIPoolKey(account, kind)
	value, ok := r.deadlines.Load(key)
	until, valid := value.(time.Time)
	if !ok || !valid || until.IsZero() {
		return false
	}
	bucket, hasPlatformSettings := nonOpenAIPoolBucketSettings(settings, account, kind)
	if !time.Now().Before(until) && hasPlatformSettings && !bucket.RecoveryProbeEnabled {
		r.clearKind(account, kind)
		return false
	}
	if !time.Now().Before(until) {
		r.maybeStartRecoveryProbe(settings, account, kind, until)
	}
	return true
}

func (r *NonOpenAIPoolRuntime) scheduleRecoveryProbe(settings NonOpenAIPoolSettings, account *Account, kind string, deadline time.Time) {
	if r == nil || account == nil || deadline.IsZero() {
		return
	}
	accountID, platform := account.ID, account.Platform
	delay := time.Until(deadline)
	if delay < 0 {
		delay = 0
	}
	time.AfterFunc(delay, func() {
		probeAccount := &Account{ID: accountID, Platform: platform, Type: AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true}}
		r.maybeStartRecoveryProbe(r.currentSettings(settings), probeAccount, kind, deadline)
	})
}

func (r *NonOpenAIPoolRuntime) maybeStartRecoveryProbe(settings NonOpenAIPoolSettings, account *Account, kind string, expectedDeadline time.Time) {
	if r == nil || account == nil || expectedDeadline.IsZero() || time.Now().Before(expectedDeadline) {
		return
	}
	if !settings.Enabled {
		r.clearKind(account, kind)
		return
	}
	key := nonOpenAIPoolKey(account, kind)
	current, ok := r.deadlines.Load(key)
	deadline, valid := current.(time.Time)
	if !ok || !valid || !deadline.Equal(expectedDeadline) {
		return
	}
	bucket, hasPlatformSettings := nonOpenAIPoolBucketSettings(settings, account, kind)
	if hasPlatformSettings && !bucket.RecoveryProbeEnabled {
		if r.deadlines.CompareAndDelete(key, current) {
			r.states.Delete(key)
			r.probeFailures.Delete(key)
		}
		return
	}
	runner := r.probeRunner(account.Platform)
	if runner == nil {
		return
	}
	if existing, loaded := r.probes.Load(key); loaded {
		lease, valid := existing.(nonOpenAIPoolProbeLease)
		timeoutSeconds := bucket.ProbeTimeoutSeconds
		if timeoutSeconds <= 0 {
			timeoutSeconds = settings.ProbeTimeoutSeconds
		}
		if valid && (timeoutSeconds <= 0 || time.Since(lease.StartedAt) < time.Duration(timeoutSeconds)*time.Second) {
			return
		}
		if !r.probes.CompareAndDelete(key, existing) {
			return
		}
	}
	lease := nonOpenAIPoolProbeLease{StartedAt: time.Now(), Token: r.nextProbe.Add(1), Deadline: expectedDeadline}
	if _, loaded := r.probes.LoadOrStore(key, lease); loaded {
		return
	}
	state := r.state(account.ID, account.Platform, kind)
	state.Until = expectedDeadline
	state.Cooling = true
	state.Due = true
	state.ProbeInFlight = true
	state.ProbeModel = bucket.RecoveryProbeModel
	state.ProbeKind = kind
	r.states.Store(key, state)

	timeoutSeconds := bucket.ProbeTimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = settings.ProbeTimeoutSeconds
	}
	if timeoutSeconds > 0 {
		time.AfterFunc(time.Duration(timeoutSeconds)*time.Second, func() {
			r.maybeStartRecoveryProbe(r.currentSettings(settings), account, kind, expectedDeadline)
		})
	}
	go func() {
		ctx := context.Background()
		cancel := func() {}
		if timeoutSeconds > 0 {
			ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
		}
		defer cancel()
		result := runner(ctx, account.ID, account.Platform, kind, bucket.RecoveryProbeModel)
		if ctx.Err() != nil && result.Success {
			result = NonOpenAIPoolProbeResult{StatusCode: 0, Reason: ctx.Err().Error(), Source: "probe_timeout"}
		}
		r.finishRecoveryProbe(r.currentSettings(settings), account, kind, lease, result)
	}()
}

func (r *NonOpenAIPoolRuntime) maybeKickFromAdmin(settings NonOpenAIPoolSettings, account *Account) {
	if r == nil || account == nil {
		return
	}
	for _, kind := range []string{NonOpenAIPoolRequestKindText, NonOpenAIPoolRequestKindImage} {
		if kind == NonOpenAIPoolRequestKindImage && !nonOpenAIPoolPlatformSupportsImageBucket(account.Platform) {
			continue
		}
		value, ok := r.deadlines.Load(nonOpenAIPoolKey(account, kind))
		deadline, valid := value.(time.Time)
		if ok && valid && !time.Now().Before(deadline) {
			r.maybeStartRecoveryProbe(settings, account, kind, deadline)
		}
	}
}

func (r *NonOpenAIPoolRuntime) finishRecoveryProbe(settings NonOpenAIPoolSettings, account *Account, kind string, lease nonOpenAIPoolProbeLease, result NonOpenAIPoolProbeResult) {
	key := nonOpenAIPoolKey(account, kind)
	loaded, ok := r.probes.Load(key)
	currentLease, valid := loaded.(nonOpenAIPoolProbeLease)
	if !ok || !valid || currentLease.Token != lease.Token {
		return
	}
	defer r.probes.CompareAndDelete(key, loaded)
	deadlineValue, ok := r.deadlines.Load(key)
	deadline, valid := deadlineValue.(time.Time)
	if !ok || !valid || !deadline.Equal(lease.Deadline) {
		return
	}
	if !settings.Enabled {
		if r.deadlines.CompareAndDelete(key, deadlineValue) {
			r.consecutiveFailures.Delete(key)
			r.probeFailures.Delete(key)
			r.states.Delete(key)
		}
		return
	}
	bucket, hasPlatformSettings := nonOpenAIPoolBucketSettings(settings, account, kind)
	if hasPlatformSettings && !bucket.RecoveryProbeEnabled {
		if r.deadlines.CompareAndDelete(key, deadlineValue) {
			r.consecutiveFailures.Delete(key)
			r.probeFailures.Delete(key)
			r.states.Delete(key)
		}
		return
	}
	if result.Success {
		if r.deadlines.CompareAndDelete(key, deadlineValue) {
			r.consecutiveFailures.Delete(key)
			r.probeFailures.Delete(key)
			r.states.Delete(key)
		}
		return
	}

	failures := incrementNonOpenAIPoolCounter(&r.probeFailures, key)
	seconds := nonOpenAIPoolCooldownSeconds(settings, result.StatusCode)
	for i := 1; i < failures && seconds < settings.ProbeMaxBackoffSeconds; i++ {
		seconds *= 2
	}
	maxSeconds := settings.MaxCooldownSeconds
	if hasPlatformSettings && bucket.SoftCooldownMaxSeconds > 0 {
		maxSeconds = bucket.SoftCooldownMaxSeconds
	}
	if settings.ProbeMaxBackoffSeconds > 0 && (maxSeconds <= 0 || settings.ProbeMaxBackoffSeconds < maxSeconds) {
		maxSeconds = settings.ProbeMaxBackoffSeconds
	}
	if maxSeconds > 0 && seconds > maxSeconds {
		seconds = maxSeconds
	}
	if seconds <= 0 {
		seconds = 1
	}
	newDeadline := time.Now().Add(time.Duration(seconds) * time.Second)
	if !r.deadlines.CompareAndSwap(key, deadlineValue, newDeadline) {
		return
	}
	reason := truncateString(sanitizeUpstreamErrorMessage(strings.TrimSpace(result.Reason)), 256)
	source := truncateString(strings.TrimSpace(result.Source), 64)
	if source == "" {
		source = "recovery_probe"
	}
	state := r.state(account.ID, account.Platform, kind)
	state.Until = newDeadline
	state.Cooling = true
	state.Due = false
	state.ProbeInFlight = false
	state.CooldownSource = "probe_backoff"
	state.ProbeModel = bucket.RecoveryProbeModel
	state.ProbeKind = kind
	state.LastProbeStatus = result.StatusCode
	state.LastProbeReason = reason
	// Preserve the error that originally placed the account in cooldown. Probe
	// errors are tracked separately, matching the OpenAI/Anthropic state model.
	if state.Reason == "" {
		state.StatusCode = result.StatusCode
		state.Reason = reason
	}
	if source != "" && state.LastProbeReason == "" {
		state.LastProbeReason = source
	}
	r.states.Store(key, state)
	r.scheduleRecoveryProbe(settings, account, kind, newDeadline)
}

func nonOpenAIPoolCooldownSeconds(settings NonOpenAIPoolSettings, statusCode int) int {
	seconds := settings.DefaultCooldownSeconds
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusPaymentRequired || statusCode == http.StatusForbidden || statusCode == http.StatusTooManyRequests:
		seconds = settings.AuthCooldownSeconds
	case statusCode == 0:
		seconds = settings.TransportCooldownSeconds
	case statusCode == http.StatusBadGateway || statusCode == 529 || statusCode >= 500:
		seconds = settings.ServerErrorCooldownSeconds
	}
	return seconds
}

func (r *NonOpenAIPoolRuntime) stateForAccount(account *Account) NonOpenAIPoolRuntimeState {
	return r.stateForAccountWithSettings(account, DefaultNonOpenAIPoolSettings())
}

func (r *NonOpenAIPoolRuntime) stateForAccountWithSettings(account *Account, settings NonOpenAIPoolSettings) NonOpenAIPoolRuntimeState {
	if account == nil || !nonOpenAIPoolPlatform(account.Platform) {
		return NonOpenAIPoolRuntimeState{}
	}
	textState := r.displayStateForKind(account, settings, NonOpenAIPoolRequestKindText)
	imageState := r.displayStateForKind(account, settings, NonOpenAIPoolRequestKindImage)
	if imageState.ProbeInFlight && !textState.ProbeInFlight {
		return imageState
	}
	if textState.ProbeInFlight {
		return textState
	}
	if !textState.Cooling {
		return imageState
	}
	if imageState.Cooling && imageState.Until.After(textState.Until) {
		return imageState
	}
	return textState
}

func (r *NonOpenAIPoolRuntime) displayStateForKind(account *Account, settings NonOpenAIPoolSettings, kind string) NonOpenAIPoolRuntimeState {
	state := r.state(account.ID, account.Platform, kind)
	if !state.ProbeInFlight {
		return state
	}
	leaseValue, ok := r.probes.Load(nonOpenAIPoolKey(account, kind))
	lease, valid := leaseValue.(nonOpenAIPoolProbeLease)
	bucket, hasPlatformSettings := nonOpenAIPoolBucketSettings(settings, account, kind)
	probeTimeout := settings.ProbeTimeoutSeconds
	if hasPlatformSettings && bucket.ProbeTimeoutSeconds > 0 {
		probeTimeout = bucket.ProbeTimeoutSeconds
	}
	if !ok || !valid || (probeTimeout > 0 && time.Since(lease.StartedAt) >= time.Duration(probeTimeout)*time.Second) {
		state.Cooling = true
		state.Due = true
		state.ProbeInFlight = false
	}
	return state
}

func (r *NonOpenAIPoolRuntime) markFailure(ctx context.Context, settings NonOpenAIPoolSettings, account *Account, statusCode int, reason, source string) {
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
	kind := nonOpenAIPoolRequestKindForAccount(ctx, account)
	if _, explicit := nonOpenAIPoolRequestKindFromContextExplicit(ctx); !explicit {
		if account.nonOpenAIPoolRequestKind == NonOpenAIPoolRequestKindImage {
			kind = NonOpenAIPoolRequestKindImage
		}
	}
	bucket, hasPlatformSettings := nonOpenAIPoolBucketSettings(settings, account, kind)
	if hasPlatformSettings {
		if bucket.SoftCooldownMaxSeconds > 0 && seconds > bucket.SoftCooldownMaxSeconds {
			seconds = bucket.SoftCooldownMaxSeconds
		}
	}
	if !hasPlatformSettings && settings.MaxCooldownSeconds > 0 && seconds > settings.MaxCooldownSeconds {
		seconds = settings.MaxCooldownSeconds
	}
	reason = sanitizeUpstreamErrorMessage(strings.TrimSpace(reason))
	key := nonOpenAIPoolKey(account, kind)
	if _, probing := r.probes.Load(key); probing {
		// Requests that started before the cooldown cannot overwrite the active
		// background probe generation.
		return
	}
	if value, ok := r.deadlines.Load(key); ok {
		if activeUntil, valid := value.(time.Time); valid {
			state := r.state(account.ID, account.Platform, kind)
			state.Until = activeUntil
			state.Cooling = true
			state.Due = !time.Now().Before(activeUntil)
			state.ProbeInFlight = false
			state.StatusCode = statusCode
			state.Reason = truncateString(strings.TrimSpace(reason), 256)
			state.CooldownSource = truncateString(strings.TrimSpace(source), 64)
			r.states.Store(key, state)
			if state.Due {
				r.maybeStartRecoveryProbe(settings, account, kind, activeUntil)
			}
			return
		}
	}
	threshold := account.GetPoolSoftCooldownErrorThreshold()
	failureCount := incrementNonOpenAIPoolCounter(&r.consecutiveFailures, key)
	if threshold > 1 && failureCount < threshold {
		return
	}
	r.consecutiveFailures.Delete(key)
	if hasPlatformSettings && bucket.SoftCooldownMaxSeconds > 0 && seconds > bucket.SoftCooldownMaxSeconds {
		seconds = bucket.SoftCooldownMaxSeconds
	}
	if !hasPlatformSettings && settings.MaxCooldownSeconds > 0 && seconds > settings.MaxCooldownSeconds {
		seconds = settings.MaxCooldownSeconds
	}
	if seconds <= 0 {
		return
	}
	account.nonOpenAIPoolProbeToken = 0
	account.nonOpenAIPoolProbeKind = ""
	r.probes.Delete(key)
	until := time.Now().Add(time.Duration(seconds) * time.Second)
	r.deadlines.Store(key, until)
	r.states.Store(key, NonOpenAIPoolRuntimeState{
		Until: until, Cooling: true, StatusCode: statusCode,
		Reason: truncateString(strings.TrimSpace(reason), 256), CooldownSource: truncateString(strings.TrimSpace(source), 64),
		ProbeModel: bucket.RecoveryProbeModel, ProbeKind: kind,
	})
	r.scheduleRecoveryProbe(settings, account, kind, until)
}

func (r *NonOpenAIPoolRuntime) markSuccess(account *Account) {
	if r == nil || account == nil || !account.IsPoolMode() || !nonOpenAIPoolPlatform(account.Platform) {
		return
	}
	// A successful request can be reported by more than one compatibility
	// service. Only the first report owns the request-scoped bucket marker; a
	// duplicate report must not fall back to the text bucket and clear its
	// failure counter after an image/media success.
	if account.nonOpenAIPoolRequestKind == "" {
		return
	}
	kind := nonOpenAIPoolRequestKindForAccount(nil, account)
	if account.nonOpenAIPoolRequestKind == NonOpenAIPoolRequestKindImage {
		kind = NonOpenAIPoolRequestKindImage
	}
	key := nonOpenAIPoolKey(account, kind)
	r.consecutiveFailures.Delete(key)
	account.nonOpenAIPoolRequestKind = ""
}

// releaseProbe rolls back a probe lease when scheduler admission fails after
// the candidate was selected. Without this rollback a failed Redis slot claim
// could leave the account marked as probing until the lease timeout.
func (r *NonOpenAIPoolRuntime) releaseProbe(account *Account) {
	if r == nil || account == nil || account.nonOpenAIPoolProbeToken == 0 {
		return
	}
	kind := account.nonOpenAIPoolProbeKind
	if kind != NonOpenAIPoolRequestKindImage {
		kind = NonOpenAIPoolRequestKindText
	}
	key := nonOpenAIPoolKey(account, kind)
	value, ok := r.probes.Load(key)
	lease, valid := value.(nonOpenAIPoolProbeLease)
	if !ok || !valid || lease.Token != account.nonOpenAIPoolProbeToken || !r.probes.CompareAndDelete(key, value) {
		return
	}
	account.nonOpenAIPoolProbeToken = 0
	account.nonOpenAIPoolProbeKind = ""
	account.nonOpenAIPoolRequestKind = ""
	state := r.state(account.ID, account.Platform, kind)
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
	target.nonOpenAIPoolProbeKind = source.nonOpenAIPoolProbeKind
	target.nonOpenAIPoolRequestKind = source.nonOpenAIPoolRequestKind
}

func (r *NonOpenAIPoolRuntime) clear(account *Account) {
	if r == nil || account == nil {
		return
	}
	account.nonOpenAIPoolProbeToken = 0
	account.nonOpenAIPoolProbeKind = ""
	account.nonOpenAIPoolRequestKind = ""
	for _, kind := range []string{NonOpenAIPoolRequestKindText, NonOpenAIPoolRequestKindImage} {
		r.clearKind(account, kind)
	}
}

func (r *NonOpenAIPoolRuntime) clearKind(account *Account, kind string) {
	if r == nil || account == nil {
		return
	}
	key := nonOpenAIPoolKey(account, kind)
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
		for _, kind := range []string{NonOpenAIPoolRequestKindText, NonOpenAIPoolRequestKindImage} {
			key := strings.ToLower(platform) + ":" + kind + suffix
			r.deadlines.Delete(key)
			r.probes.Delete(key)
			r.consecutiveFailures.Delete(key)
			r.probeFailures.Delete(key)
			r.states.Delete(key)
		}
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

func (r *NonOpenAIPoolRuntime) state(accountID int64, platform string, kinds ...string) NonOpenAIPoolRuntimeState {
	if r == nil {
		return NonOpenAIPoolRuntimeState{}
	}
	kind := NonOpenAIPoolRequestKindText
	if len(kinds) > 0 && kinds[0] == NonOpenAIPoolRequestKindImage {
		kind = NonOpenAIPoolRequestKindImage
	}
	key := strings.ToLower(strings.TrimSpace(platform)) + ":" + kind + ":" + formatInt64(accountID)
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
