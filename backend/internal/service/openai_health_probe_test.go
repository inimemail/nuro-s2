package service

import (
	"bytes"
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

func TestConfigureOpenAIResponsesHealthProbe(t *testing.T) {
	setGinTestMode()
	body := healthProbeRequestBody()

	for _, model := range []string{"gpt-5.4", "gpt-5.5", "o4-mini"} {
		t.Run("valid profile "+model, func(t *testing.T) {
			modelBody := healthProbeRequestBodyForModel(model)
			c, _ := healthProbeTestContext(modelBody, OpenAIHealthProbeProfileResponsesV1)
			enabled, err := ConfigureOpenAIResponsesHealthProbe(c, modelBody, model, false)
			require.NoError(t, err)
			require.True(t, enabled)
			require.True(t, IsOpenAIResponsesHealthProbe(c))
			storedModel, ok := OpenAIResponsesHealthProbeModel(c)
			require.True(t, ok)
			require.Equal(t, model, storedModel)
		})
	}

	t.Run("valid profile without reasoning", func(t *testing.T) {
		modelBody := []byte(`{"model":"gpt-4o","instructions":"Return exactly MONITOR_OK as plain text.","input":"Return exactly MONITOR_OK.","max_output_tokens":512,"stream":false,"store":false}`)
		c, _ := healthProbeTestContext(modelBody, OpenAIHealthProbeProfileResponsesV1)
		enabled, err := ConfigureOpenAIResponsesHealthProbe(c, modelBody, "gpt-4o", false)
		require.NoError(t, err)
		require.True(t, enabled)
	})

	tests := []struct {
		name    string
		profile string
		body    []byte
		model   string
		stream  bool
	}{
		{name: "no profile", body: body, model: "gpt-5.5"},
		{name: "unknown profile", profile: "unknown", body: body, model: "gpt-5.5"},
		{name: "mismatched model", profile: OpenAIHealthProbeProfileResponsesV1, body: body, model: "gpt-5.4"},
		{name: "empty model", profile: OpenAIHealthProbeProfileResponsesV1, body: body},
		{name: "streaming", profile: OpenAIHealthProbeProfileResponsesV1, body: body, model: "gpt-5.5", stream: true},
		{name: "missing stream", profile: OpenAIHealthProbeProfileResponsesV1, body: []byte(`{"model":"gpt-5.5","instructions":"Return exactly MONITOR_OK as plain text.","input":"Return exactly MONITOR_OK.","reasoning":{"effort":"none"},"max_output_tokens":512,"store":false}`), model: "gpt-5.5"},
		{name: "tools", profile: OpenAIHealthProbeProfileResponsesV1, body: []byte(`{"model":"gpt-5.5","instructions":"reply","input":"ok","tools":[{"type":"function","name":"x"}],"stream":false}`), model: "gpt-5.5"},
		{name: "prompt cache key", profile: OpenAIHealthProbeProfileResponsesV1, body: []byte(`{"model":"gpt-5.5","instructions":"Return exactly MONITOR_OK as plain text.","input":"Return exactly MONITOR_OK.","prompt_cache_key":"business-cache","max_output_tokens":512,"stream":false,"store":false}`), model: "gpt-5.5"},
		{name: "metadata", profile: OpenAIHealthProbeProfileResponsesV1, body: []byte(`{"model":"gpt-5.5","instructions":"Return exactly MONITOR_OK as plain text.","input":"Return exactly MONITOR_OK.","metadata":{"source":"business"},"max_output_tokens":512,"stream":false,"store":false}`), model: "gpt-5.5"},
		{name: "reasoning extra field", profile: OpenAIHealthProbeProfileResponsesV1, body: []byte(`{"model":"gpt-5.5","instructions":"Return exactly MONITOR_OK as plain text.","input":"Return exactly MONITOR_OK.","reasoning":{"effort":"none","summary":"auto"},"max_output_tokens":512,"stream":false,"store":false}`), model: "gpt-5.5"},
		{name: "excessive output", profile: OpenAIHealthProbeProfileResponsesV1, body: []byte(`{"model":"gpt-5.5","instructions":"reply","input":"ok","max_output_tokens":2048,"stream":false}`), model: "gpt-5.5"},
		{name: "fractional output", profile: OpenAIHealthProbeProfileResponsesV1, body: []byte(`{"model":"gpt-5.5","instructions":"Return exactly MONITOR_OK as plain text.","input":"Return exactly MONITOR_OK.","reasoning":{"effort":"none"},"max_output_tokens":512.5,"stream":false,"store":false}`), model: "gpt-5.5"},
		{name: "unsupported reasoning", profile: OpenAIHealthProbeProfileResponsesV1, body: []byte(`{"model":"gpt-5.5","instructions":"Return exactly MONITOR_OK as plain text.","input":"Return exactly MONITOR_OK.","reasoning":{"effort":"low"},"max_output_tokens":512,"stream":false,"store":false}`), model: "gpt-5.5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := healthProbeTestContext(tt.body, tt.profile)
			enabled, err := ConfigureOpenAIResponsesHealthProbe(c, tt.body, tt.model, tt.stream)
			if tt.profile == "" {
				require.NoError(t, err)
				require.False(t, enabled)
				return
			}
			require.Error(t, err)
			require.False(t, enabled)
			require.False(t, IsOpenAIResponsesHealthProbe(c))
		})
	}
}

