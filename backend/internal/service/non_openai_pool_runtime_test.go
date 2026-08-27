package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func nonOpenAIPoolTestAccount(id int64, platform string) *Account {
	return &Account{ID: id, Platform: platform, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"pool_mode":                          true,
		"pool_soft_cooldown_error_threshold": 1,
	}}
}

func waitForNonOpenAIPool(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

type nonOpenAIPoolAccountRepoStub struct {
	AccountRepository
	rateLimitCalls int
}

func (r *nonOpenAIPoolAccountRepoStub) SetRateLimited(context.Context, int64, time.Time) error {
	r.rateLimitCalls++
	return nil
}

func TestNonOpenAIPoolRuntimeIsolatedByPlatformAndAccount(t *testing.T) {
	runtime := &NonOpenAIPoolRuntime{}
	settings := DefaultNonOpenAIPoolSettings()
	grok := nonOpenAIPoolTestAccount(7, PlatformGrok)
	kimi := nonOpenAIPoolTestAccount(7, PlatformKimi)
	runtime.markFailure(context.Background(), settings, grok, 500, "server", "test")
	if !runtime.shouldSkip(context.Background(), settings, grok) {
		t.Fatal("grok should be cooling")
	}
	if runtime.shouldSkip(context.Background(), settings, kimi) {
		t.Fatal("kimi must not share grok cooldown")
	}
}

func TestNonOpenAIPoolRuntimeIsolatesTextAndImageBuckets(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(71, PlatformGrok)
	imageCtx := WithNonOpenAIPoolRequestKind(context.Background(), NonOpenAIPoolRequestKindImage)

	runtime.markFailure(context.Background(), settings, account, http.StatusServiceUnavailable, "text unavailable", "test")
	if !runtime.shouldSkip(context.Background(), settings, account) {
		t.Fatal("text bucket should be cooling")
	}
	if runtime.shouldSkip(imageCtx, settings, nonOpenAIPoolTestAccount(account.ID, account.Platform)) {
		t.Fatal("text cooldown must not block image/media traffic")
	}

	runtime.clear(account)
	runtime.markFailure(imageCtx, settings, account, http.StatusServiceUnavailable, "image unavailable", "test")
	if !runtime.shouldSkip(imageCtx, settings, account) {
		t.Fatal("image bucket should be cooling")
	}
	if runtime.shouldSkip(context.Background(), settings, nonOpenAIPoolTestAccount(account.ID, account.Platform)) {
		t.Fatal("image/media cooldown must not block text traffic")
	}
}

func TestNonOpenAIPoolImageProbeSuccessDoesNotClearTextBucket(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(72, PlatformGemini)
	imageCtx := WithNonOpenAIPoolRequestKind(context.Background(), NonOpenAIPoolRequestKindImage)
	completed := make(chan struct{}, 1)
	runtime.registerProbeRunner(PlatformGemini, func(context.Context, int64, string, string, string) NonOpenAIPoolProbeResult {
		completed <- struct{}{}
		return NonOpenAIPoolProbeResult{Success: true, StatusCode: http.StatusOK}
	})

	runtime.markFailure(context.Background(), settings, account, http.StatusServiceUnavailable, "text unavailable", "test")
	runtime.markFailure(imageCtx, settings, account, http.StatusServiceUnavailable, "image unavailable", "test")
	imageKey := nonOpenAIPoolKey(account, NonOpenAIPoolRequestKindImage)
	deadline := time.Now().Add(-time.Second)
	runtime.deadlines.Store(imageKey, deadline)
	if !runtime.shouldSkip(imageCtx, settings, account) {
		t.Fatal("expired image bucket must remain excluded during background recovery")
	}
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("image recovery probe did not run")
	}
	waitForNonOpenAIPool(t, time.Second, func() bool {
		_, cooling := runtime.deadlines.Load(imageKey)
		return !cooling
	})
	if runtime.shouldSkip(imageCtx, settings, nonOpenAIPoolTestAccount(account.ID, account.Platform)) {
		t.Fatal("successful image probe should clear the image bucket")
	}
	if !runtime.shouldSkip(context.Background(), settings, nonOpenAIPoolTestAccount(account.ID, account.Platform)) {
		t.Fatal("successful image probe must not clear the text bucket")
	}
}

