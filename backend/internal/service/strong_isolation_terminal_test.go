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

func strongIsolationTestConfig() *config.Config {
	return &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
		Enabled: false, AllowInsecureHTTP: true,
	}}}
}

func strongIsolationOpenAIAPIKeyAccount(enabled bool) *Account {
	return &Account{
		ID: 901, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{
			"api_key":                           "sk-test",
			"base_url":                          "http://upstream.example",
			"pool_mode":                         true,
			"upstream_strong_isolation_enabled": enabled,
		},
	}
}

func successfulRawChatResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_isolation","object":"chat.completion","model":"gpt-5.4","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		)),
	}
}

func TestRawChatStrongIsolationFinalTransformAndDisabledNoOp(t *testing.T) {
	setGinTestMode()
	body := []byte(`{"model":"gpt-5.4","stream":false,"messages":[{"role":"user","content":"hi"}],"conversation_id":"conv","session_id":"sess","previous_response_id":"resp","client_metadata":{"private":"value"},"store":true}`)

	for _, tt := range []struct {
		name    string
		enabled bool
	}{
		{name: "enabled", enabled: true},
		{name: "disabled", enabled: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			upstream := &httpUpstreamRecorder{resp: successfulRawChatResponse()}
			svc := &OpenAIGatewayService{cfg: strongIsolationTestConfig(), httpUpstream: upstream}

			result, err := svc.forwardAsRawChatCompletions(context.Background(), c, strongIsolationOpenAIAPIKeyAccount(tt.enabled), body, "")
			require.NoError(t, err)
			require.NotNil(t, result)
			if tt.enabled {
				for _, field := range []string{"conversation_id", "session_id", "previous_response_id", "client_metadata"} {
					require.False(t, gjson.GetBytes(upstream.lastBody, field).Exists(), field)
				}
				require.False(t, gjson.GetBytes(upstream.lastBody, "store").Bool())
			} else {
				require.Equal(t, "conv", gjson.GetBytes(upstream.lastBody, "conversation_id").String())
				require.Equal(t, "sess", gjson.GetBytes(upstream.lastBody, "session_id").String())
				require.Equal(t, "resp", gjson.GetBytes(upstream.lastBody, "previous_response_id").String())
				require.True(t, gjson.GetBytes(upstream.lastBody, "store").Bool())
			}
		})
	}
}

func TestResponsesChatFallbackKeepsMaxTokenAliasAndStrongIsolation(t *testing.T) {
	setGinTestMode()
	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":false,"max_tokens":123,"store":true,"previous_response_id":"resp-private"}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{resp: successfulRawChatResponse()}
	svc := &OpenAIGatewayService{cfg: strongIsolationTestConfig(), httpUpstream: upstream}

	result, err := svc.forwardResponsesViaRawChatCompletions(context.Background(), c, strongIsolationOpenAIAPIKeyAccount(true), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, int64(123), gjson.GetBytes(upstream.lastBody, "max_completion_tokens").Int())
	for _, field := range []string{"conversation_id", "session_id", "previous_response_id", "client_metadata"} {
		require.False(t, gjson.GetBytes(upstream.lastBody, field).Exists(), field)
	}
	require.False(t, gjson.GetBytes(upstream.lastBody, "store").Bool())
}

