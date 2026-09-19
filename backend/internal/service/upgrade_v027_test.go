package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type v027ResponseBindingStore struct {
	OpenAIWSStateStore
	bind func(context.Context) error
}

func (s *v027ResponseBindingStore) BindResponseAccount(ctx context.Context, _ int64, _ string, _ int64, _ time.Duration) error {
	return s.bind(ctx)
}

func TestV027HTTPResponseBindingSurvivesCancellationWithBoundedDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	store := &v027ResponseBindingStore{bind: func(ctx context.Context) error {
		called = true
		require.NoError(t, ctx.Err())
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.Positive(t, time.Until(deadline))
		require.LessOrEqual(t, time.Until(deadline), 750*time.Millisecond)
		return nil
	}}
	s := &OpenAIGatewayService{openaiWSStateStore: store}
	s.bindHTTPResponseAccount(ctx, &gin.Context{}, &Account{ID: 1}, "resp-test")
	require.True(t, called)
}

func TestV027StrictChatDestinationAndPreservation(t *testing.T) {
	body := []byte(`{ "large":9007199254740993,"messages":[{"role":"developer","content":"rules"},{"role":"assistant","content":null,"tool_calls":[{"id":"call1"}]},{"role":"assistant","reasoning_content":"real reasoning"}]}`)
	for _, platform := range []string{PlatformGrok, PlatformOpenCodeGo, PlatformAnthropic, PlatformOpenAI} {
		out, err := normalizeStrictChatRequest(&Account{Platform: platform, Type: AccountTypeAPIKey}, "https://unrelated.example/v1/chat/completions", body)
		require.NoError(t, err)
		require.Equal(t, body, out)
	}
	out, err := normalizeStrictChatRequest(&Account{Platform: PlatformDeepSeek, Type: AccountTypeAPIKey}, "https://proxy.example", body)
	require.NoError(t, err)
	require.Equal(t, "system", gjson.GetBytes(out, "messages.0.role").String())
	require.Equal(t, " ", gjson.GetBytes(out, "messages.1.reasoning_content").String())
	require.Equal(t, "real reasoning", gjson.GetBytes(out, "messages.2.reasoning_content").String())
	require.Contains(t, string(out), "9007199254740993")
	require.Equal(t, "developer", gjson.GetBytes(body, "messages.0.role").String())
	second, err := normalizeStrictChatRequest(&Account{Platform: PlatformDeepSeek, Type: AccountTypeAPIKey}, "", out)
	require.NoError(t, err)
	require.Equal(t, out, second)
}

func TestV027SeedanceCapabilityAndURL(t *testing.T) {
	a := newCodexModelsAPIKeyTestAccount("https://provider.example/v1")
	require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance))
	a.Credentials["seedance_enabled"] = true
	require.True(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance))
	a.Type = AccountTypeOAuth
	require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance))
	a.Type = AccountTypeAPIKey
	for _, base := range []string{"https://provider.example", "https://provider.example/v1", "https://provider.example/api/v3", "https://provider.example/api/v3/"} {
		got, err := buildSeedanceURL(base, "task-1")
		require.NoError(t, err)
		require.Equal(t, "https://provider.example/api/v3/contents/generations/tasks/task-1", got)
	}
	for _, id := range []string{"../escape", "a/b", "a?token=bad", "a#bad"} {
		_, err := buildSeedanceURL("https://provider.example", id)
		require.Error(t, err)
	}
}