func TestNonOpenAIPoolDuplicateImageSuccessDoesNotResetTextFailures(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(73, PlatformGemini)
	account.Credentials["pool_soft_cooldown_error_threshold"] = 2
	imageCtx := WithNonOpenAIPoolRequestKind(context.Background(), NonOpenAIPoolRequestKindImage)

	runtime.markFailure(context.Background(), settings, account, http.StatusServiceUnavailable, "text unavailable", "test")
	if runtime.shouldSkip(imageCtx, settings, account) {
		t.Fatal("healthy image bucket should remain schedulable")
	}
	runtime.markSuccess(account)
	runtime.markSuccess(account)

	runtime.markFailure(context.Background(), settings, account, http.StatusServiceUnavailable, "text unavailable", "test")
	if !runtime.shouldSkip(context.Background(), settings, nonOpenAIPoolTestAccount(account.ID, account.Platform)) {
		t.Fatal("duplicate image success must not reset the text failure counter")
	}
}

func TestNonOpenAIPoolStaleProbeSuccessDoesNotResetCurrentGeneration(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	account := nonOpenAIPoolTestAccount(74, PlatformGrok)
	key := nonOpenAIPoolKey(account, NonOpenAIPoolRequestKindImage)
	oldDeadline := time.Now().Add(-2 * time.Second)
	currentDeadline := time.Now().Add(time.Second)
	oldLease := nonOpenAIPoolProbeLease{StartedAt: time.Now(), Token: 41, Deadline: oldDeadline}
	runtime.deadlines.Store(key, currentDeadline)
	runtime.probes.Store(key, nonOpenAIPoolProbeLease{StartedAt: time.Now(), Token: 42, Deadline: currentDeadline})
	runtime.consecutiveFailures.Store(key, 1)

	runtime.finishRecoveryProbe(DefaultNonOpenAIPoolSettings(), account, NonOpenAIPoolRequestKindImage, oldLease, NonOpenAIPoolProbeResult{Success: true})

	if value, ok := runtime.consecutiveFailures.Load(key); !ok || value != 1 {
		t.Fatalf("stale probe success changed current failure count: value=%v present=%v", value, ok)
	}
	if value, ok := runtime.probes.Load(key); !ok || value.(nonOpenAIPoolProbeLease).Token != 42 {
		t.Fatalf("stale probe success changed current lease: value=%v present=%v", value, ok)
	}
}

func TestNonOpenAIPoolStateForAccountPrefersActiveProbeThenLongerCooldown(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	account := nonOpenAIPoolTestAccount(721, PlatformGrok)
	now := time.Now()
	textKey := nonOpenAIPoolKey(account, NonOpenAIPoolRequestKindText)
	imageKey := nonOpenAIPoolKey(account, NonOpenAIPoolRequestKindImage)

	runtime.states.Store(textKey, NonOpenAIPoolRuntimeState{Cooling: true, Until: now.Add(5 * time.Second), Reason: "text"})
	runtime.states.Store(imageKey, NonOpenAIPoolRuntimeState{Cooling: true, Until: now.Add(10 * time.Second), Reason: "image"})
	if state := runtime.stateForAccount(account); state.Reason != "image" {
		t.Fatalf("display state reason = %q, want longer image cooldown", state.Reason)
	}

	textState := NonOpenAIPoolRuntimeState{Cooling: true, Due: true, ProbeInFlight: true, Until: now.Add(-time.Second), Reason: "text probe"}
	runtime.states.Store(textKey, textState)
	runtime.probes.Store(textKey, nonOpenAIPoolProbeLease{StartedAt: now, Token: 7})
	if state := runtime.stateForAccount(account); state.Reason != "text probe" || !state.ProbeInFlight {
		t.Fatalf("display state = %+v, want active text probe", state)
	}
}

func TestNonOpenAIPoolDisplayTreatsExpiredProbeLeaseAsDue(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	platform := settings.Platforms[PlatformGrok]
	platform.Image.ProbeTimeoutSeconds = 1
	settings.Platforms[PlatformGrok] = platform
	account := nonOpenAIPoolTestAccount(722, PlatformGrok)
	key := nonOpenAIPoolKey(account, NonOpenAIPoolRequestKindImage)
	runtime.states.Store(key, NonOpenAIPoolRuntimeState{
		Until: time.Now().Add(-time.Second), Cooling: true, Due: true, ProbeInFlight: true,
	})
	runtime.probes.Store(key, nonOpenAIPoolProbeLease{StartedAt: time.Now().Add(-2 * time.Second), Token: 99})

	state := runtime.stateForAccountWithSettings(account, settings)
	if !state.Cooling || !state.Due || state.ProbeInFlight {
		t.Fatalf("expired display probe state = %+v, want cooling and due without in-flight", state)
	}
	if _, ok := runtime.probes.Load(key); !ok {
		t.Fatal("read-only display state must not delete the probe lease")
	}
}