func TestOpenAIHealthProbeEmptyJSONTriggersRequestLocalFailover(t *testing.T) {
	setGinTestMode()
	body := []byte(`{"id":"resp_empty","object":"response","model":"gpt-5.5","output":[{"type":"reasoning","summary":[]}],"usage":{"input_tokens":12,"output_tokens":4,"total_tokens":16}}`)
	c, recorder := configuredHealthProbeContext(t)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"req-empty"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	account := &Account{ID: 91, Name: "probe-account", Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	service := &OpenAIGatewayService{cfg: &config.Config{}}

	result, err := service.handleNonStreamingResponse(context.Background(), response, c, account, "gpt-5.5", "gpt-5.5")
	require.Nil(t, result)
	require.Error(t, err)
	require.Empty(t, recorder.Body.String())

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.True(t, failoverErr.SkipPoolSoftCooldown)
	require.True(t, failoverErr.SkipPromptCacheAvoidance)
	require.True(t, failoverErr.SkipStickySessionEviction)
	require.True(t, failoverErr.SkipSchedulePenalty)
	require.Equal(t, "gpt-5.5", failoverErr.ProbeModel)
	require.True(t, IsOpenAIHealthProbeEmptyErrorBody(failoverErr.ResponseBody))
	require.Equal(t, "req-empty", failoverErr.ResponseHeaders.Get("x-request-id"))
}

func TestOpenAIHealthProbeEmptySSEResponseTriggersFailover(t *testing.T) {
	setGinTestMode()
	c, recorder := configuredHealthProbeContext(t)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.reasoning_summary_text.delta","delta":"thinking"}`,
			`data: {"type":"response.completed","response":{"id":"resp_empty_sse","object":"response","model":"gpt-5.5","output":[{"type":"reasoning","summary":[]}],"usage":{"input_tokens":12,"output_tokens":4,"total_tokens":16}}}`,
			`data: [DONE]`,
		}, "\n"))),
	}
	service := &OpenAIGatewayService{cfg: &config.Config{}}

	result, err := service.handleNonStreamingResponse(context.Background(), response, c, &Account{ID: 92, Platform: PlatformOpenAI}, "gpt-5.5", "gpt-5.5")
	require.Nil(t, result)
	require.Error(t, err)
	require.Empty(t, recorder.Body.String())
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.True(t, IsOpenAIHealthProbeEmptyErrorBody(failoverErr.ResponseBody))
}

func TestOpenAIHealthProbeNonEmptyResponsePassesThrough(t *testing.T) {
	setGinTestMode()
	c, recorder := configuredHealthProbeContext(t)
	body := []byte(`{"id":"resp_ok","object":"response","model":"gpt-5.5","output":[{"type":"reasoning","summary":[]},{"type":"message","content":[{"type":"output_text","text":"MONITOR_OK"}]}],"usage":{"input_tokens":12,"output_tokens":2,"total_tokens":14}}`)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	service := &OpenAIGatewayService{cfg: &config.Config{}}

	result, err := service.handleNonStreamingResponse(context.Background(), response, c, &Account{ID: 93, Platform: PlatformOpenAI}, "gpt-5.5", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, recorder.Body.String(), "MONITOR_OK")
}