func TestV027SeedanceUsageAndFrozenSettlement(t *testing.T) {
	for _, body := range []string{`{"status":"succeeded"}`, `{"status":"running","usage":{"completion_tokens":12}}`, `{"status":"succeeded","usage":{"completion_tokens":-1}}`, `{"status":"succeeded","usage":{"completion_tokens":"12"}}`, `{"status":"succeeded","usage":{"completion_tokens":1.5}}`} {
		_, _, ok := seedanceTerminalUsage([]byte(body))
		require.False(t, ok, body)
	}
	state, tokens, ok := seedanceTerminalUsage([]byte(`{"status":"succeeded","usage":{"completion_tokens":100}}`))
	require.True(t, ok)
	require.Equal(t, 100, tokens)
	task := &SeedanceTask{ID: "operation", State: state, CreatedAt: time.Now(), Snapshot: SeedanceSnapshot{BillingMode: BillingModeToken, UnitTotal: .001, UnitActual: .002, AccountMultiplier: 3, KeyQuota: true, KeyRateLimit: true, AccountQuota: true}}
	s := &SeedanceService{}
	cmd, usage, err := s.settlement(task, tokens)
	require.NoError(t, err)
	require.InDelta(t, .2, cmd.BalanceCost, 1e-9)
	require.InDelta(t, .3, cmd.AccountQuotaCost, 1e-9)
	require.InDelta(t, .2, cmd.APIKeyQuotaCost, 1e-9)
	require.Equal(t, 100, usage.OutputTokens)
	require.Equal(t, 1, usage.VideoCount)
	task.CreatedAt = time.Now().Add(-30 * 24 * time.Hour)
	_, lateUsage, err := s.settlement(task, tokens)
	require.NoError(t, err)
	require.Nil(t, lateUsage.DurationMs, "late reconciliation must not overflow the existing duration column")
	_, _, oversized := seedanceTerminalUsage([]byte(`{"status":"succeeded","usage":{"completion_tokens":2147483648}}`))
	require.False(t, oversized)
	task.Snapshot.BillingMode = BillingModePerRequest
	task.State = "failed"
	cmd, _, err = s.settlement(task, 0)
	require.NoError(t, err)
	require.Zero(t, cmd.BalanceCost)
	_, _, err = s.settlement(task, int(^uint(0)>>1))
	require.NoError(t, err) // per-request cost is independent of token count
	task.Snapshot.BillingMode = BillingModeToken
	_, _, err = s.settlement(task, int(^uint(0)>>1))
	require.Error(t, err)
}

func TestV027SeedanceSchedulerRequiresExplicitEligibleAPIKey(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		resetOpenAIAdvancedSchedulerSettingCacheForTest()
		t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
		groupID := int64(91827)
		accounts := []Account{
			{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}, Credentials: map[string]any{"base_url": "https://provider.example"}},
			{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}, Credentials: map[string]any{"base_url": "https://provider.example", "seedance_enabled": true}},
			{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, GroupIDs: []int64{groupID}, Credentials: map[string]any{"base_url": "https://provider.example", "seedance_enabled": true}},
		}
		s := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cache: &schedulerTestGatewayCache{}, cfg: &config.Config{}, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
		if advanced {
			s.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true")
		}
		selection, _, err := s.SelectAccountWithSchedulerForCapabilityOnPlatformLockedPriority(context.Background(), &groupID, "", "", "seedance-test", nil, OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilitySeedance, false, PlatformOpenAI, -1)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.Equal(t, int64(3), selection.Account.ID)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	}
}

type seedanceCaptureRepo struct {
	SeedanceTaskRepository
	provider, state string
	calls           int
}

func (r *seedanceCaptureRepo) Submitted(_ context.Context, _ string, id string, _ []byte, state string) error {
	r.provider = id
	r.state = state
	r.calls++
	return nil
}

func TestV027SeedanceUnknownNeverReplays(t *testing.T) {
	for _, reply := range []string{"transport", "missing", "numeric_id", "server_error", "timeout", "accepted", "rejected"} {
		t.Run(reply, func(t *testing.T) {
			calls := 0
			upstream := &codexModelsMemoryUpstream{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				calls++
				require.Equal(t, "Bearer sk-upstream", req.Header.Get("Authorization"))
				require.NoError(t, req.Context().Err())
				body, _ := io.ReadAll(req.Body)
				require.Equal(t, "ep-native", gjson.GetBytes(body, "model").String())
				require.Equal(t, "future", gjson.GetBytes(body, "unknown").String())
				if reply == "transport" {
					return nil, errors.New("disconnected after send")
				}
				status, data := 200, `{"id":"provider-task"}`
				if reply == "missing" {
					data = `{}`
				}
				if reply == "rejected" {
					status = 400
				}
				if reply == "numeric_id" {
					data = `{"id":123}`
				}
				if reply == "server_error" {
					status = 503
				}
				if reply == "timeout" {
					status = 408
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(data))}, nil
			}}
			repo := &seedanceCaptureRepo{}
			s := NewSeedanceService(repo, newCodexModelsAPIKeyTestService(upstream), nil)
			task := &SeedanceTask{ID: "local-id", Snapshot: SeedanceSnapshot{UpstreamModel: "ep-native"}}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, _, err := s.Create(ctx, newCodexModelsAPIKeyTestAccount("https://provider.example"), task, []byte(`{"model":"seedance","unknown":"future"}`))
			require.Equal(t, 1, calls)
			require.Equal(t, 1, repo.calls)
			switch reply {
			case "accepted":
				require.NoError(t, err)
				require.Equal(t, "provider-task", repo.provider)
				require.Equal(t, "queued", repo.state)
			case "rejected":
				require.NoError(t, err)
				require.Equal(t, "rejected", repo.state)
			default:
				require.Error(t, err)
				require.Equal(t, "submission_unknown", repo.state)
			}
		})
	}
}

