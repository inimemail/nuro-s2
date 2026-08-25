package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
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

func TestNonOpenAIPoolRuntimeAllowsSingleRecoveryProbe(t *testing.T) {
	runtime := &NonOpenAIPoolRuntime{}
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(9, PlatformDeepSeek)
	runtime.deadlines.Store(nonOpenAIPoolKey(account), time.Now().Add(-time.Second))
	if runtime.shouldSkip(context.Background(), settings, account) {
		t.Fatal("first request after deadline should be the probe")
	}
	if account.nonOpenAIPoolProbeToken == 0 {
		t.Fatal("probe request should carry a lease token")
	}
	if runtime.shouldSkip(context.Background(), settings, account) {
		t.Fatal("the same probe request must remain eligible on repeated checks")
	}
	concurrentCopy := nonOpenAIPoolTestAccount(account.ID, account.Platform)
	if !runtime.shouldSkip(context.Background(), settings, concurrentCopy) {
		t.Fatal("a concurrent request must be skipped while the probe is in flight")
	}
}

func TestNonOpenAIPoolRuntimeConcurrentProbeLease(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	base := nonOpenAIPoolTestAccount(10, PlatformKimi)
	runtime.deadlines.Store(nonOpenAIPoolKey(base), time.Now().Add(-time.Second))

	const requestCount = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	allowed := make(chan *Account, requestCount)
	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			account := nonOpenAIPoolTestAccount(base.ID, base.Platform)
			<-start
			if !runtime.shouldSkip(context.Background(), settings, account) {
				allowed <- account
			}
		}()
	}
	close(start)
	wg.Wait()
	close(allowed)
	if got := len(allowed); got != 1 {
		t.Fatalf("allowed probe requests = %d, want 1", got)
	}
}

func TestNonOpenAIPoolRuntimeReleaseProbeRollsBackAdmissionFailure(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(101, PlatformKimi)
	runtime.deadlines.Store(nonOpenAIPoolKey(account), time.Now().Add(-time.Second))
	if runtime.shouldSkip(context.Background(), settings, account) {
		t.Fatal("expired account should claim the recovery probe")
	}
	if account.nonOpenAIPoolProbeToken == 0 {
		t.Fatal("expected probe token")
	}
	runtime.releaseProbe(account)
	if account.nonOpenAIPoolProbeToken != 0 {
		t.Fatal("probe token was not released")
	}
	if _, loaded := runtime.probes.Load(nonOpenAIPoolKey(account)); loaded {
		t.Fatal("probe lease was not released")
	}
	copy := nonOpenAIPoolTestAccount(account.ID, account.Platform)
	if runtime.shouldSkip(context.Background(), settings, copy) {
		t.Fatal("released probe should be claimable again")
	}
}

func TestNonOpenAIPoolSchedulerPrecheckDoesNotClaimProbe(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	account := nonOpenAIPoolTestAccount(21, PlatformKimi)
	runtime.markFailure(context.Background(), settings, account, http.StatusServiceUnavailable, "unavailable", "test")

	if !runtime.candidateBlocked(settings, account) {
		t.Fatal("active cooldown must be excluded by the scheduler precheck")
	}
	key := nonOpenAIPoolKey(account)
	deadline := time.Now().Add(-time.Second)
	runtime.deadlines.Store(key, deadline)
	state := runtime.stateForAccount(account)
	state.Until = deadline
	state.Due = true
	runtime.states.Store(key, state)

	if runtime.candidateBlocked(settings, account) {
		t.Fatal("expired cooldown must remain eligible for the selected request to probe")
	}
	if _, claimed := runtime.probes.Load(key); claimed {
		t.Fatal("scheduler precheck must not claim a recovery probe")
	}
	if runtime.shouldSkip(context.Background(), settings, account) {
		t.Fatal("the selected request should claim and execute the recovery probe")
	}
	if account.nonOpenAIPoolProbeToken == 0 {
		t.Fatal("selected recovery probe must carry its request token")
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
	runtime.markFailure(context.Background(), settings, account, 500, "server", "test")
	runtime.deadlines.Store(nonOpenAIPoolKey(account), time.Now().Add(-time.Second))
	if runtime.shouldSkip(context.Background(), settings, account) {
		t.Fatal("expired account should be allowed as recovery probe")
	}
	runtime.markSuccess(account)
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
	runtime.deadlines.Store(nonOpenAIPoolKey(account), time.Now().Add(-time.Second))
	if runtime.shouldSkip(context.Background(), settings, account) {
		t.Fatal("expired account should be allowed as recovery probe")
	}
	runtime.markFailure(context.Background(), settings, account, 503, "unavailable", "probe")
	if !runtime.shouldSkip(context.Background(), settings, nonOpenAIPoolTestAccount(account.ID, account.Platform)) {
		t.Fatal("failed probe should re-enter cooldown")
	}
}

func TestNonOpenAIPoolRuntimeProbeFailureWithZeroBackoffReleasesLease(t *testing.T) {
	runtime := NewNonOpenAIPoolRuntime()
	settings := DefaultNonOpenAIPoolSettings()
	settings.ServerErrorCooldownSeconds = 0
	settings.MaxCooldownSeconds = 0
	account := nonOpenAIPoolTestAccount(112, PlatformKimi)
	key := nonOpenAIPoolKey(account)
	runtime.deadlines.Store(key, time.Now().Add(-time.Second))
	if runtime.shouldSkip(context.Background(), settings, account) {
		t.Fatal("expired account should be allowed as recovery probe")
	}
	if account.nonOpenAIPoolProbeToken == 0 {
		t.Fatal("expected probe token")
	}
	runtime.markFailure(context.Background(), settings, account, http.StatusServiceUnavailable, "unavailable", "probe")
	if account.nonOpenAIPoolProbeToken != 0 {
		t.Fatal("probe token should be released when no backoff is configured")
	}
	if _, loaded := runtime.probes.Load(key); loaded {
		t.Fatal("probe lease should be removed when no backoff is configured")
	}
	if _, loaded := runtime.deadlines.Load(key); loaded {
		t.Fatal("expired deadline should be removed when no backoff is configured")
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