func TestNonOpenAIPoolSelectedImageRequestKeepsBucketWithoutContext(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(73, PlatformGemini)
	imageCtx := WithNonOpenAIPoolRequestKind(context.Background(), NonOpenAIPoolRequestKindImage)

	if runtime.shouldSkip(imageCtx, settings, account) {
		t.Fatal("healthy image request should be eligible")
	}
	runtime.markFailure(context.Background(), settings, account, http.StatusServiceUnavailable, "image unavailable", "test")

	if state := runtime.state(account.ID, account.Platform, NonOpenAIPoolRequestKindImage); !state.Cooling {
		t.Fatal("selected image request should keep its image bucket through detached error handling")
	}
	if state := runtime.state(account.ID, account.Platform, NonOpenAIPoolRequestKindText); state.Cooling {
		t.Fatal("detached image failure must not cool the normal-model bucket")
	}
}

func TestNonOpenAIPoolMappedGeminiImageModelUsesImageBucket(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(74, PlatformAntigravity)
	account.Credentials["model_mapping"] = map[string]any{"image-alias": "gemini-3-pro-image"}

	ctx := withNonOpenAIPoolModelKind(context.Background(), account, "image-alias")
	if kind := nonOpenAIPoolRequestKindForAccount(ctx, account); kind != NonOpenAIPoolRequestKindImage {
		t.Fatalf("mapped image request kind = %q, want image", kind)
	}
	if runtime.shouldSkip(ctx, settings, account) {
		t.Fatal("healthy mapped image request should be eligible")
	}
	runtime.markFailure(ctx, settings, account, http.StatusServiceUnavailable, "image unavailable", "test")
	if state := runtime.state(account.ID, account.Platform, NonOpenAIPoolRequestKindImage); !state.Cooling {
		t.Fatal("mapped image failure should cool the image bucket")
	}
}

func TestNonOpenAIPoolNormalOnlyPlatformUsesTextBucketForImageIntent(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(75, PlatformKimi)
	imageCtx := WithNonOpenAIPoolRequestKind(context.Background(), NonOpenAIPoolRequestKindImage)

	runtime.markFailure(imageCtx, settings, account, http.StatusServiceUnavailable, "unavailable", "test")
	if state := runtime.state(account.ID, account.Platform, NonOpenAIPoolRequestKindText); !state.Cooling {
		t.Fatal("normal-only platform should keep all traffic in its normal-model bucket")
	}
	if state := runtime.state(account.ID, account.Platform, NonOpenAIPoolRequestKindImage); state.Cooling {
		t.Fatal("normal-only platform must not create a hidden image bucket")
	}
}

func TestNonOpenAIPoolPlatformMaxIsNotCappedByLegacyGlobalMax(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	settings.MaxCooldownSeconds = 1
	settings.ServerErrorCooldownSeconds = 5
	account := nonOpenAIPoolTestAccount(76, PlatformGrok)

	runtime.markFailure(context.Background(), settings, account, http.StatusServiceUnavailable, "unavailable", "test")
	state := runtime.state(account.ID, account.Platform, NonOpenAIPoolRequestKindText)
	remaining := time.Until(state.Until)
	if remaining < 4*time.Second {
		t.Fatalf("platform cooldown was capped by legacy global max: remaining=%s", remaining)
	}
	if remaining > 6*time.Second {
		t.Fatalf("platform cooldown exceeded configured error duration: remaining=%s", remaining)
	}
}

func TestNonOpenAIPoolRuntimeStartsProbeWithoutDownstreamTraffic(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(9, PlatformDeepSeek)
	called := make(chan struct{}, 1)
	runtime.registerProbeRunner(PlatformDeepSeek, func(context.Context, int64, string, string, string) NonOpenAIPoolProbeResult {
		called <- struct{}{}
		return NonOpenAIPoolProbeResult{Success: true, StatusCode: http.StatusOK}
	})
	deadline := time.Now().Add(20 * time.Millisecond)
	runtime.deadlines.Store(nonOpenAIPoolKey(account), deadline)
	runtime.scheduleRecoveryProbe(settings, account, NonOpenAIPoolRequestKindText, deadline)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("background recovery probe was not started")
	}
}