func TestV027GeminiSDKScope(t *testing.T) {
	require.True(t, geminiClientRejectsSSEComments("google-genai-sdk/1.71 gl-go/go1.26"))
	require.True(t, geminiClientRejectsSSEComments("google-genai-sdk/1.71 gl-python/3.12"))
	require.False(t, geminiClientRejectsSSEComments("google-genai-sdk/1.71 gl-node/22"))
	require.False(t, geminiClientRejectsSSEComments("unrelated gl-go/1"))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Request.Header.Set("User-Agent", "google-genai-sdk/1.71")
	c.Request.Header.Set("X-Goog-Api-Client", "gl-python/3.12")
	require.True(t, downstreamRejectsSSEComments(c))
	c.Request.Header.Set("X-Goog-Api-Client", "gl-node/22")
	require.False(t, downstreamRejectsSSEComments(c))
}

func TestV027GeminiNativeCommentsRespectSDKWithoutChangingPayloadOrUsage(t *testing.T) {
	for _, runtime := range []string{"gl-go/go1.26", "gl-python/3.12", "gl-node/22"} {
		for _, oauth := range []bool{false, true} {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-test:streamGenerateContent", nil)
			c.Request.Header.Set("User-Agent", "google-genai-sdk/1.71")
			c.Request.Header.Set("X-Goog-Api-Client", runtime)
			payload := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2,"totalTokenCount":6}}`
			input := payload
			if oauth {
				input = `{"response":` + payload + `}`
			}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(": keepalive\n\ndata: " + input + "\n\n"))}
			svc := &GeminiMessagesCompatService{cfg: &config.Config{}}
			result, err := svc.handleNativeStreamingResponse(c, resp, time.Now(), oauth)
			require.NoError(t, err)
			require.False(t, result.failedOutcome)
			require.NotNil(t, result.usage)
			require.Equal(t, 4, result.usage.InputTokens)
			require.Equal(t, 2, result.usage.OutputTokens)
			require.Contains(t, w.Body.String(), "data: "+payload+"\n\n")
			if runtime == "gl-node/22" {
				require.Contains(t, w.Body.String(), ":\n")
			} else {
				require.NotContains(t, w.Body.String(), ":\n")
			}
		}
	}
}

func TestV027CNQuota403IsScopedAndUsesLatestExhaustedReset(t *testing.T) {
	a := &Account{Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{"account_mode": "coding"}, Extra: map[string]any{}}
	require.True(t, isCNProviderQuotaExhausted403(a, nil, "Usage limit exceeded"))
	require.False(t, isCNProviderQuotaExhausted403(a, nil, "Invalid API key"))
	require.True(t, isCNProviderQuotaExhausted403(a, []byte(`{"error":{"type":"access_terminated_error"}}`), ""))
	now := time.Now().UTC().Truncate(time.Second)
	a.Extra[cnExtraKey(a.Platform, cnExtraSuffix5hUsed)] = 100.0
	a.Extra[cnExtraKey(a.Platform, cnExtraSuffix5hReset)] = now.Add(time.Hour).Format(time.RFC3339)
	a.Extra[cnExtraKey(a.Platform, cnExtraSuffixWeeklyUsed)] = 100.0
	a.Extra[cnExtraKey(a.Platform, cnExtraSuffixWeeklyReset)] = now.Add(24 * time.Hour).Format(time.RFC3339)
	require.Equal(t, now.Add(24*time.Hour), cnQuota403Reset(a, now))
	a.Extra[cnExtraKey(a.Platform, cnExtraSuffixWeeklyUsed)] = 90.0
	require.Equal(t, now.Add(time.Hour), cnQuota403Reset(a, now))
	a.Credentials["account_mode"] = "apikey"
	require.False(t, isCNProviderQuotaExhausted403(a, nil, "usage limit"))
	a.Platform = PlatformOpenAI
	a.Credentials["account_mode"] = "coding"
	require.False(t, isCNProviderQuotaExhausted403(a, nil, "usage limit"))
}

func TestV027GeminiThinkingExplicitMappingWins(t *testing.T) {
	a := &Account{Platform: PlatformAntigravity, Credentials: map[string]any{"model_mapping": map[string]any{"gemini-3.8-flash-low": "low-target", "gemini-3.8-flash-high": "high-target"}}}
	got, ok := resolveGeminiThinkingVariant(a, "gemini-3.8-flash", []byte(`{"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`))
	require.True(t, ok)
	require.Equal(t, "low-target", got)
	a.Credentials["model_mapping"] = map[string]any{"gemini-*": "explicit-target"}
	_, ok = resolveGeminiThinkingVariant(a, "gemini-3.8-flash", nil)
	require.False(t, ok)
}

func TestV027GeminiThinkingVariantSelectionHonorsScopeCooldownAndPriority(t *testing.T) {
	account := Account{ID: 1, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Priority: 1, Extra: map[string]any{"mixed_scheduling": true}, Credentials: map[string]any{"model_mapping": map[string]any{"gemini-3.8-flash-low": "low-target", "gemini-3.8-flash-high": "high-target"}}}
	svc := &GeminiMessagesCompatService{}
	ctx := WithGeminiThinkingVariantRequest(context.Background(), []byte(`{"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`))
	require.False(t, svc.isAccountUsableForRequest(context.Background(), &account, "gemini-3.8-flash", PlatformGemini, true), "Messages keeps the original whitelist")
	require.True(t, svc.isAccountUsableForRequest(ctx, &account, "gemini-3.8-flash", PlatformGemini, true))
	require.False(t, svc.isAccountUsableForRequest(ctx, &account, "gemini-3.8-flash", PlatformGemini, false), "mixed scheduling still requires opt-in")
	second := account
	second.ID, second.Priority = 2, 5
	selected := svc.selectBestGeminiAccount(ctx, nil, []Account{second, account}, "gemini-3.8-flash", nil, PlatformGemini, true, AccountSchedulingStrategyStrictPriority, 0)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.ID)
	account.Extra = map[string]any{"mixed_scheduling": true, "model_rate_limits": map[string]any{"low-target": map[string]any{"rate_limit_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)}}}
	require.False(t, svc.isAccountUsableForRequest(ctx, &account, "gemini-3.8-flash", PlatformGemini, true), "the chosen variant's cooldown must apply")
	selected = svc.selectBestGeminiAccount(ctx, nil, []Account{account, second}, "gemini-3.8-flash", nil, PlatformGemini, true, AccountSchedulingStrategyStrictPriority, 0)
	require.NotNil(t, selected)
	require.Equal(t, int64(2), selected.ID)
	account.Credentials = map[string]any{"model_mapping": map[string]any{"gemini-3.8-flash": "explicit-target", "gemini-3.8-flash-low": "low-target"}}
	require.Equal(t, "gemini-3.8-flash", geminiThinkingVariantSchedulingModel(ctx, &account, "gemini-3.8-flash"))
	account.Platform = PlatformGemini
	require.Equal(t, "gemini-3.8-flash", geminiThinkingVariantSchedulingModel(ctx, &account, "gemini-3.8-flash"))
}

func TestV027GeminiThinkingVariantForwardChecksActualVariantCooldown(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.8-flash:generateContent", strings.NewReader(string(body)))
	account := &Account{ID: 1, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "test-token", "model_mapping": map[string]any{"gemini-3.8-flash-low": "low-target"}}, Extra: map[string]any{"model_rate_limits": map[string]any{"low-target": map[string]any{"rate_limit_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)}}}}
	calls := 0
	upstream := &codexModelsMemoryUpstream{do: func(*http.Request, string, int64, int) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected request")
	}}
	svc := &AntigravityGatewayService{tokenProvider: &AntigravityTokenProvider{}, httpUpstream: upstream}
	result, err := svc.ForwardGemini(context.Background(), c, account, "gemini-3.8-flash", "generateContent", false, body, false)
	require.Nil(t, result)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, http.StatusServiceUnavailable, failover.StatusCode)
	require.Zero(t, calls)
}

func TestV027SeedanceIdempotencyIdentityIsOwnerScoped(t *testing.T) {
	key := &APIKey{ID: 2, UserID: 1}
	first, err := SeedanceIdempotencyID(key, "request-1")
	require.NoError(t, err)
	second, err := SeedanceIdempotencyID(key, "request-1")
	require.NoError(t, err)
	require.Equal(t, first, second)
	key.ID = 3
	other, err := SeedanceIdempotencyID(key, "request-1")
	require.NoError(t, err)
	require.NotEqual(t, first, other)
	_, err = SeedanceIdempotencyID(key, strings.Repeat("a", 257))
	require.Error(t, err)
}

func TestV027SeedanceMissingPriceIsNotFree(t *testing.T) {
	resolver := &ModelPricingResolver{}
	for _, mode := range []BillingMode{BillingModeToken, BillingModePerRequest} {
		pricing := &ResolvedPricing{Mode: mode, Source: PricingSourceGroup, BasePricing: &ModelPricing{}, channelPricing: &ChannelModelPricing{}}
		require.False(t, seedancePricingComplete(resolver, pricing))
		zero := 0.0
		if mode == BillingModeToken {
			pricing.channelPricing.OutputPrice = &zero
		} else {
			pricing.channelPricing.PerRequestPrice = &zero
		}
		require.True(t, seedancePricingComplete(resolver, pricing), "explicit zero remains supported")
	}
}

type v027SeedanceWorkerRepo struct {
	SeedanceTaskRepository
	task              *SeedanceTask
	settled, observed int
}

func (r *v027SeedanceWorkerRepo) Claim(context.Context) (*SeedanceTask, error) { return r.task, nil }
func (r *v027SeedanceWorkerRepo) Observe(context.Context, string, []byte, string, time.Duration) error {
	r.observed++
	return nil
}
func (r *v027SeedanceWorkerRepo) Settle(context.Context, *SeedanceTask, *UsageBillingCommand, *UsageLog) (*UsageBillingApplyResult, error) {
	r.settled++
	return &UsageBillingApplyResult{Applied: true}, nil
}
func (r *v027SeedanceWorkerRepo) CompleteEffects(context.Context, string) error     { return nil }
func (r *v027SeedanceWorkerRepo) SaveTerminal(context.Context, *SeedanceTask) error { return nil }

type v027SeedanceOwnerRepo struct {
	AccountRepository
	account *Account
}

func (r *v027SeedanceOwnerRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func TestV027SeedanceWorkerRequiresMatchingTaskAndRealUsage(t *testing.T) {
	for _, scenario := range []string{"complete", "missing_usage", "different_id", "credential_changed"} {
		t.Run(scenario, func(t *testing.T) {
			a := newCodexModelsAPIKeyTestAccount("https://provider.example")
			a.Credentials["seedance_enabled"] = false // Existing task lookup survives disabling generation.
			calls := 0
			u := &codexModelsMemoryUpstream{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				calls++
				require.Equal(t, "GET", req.Method)
				body := `{"id":"task1","status":"succeeded","usage":{"completion_tokens":25}}`
				if scenario == "missing_usage" {
					body = `{"id":"task1","status":"succeeded"}`
				}
				if scenario == "different_id" {
					body = `{"id":"other-task","status":"succeeded","usage":{"completion_tokens":25}}`
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			}}
			gateway := newCodexModelsAPIKeyTestService(u)
			gateway.accountRepo = &v027SeedanceOwnerRepo{account: a}
			task := &SeedanceTask{ID: "local", ProviderID: "task1", CreatedAt: time.Now(), Snapshot: SeedanceSnapshot{UpstreamBaseURL: "https://provider.example", CredentialFingerprint: HashUsageRequestPayload([]byte("sk-upstream")), BillingMode: BillingModeToken, UnitTotal: .1, UnitActual: .2, AccountMultiplier: 1}}
			if scenario == "credential_changed" {
				a.Credentials["api_key"] = "different-principal"
			}
			repo := &v027SeedanceWorkerRepo{task: task}
			s := NewSeedanceService(repo, gateway, nil)
			s.pollOne(context.Background())
			if scenario == "complete" {
				require.Equal(t, 1, repo.settled)
			} else {
				require.Zero(t, repo.settled)
			}
			if scenario == "credential_changed" {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
			}
		})
	}
}
