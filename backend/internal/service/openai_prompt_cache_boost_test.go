package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func promptCacheBoostTestConfig() *config.Config {
	return &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false},
		},
	}
}

func promptCacheBoostTestAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Name:        "openai-pcache-boost",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":                    "sk-test",
			"base_url":                   "https://api.openai.com/v1",
			"pool_mode":                  true,
			"prompt_cache_boost_enabled": true,
		},
	}
}

func promptCacheBoostResponsesTestAccount(id int64) *Account {
	account := promptCacheBoostTestAccount(id)
	account.Extra = map[string]any{"openai_responses_supported": true}
	return account
}

func promptCacheBoostOAuthTestAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Name:        "openai-oauth-pcache-boost",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":                      "oauth-test-token",
			"prompt_cache_boost_enabled":        true,
			"upstream_strong_isolation_enabled": true,
		},
	}
}

func promptCacheBoostJSONResponse(responseID string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"x-request-id": []string{"rid_" + responseID},
		},
		Body: io.NopCloser(strings.NewReader(`{"id":"` + responseID + `","object":"response","model":"gpt-5.5","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}`)),
	}
}

func promptCacheBoostUnsupportedResponse(message string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_unsupported"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"` + message + `"}}`)),
	}
}

func TestOpenAIGatewayService_ForwardPromptCacheBoostInjectsFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.5","stream":false,"input":[{"role":"user","content":"hello"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: promptCacheBoostJSONResponse("resp_pcache_forward")}
	svc := &OpenAIGatewayService{
		cfg:          promptCacheBoostTestConfig(),
		httpUpstream: upstream,
	}
	account := promptCacheBoostTestAccount(301)

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.NotNil(t, upstream.lastReq)
	require.True(t, strings.HasPrefix(gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String(), "nuro-pcache-"))
	require.Equal(t, "24h", gjson.GetBytes(upstream.lastBody, "prompt_cache_retention").String())
	require.Empty(t, upstream.lastReq.Header.Get("session_id"))
}

func TestOpenAIGatewayService_UpstreamStrongIsolationKeepsCacheBoostButDropsContinuation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.5","stream":false,"store":true,"previous_response_id":"resp_leaky","conversation_id":"conv_leaky","input":[{"role":"user","content":"hello"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("session_id", "client-session")
	c.Request.Header.Set("conversation_id", "client-conversation")
	c.Request.Header.Set("originator", "codex_cli_rs")
	c.Request.Header.Set("x-codex-turn-state", "state")
	c.Request.Header.Set("x-codex-turn-metadata", "metadata")

	upstream := &httpUpstreamRecorder{resp: promptCacheBoostJSONResponse("resp_isolated_forward")}
	svc := &OpenAIGatewayService{
		cfg:          promptCacheBoostTestConfig(),
		httpUpstream: upstream,
	}
	account := promptCacheBoostTestAccount(309)
	account.Credentials["upstream_strong_isolation_enabled"] = true

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, strings.HasPrefix(gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String(), "nuro-pcache-"))
	require.Equal(t, "24h", gjson.GetBytes(upstream.lastBody, "prompt_cache_retention").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "previous_response_id").Exists())
	require.False(t, gjson.GetBytes(upstream.lastBody, "conversation_id").Exists())
	require.False(t, gjson.GetBytes(upstream.lastBody, "store").Bool())
	require.Empty(t, upstream.lastReq.Header.Get("session_id"))
	require.Empty(t, upstream.lastReq.Header.Get("conversation_id"))
	require.Empty(t, upstream.lastReq.Header.Get("originator"))
	require.Empty(t, upstream.lastReq.Header.Get("x-codex-turn-state"))
	require.Empty(t, upstream.lastReq.Header.Get("x-codex-turn-metadata"))
}

func TestOpenAIGatewayService_OAuthUpstreamStrongIsolationDropsContinuation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.5","stream":false,"store":true,"previous_response_id":"resp_leaky","conversation_id":"conv_leaky","input":[{"role":"user","content":"hello"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("session_id", "client-session")
	c.Request.Header.Set("conversation_id", "client-conversation")
	c.Request.Header.Set("originator", "codex_cli_rs")
	c.Request.Header.Set("x-codex-turn-state", "state")
	c.Request.Header.Set("x-codex-turn-metadata", "metadata")

	upstream := &httpUpstreamRecorder{resp: promptCacheBoostJSONResponse("resp_oauth_isolated_forward")}
	svc := &OpenAIGatewayService{
		cfg:          promptCacheBoostTestConfig(),
		httpUpstream: upstream,
	}
	account := promptCacheBoostOAuthTestAccount(310)

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "Bearer oauth-test-token", upstream.lastReq.Header.Get("Authorization"))
	require.True(t, strings.HasPrefix(gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String(), "nuro-pcache-"))
	require.Equal(t, "24h", gjson.GetBytes(upstream.lastBody, "prompt_cache_retention").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "previous_response_id").Exists())
	require.False(t, gjson.GetBytes(upstream.lastBody, "conversation_id").Exists())
	require.False(t, gjson.GetBytes(upstream.lastBody, "store").Bool())
	require.Empty(t, upstream.lastReq.Header.Get("session_id"))
	require.Empty(t, upstream.lastReq.Header.Get("conversation_id"))
	require.Empty(t, upstream.lastReq.Header.Get("originator"))
	require.Empty(t, upstream.lastReq.Header.Get("x-codex-turn-state"))
	require.Empty(t, upstream.lastReq.Header.Get("x-codex-turn-metadata"))
}

