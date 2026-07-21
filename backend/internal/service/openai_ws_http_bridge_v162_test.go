package service

import (
	"context"
	"errors"
	"fmt"
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

func TestPrepareOpenAIWSHTTPBridgeBodyStripsWSFields(t *testing.T) {
	body, err := prepareOpenAIWSHTTPBridgeBody([]byte(`{"type":"response.create","generate":true,"model":"gpt-5","stream":false,"previous_response_id":"resp_prev","input":"hi"}`))
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(body, "type").Exists())
	require.False(t, gjson.GetBytes(body, "generate").Exists())
	require.False(t, gjson.GetBytes(body, "previous_response_id").Exists())
	require.Equal(t, "gpt-5", gjson.GetBytes(body, "model").String())
	require.True(t, gjson.GetBytes(body, "stream").Bool())
	require.Equal(t, "hi", gjson.GetBytes(body, "input").String())
}

func TestOpenAIWSHTTPBridgeDecisionKeepsSmallFramesOnWS(t *testing.T) {
	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				OpenAIWS: config.GatewayOpenAIWSConfig{
					HTTPBridgeEnabled:        true,
					HTTPBridgeThresholdBytes: 100,
				},
			},
		},
	}

	require.False(t, svc.shouldBridgeOpenAIWSHTTP(nil, 99, ""))
	require.True(t, svc.shouldBridgeOpenAIWSHTTP(nil, 100, ""))
	require.False(t, svc.shouldBridgeOpenAIWSHTTP(nil, 1000, "resp_existing"))

	svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = false
	require.False(t, svc.shouldBridgeOpenAIWSHTTP(nil, 1000, ""))
	require.True(t, svc.shouldBridgeOpenAIWSHTTP(&Account{Platform: PlatformGrok}, 1, "resp_existing"))
}

func TestProxyOpenAIWSHTTPBridgeTurnTransportErrorFailoverSafety(t *testing.T) {
	setGinTestMode()

	tests := []struct {
		name         string
		turn         int
		wantFailover bool
		wantWrites   int
	}{
		{name: "first_turn_fails_over_before_downstream_event", turn: 1, wantFailover: true},
		{name: "later_turn_does_not_replay_completed_turns", turn: 2, wantWrites: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{err: io.EOF}
			svc := &OpenAIGatewayService{
				cfg:          &config.Config{},
				httpUpstream: upstream,
			}
			account := &Account{
				ID:          8,
				Name:        "api-key",
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Concurrency: 1,
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			payload := []byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`)
			var writes [][]byte

			result, err := svc.proxyOpenAIWSHTTPBridgeTurn(
				context.Background(), c, account, "sk-test", payload, len(payload),
				"gpt-5", "", "", "", "", tt.turn,
				func(message []byte) error {
					writes = append(writes, append([]byte(nil), message...))
					return nil
				},
			)

			require.Nil(t, result)
			var failoverErr *UpstreamFailoverError
			if tt.wantFailover {
				require.ErrorAs(t, err, &failoverErr)
				require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
				require.JSONEq(t, string(openAITransportFailoverBody), string(failoverErr.ResponseBody))
			} else {
				require.Error(t, err)
				require.False(t, errors.As(err, &failoverErr))
			}
			require.Len(t, writes, tt.wantWrites)
			if tt.wantWrites > 0 {
				require.Equal(t, "error", gjson.GetBytes(writes[0], "type").String())
				require.Equal(t, int64(http.StatusBadGateway), gjson.GetBytes(writes[0], "status").Int())
			}
		})
	}
}

func TestProxyOpenAIWSHTTPBridgeTurnHTTPStatusFailoverSafety(t *testing.T) {
	setGinTestMode()

	tests := []struct {
		name         string
		turn         int
		status       int
		wantFailover bool
		wantWrites   int
	}{
		{name: "first_turn_401", turn: 1, status: http.StatusUnauthorized, wantFailover: true},
		{name: "first_turn_429", turn: 1, status: http.StatusTooManyRequests, wantFailover: true},
		{name: "first_turn_500", turn: 1, status: http.StatusInternalServerError, wantFailover: true},
		{name: "later_turn_500_does_not_replay", turn: 2, status: http.StatusInternalServerError, wantWrites: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: tt.status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","message":"temporary upstream failure"}}`)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			payload := []byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`)
			var writes [][]byte

			result, err := svc.proxyOpenAIWSHTTPBridgeTurn(
				context.Background(), c, account, "sk-test", payload, len(payload),
				"gpt-5", "", "", "", "", tt.turn,
				func(message []byte) error {
					writes = append(writes, append([]byte(nil), message...))
					return nil
				},
			)

			require.Nil(t, result)
			var failoverErr *UpstreamFailoverError
			if tt.wantFailover {
				require.ErrorAs(t, err, &failoverErr)
				require.Equal(t, tt.status, failoverErr.StatusCode)
			} else {
				require.Error(t, err)
				require.False(t, errors.As(err, &failoverErr))
			}
			require.Len(t, writes, tt.wantWrites)
		})
	}
}

func TestProxyOpenAIWSHTTPBridgeTurnSSEErrorFailoverSafety(t *testing.T) {
	setGinTestMode()

	for _, turn := range []int{1, 2} {
		t.Run(fmt.Sprintf("turn_%d", turn), func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					"data: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"limited\"}}\n\n",
				)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{ID: 10, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			payload := []byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`)
			var writes [][]byte

			result, err := svc.proxyOpenAIWSHTTPBridgeTurn(
				context.Background(), c, account, "sk-test", payload, len(payload),
				"gpt-5", "", "", "", "", turn,
				func(message []byte) error {
					writes = append(writes, append([]byte(nil), message...))
					return nil
				},
			)

			var failoverErr *UpstreamFailoverError
			if turn == 1 {
				require.Nil(t, result)
				require.ErrorAs(t, err, &failoverErr)
				require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
				require.Empty(t, writes)
			} else {
				require.NotNil(t, result)
				require.Error(t, err)
				require.False(t, errors.As(err, &failoverErr))
				require.Len(t, writes, 1)
			}
		})
	}
}

func TestProxyOpenAIWSHTTPBridgeTurnRequiresTerminalEvent(t *testing.T) {
	setGinTestMode()

	tests := []struct {
		name         string
		body         string
		wantFailover bool
		wantWrites   int
	}{
		{name: "done_without_events_fails_over", body: "data: [DONE]\n\n", wantFailover: true},
		{
			name: "created_then_done_is_truncated_not_success",
			body: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_truncated\"}}\n\n" +
				"data: [DONE]\n\n",
			wantWrites: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(tt.body)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			payload := []byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`)
			var writes [][]byte

			result, err := svc.proxyOpenAIWSHTTPBridgeTurn(
				context.Background(), c, account, "sk-test", payload, len(payload),
				"gpt-5", "", "", "", "", 1,
				func(message []byte) error {
					writes = append(writes, append([]byte(nil), message...))
					return nil
				},
			)

			var failoverErr *UpstreamFailoverError
			if tt.wantFailover {
				require.Nil(t, result)
				require.ErrorAs(t, err, &failoverErr)
			} else {
				require.NotNil(t, result)
				require.Error(t, err)
				require.False(t, errors.As(err, &failoverErr))
			}
			require.Len(t, writes, tt.wantWrites)
		})
	}
}