func TestOpenAIHealthProbeMarkerIsNotForwardedUpstream(t *testing.T) {
	header := strings.ToLower(OpenAIHealthProbeHeader)
	require.False(t, openaiAllowedHeaders[header])
	require.False(t, openaiPassthroughAllowedHeaders[header])
}

func TestOpenAIHealthProbeUpstreamContextKeepsCancellation(t *testing.T) {
	c, _ := configuredHealthProbeContext(t)
	parent, cancel := context.WithCancel(WithOpenAIHealthProbeRequestContext(context.Background()))
	upstream, release := openAIUpstreamRequestContext(parent, c)
	release()
	require.True(t, IsOpenAIHealthProbeRequestContext(upstream))
	cancel()
	require.ErrorIs(t, upstream.Err(), context.Canceled)
}

func TestOpenAIHealthProbeUpstreamErrorDoesNotBlockAccountScheduling(t *testing.T) {
	probeAccount := &Account{ID: 95, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	probeService := &OpenAIGatewayService{}
	probeService.handleOpenAIAccountUpstreamError(
		WithOpenAIHealthProbeRequestContext(context.Background()),
		probeAccount,
		http.StatusTooManyRequests,
		nil,
		nil,
	)
	require.False(t, probeService.isOpenAIAccountRuntimeBlocked(probeAccount))

	businessAccount := &Account{ID: 96, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	businessService := &OpenAIGatewayService{}
	businessService.handleOpenAIAccountUpstreamError(context.Background(), businessAccount, http.StatusTooManyRequests, nil, nil)
	require.True(t, businessService.isOpenAIAccountRuntimeBlocked(businessAccount))
}

func TestOpenAIHealthProbeDoesNotPersistCodexUsageSnapshot(t *testing.T) {
	usedPercent := 25.0
	repo := &snapshotUpdateAccountRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{},
		updateExtraCalls:      make(chan map[string]any, 1),
	}
	service := &OpenAIGatewayService{accountRepo: repo}
	service.updateCodexUsageSnapshot(
		WithOpenAIHealthProbeRequestContext(context.Background()),
		97,
		&OpenAICodexUsageSnapshot{PrimaryUsedPercent: &usedPercent},
	)
	select {
	case <-repo.updateExtraCalls:
		t.Fatal("health probe must not persist Codex usage snapshots")
	default:
	}
}

func TestOpenAIHealthProbeDoesNotBindResponseAccount(t *testing.T) {
	c, _ := configuredHealthProbeContext(t)
	service := &OpenAIGatewayService{}
	service.bindHTTPResponseAccount(context.Background(), c, &Account{ID: 98, Platform: PlatformOpenAI}, "resp_health_probe")

	accountID, err := service.getOpenAIWSStateStore().GetResponseAccount(context.Background(), 0, "resp_health_probe")
	require.NoError(t, err)
	require.Zero(t, accountID)
}

func TestOpenAIHealthProbeSessionIsRequestLocalAndCleaned(t *testing.T) {
	first := NewOpenAIHealthProbeSessionHash()
	second := NewOpenAIHealthProbeSessionHash()
	require.NotEqual(t, first, second)
	require.True(t, IsOpenAIHealthProbeSessionHash(first))
	require.False(t, IsOpenAIPromptCacheBoostAffinitySessionHash(first))

	cache := &stubGatewayCache{sessionBindings: map[string]int64{"openai:" + first: 91, "openai:business-session": 92}}
	service := &OpenAIGatewayService{cache: cache}
	service.ReleaseOpenAIHealthProbeSession(context.Background(), nil, first)
	require.NotContains(t, cache.sessionBindings, "openai:"+first)
	require.Equal(t, int64(92), cache.sessionBindings["openai:business-session"])

	service.ReleaseOpenAIHealthProbeSession(context.Background(), nil, "business-session")
	require.Equal(t, int64(92), cache.sessionBindings["openai:business-session"])
}

func TestIsolateOpenAIHealthProbeFailoverOnlyChangesMarkedRequests(t *testing.T) {
	marked, _ := configuredHealthProbeContext(t)
	markedErr := &UpstreamFailoverError{StatusCode: http.StatusTooManyRequests}
	IsolateOpenAIHealthProbeFailover(marked, markedErr)
	require.True(t, markedErr.SkipPoolSoftCooldown)
	require.True(t, markedErr.SkipPromptCacheAvoidance)
	require.True(t, markedErr.SkipStickySessionEviction)
	require.True(t, markedErr.SkipSchedulePenalty)

	unmarked, _ := healthProbeTestContext(healthProbeRequestBody(), "")
	unmarkedErr := &UpstreamFailoverError{StatusCode: http.StatusTooManyRequests}
	IsolateOpenAIHealthProbeFailover(unmarked, unmarkedErr)
	require.False(t, unmarkedErr.SkipPoolSoftCooldown)
	require.False(t, unmarkedErr.SkipPromptCacheAvoidance)
	require.False(t, unmarkedErr.SkipStickySessionEviction)
	require.False(t, unmarkedErr.SkipSchedulePenalty)
}

func TestApplyOpenAIHealthProbeRetryPolicyUsesPoolConditionsPlusEmptyResponse(t *testing.T) {
	marked, _ := configuredHealthProbeContext(t)
	account := &Account{
		ID:       96,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                    true,
			"pool_mode_retry_status_codes": []any{float64(http.StatusTeapot)},
		},
	}

	tests := []struct {
		name       string
		statusCode int
		body       []byte
		message    string
		expected   bool
	}{
		{
			name:       "pool built-in server error",
			statusCode: http.StatusBadGateway,
			expected:   true,
		},
		{
			name:       "account configured status",
			statusCode: http.StatusTeapot,
			expected:   true,
		},
		{
			name:       "status outside pool retry conditions",
			statusCode: http.StatusConflict,
			expected:   false,
		},
		{
			name:       "explicit client error remains non-retryable",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":{"message":"maximum context length exceeded","type":"invalid_request_error"}}`),
			message:    "maximum context length exceeded",
			expected:   false,
		},
		{
			name:       "probe-only upstream 2xx empty marker",
			statusCode: http.StatusBadGateway,
			body:       openAIHealthProbeErrorBody(),
			expected:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failoverErr := &UpstreamFailoverError{
				StatusCode:             tt.statusCode,
				ResponseBody:           tt.body,
				Message:                tt.message,
				RetryableOnSameAccount: !tt.expected,
			}

			ApplyOpenAIHealthProbeRetryPolicy(marked, account, failoverErr)

			require.Equal(t, tt.expected, failoverErr.RetryableOnSameAccount)
		})
	}
}