func TestOpenAIUpstreamStrongIsolationWSKeepsPromptCacheKeyButDropsContinuation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("accept-language", "zh-CN")
	c.Request.Header.Set("session_id", "client-session")
	c.Request.Header.Set("conversation_id", "client-conversation")
	c.Request.Header.Set("originator", "codex_cli_rs")
	c.Request.Header.Set(openAIWSTurnStateHeader, "turn-state")
	c.Request.Header.Set(openAIWSTurnMetadataHeader, "turn-metadata")

	account := promptCacheBoostTestAccount(311)
	account.Credentials["upstream_strong_isolation_enabled"] = true
	svc := &OpenAIGatewayService{}

	headers, resolution := svc.buildOpenAIWSHeaders(
		c,
		account,
		"sk-test",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		true,
		"stored-turn-state",
		"stored-turn-metadata",
		"nuro-pcache-client",
	)
	require.Equal(t, "Bearer sk-test", headers.Get("authorization"))
	require.Equal(t, openAIWSBetaV2Value, headers.Get("OpenAI-Beta"))
	require.Equal(t, "zh-CN", headers.Get("accept-language"))
	require.Empty(t, headers.Get("session_id"))
	require.Empty(t, headers.Get("conversation_id"))
	require.Empty(t, headers.Get("originator"))
	require.Empty(t, headers.Get(openAIWSTurnStateHeader))
	require.Empty(t, headers.Get(openAIWSTurnMetadataHeader))
	require.Equal(t, "strong_isolation", resolution.SessionSource)
	require.Equal(t, "strong_isolation", resolution.ConversationSource)

	payload := svc.buildOpenAIWSCreatePayload(map[string]any{
		"model":                "gpt-5.5",
		"prompt_cache_key":     "nuro-pcache-ws",
		"previous_response_id": "resp_leaky",
		"conversation_id":      "conv_leaky",
		"session_id":           "sess_leaky",
		"store":                true,
		"client_metadata": map[string]any{
			openAIWSTurnMetadataHeader: "metadata",
			openAIWSTurnStateHeader:    "state",
			"safe":                     "keep",
		},
	}, account)
	require.Equal(t, "nuro-pcache-ws", payload["prompt_cache_key"])
	require.Equal(t, false, payload["store"])
	require.NotContains(t, payload, "previous_response_id")
	require.NotContains(t, payload, "conversation_id")
	require.NotContains(t, payload, "session_id")
	require.Equal(t, "response.create", payload["type"])
	require.NotContains(t, payload, "client_metadata")
}

func TestOpenAIUpstreamStrongIsolationWSBodyKeepsPromptCacheKeyButDropsContinuation(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-5.5","prompt_cache_key":"nuro-pcache-raw","previous_response_id":"resp_leaky","conversation_id":"conv_leaky","session_id":"sess_leaky","store":true,"client_metadata":{"x-codex-turn-metadata":"metadata","x-codex-turn-state":"state","safe":"keep"}}`)

	isolated, changed, err := applyOpenAIUpstreamStrongIsolationWSBody(body, true)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "nuro-pcache-raw", gjson.GetBytes(isolated, "prompt_cache_key").String())
	require.False(t, gjson.GetBytes(isolated, "store").Bool())
	require.False(t, gjson.GetBytes(isolated, "previous_response_id").Exists())
	require.False(t, gjson.GetBytes(isolated, "conversation_id").Exists())
	require.False(t, gjson.GetBytes(isolated, "session_id").Exists())
	require.False(t, gjson.GetBytes(isolated, "client_metadata").Exists())
}

func TestOpenAIPromptCacheBoost_StaticPrefixIgnoresFirstUserContent(t *testing.T) {
	staticPolicy := strings.Repeat("shared routing policy and tool instructions. ", 80)
	bodyA := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"` + staticPolicy + `"},{"role":"user","content":"first downstream user question"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`)
	bodyB := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"` + staticPolicy + `"},{"role":"user","content":"second downstream user question"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`)
	account := promptCacheBoostTestAccount(351)

	keyA := deriveOpenAIVirtualPromptCacheKey(account, "gpt-5.5", bodyA)
	keyB := deriveOpenAIVirtualPromptCacheKey(account, "gpt-5.5", bodyB)
	require.NotEmpty(t, keyA)
	require.Equal(t, keyA, keyB)

	affinityA := DeriveOpenAIPromptCacheBoostAffinityHash("gpt-5.5", bodyA)
	affinityB := DeriveOpenAIPromptCacheBoostAffinityHash("gpt-5.5", bodyB)
	require.NotEmpty(t, affinityA)
	require.Equal(t, affinityA, affinityB)
	require.True(t, IsOpenAIPromptCacheBoostAffinitySessionHash(affinityA))
}