func TestNonOpenAIPoolRuntimeTimerUsesLatestProbeSettings(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	initial := DefaultNonOpenAIPoolSettings()
	latest := DefaultNonOpenAIPoolSettings()
	platform := latest.Platforms[PlatformDeepSeek]
	platform.RecoveryProbeModel = "deepseek-latest-probe"
	latest.Platforms[PlatformDeepSeek] = platform
	runtime.settingsProvider = func() NonOpenAIPoolSettings { return latest }
	account := nonOpenAIPoolTestAccount(91, PlatformDeepSeek)
	models := make(chan string, 1)
	runtime.registerProbeRunner(PlatformDeepSeek, func(_ context.Context, _ int64, _, _, model string) NonOpenAIPoolProbeResult {
		models <- model
		return NonOpenAIPoolProbeResult{Success: true, StatusCode: http.StatusOK}
	})
	deadline := time.Now().Add(20 * time.Millisecond)
	runtime.deadlines.Store(nonOpenAIPoolKey(account), deadline)
	runtime.scheduleRecoveryProbe(initial, account, NonOpenAIPoolRequestKindText, deadline)
	select {
	case model := <-models:
		if model != "deepseek-latest-probe" {
			t.Fatalf("probe model = %q, want latest setting", model)
		}
	case <-time.After(time.Second):
		t.Fatal("background recovery probe was not started")
	}
}

func TestNonOpenAIPoolRuntimeProbeTimeoutIsAutomaticallyTakenOver(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	platform := settings.Platforms[PlatformKimi]
	platform.ProbeTimeoutSeconds = 1
	settings.Platforms[PlatformKimi] = platform
	runtime.settingsProvider = func() NonOpenAIPoolSettings { return settings }
	account := nonOpenAIPoolTestAccount(92, PlatformKimi)
	key := nonOpenAIPoolKey(account)
	deadline := time.Now().Add(-time.Second)
	runtime.deadlines.Store(key, deadline)
	var calls atomic.Int32
	releaseStaleProbe := make(chan struct{})
	runtime.registerProbeRunner(PlatformKimi, func(_ context.Context, _ int64, _, _, _ string) NonOpenAIPoolProbeResult {
		if calls.Add(1) == 1 {
			// Simulate a transport that ignores context cancellation. The lease
			// timer must still let a new generation take over.
			<-releaseStaleProbe
			return NonOpenAIPoolProbeResult{Success: true, StatusCode: http.StatusOK}
		}
		return NonOpenAIPoolProbeResult{Success: true, StatusCode: http.StatusOK}
	})
	runtime.maybeStartRecoveryProbe(settings, account, NonOpenAIPoolRequestKindText, deadline)
	waitForNonOpenAIPool(t, 3*time.Second, func() bool {
		_, cooling := runtime.deadlines.Load(key)
		return calls.Load() >= 2 && !cooling
	})
	close(releaseStaleProbe)
}

func TestNonOpenAIPoolRuntimeConcurrentKickRunsSingleProbeAndBlocksRequests(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(10, PlatformKimi)
	key := nonOpenAIPoolKey(account)
	deadline := time.Now().Add(-time.Second)
	runtime.deadlines.Store(key, deadline)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var mu sync.Mutex
	calls := 0
	runtime.registerProbeRunner(PlatformKimi, func(context.Context, int64, string, string, string) NonOpenAIPoolProbeResult {
		mu.Lock()
		calls++
		mu.Unlock()
		started <- struct{}{}
		<-release
		return NonOpenAIPoolProbeResult{Success: true, StatusCode: http.StatusOK}
	})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.maybeStartRecoveryProbe(settings, account, NonOpenAIPoolRequestKindText, deadline)
		}()
	}
	wg.Wait()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	if !runtime.candidateBlocked(settings, account) || !runtime.shouldSkip(context.Background(), settings, account) {
		t.Fatal("real downstream requests must remain excluded during recovery")
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("probe calls = %d, want 1", gotCalls)
	}
	close(release)
}