func TestApplyOpenAIHealthProbeRetryPolicyDoesNotChangeNormalRequests(t *testing.T) {
	unmarked, _ := healthProbeTestContext(healthProbeRequestBody(), "")
	failoverErr := &UpstreamFailoverError{StatusCode: http.StatusConflict, RetryableOnSameAccount: true}

	ApplyOpenAIHealthProbeRetryPolicy(unmarked, &Account{}, failoverErr)

	require.True(t, failoverErr.RetryableOnSameAccount)
}

func TestApplyOpenAIHealthProbeRetryPolicyPreservesNonPoolDecision(t *testing.T) {
	marked, _ := configuredHealthProbeContext(t)
	account := &Account{ID: 97, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	retryableErr := &UpstreamFailoverError{StatusCode: http.StatusConflict, RetryableOnSameAccount: true}
	ApplyOpenAIHealthProbeRetryPolicy(marked, account, retryableErr)
	require.True(t, retryableErr.RetryableOnSameAccount)

	nonRetryableErr := &UpstreamFailoverError{StatusCode: http.StatusConflict}
	ApplyOpenAIHealthProbeRetryPolicy(marked, account, nonRetryableErr)
	require.False(t, nonRetryableErr.RetryableOnSameAccount)
}

func TestOpenAIHealthProbeDefaultFallbackOnlyStartsOnceForEmptyResponse(t *testing.T) {
	marked, _ := configuredHealthProbeContext(t)
	marked.Request.Header.Set("X-Custom-Probe", "preserved")
	originalHeaders := marked.Request.Header.Clone()
	emptyErr := &UpstreamFailoverError{ResponseBody: openAIHealthProbeErrorBody()}

	require.True(t, ShouldStartOpenAIHealthProbeDefaultFallback(marked, emptyErr, false))
	require.False(t, ShouldStartOpenAIHealthProbeDefaultFallback(marked, emptyErr, true))
	require.False(t, ShouldStartOpenAIHealthProbeDefaultFallback(marked, &UpstreamFailoverError{
		ResponseBody: []byte(`{"error":{"message":"ordinary upstream error"}}`),
	}, false))

	unmarked, _ := healthProbeTestContext(healthProbeRequestBody(), "")
	require.False(t, ShouldStartOpenAIHealthProbeDefaultFallback(unmarked, emptyErr, false))
	require.Equal(t, originalHeaders, marked.Request.Header)
	require.Equal(t, OpenAIHealthProbeProfileResponsesV1, marked.GetHeader(OpenAIHealthProbeHeader))
}

func TestBuildOpenAIHealthProbeDefaultFallbackBodyUsesDefaultResponsesShape(t *testing.T) {
	body, err := BuildOpenAIHealthProbeDefaultFallbackBody(" gpt-5.5 ")
	require.NoError(t, err)
	require.Equal(t, "gpt-5.5", gjson.GetBytes(body, "model").String())
	require.Equal(t, false, gjson.GetBytes(body, "stream").Bool())
	require.Equal(t, float64(monitorChallengeMaxTokens), gjson.GetBytes(body, "max_output_tokens").Float())
	require.NotEmpty(t, strings.TrimSpace(gjson.GetBytes(body, "instructions").String()))
	require.NotEqual(t, openAIHealthProbeInput, gjson.GetBytes(body, "input").String())
	require.False(t, gjson.GetBytes(body, "reasoning").Exists())
	require.False(t, gjson.GetBytes(body, "store").Exists())
}

func TestOpenAIHealthProbeDoesNotChangeUnmarkedEmptyResponse(t *testing.T) {
	setGinTestMode()
	body := healthProbeRequestBody()
	c, recorder := healthProbeTestContext(body, "")
	responseBody := []byte(`{"id":"resp_empty","object":"response","model":"gpt-5.5","output":[],"usage":{"input_tokens":4,"output_tokens":0,"total_tokens":4}}`)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(responseBody)),
	}
	service := &OpenAIGatewayService{cfg: &config.Config{}}

	result, err := service.handleNonStreamingResponse(context.Background(), response, c, &Account{ID: 94, Platform: PlatformOpenAI}, "gpt-5.5", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, recorder.Body.String(), `"output":[]`)
}