func TestOpenAIPromptCacheBoost_StaticPrefixSeparatesDifferentTools(t *testing.T) {
	staticPolicy := strings.Repeat("shared routing policy and tool instructions. ", 80)
	bodyA := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"` + staticPolicy + `"},{"role":"user","content":"same user question"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`)
	bodyB := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"` + staticPolicy + `"},{"role":"user","content":"same user question"}],"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`)
	account := promptCacheBoostTestAccount(352)

	keyA := deriveOpenAIVirtualPromptCacheKey(account, "gpt-5.5", bodyA)
	keyB := deriveOpenAIVirtualPromptCacheKey(account, "gpt-5.5", bodyB)
	require.NotEmpty(t, keyA)
	require.NotEmpty(t, keyB)
	require.NotEqual(t, keyA, keyB)
}

func TestOpenAIPromptCacheBoost_SmallPromptKeepsContentSpecificFallback(t *testing.T) {
	account := promptCacheBoostTestAccount(353)
	bodyA := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"short"},{"role":"user","content":"first"}]}`)
	bodyB := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"short"},{"role":"user","content":"second"}]}`)

	require.NotEqual(t,
		deriveOpenAIVirtualPromptCacheKey(account, "gpt-5.5", bodyA),
		deriveOpenAIVirtualPromptCacheKey(account, "gpt-5.5", bodyB),
	)
	require.Empty(t, DeriveOpenAIPromptCacheBoostAffinityHash("gpt-5.5", bodyA))
}

func TestOpenAIPromptCacheBoost_AggressiveSmallStaticPrefixIgnoresFirstUserContent(t *testing.T) {
	account := promptCacheBoostTestAccount(364)
	account.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	bodyA := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"short shared policy"},{"role":"user","content":"first"}]}`)
	bodyB := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"short shared policy"},{"role":"user","content":"second"}]}`)

	keyA := deriveOpenAIVirtualPromptCacheKey(account, "gpt-5.5", bodyA)
	keyB := deriveOpenAIVirtualPromptCacheKey(account, "gpt-5.5", bodyB)
	require.NotEmpty(t, keyA)
	require.Equal(t, keyA, keyB)

	affinityA := deriveOpenAIPromptCacheBoostAffinityHashForAccount(account, "gpt-5.5", bodyA)
	affinityB := deriveOpenAIPromptCacheBoostAffinityHashForAccount(account, "gpt-5.5", bodyB)
	require.NotEmpty(t, affinityA)
	require.Equal(t, affinityA, affinityB)
	require.True(t, IsOpenAIPromptCacheBoostAffinitySessionHash(affinityA))
	require.True(t, IsOpenAIPromptCacheBoostAggressiveAffinitySessionHash(affinityA))
}

func TestOpenAIPromptCacheBoost_AffinityStickyTTLUsesRetentionWindow(t *testing.T) {
	ctx := context.Background()
	account := promptCacheBoostTestAccount(365)
	account.Status = StatusActive
	account.Schedulable = true
	aggressiveAccount := promptCacheBoostTestAccount(3651)
	aggressiveAccount.Status = StatusActive
	aggressiveAccount.Schedulable = true
	aggressiveAccount.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	cache := &stubGatewayCache{}
	svc := &OpenAIGatewayService{
		accountRepo: stubOpenAIAccountRepo{accounts: []Account{*account, *aggressiveAccount}},
		cache:       cache,
	}

	sessionHash := openAIPromptCacheBoostAffinitySessionPrefix + "ttl"
	require.NoError(t, svc.BindStickySession(ctx, nil, sessionHash, account.ID))
	require.Equal(t, openaiStickySessionTTL, cache.sessionTTLs["openai:"+sessionHash])

	aggressiveSessionHash := openAIPromptCacheBoostAggressiveAffinitySessionPrefix + "ttl"
	require.NoError(t, svc.BindStickySession(ctx, nil, aggressiveSessionHash, aggressiveAccount.ID))
	require.Equal(t, openAIPromptCacheBoostAffinityStickyTTL, cache.sessionTTLs["openai:"+aggressiveSessionHash])
	require.NoError(t, svc.BindStickySession(ctx, nil, aggressiveSessionHash+"-normal", account.ID))
	_, ok := cache.sessionTTLs["openai:"+aggressiveSessionHash+"-normal"]
	require.False(t, ok)

	upstreamPriorityAccount := promptCacheBoostTestAccount(3652)
	upstreamPriorityAccount.Status = StatusActive
	upstreamPriorityAccount.Schedulable = true
	upstreamPriorityAccount.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	upstreamPriorityAccount.Credentials["prompt_cache_boost_upstream_hit_priority_enabled"] = true
	svc.accountRepo = stubOpenAIAccountRepo{accounts: []Account{*account, *aggressiveAccount, *upstreamPriorityAccount}}
	upstreamSessionHash := openAIPromptCacheBoostUpstreamAffinitySessionPrefix + "ttl"
	require.NoError(t, svc.BindStickySession(ctx, nil, upstreamSessionHash, upstreamPriorityAccount.ID))
	require.Equal(t, openAIPromptCacheBoostAffinityStickyTTL, cache.sessionTTLs["openai:"+upstreamSessionHash])

	normalSessionHash := "normal-ttl"
	require.NoError(t, svc.BindStickySession(ctx, nil, normalSessionHash, account.ID))
	require.Equal(t, openaiStickySessionTTL, cache.sessionTTLs["openai:"+normalSessionHash])
}