func TestOpenAIStickyTemporaryUnavailableUsesNonOpenAIRequestKind(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(212, PlatformGrok)
	runtime.markFailure(context.Background(), settings, account, http.StatusServiceUnavailable, "text unavailable", "test")
	service := &OpenAIGatewayService{nonOpenAIPoolRuntime: runtime}

	imageCtx := WithNonOpenAIPoolRequestKind(context.Background(), NonOpenAIPoolRequestKindImage)
	if openAIStickyAccountTemporarilyUnavailable(imageCtx, service, account, nil) {
		t.Fatal("text cooldown must not make a healthy image sticky request unavailable")
	}
	if !openAIStickyAccountTemporarilyUnavailable(context.Background(), service, account, nil) {
		t.Fatal("text request must observe the text cooldown")
	}
}

func TestAdvancedSchedulerCompatibilityExcludesNonOpenAICoolingAccountOnly(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	domestic := nonOpenAIPoolTestAccount(22, PlatformDeepSeek)
	domestic.Status = StatusActive
	domestic.Schedulable = true
	openAI := nonOpenAIPoolTestAccount(23, PlatformOpenAI)
	openAI.Status = StatusActive
	openAI.Schedulable = true
	runtime.markFailure(context.Background(), settings, domestic, http.StatusServiceUnavailable, "unavailable", "test")

	service := &OpenAIGatewayService{nonOpenAIPoolRuntime: runtime}
	scheduler := &defaultOpenAIAccountScheduler{service: service}
	domesticReq := OpenAIAccountScheduleRequest{RequestPlatform: PlatformDeepSeek, LockedPriority: -1}
	if compatible, reason := scheduler.isAccountRequestCompatibleReason(context.Background(), domestic, domesticReq); compatible || reason != "runtime_blocked" {
		t.Fatalf("cooling domestic account compatibility = %v, reason = %q", compatible, reason)
	}
	openAIReq := OpenAIAccountScheduleRequest{RequestPlatform: PlatformOpenAI, LockedPriority: -1}
	if compatible, reason := scheduler.isAccountRequestCompatibleReason(context.Background(), openAI, openAIReq); !compatible {
		t.Fatalf("OpenAI compatibility was changed by non-OpenAI cooldown: reason = %q", reason)
	}
}

func TestNonOpenAIPoolRuntimeProbeSuccessClearsState(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(11, PlatformZhipu)
	key := nonOpenAIPoolKey(account)
	deadline := time.Now().Add(-time.Second)
	lease := nonOpenAIPoolProbeLease{StartedAt: time.Now(), Token: 1, Deadline: deadline}
	runtime.deadlines.Store(key, deadline)
	runtime.probes.Store(key, lease)
	runtime.states.Store(key, NonOpenAIPoolRuntimeState{Until: deadline, Cooling: true, Due: true, ProbeInFlight: true})
	runtime.finishRecoveryProbe(settings, account, NonOpenAIPoolRequestKindText, lease, NonOpenAIPoolProbeResult{Success: true, StatusCode: http.StatusOK})
	if runtime.shouldSkip(context.Background(), settings, nonOpenAIPoolTestAccount(account.ID, account.Platform)) {
		t.Fatal("successful probe should clear cooldown")
	}
	if state := runtime.stateForAccount(account); state.Cooling {
		t.Fatal("successful probe should clear display state")
	}
}

func TestNonOpenAIPoolRuntimeProbeFailureReentersCooldown(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(12, PlatformGemini)
	key := nonOpenAIPoolKey(account)
	deadline := time.Now().Add(-time.Second)
	lease := nonOpenAIPoolProbeLease{StartedAt: time.Now(), Token: 1, Deadline: deadline}
	runtime.deadlines.Store(key, deadline)
	runtime.probes.Store(key, lease)
	runtime.finishRecoveryProbe(settings, account, NonOpenAIPoolRequestKindText, lease, NonOpenAIPoolProbeResult{StatusCode: 503, Reason: "unavailable"})
	if !runtime.shouldSkip(context.Background(), settings, nonOpenAIPoolTestAccount(account.ID, account.Platform)) {
		t.Fatal("failed probe should re-enter cooldown")
	}
}