func TestHasOpenAIHealthProbeAlternativeAccountFiltersCandidates(t *testing.T) {
	groupID := int64(901)
	current := healthProbeAlternativeAccount(201, 3, groupID)
	alternative := healthProbeAlternativeAccount(202, 3, groupID)
	differentPriority := healthProbeAlternativeAccount(203, 4, groupID)
	req := OpenAIAccountScheduleRequest{
		GroupID:            &groupID,
		RequestedModel:     "gpt-5.5",
		RequiredTransport:  OpenAIUpstreamTransportAny,
		RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
		RequestPlatform:    PlatformOpenAI,
	}

	t.Run("same priority untried account is available", func(t *testing.T) {
		svc := healthProbeAlternativeService([]Account{current, alternative, differentPriority}, nil)
		require.True(t, svc.HasOpenAIHealthProbeAlternativeAccount(context.Background(), &current, req))
	})

	t.Run("excluded account is not available", func(t *testing.T) {
		svc := healthProbeAlternativeService([]Account{current, alternative}, nil)
		excludedReq := req
		excludedReq.ExcludedIDs = map[int64]struct{}{alternative.ID: {}}
		require.False(t, svc.HasOpenAIHealthProbeAlternativeAccount(context.Background(), &current, excludedReq))
	})

	t.Run("different priority is not available", func(t *testing.T) {
		svc := healthProbeAlternativeService([]Account{current, differentPriority}, nil)
		require.False(t, svc.HasOpenAIHealthProbeAlternativeAccount(context.Background(), &current, req))
	})

	t.Run("different group is not available", func(t *testing.T) {
		otherGroup := healthProbeAlternativeAccount(204, 3, groupID+1)
		svc := healthProbeAlternativeService([]Account{current, otherGroup}, nil)
		require.False(t, svc.HasOpenAIHealthProbeAlternativeAccount(context.Background(), &current, req))
	})

	t.Run("runtime blocked account is not available", func(t *testing.T) {
		svc := healthProbeAlternativeService([]Account{current, alternative}, nil)
		svc.openaiAccountRuntimeBlockUntil.Store(alternative.ID, time.Now().Add(time.Minute))
		require.False(t, svc.HasOpenAIHealthProbeAlternativeAccount(context.Background(), &current, req))
	})

	t.Run("soft cooling pool account is not available", func(t *testing.T) {
		poolAlternative := alternative
		poolAlternative.Credentials = map[string]interface{}{"pool_mode": true}
		svc := healthProbeAlternativeService([]Account{current, poolAlternative}, nil)
		svc.openaiPoolSoftCooldownUntil.Store(poolAlternative.ID, time.Now().Add(time.Minute))
		require.False(t, svc.HasOpenAIHealthProbeAlternativeAccount(context.Background(), &current, req))
	})
}