func TestOpenAIPromptCacheBoost_GroupAffinityUsesAggressiveWhenAvailable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	groupID := int64(1)
	normal := *promptCacheBoostTestAccount(366)
	normal.Status = StatusActive
	normal.Schedulable = true
	normal.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelNormal
	aggressive := *promptCacheBoostTestAccount(367)
	aggressive.Status = StatusActive
	aggressive.Schedulable = true
	aggressive.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"shared policy"},{"role":"user","content":"hello"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	svc := &OpenAIGatewayService{
		schedulerSnapshot: NewSchedulerSnapshotService(&openAISnapshotCacheStub{
			snapshotAccounts: []*Account{&normal, &aggressive},
		}, nil, nil, nil, nil, nil),
	}

	sessionHash := svc.GeneratePromptCacheBoostAffinitySessionHashForGroup(ctx, c, &groupID, body, "gpt-5.5")
	require.True(t, IsOpenAIPromptCacheBoostAggressiveAffinitySessionHash(sessionHash))
}

func TestOpenAIPromptCacheBoost_GroupAffinityUsesUpstreamPriorityWhenAvailable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	groupID := int64(11)
	aggressive := *promptCacheBoostTestAccount(3671)
	aggressive.Status = StatusActive
	aggressive.Schedulable = true
	aggressive.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	upstreamPriority := *promptCacheBoostTestAccount(3672)
	upstreamPriority.Status = StatusActive
	upstreamPriority.Schedulable = true
	upstreamPriority.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	upstreamPriority.Credentials["prompt_cache_boost_upstream_hit_priority_enabled"] = true

	bodyA := []byte(`{"model":"alias-a","messages":[{"role":"system","content":"shared policy"},{"role":"user","content":"hello"}],"tool_choice":"auto"}`)
	bodyB := []byte(`{"model":"alias-b","messages":[{"role":"system","content":"shared policy"},{"role":"user","content":"hello"}]}`)
	mappedA := ReplaceModelInBody(bodyA, "gpt-5.5")
	mappedB := ReplaceModelInBody(bodyB, "gpt-5.5")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(bodyA))
	svc := &OpenAIGatewayService{
		schedulerSnapshot: NewSchedulerSnapshotService(&openAISnapshotCacheStub{
			snapshotAccounts: []*Account{&aggressive, &upstreamPriority},
		}, nil, nil, nil, nil, nil),
	}

	hashA := svc.GeneratePromptCacheBoostAffinitySessionHashForGroupWithMapped(ctx, c, &groupID, bodyA, "alias-a", mappedA, "gpt-5.5")
	hashB := svc.GeneratePromptCacheBoostAffinitySessionHashForGroupWithMapped(ctx, c, &groupID, bodyB, "alias-b", mappedB, "gpt-5.5")
	require.True(t, IsOpenAIPromptCacheBoostUpstreamAffinitySessionHash(hashA))
	require.Equal(t, hashA, hashB)
}