func TestMessagesStrongIsolationCannotRestoreCompatTurnState(t *testing.T) {
	setGinTestMode()
	body := []byte(`{"model":"gpt-5.5","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("session_id", "client-session")
	c.Request.Header.Set("conversation_id", "client-conversation")
	c.Request.Header.Set("x-codex-turn-state", "client-turn-state")
	c.Request.Header.Set("x-codex-turn-metadata", "client-turn-metadata")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(successfulOpenAIResponsesSSE())),
	}}
	svc := &OpenAIGatewayService{cfg: strongIsolationTestConfig(), httpUpstream: upstream}
	account := &Account{
		ID: 902, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{
			"access_token":                      "oauth-token",
			"upstream_strong_isolation_enabled": true,
		},
	}
	svc.bindOpenAICompatSessionTurnState(context.Background(), c, account, "stable-key", "stored-turn-state")

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "stable-key", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	for _, header := range openAIUpstreamStrongIsolationHeaders {
		require.Empty(t, upstream.lastReq.Header.Get(header), header)
	}
	require.False(t, gjson.GetBytes(upstream.lastBody, "store").Bool())
}

func TestOpenAIUpstreamBuilderStrongIsolationHeadersAndDisabledNoOp(t *testing.T) {
	setGinTestMode()
	for _, tt := range []struct {
		name    string
		enabled bool
	}{
		{name: "enabled", enabled: true},
		{name: "disabled", enabled: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			for _, header := range openAIUpstreamStrongIsolationHeaders {
				c.Request.Header.Set(header, "client-value")
			}
			svc := &OpenAIGatewayService{cfg: strongIsolationTestConfig()}
			req, err := svc.buildUpstreamRequest(context.Background(), c, strongIsolationOpenAIAPIKeyAccount(tt.enabled), []byte(`{"model":"gpt-5.4"}`), "sk-test", false, "", false)
			require.NoError(t, err)
			if tt.enabled {
				for _, header := range openAIUpstreamStrongIsolationHeaders {
					require.Empty(t, req.Header.Get(header), header)
				}
			} else {
				require.Equal(t, "client-value", req.Header.Get("session_id"))
				require.Equal(t, "client-value", req.Header.Get("x-codex-turn-state"))
			}
		})
	}
}

func strongIsolationAnthropicAccount(accountType string, enabled bool) *Account {
	credentials := map[string]any{"anthropic_upstream_strong_isolation_enabled": enabled}
	if accountType == AccountTypeAPIKey {
		credentials["pool_mode"] = true
	}
	return &Account{ID: 903, Platform: PlatformAnthropic, Type: accountType, Credentials: credentials}
}

func assertAnthropicStrongIsolationRequest(t *testing.T, req *http.Request) {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	for _, field := range anthropicUpstreamStrongIsolationBodyFields {
		require.False(t, gjson.GetBytes(body, field).Exists(), field)
	}
	for _, header := range anthropicUpstreamStrongIsolationHeaders {
		require.Empty(t, req.Header.Get(header), header)
	}
}

func TestAnthropicStrongIsolationIsFinalForMessagesAndCountTokensBuilders(t *testing.T) {
	setGinTestMode()
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[],"client_metadata":{"private":true},"conversation_id":"conv","session_id":"sess"}`)
	newContext := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		c.Request.Header.Set("X-Claude-Code-Session-Id", "client-session")
		return c
	}
	svc := &GatewayService{cfg: &config.Config{}}

	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth, AccountTypeSetupToken} {
		t.Run("messages_"+accountType, func(t *testing.T) {
			account := strongIsolationAnthropicAccount(accountType, true)
			tokenType := "oauth"
			if accountType == AccountTypeAPIKey {
				tokenType = "apikey"
			}
			req, err := svc.buildUpstreamRequest(context.Background(), newContext(), account, body, "token", tokenType, "claude-sonnet-4-6", false, false)
			require.NoError(t, err)
			assertAnthropicStrongIsolationRequest(t, req)
		})

		t.Run("count_tokens_"+accountType, func(t *testing.T) {
			account := strongIsolationAnthropicAccount(accountType, true)
			tokenType := "oauth"
			if accountType == AccountTypeAPIKey {
				tokenType = "apikey"
			}
			req, err := svc.buildCountTokensRequest(context.Background(), newContext(), account, body, "token", tokenType, "claude-sonnet-4-6", false)
			require.NoError(t, err)
			assertAnthropicStrongIsolationRequest(t, req)
		})
	}

	apiKey := strongIsolationAnthropicAccount(AccountTypeAPIKey, true)
	req, err := svc.buildUpstreamRequestAnthropicAPIKeyPassthrough(context.Background(), newContext(), apiKey, body, "token")
	require.NoError(t, err)
	assertAnthropicStrongIsolationRequest(t, req)
	req, err = svc.buildCountTokensRequestAnthropicAPIKeyPassthrough(context.Background(), newContext(), apiKey, body, "token")
	require.NoError(t, err)
	assertAnthropicStrongIsolationRequest(t, req)
}

func TestAnthropicStrongIsolationDisabledIsBodyAndHeaderNoOp(t *testing.T) {
	setGinTestMode()
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[],"client_metadata":{"private":true},"conversation_id":"conv","session_id":"sess"}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("X-Claude-Code-Session-Id", "client-session")
	svc := &GatewayService{cfg: &config.Config{}}

	req, err := svc.buildUpstreamRequestAnthropicAPIKeyPassthrough(context.Background(), c, strongIsolationAnthropicAccount(AccountTypeAPIKey, false), body, "token")
	require.NoError(t, err)
	forwarded, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, "conv", gjson.GetBytes(forwarded, "conversation_id").String())
	require.Equal(t, "sess", gjson.GetBytes(forwarded, "session_id").String())
	require.True(t, gjson.GetBytes(forwarded, "client_metadata.private").Bool())
	require.Equal(t, "client-session", req.Header.Get("X-Claude-Code-Session-Id"))
}