func TestNonOpenAIPoolRuntimeDisablingProbeDuringFlightDoesNotExtendCooldown(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(120, PlatformGemini)
	key := nonOpenAIPoolKey(account)
	deadline := time.Now().Add(-time.Second)
	lease := nonOpenAIPoolProbeLease{StartedAt: time.Now(), Token: 1, Deadline: deadline}
	runtime.deadlines.Store(key, deadline)
	runtime.probes.Store(key, lease)

	platform := settings.Platforms[PlatformGemini]
	platform.RecoveryProbeEnabled = false
	settings.Platforms[PlatformGemini] = platform
	runtime.finishRecoveryProbe(settings, account, NonOpenAIPoolRequestKindText, lease, NonOpenAIPoolProbeResult{StatusCode: http.StatusServiceUnavailable, Reason: "unavailable"})
	if runtime.candidateBlocked(settings, account) {
		t.Fatal("disabling recovery probes must not extend an expired cooldown")
	}
	if state := runtime.stateForAccountWithSettings(account, settings); state.Cooling {
		t.Fatalf("disabled recovery probe retained display state: %+v", state)
	}
}

func TestNonOpenAIPoolRuntimeDisablingFeatureDuringFlightDoesNotRestoreCooldown(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(119, PlatformGemini)
	key := nonOpenAIPoolKey(account)
	deadline := time.Now().Add(-time.Second)
	lease := nonOpenAIPoolProbeLease{StartedAt: time.Now(), Token: 1, Deadline: deadline}
	runtime.deadlines.Store(key, deadline)
	runtime.probes.Store(key, lease)

	settings.Enabled = false
	runtime.finishRecoveryProbe(settings, account, NonOpenAIPoolRequestKindText, lease, NonOpenAIPoolProbeResult{StatusCode: http.StatusServiceUnavailable, Reason: "unavailable"})
	if runtime.candidateBlocked(settings, account) {
		t.Fatal("disabling non-OpenAI pool recovery must not restore cooldown after an in-flight failure")
	}
	if state := runtime.stateForAccountWithSettings(account, settings); state.Cooling {
		t.Fatalf("disabled feature retained display state: %+v", state)
	}
}

func TestNonOpenAIPoolProbeFailurePreservesOriginalCooldownReason(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(118, PlatformDeepSeek)
	key := nonOpenAIPoolKey(account)
	deadline := time.Now().Add(-time.Second)
	lease := nonOpenAIPoolProbeLease{StartedAt: time.Now(), Token: 1, Deadline: deadline}
	runtime.deadlines.Store(key, deadline)
	runtime.probes.Store(key, lease)
	runtime.states.Store(key, NonOpenAIPoolRuntimeState{
		Until: deadline, Cooling: true, Due: true, ProbeInFlight: true,
		StatusCode: http.StatusTooManyRequests, Reason: "original rate limit",
	})

	runtime.finishRecoveryProbe(settings, account, NonOpenAIPoolRequestKindText, lease, NonOpenAIPoolProbeResult{
		StatusCode: http.StatusServiceUnavailable, Reason: "probe unavailable",
	})
	state := runtime.stateForAccount(account)
	if state.StatusCode != http.StatusTooManyRequests || state.Reason != "original rate limit" {
		t.Fatalf("original cooldown cause was overwritten: %+v", state)
	}
	if state.LastProbeStatus != http.StatusServiceUnavailable || state.LastProbeReason != "probe unavailable" {
		t.Fatalf("last probe failure was not retained separately: %+v", state)
	}
	if state.CooldownSource != "probe_backoff" || state.ProbeModel == "" || state.ProbeKind != NonOpenAIPoolRequestKindText {
		t.Fatalf("probe backoff metadata is incomplete: %+v", state)
	}
}

func TestNonOpenAIPoolOldFailureDuringActiveCooldownResetsDueDisplay(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(121, PlatformGemini)
	key := nonOpenAIPoolKey(account)
	until := time.Now().Add(10 * time.Second)
	runtime.deadlines.Store(key, until)
	runtime.states.Store(key, NonOpenAIPoolRuntimeState{
		Until: until, Cooling: true, Due: true, ProbeInFlight: true,
	})

	runtime.markFailure(context.Background(), settings, account, http.StatusServiceUnavailable, "late failure", "test")
	state := runtime.state(account.ID, account.Platform)
	if !state.Cooling || state.Due || state.ProbeInFlight {
		t.Fatalf("active cooldown display state = %+v, want cooling without due/probe", state)
	}
}

