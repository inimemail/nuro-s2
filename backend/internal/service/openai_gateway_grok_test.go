package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPatchGrokResponsesBody(t *testing.T) {
	body := []byte(`{"model":"grok","input":"hi","prompt_cache_retention":"24h","safety_identifier":"u"}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.3")
	require.NoError(t, err)
	require.Equal(t, "grok-4.3", gjson.GetBytes(patched, "model").String())
	require.False(t, gjson.GetBytes(patched, "prompt_cache_retention").Exists())
	require.False(t, gjson.GetBytes(patched, "safety_identifier").Exists())
}

func TestOpenAIGatewayService_ForwardGrokResponses_UsesGrokUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"xai_req_1"}},
			Body: io.NopCloser(bytes.NewBufferString(
				`{"id":"resp_1","object":"response","model":"grok-4.3","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`,
			)),
		},
	}
	svc := &OpenAIGatewayService{
		cfg:                   &config.Config{},
		httpUpstream:          upstream,
		codexSnapshotThrottle: newAccountWriteThrottle(time.Minute),
	}
	account := &Account{
		ID:          77,
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "grok-token"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"grok","input":"hi","stream":false}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "grok", result.Model)
	require.Equal(t, "grok-4.3", result.UpstreamModel)
	require.Equal(t, "Bearer grok-token", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "https://api.x.ai/v1/responses", upstream.lastReq.URL.String())
	require.Equal(t, "grok-4.3", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestOpenAIGatewayService_ForwardGrokResponses_429TempUnschedules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"7"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"message":"rate limited"}}`)),
		},
	}
	repo := &openAIGrokAccountRepoStub{}
	svc := &OpenAIGatewayService{
		cfg:                   &config.Config{},
		httpUpstream:          upstream,
		accountRepo:           repo,
		codexSnapshotThrottle: newAccountWriteThrottle(time.Minute),
	}
	account := &Account{
		ID:          88,
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "grok-token"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"grok","input":"hi","stream":false}`))
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, int64(88), repo.tempUnschedAccountID)
	require.Contains(t, repo.tempUnschedReason, "grok rate limited")
	require.True(t, time.Until(repo.tempUnschedUntil) > 5*time.Second)
}

type openAIGrokAccountRepoStub struct {
	AccountRepository
	tempUnschedAccountID int64
	tempUnschedUntil     time.Time
	tempUnschedReason    string
}

func (r *openAIGrokAccountRepoStub) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.tempUnschedAccountID = id
	r.tempUnschedUntil = until
	r.tempUnschedReason = reason
	return nil
}

func (r *openAIGrokAccountRepoStub) UpdateExtra(_ context.Context, _ int64, _ map[string]any) error {
	return nil
}