func TestOpenAIPromptCacheBoost_GroupAffinityUsesExplicitPromptCacheKeyForUpstreamPriority(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	groupID := int64(12)
	upstreamPriority := *promptCacheBoostTestAccount(3681)
	upstreamPriority.Status = StatusActive
	upstreamPriority.Schedulable = true
	upstreamPriority.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	upstreamPriority.Credentials["prompt_cache_boost_upstream_hit_priority_enabled"] = true

	bodyA := []byte(`{"model":"alias-a","prompt_cache_key":"client-cache-key","messages":[{"role":"user","content":"hello"}]}`)
	bodyB := []byte(`{"model":"alias-b","prompt_cache_key":"client-cache-key","messages":[{"role":"user","content":"different"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(bodyA))
	svc := &OpenAIGatewayService{
		schedulerSnapshot: NewSchedulerSnapshotService(&openAISnapshotCacheStub{
			snapshotAccounts: []*Account{&upstreamPriority},
		}, nil, nil, nil, nil, nil),
	}

	hashA := svc.GeneratePromptCacheBoostAffinitySessionHashForGroupWithMapped(ctx, c, &groupID, bodyA, "alias-a", ReplaceModelInBody(bodyA, "gpt-5.5"), "gpt-5.5")
	hashB := svc.GeneratePromptCacheBoostAffinitySessionHashForGroupWithMapped(ctx, c, &groupID, bodyB, "alias-b", ReplaceModelInBody(bodyB, "gpt-5.5"), "gpt-5.5")
	require.True(t, IsOpenAIPromptCacheBoostUpstreamAffinitySessionHash(hashA))
	require.Equal(t, hashA, hashB)

	c.Request.Header.Set("session_id", "client-session")
	require.Empty(t, svc.GeneratePromptCacheBoostAffinitySessionHashForGroupWithMapped(ctx, c, &groupID, bodyA, "alias-a", ReplaceModelInBody(bodyA, "gpt-5.5"), "gpt-5.5"))
}

func TestOpenAIPromptCacheBoost_UpstreamPriorityDisabledWhenBoostOrToggleOff(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	groupID := int64(13)
	boostOff := *promptCacheBoostTestAccount(3682)
	boostOff.Status = StatusActive
	boostOff.Schedulable = true
	boostOff.Credentials["prompt_cache_boost_enabled"] = false
	boostOff.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	boostOff.Credentials["prompt_cache_boost_upstream_hit_priority_enabled"] = true
	toggleOff := *promptCacheBoostTestAccount(3683)
	toggleOff.Status = StatusActive
	toggleOff.Schedulable = true
	toggleOff.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	delete(toggleOff.Credentials, "prompt_cache_boost_upstream_hit_priority_enabled")
	body := []byte(`{"model":"alias-a","prompt_cache_key":"client-cache-key","messages":[{"role":"user","content":"hello"}]}`)

	require.False(t, boostOff.IsOpenAIPromptCacheBoostEnabled())
	require.False(t, boostOff.IsOpenAIPromptCacheBoostAggressive())
	require.False(t, boostOff.IsOpenAIPromptCacheBoostUpstreamHitPriorityEnabled())
	require.Empty(t, deriveOpenAIVirtualPromptCacheKey(&boostOff, "gpt-5.5", body))
	require.Empty(t, deriveOpenAIPromptCacheBoostAffinityHashForAccount(&boostOff, "gpt-5.5", body))
	require.True(t, toggleOff.IsOpenAIPromptCacheBoostAggressive())
	require.False(t, toggleOff.IsOpenAIPromptCacheBoostUpstreamHitPriorityEnabled())

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	svc := &OpenAIGatewayService{
		schedulerSnapshot: NewSchedulerSnapshotService(&openAISnapshotCacheStub{
			snapshotAccounts: []*Account{&boostOff, &toggleOff},
		}, nil, nil, nil, nil, nil),
	}

	hash := svc.GeneratePromptCacheBoostAffinitySessionHashForGroupWithMapped(ctx, c, &groupID, body, "alias-a", ReplaceModelInBody(body, "gpt-5.5"), "gpt-5.5")
	require.Empty(t, hash)
}

func TestOpenAIPromptCacheBoost_NormalizeAggressiveAffinityFallsBackForNormalAccount(t *testing.T) {
	svc := &OpenAIGatewayService{}
	aggressiveSessionHash := openAIPromptCacheBoostAggressiveAffinitySessionPrefix + "test"
	normal := promptCacheBoostTestAccount(368)
	normal.Status = StatusActive
	normal.Schedulable = true
	normal.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelNormal
	aggressive := promptCacheBoostTestAccount(369)
	aggressive.Status = StatusActive
	aggressive.Schedulable = true
	aggressive.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive

	require.Empty(t, svc.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(aggressiveSessionHash, normal))
	require.Equal(t, aggressiveSessionHash, svc.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(aggressiveSessionHash, aggressive))
}

func TestOpenAIPromptCacheBoost_NormalizeUpstreamAffinityRequiresUpstreamPriorityAccount(t *testing.T) {
	svc := &OpenAIGatewayService{}
	upstreamSessionHash := openAIPromptCacheBoostUpstreamAffinitySessionPrefix + "test"
	aggressive := promptCacheBoostTestAccount(3691)
	aggressive.Status = StatusActive
	aggressive.Schedulable = true
	aggressive.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	upstreamPriority := promptCacheBoostTestAccount(3692)
	upstreamPriority.Status = StatusActive
	upstreamPriority.Schedulable = true
	upstreamPriority.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	upstreamPriority.Credentials["prompt_cache_boost_upstream_hit_priority_enabled"] = true

	require.Empty(t, svc.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(upstreamSessionHash, aggressive))
	require.Equal(t, upstreamSessionHash, svc.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(upstreamSessionHash, upstreamPriority))
}

func TestOpenAIPromptCacheBoost_UpstreamPrioritySeedCanonicalizesDefaults(t *testing.T) {
	account := promptCacheBoostTestAccount(3693)
	account.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	account.Credentials["prompt_cache_boost_upstream_hit_priority_enabled"] = true
	staticPolicy := strings.Repeat("shared policy. ", 80)
	bodyA := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"` + staticPolicy + `"},{"role":"user","content":"hello"}],"tool_choice":"auto","response_format":{"type":"text"}}`)
	bodyB := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"` + staticPolicy + `"},{"role":"user","content":"hello"}]}`)

	require.Equal(t,
		deriveOpenAIVirtualPromptCacheKey(account, "gpt-5.5", bodyA),
		deriveOpenAIVirtualPromptCacheKey(account, "gpt-5.5", bodyB),
	)
}

func TestOpenAIAnthropicVirtualPromptCacheKey_IgnoresFirstUserContent(t *testing.T) {
	staticSystem := strings.Repeat("shared anthropic system prompt. ", 80)
	bodyA := []byte(`{"model":"claude-sonnet-4-5","system":"` + staticSystem + `","max_tokens":16,"messages":[{"role":"user","content":"first downstream user"}],"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`)
	bodyB := []byte(`{"model":"claude-sonnet-4-5","system":"` + staticSystem + `","max_tokens":16,"messages":[{"role":"user","content":"second downstream user"}],"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`)
	var reqA, reqB apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal(bodyA, &reqA))
	require.NoError(t, json.Unmarshal(bodyB, &reqB))
	account := promptCacheBoostTestAccount(354)

	keyA := deriveOpenAIAnthropicVirtualPromptCacheKey(account, &reqA, "gpt-5.5")
	keyB := deriveOpenAIAnthropicVirtualPromptCacheKey(account, &reqB, "gpt-5.5")
	require.NotEmpty(t, keyA)
	require.Equal(t, keyA, keyB)
}