func TestNonOpenAIPoolRuntimeProbeFailureUsesMinimumBackoff(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	settings.ServerErrorCooldownSeconds = 0
	settings.MaxCooldownSeconds = 0
	account := nonOpenAIPoolTestAccount(112, PlatformKimi)
	key := nonOpenAIPoolKey(account)
	deadline := time.Now().Add(-time.Second)
	lease := nonOpenAIPoolProbeLease{StartedAt: time.Now(), Token: 1, Deadline: deadline}
	runtime.deadlines.Store(key, deadline)
	runtime.probes.Store(key, lease)
	runtime.finishRecoveryProbe(settings, account, NonOpenAIPoolRequestKindText, lease, NonOpenAIPoolProbeResult{StatusCode: http.StatusServiceUnavailable, Reason: "unavailable"})
	if _, loaded := runtime.probes.Load(key); loaded {
		t.Fatal("completed probe lease should be removed")
	}
	if value, loaded := runtime.deadlines.Load(key); !loaded || !value.(time.Time).After(time.Now()) {
		t.Fatal("failed probe should retain a future retry deadline")
	}
}

func TestNonOpenAIPoolRuntimeDisabledClearsState(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(13, PlatformAntigravity)
	runtime.markFailure(context.Background(), settings, account, 429, "limited", "test")
	settings.Enabled = false
	if runtime.shouldSkip(context.Background(), settings, account) {
		t.Fatal("disabled runtime must not skip accounts")
	}
	if state := runtime.stateForAccount(account); state.Cooling {
		t.Fatal("disabled runtime should clear existing state")
	}
}

func TestGatewayRuntimeOnlyClearRemovesDomesticPoolState(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(131, PlatformKimi)
	runtime.markFailure(context.Background(), settings, account, http.StatusServiceUnavailable, "unavailable", "test")
	service := &GatewayService{nonOpenAIPoolRuntime: runtime}

	service.ClearAccountRuntimeBlockOnly(account.ID)
	if state := runtime.stateForAccount(account); state.Cooling {
		t.Fatal("runtime-only clear should remove domestic pool cooldown")
	}
}

func TestNonOpenAIPoolRuntimeNeverTouchesOpenAI(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(14, PlatformOpenAI)
	runtime.markFailure(context.Background(), settings, account, 500, "server", "test")
	if runtime.shouldSkip(context.Background(), settings, account) {
		t.Fatal("OpenAI accounts must stay outside the non-OpenAI runtime")
	}
	if state := runtime.stateForAccount(account); state.Cooling {
		t.Fatal("OpenAI accounts must not receive non-OpenAI state")
	}
}

func TestNonOpenAIPoolTransportFailureUsesTransportCooldown(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	service := &OpenAIGatewayService{nonOpenAIPoolRuntime: runtime}
	account := nonOpenAIPoolTestAccount(18, PlatformKimi)

	failoverErr := service.newOpenAIPoolRequestFailoverError(nil, account, nil, errors.New("dial timeout"), false)
	if failoverErr == nil {
		t.Fatal("transport failure should keep the existing account failover behavior")
	}
	state := runtime.stateForAccount(account)
	if !state.Cooling {
		t.Fatal("transport failure should start soft cooldown")
	}
	if state.StatusCode != 0 {
		t.Fatalf("transport failure status = %d, want 0 so transport settings are used", state.StatusCode)
	}
}

func TestGeminiSkippedPoolRateLimitEntersNonOpenAIPoolRuntime(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	service := &GeminiMessagesCompatService{nonOpenAIPoolRuntime: runtime}
	account := nonOpenAIPoolTestAccount(24, PlatformGemini)

	if failoverErr := service.poolModeSkippedFailoverError(nil, account, http.StatusTooManyRequests, []byte(`{"error":{"message":"rate limited"}}`), ""); failoverErr == nil {
		t.Fatal("pool rate-limit error should remain failoverable")
	}
	if state := runtime.stateForAccount(account); !state.Cooling || state.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("gemini skipped 429 state = %+v, want active cooldown", state)
	}
}

func TestGrokPoolErrorEntersNonOpenAIPoolRuntime(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	service := &OpenAIGatewayService{nonOpenAIPoolRuntime: runtime}
	account := nonOpenAIPoolTestAccount(19, PlatformGrok)

	service.handleGrokAccountUpstreamError(
		context.Background(),
		account,
		http.StatusServiceUnavailable,
		http.Header{},
		[]byte(`{"error":{"message":"temporarily unavailable"}}`),
	)

	if state := runtime.stateForAccount(account); !state.Cooling || state.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("grok pool state = %+v, want active 503 cooldown", state)
	}
}