func TestHasOpenAIHealthProbeAlternativeAccountRequiresReliableCapacity(t *testing.T) {
	groupID := int64(902)
	current := healthProbeAlternativeAccount(211, 2, groupID)
	alternative := healthProbeAlternativeAccount(212, 2, groupID)
	req := OpenAIAccountScheduleRequest{
		GroupID:            &groupID,
		RequestedModel:     "gpt-5.5",
		RequiredTransport:  OpenAIUpstreamTransportAny,
		RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
		RequestPlatform:    PlatformOpenAI,
	}

	t.Run("fully loaded account is not available", func(t *testing.T) {
		cache := schedulerTestConcurrencyCache{loadMap: map[int64]*AccountLoadInfo{
			alternative.ID: {AccountID: alternative.ID, LoadRate: 100},
		}}
		svc := healthProbeAlternativeService([]Account{current, alternative}, cache)
		require.False(t, svc.HasOpenAIHealthProbeAlternativeAccount(context.Background(), &current, req))
	})

	t.Run("load lookup error keeps current account budget", func(t *testing.T) {
		cache := schedulerTestConcurrencyCache{loadBatchErr: errors.New("load unavailable")}
		svc := healthProbeAlternativeService([]Account{current, alternative}, cache)
		require.False(t, svc.HasOpenAIHealthProbeAlternativeAccount(context.Background(), &current, req))
	})

	t.Run("account below capacity is available", func(t *testing.T) {
		cache := schedulerTestConcurrencyCache{loadMap: map[int64]*AccountLoadInfo{
			alternative.ID: {AccountID: alternative.ID, LoadRate: 99},
		}}
		svc := healthProbeAlternativeService([]Account{current, alternative}, cache)
		require.True(t, svc.HasOpenAIHealthProbeAlternativeAccount(context.Background(), &current, req))
	})
}

func TestHasOpenAIHealthProbePeerAccountIgnoresTemporaryCapacity(t *testing.T) {
	groupID := int64(904)
	current := healthProbeAlternativeAccount(231, 2, groupID)
	peer := healthProbeAlternativeAccount(232, 2, groupID)
	req := OpenAIAccountScheduleRequest{
		GroupID:            &groupID,
		RequestedModel:     "gpt-5.5",
		RequiredTransport:  OpenAIUpstreamTransportAny,
		RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
		RequestPlatform:    PlatformOpenAI,
	}
	cache := schedulerTestConcurrencyCache{loadMap: map[int64]*AccountLoadInfo{
		peer.ID: {AccountID: peer.ID, LoadRate: 100},
	}}

	hasPeer, known := healthProbeAlternativeService([]Account{current, peer}, cache).
		HasOpenAIHealthProbePeerAccount(context.Background(), &current, req)
	require.True(t, known)
	require.True(t, hasPeer)

	hasPeer, known = healthProbeAlternativeService([]Account{current}, cache).
		HasOpenAIHealthProbePeerAccount(context.Background(), &current, req)
	require.True(t, known)
	require.False(t, hasPeer)
}