func TestOpenAIPromptCacheBoost_BindStickySessionRequiresEnabledSchedulableAccount(t *testing.T) {
	ctx := context.Background()
	sessionHash := openAIPromptCacheBoostAffinitySessionPrefix + "bind-disabled"
	disabled := *promptCacheBoostTestAccount(355)
	disabled.Status = StatusActive
	disabled.Schedulable = true
	disabled.Credentials = map[string]any{
		"api_key":   "sk-test",
		"base_url":  "https://api.openai.com/v1",
		"pool_mode": true,
	}
	cache := &stubGatewayCache{}
	svc := &OpenAIGatewayService{
		accountRepo: stubOpenAIAccountRepo{accounts: []Account{disabled}},
		cache:       cache,
	}

	require.NoError(t, svc.BindStickySession(ctx, nil, sessionHash, disabled.ID))
	require.Empty(t, cache.sessionBindings)
}

func TestOpenAIPromptCacheBoost_NormalizeAffinitySessionHashScopesToEnabledTextPool(t *testing.T) {
	sessionHash := openAIPromptCacheBoostAffinitySessionPrefix + "normalize"
	normalSessionHash := "normal-session"
	enabled := *promptCacheBoostTestAccount(358)
	enabled.Status = StatusActive
	enabled.Schedulable = true

	disabled := enabled
	disabled.ID = 359
	disabled.Credentials = map[string]any{
		"api_key":   "sk-disabled",
		"base_url":  "https://api.openai.com/v1",
		"pool_mode": true,
	}

	imagePool := enabled
	imagePool.ID = 360
	imagePool.Credentials = map[string]any{
		"api_key":                    "sk-image",
		"base_url":                   "https://api.openai.com/v1",
		"pool_mode":                  true,
		"image_pool_mode":            true,
		"prompt_cache_boost_enabled": true,
	}

	oauth := enabled
	oauth.ID = 361
	oauth.Type = AccountTypeOAuth
	oauth.Credentials = map[string]any{
		"prompt_cache_boost_enabled": true,
	}

	softCooling := enabled
	softCooling.ID = 362
	runtimeBlocked := enabled
	runtimeBlocked.ID = 363

	svc := &OpenAIGatewayService{}
	svc.openaiPoolSoftCooldownUntil.Store(softCooling.ID, time.Now().Add(time.Minute))
	svc.openaiAccountRuntimeBlockUntil.Store(runtimeBlocked.ID, time.Now().Add(time.Minute))

	require.Equal(t, sessionHash, svc.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(sessionHash, &enabled))
	require.Empty(t, svc.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(sessionHash, &disabled))
	require.Empty(t, svc.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(sessionHash, &imagePool))
	require.Equal(t, sessionHash, svc.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(sessionHash, &oauth))
	require.Empty(t, svc.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(sessionHash, &softCooling))
	require.Empty(t, svc.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(sessionHash, &runtimeBlocked))
	require.Equal(t, normalSessionHash, svc.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(normalSessionHash, &disabled))
}

func TestOpenAIPromptCacheBoost_BindStickySessionRejectsSoftCoolingAndRuntimeBlockedAccounts(t *testing.T) {
	ctx := context.Background()
	softCooling := *promptCacheBoostTestAccount(356)
	softCooling.Status = StatusActive
	softCooling.Schedulable = true
	runtimeBlocked := *promptCacheBoostTestAccount(357)
	runtimeBlocked.Status = StatusActive
	runtimeBlocked.Schedulable = true

	cache := &stubGatewayCache{}
	svc := &OpenAIGatewayService{
		accountRepo: stubOpenAIAccountRepo{accounts: []Account{softCooling, runtimeBlocked}},
		cache:       cache,
	}
	svc.openaiPoolSoftCooldownUntil.Store(softCooling.ID, time.Now().Add(time.Minute))
	svc.BlockAccountScheduling(&runtimeBlocked, time.Now().Add(time.Minute), "test")

	softSessionHash := openAIPromptCacheBoostAffinitySessionPrefix + "bind-soft-cooling"
	require.NoError(t, svc.BindStickySession(ctx, nil, softSessionHash, softCooling.ID))
	require.NotContains(t, cache.sessionBindings, "openai:"+softSessionHash)

	blockedSessionHash := openAIPromptCacheBoostAffinitySessionPrefix + "bind-runtime-blocked"
	require.NoError(t, svc.BindStickySession(ctx, nil, blockedSessionHash, runtimeBlocked.ID))
	require.NotContains(t, cache.sessionBindings, "openai:"+blockedSessionHash)
}