func TestCommittedDomesticHTTPFailureIsCountedOnce(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	service := &OpenAIGatewayService{nonOpenAIPoolRuntime: runtime}
	account := nonOpenAIPoolTestAccount(20, PlatformDeepSeek)
	account.Credentials["pool_soft_cooldown_error_threshold"] = 2
	body := []byte(`{"error":{"message":"temporarily unavailable"}}`)

	service.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusServiceUnavailable, http.Header{}, body)
	service.RecordOpenAIPoolFailureAfterCommittedResponse(
		context.Background(), account, http.StatusServiceUnavailable, body, "deepseek-chat", "temporarily unavailable",
	)
	if state := runtime.stateForAccount(account); state.Cooling {
		t.Fatalf("one committed failure was counted twice: %+v", state)
	}

	service.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusServiceUnavailable, http.Header{}, body)
	if state := runtime.stateForAccount(account); !state.Cooling {
		t.Fatal("second distinct failure should reach the configured threshold")
	}
}

func TestNonOpenAIPoolRuntimeHonorsConsecutiveFailureThreshold(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(16, PlatformDeepSeek)
	account.Credentials["pool_soft_cooldown_error_threshold"] = 3

	for attempt := 1; attempt <= 2; attempt++ {
		runtime.markFailure(context.Background(), settings, account, 503, "unavailable", "test")
		if runtime.shouldSkip(context.Background(), settings, account) {
			t.Fatalf("account cooled after %d failures, threshold is 3", attempt)
		}
	}
	if runtime.shouldSkip(context.Background(), settings, account) {
		t.Fatal("account should remain schedulable below the failure threshold")
	}
	runtime.markSuccess(account)
	runtime.markFailure(context.Background(), settings, account, 503, "unavailable", "test")
	if runtime.shouldSkip(context.Background(), settings, account) {
		t.Fatal("a normal success should reset the consecutive failure count")
	}
	runtime.markFailure(context.Background(), settings, account, 503, "unavailable", "test")
	runtime.markFailure(context.Background(), settings, account, 503, "unavailable", "test")
	if !runtime.shouldSkip(context.Background(), settings, account) {
		t.Fatal("account should cool when the consecutive failure threshold is reached")
	}
}

func TestGeminiPoolFailureUsesSoftCooldownWithoutHardRateLimit(t *testing.T) {
	repo := &nonOpenAIPoolAccountRepoStub{}
	runtime := NewNonOpenAIPoolRuntime()
	service := &GeminiMessagesCompatService{
		rateLimitService:     NewRateLimitService(repo, nil, &config.Config{}, nil, nil),
		nonOpenAIPoolRuntime: runtime,
	}
	account := nonOpenAIPoolTestAccount(15, PlatformGemini)
	service.handleGeminiUpstreamError(
		context.Background(),
		account,
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(`{"error":{"message":"rate limited"}}`),
	)

	if repo.rateLimitCalls != 0 {
		t.Fatalf("hard rate-limit writes = %d, want 0 for pool mode", repo.rateLimitCalls)
	}
	if state := runtime.stateForAccount(account); !state.Cooling || state.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("soft cooldown state = %+v, want active 429 cooldown", state)
	}
}

func TestAntigravityPoolFailureUsesSoftCooldownWithoutHardRateLimit(t *testing.T) {
	repo := &nonOpenAIPoolAccountRepoStub{}
	runtime := NewNonOpenAIPoolRuntime()
	service := &AntigravityGatewayService{
		accountRepo:          repo,
		rateLimitService:     NewRateLimitService(repo, nil, &config.Config{}, nil, nil),
		nonOpenAIPoolRuntime: runtime,
	}
	account := nonOpenAIPoolTestAccount(17, PlatformAntigravity)
	service.handleUpstreamError(
		context.Background(),
		"test",
		account,
		http.StatusTooManyRequests,
		http.Header{},
		[]byte(`{"error":{"message":"rate limited"}}`),
		"gemini-test",
		0,
		"",
		false,
	)

	if repo.rateLimitCalls != 0 {
		t.Fatalf("hard rate-limit writes = %d, want 0 for pool mode", repo.rateLimitCalls)
	}
	if state := runtime.stateForAccount(account); !state.Cooling || state.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("soft cooldown state = %+v, want active 429 cooldown", state)
	}
}