func TestOpenAIHealthProbePeerLookupKeepsPartialCandidatesWithoutClaimingCompleteness(t *testing.T) {
	groupID := int64(905)
	current := healthProbeAlternativeAccount(241, 2, groupID)
	brokenPeer := healthProbeAlternativeAccount(242, 2, groupID)
	validPeer := healthProbeAlternativeAccount(243, 2, groupID)
	accounts := []Account{current, brokenPeer, validPeer}
	snapshotAccounts := make([]*Account, 0, len(accounts))
	accountsByID := make(map[int64]*Account, len(accounts))
	for i := range accounts {
		account := accounts[i]
		snapshotAccounts = append(snapshotAccounts, &account)
		accountsByID[account.ID] = &account
	}
	cache := &openAISnapshotCacheStub{
		snapshotAccounts: snapshotAccounts,
		accountsByID:     accountsByID,
		accountErrors: map[int64]error{
			brokenPeer.ID: errors.New("snapshot refresh failed"),
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts},
		schedulerSnapshot: &SchedulerSnapshotService{
			cache:       cache,
			accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{current, validPeer}},
			groupRepo: &schedulerTestGroupRepo{groups: map[int64]*Group{
				groupID: {ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true},
			}},
		},
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{loadMap: map[int64]*AccountLoadInfo{
			validPeer.ID: {AccountID: validPeer.ID, LoadRate: 0},
		}}),
	}
	req := OpenAIAccountScheduleRequest{
		GroupID:            &groupID,
		RequestedModel:     "gpt-5.5",
		RequiredTransport:  OpenAIUpstreamTransportAny,
		RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
		RequestPlatform:    PlatformOpenAI,
	}

	require.True(t, svc.HasOpenAIHealthProbeAlternativeAccount(context.Background(), &current, req),
		"a valid partial candidate must remain usable for capacity-aware retry")
	hasPeer, known := svc.HasOpenAIHealthProbePeerAccount(context.Background(), &current, req)
	require.True(t, hasPeer)
	require.False(t, known, "partial refresh failures must prevent a single-account conclusion")
}

func TestHasOpenAIHealthProbeAlternativeAccountDoesNotWarmSchedulingLoadCache(t *testing.T) {
	groupID := int64(903)
	current := healthProbeAlternativeAccount(221, 2, groupID)
	alternative := healthProbeAlternativeAccount(222, 2, groupID)
	loadBatchCalls := 0
	cache := schedulerTestConcurrencyCache{loadBatchCalls: &loadBatchCalls}
	svc := healthProbeAlternativeService([]Account{current, alternative}, cache)
	req := OpenAIAccountScheduleRequest{
		GroupID:            &groupID,
		RequestedModel:     "gpt-5.5",
		RequiredTransport:  OpenAIUpstreamTransportAny,
		RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
		RequestPlatform:    PlatformOpenAI,
	}

	require.True(t, svc.HasOpenAIHealthProbeAlternativeAccount(context.Background(), &current, req))
	require.Equal(t, 1, loadBatchCalls)

	loadReq := []AccountWithConcurrency{{ID: alternative.ID, MaxConcurrency: alternative.EffectiveLoadFactor()}}
	_, err := svc.concurrencyService.GetAccountsLoadBatch(context.Background(), loadReq)
	require.NoError(t, err)
	require.Equal(t, 2, loadBatchCalls)
	_, err = svc.concurrencyService.GetAccountsLoadBatch(context.Background(), loadReq)
	require.NoError(t, err)
	require.Equal(t, 2, loadBatchCalls)
}

func healthProbeAlternativeAccount(id int64, priority int, groupID int64) Account {
	return Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 10,
		Priority:    priority,
		GroupIDs:    []int64{groupID},
	}
}

func healthProbeAlternativeService(accounts []Account, concurrencyCache ConcurrencyCache) *OpenAIGatewayService {
	svc := &OpenAIGatewayService{accountRepo: stubOpenAIAccountRepo{accounts: accounts}}
	if concurrencyCache != nil {
		svc.concurrencyService = NewConcurrencyService(concurrencyCache)
	}
	return svc
}

func configuredHealthProbeContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	body := healthProbeRequestBody()
	c, recorder := healthProbeTestContext(body, OpenAIHealthProbeProfileResponsesV1)
	enabled, err := ConfigureOpenAIResponsesHealthProbe(c, body, "gpt-5.5", false)
	require.NoError(t, err)
	require.True(t, enabled)
	return c, recorder
}

func healthProbeTestContext(body []byte, profile string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if profile != "" {
		c.Request.Header.Set(OpenAIHealthProbeHeader, profile)
	}
	return c, recorder
}

func healthProbeRequestBody() []byte {
	return healthProbeRequestBodyForModel("gpt-5.5")
}

func healthProbeRequestBodyForModel(model string) []byte {
	return []byte(`{"model":"` + model + `","instructions":"Return exactly MONITOR_OK as plain text.","input":"Return exactly MONITOR_OK.","reasoning":{"effort":"none"},"max_output_tokens":512,"stream":false,"store":false}`)
}