func TestOpenAIPromptCacheBoost_BindStickySessionRejectsRuntimeBlockedOAuthAccount(t *testing.T) {
	ctx := context.Background()
	oauth := *promptCacheBoostTestAccount(358)
	oauth.Type = AccountTypeOAuth
	oauth.Status = StatusActive
	oauth.Schedulable = true
	oauth.Credentials = map[string]any{
		"prompt_cache_boost_enabled": true,
	}

	cache := &stubGatewayCache{}
	svc := &OpenAIGatewayService{
		accountRepo: stubOpenAIAccountRepo{accounts: []Account{oauth}},
		cache:       cache,
	}
	svc.BlockAccountScheduling(&oauth, time.Now().Add(time.Minute), "oauth_runtime_block")

	sessionHash := openAIPromptCacheBoostAffinitySessionPrefix + "bind-oauth-runtime-blocked"
	require.NoError(t, svc.BindStickySession(ctx, nil, sessionHash, oauth.ID))
	require.NotContains(t, cache.sessionBindings, "openai:"+sessionHash)
}

func TestAccountWriteThrottlePrunesPromptCacheHitRateLogState(t *testing.T) {
	throttle := newAccountWriteThrottle(5 * time.Minute)
	now := time.Now()

	for i := int64(1); i <= accountWriteThrottleMaxEntries+32; i++ {
		require.True(t, throttle.Allow(i, now.Add(time.Duration(i)*time.Second)))
	}

	require.LessOrEqual(t, len(throttle.lastByID), accountWriteThrottleMaxEntries)
}

func TestOpenAIGatewayService_ForwardPromptCacheBoostUnsupportedRetentionRetries(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.5","stream":false,"input":[{"role":"user","content":"hello"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		promptCacheBoostUnsupportedResponse("Unsupported parameter: 'prompt_cache_retention'"),
		promptCacheBoostJSONResponse("resp_pcache_forward_retry"),
	}}
	svc := &OpenAIGatewayService{
		cfg:          promptCacheBoostTestConfig(),
		httpUpstream: upstream,
	}
	account := promptCacheBoostTestAccount(302)

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 2)
	require.Equal(t, "24h", gjson.GetBytes(upstream.bodies[0], "prompt_cache_retention").String())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "prompt_cache_retention").Exists())
	require.True(t, strings.HasPrefix(gjson.GetBytes(upstream.bodies[1], "prompt_cache_key").String(), "nuro-pcache-"))
	require.True(t, svc.isOpenAIPromptCacheBoostKeyRuntimeEnabled(account))
	require.False(t, svc.isOpenAIPromptCacheBoostRetentionRuntimeEnabled(account))
}

func TestOpenAIPromptCacheBoostUnsupportedAfterCommittedResponseDisablesFutureInjection(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := promptCacheBoostTestAccount(303)
	require.True(t, svc.isOpenAIPromptCacheBoostKeyRuntimeEnabled(account))
	require.True(t, svc.isOpenAIPromptCacheBoostRetentionRuntimeEnabled(account))

	svc.RecordOpenAIPromptCacheBoostUnsupportedAfterCommittedResponse(
		account,
		http.StatusBadRequest,
		"Unsupported parameter: 'prompt_cache_retention'",
		nil,
		true,
		true,
	)

	require.True(t, svc.isOpenAIPromptCacheBoostKeyRuntimeEnabled(account))
	require.False(t, svc.isOpenAIPromptCacheBoostRetentionRuntimeEnabled(account))
}

func TestForwardAsChatCompletions_PromptCacheBoostInjectsFieldsWithoutGeneratedSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"shared policy"},{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_pcache_chat", "gpt-5.5")}
	svc := &OpenAIGatewayService{
		cfg:          promptCacheBoostTestConfig(),
		httpUpstream: upstream,
	}
	account := promptCacheBoostResponsesTestAccount(303)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, strings.HasPrefix(gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String(), "nuro-pcache-"))
	require.Equal(t, "24h", gjson.GetBytes(upstream.lastBody, "prompt_cache_retention").String())
	require.Empty(t, upstream.lastReq.Header.Get("session_id"))
}

func TestForwardAsChatCompletions_ExplicitPromptCacheKeySetsSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.5","prompt_cache_key":"client-cache-key","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_pcache_chat_explicit", "gpt-5.5")}
	svc := &OpenAIGatewayService{
		cfg:          promptCacheBoostTestConfig(),
		httpUpstream: upstream,
	}
	account := promptCacheBoostResponsesTestAccount(304)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "client-cache-key", gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
	require.Equal(t, generateSessionUUID(isolateOpenAISessionID(0, "client-cache-key")), upstream.lastReq.Header.Get("session_id"))
}

