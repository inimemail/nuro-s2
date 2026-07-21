package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestAntigravityForwardUpstreamSanitizesHTTPError(t *testing.T) {
	setGinTestMode()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(nil))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadGateway,
		Header: http.Header{
			"Content-Type": []string{"text/html; charset=UTF-8"},
			"Location":     []string{"https://xiaobaishu.org/error"},
		},
		Body: io.NopCloser(strings.NewReader(
			`<!DOCTYPE html><title>xiaobaishu.org | 502</title><a href="https://www.cloudflare.com/5xx">cloudflare</a>`,
		)),
	}}
	svc := &AntigravityGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          9,
		Name:        "upstream-account",
		Platform:    PlatformAntigravity,
		Type:        AccountTypeUpstream,
		Concurrency: 1,
		Credentials: map[string]any{
			"base_url": "https://xiaobaishu.org",
			"api_key":  "secret",
		},
	}

	result, err := svc.ForwardUpstream(
		context.Background(),
		c,
		account,
		[]byte(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`),
	)

	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadGateway, recorder.Code)
	require.JSONEq(t, `{"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}`, recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), "xiaobaishu.org")
	require.NotContains(t, recorder.Body.String(), "cloudflare.com")
	require.NotContains(t, recorder.Body.String(), "DOCTYPE")
	require.Empty(t, recorder.Header().Get("Location"))
}

func TestSanitizeAntigravityUpstreamSSEErrorLine(t *testing.T) {
	errorEvent := false
	line, upstreamError := sanitizeAntigravityUpstreamSSEErrorLine("event: error", &errorEvent)
	require.Equal(t, "event: error", line)
	require.False(t, upstreamError)
	safe, upstreamError := sanitizeAntigravityUpstreamSSEErrorLine(
		`data: {"type":"error","error":{"message":"https://xiaobaishu.org/502"}}`,
		&errorEvent,
	)
	require.Equal(t, `data: {"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}`, safe)
	require.True(t, upstreamError)
	require.NotContains(t, safe, "xiaobaishu.org")
	require.False(t, errorEvent)
	normal, upstreamError := sanitizeAntigravityUpstreamSSEErrorLine(`data: {"type":"message_delta","delta":{"text":"https://example.com"},"error":null}`, &errorEvent)
	require.Equal(t, `data: {"type":"message_delta","delta":{"text":"https://example.com"},"error":null}`, normal)
	require.False(t, upstreamError)

	safe, upstreamError = sanitizeAntigravityUpstreamSSEErrorLine(`<!DOCTYPE html><title>xiaobaishu.org</title>`, &errorEvent)
	require.True(t, upstreamError)
	require.NotContains(t, safe, "xiaobaishu.org")

	safe, upstreamError = sanitizeAntigravityUpstreamSSEErrorLine(`data: {"message":"private-provider.example failed"}`, &errorEvent)
	require.True(t, upstreamError)
	require.Contains(t, safe, safeUpstreamErrorMessage)
	require.NotContains(t, safe, "private-provider.example")
}

func TestAntigravityForwardUpstreamRejectsInvalidSuccessfulResponse(t *testing.T) {
	setGinTestMode()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(nil))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(strings.NewReader(`<!DOCTYPE html><title>xiaobaishu.org</title>`)),
	}}
	svc := &AntigravityGatewayService{httpUpstream: upstream}
	account := &Account{
		ID: 10, Name: "upstream-account", Platform: PlatformAntigravity, Type: AccountTypeUpstream, Concurrency: 1,
		Credentials: map[string]any{"base_url": "https://xiaobaishu.org", "api_key": "secret"},
	}

	result, err := svc.ForwardUpstream(context.Background(), c, account, []byte(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`))
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Empty(t, recorder.Body.String())
}

func TestAntigravityForwardGeminiSanitizesHTTPErrorUsingGoogleProtocol(t *testing.T) {
	setGinTestMode()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", bytes.NewReader(nil))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=UTF-8"}},
		Body: io.NopCloser(strings.NewReader(
			`<!DOCTYPE html><title>xiaobaishu.org | 400</title><a href="https://www.cloudflare.com/error">cloudflare</a>`,
		)),
	}}
	svc := &AntigravityGatewayService{
		httpUpstream:   upstream,
		tokenProvider:  &AntigravityTokenProvider{},
		settingService: NewSettingService(&antigravitySettingRepoStub{}, &config.Config{}),
	}
	account := &Account{
		ID: 11, Name: "antigravity-account", Platform: PlatformAntigravity, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "secret", "project_id": "project"},
	}

	result, err := svc.ForwardGemini(
		context.Background(), c, account, "gemini-2.5-flash", "generateContent", false,
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`), false,
	)

	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.JSONEq(t, `{"error":{"code":400,"message":"Upstream request failed","status":"INVALID_ARGUMENT"}}`, recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), "xiaobaishu.org")
	require.NotContains(t, recorder.Body.String(), "cloudflare.com")
	require.NotContains(t, recorder.Body.String(), "DOCTYPE")
}
