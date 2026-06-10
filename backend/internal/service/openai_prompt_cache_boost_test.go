package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
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
