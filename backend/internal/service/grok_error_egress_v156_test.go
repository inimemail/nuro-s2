//go:build unit

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

func TestForwardGrokMediaHTTPErrorDoesNotExposeUpstreamIdentity(t *testing.T) {
	setGinTestMode()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"Location":     []string{"https://private-provider.example/error"},
			"Server":       []string{"private-provider-edge"},
			"Via":          []string{"private-provider.example"},
		},
		Body: io.NopCloser(strings.NewReader(
			`<!DOCTYPE html><title>private-provider.example | 400</title><a href="https://www.cloudflare.com/error">error</a>`,
		)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID: 901, Platform: PlatformGrok, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "grok-api-key"},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)

	result, err := svc.ForwardGrokMedia(
		context.Background(), c, account, GrokMediaEndpointImagesGenerations, "",
		[]byte(`{"model":"grok-imagine","prompt":"draw"}`), "application/json",
	)

	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, safeUpstreamErrorMessage, gjson.GetBytes(recorder.Body.Bytes(), "error.message").String())
	for _, secret := range []string{"private-provider", "cloudflare", "DOCTYPE", "Location", "Server", "Via"} {
		require.NotContains(t, strings.ToLower(recorder.Body.String()), strings.ToLower(secret))
		require.NotContains(t, strings.ToLower(recorder.Header().Get("Location")), strings.ToLower(secret))
		require.NotContains(t, strings.ToLower(recorder.Header().Get("Server")), strings.ToLower(secret))
		require.NotContains(t, strings.ToLower(recorder.Header().Get("Via")), strings.ToLower(secret))
	}
}

func TestForwardGrokMediaFailedTerminalDoesNotExposeUpstreamIdentity(t *testing.T) {
	setGinTestMode()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"request_id":"video_req_safe","status":"failed","error":{"message":"https://private-provider.example failed"},"provider":"private-provider"}`,
		)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID: 902, Platform: PlatformGrok, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "grok-api-key"},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/video_req_safe", nil)

	result, err := svc.ForwardGrokMedia(
		context.Background(), c, account, GrokMediaEndpointVideoStatus, "video_req_safe", nil, "",
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "grok_media.failed", result.TerminalEventType)
	require.JSONEq(t, `{"request_id":"video_req_safe","status":"failed"}`, recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), "private-provider")
}

func TestForwardGrokResponsesHTTPErrorDoesNotExposeUpstreamIdentity(t *testing.T) {
	setGinTestMode()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"Location":     []string{"https://private-provider.example/error"},
			"Server":       []string{"private-provider-edge"},
		},
		Body: io.NopCloser(bytes.NewBufferString(
			`<!DOCTYPE html><title>private-provider.example | 400</title>`,
		)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID: 903, Platform: PlatformGrok, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "grok-api-key"},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"grok-4.5","input":"hi","stream":false}`))

	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadGateway, recorder.Code)
	require.Equal(t, safeUpstreamErrorMessage, gjson.GetBytes(recorder.Body.Bytes(), "error.message").String())
	require.NotContains(t, recorder.Body.String(), "private-provider")
	require.NotContains(t, recorder.Body.String(), "DOCTYPE")
	require.Empty(t, recorder.Header().Get("Location"))
	require.Empty(t, recorder.Header().Get("Server"))
}