func TestForwardAsChatCompletions_UpstreamStrongIsolationDoesNotSetSessionForExplicitPromptCacheKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.5","prompt_cache_key":"client-cache-key","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("session_id", "client-session")

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_pcache_chat_isolated", "gpt-5.5")}
	svc := &OpenAIGatewayService{
		cfg:          promptCacheBoostTestConfig(),
		httpUpstream: upstream,
	}
	account := promptCacheBoostResponsesTestAccount(310)
	account.Credentials["upstream_strong_isolation_enabled"] = true

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "client-cache-key", gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "store").Bool())
	require.Empty(t, upstream.lastReq.Header.Get("session_id"))
	require.Empty(t, upstream.lastReq.Header.Get("conversation_id"))
}

func TestForwardAsChatCompletions_PromptCacheBoostUnsupportedRetentionRetries(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"shared policy"},{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		promptCacheBoostUnsupportedResponse("Unsupported parameter: 'prompt_cache_retention'"),
		openAICompatSSECompletedResponse("resp_pcache_chat_retry", "gpt-5.5"),
	}}
	svc := &OpenAIGatewayService{
		cfg:          promptCacheBoostTestConfig(),
		httpUpstream: upstream,
	}
	account := promptCacheBoostResponsesTestAccount(305)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 2)
	require.Equal(t, "24h", gjson.GetBytes(upstream.bodies[0], "prompt_cache_retention").String())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "prompt_cache_retention").Exists())
	require.True(t, strings.HasPrefix(gjson.GetBytes(upstream.bodies[1], "prompt_cache_key").String(), "nuro-pcache-"))
	require.Empty(t, upstream.requests[0].Header.Get("session_id"))
	require.Empty(t, upstream.requests[1].Header.Get("session_id"))
}

func TestForwardAsAnthropic_PromptCacheBoostInjectsFieldsWithoutGeneratedSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_pcache_messages", "gpt-4o")}
	svc := &OpenAIGatewayService{
		cfg:          promptCacheBoostTestConfig(),
		httpUpstream: upstream,
	}
	account := promptCacheBoostTestAccount(306)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-4o")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, strings.HasPrefix(gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String(), "nuro-pcache-"))
	require.Equal(t, "24h", gjson.GetBytes(upstream.lastBody, "prompt_cache_retention").String())
	require.Empty(t, upstream.lastReq.Header.Get("session_id"))
}

func TestForwardAsAnthropic_PromptCacheBoostKeepsLargeReplayWithoutAutoSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	messages := make([]string, 0, openAICompatAnthropicReplayMaxTailMessages+3)
	for i := 0; i < openAICompatAnthropicReplayMaxTailMessages+3; i++ {
		messages = append(messages, `{"role":"user","content":"message-`+strings.Repeat("x", 2048)+`-`+string(rune('a'+i))+`"}`)
	}
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[` + strings.Join(messages, ",") + `],"stream":false}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_pcache_large_messages", "gpt-5.3-codex")}
	svc := &OpenAIGatewayService{
		cfg:          promptCacheBoostTestConfig(),
		httpUpstream: upstream,
	}
	account := promptCacheBoostTestAccount(307)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.3-codex")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, int64(openAICompatAnthropicReplayMaxTailMessages+4), gjson.GetBytes(upstream.lastBody, "input.#").Int())
	require.Equal(t, "developer", gjson.GetBytes(upstream.lastBody, "input.0.role").String())
	require.Contains(t, gjson.GetBytes(upstream.lastBody, "input.1.content.0.text").String(), "message-")
	require.Contains(t, gjson.GetBytes(upstream.lastBody, "input.15.content.0.text").String(), "message-")
	require.Equal(t, "24h", gjson.GetBytes(upstream.lastBody, "prompt_cache_retention").String())
	require.Empty(t, upstream.lastReq.Header.Get("session_id"))
}

func TestForwardAsAnthropic_PromptCacheBoostUnsupportedFieldsRetryWithoutFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		promptCacheBoostUnsupportedResponse("Unsupported parameter: 'prompt_cache_key' and 'prompt_cache_retention'"),
		openAICompatSSECompletedResponse("resp_pcache_messages_retry", "gpt-4o"),
	}}
	svc := &OpenAIGatewayService{
		cfg:          promptCacheBoostTestConfig(),
		httpUpstream: upstream,
	}
	account := promptCacheBoostTestAccount(308)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-4o")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 2)
	require.True(t, strings.HasPrefix(gjson.GetBytes(upstream.bodies[0], "prompt_cache_key").String(), "nuro-pcache-"))
	require.Equal(t, "24h", gjson.GetBytes(upstream.bodies[0], "prompt_cache_retention").String())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "prompt_cache_key").Exists())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "prompt_cache_retention").Exists())
	require.Empty(t, upstream.requests[0].Header.Get("session_id"))
	require.Empty(t, upstream.requests[1].Header.Get("session_id"))
}
