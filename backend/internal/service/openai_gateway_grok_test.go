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
	body := []byte(`{"model":"grok","input":"hi","prompt_cache_retention":"24h","safety_identifier":"u","presence_penalty":1,"frequencyPenalty":1,"stop":["x"]}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.5")
	require.NoError(t, err)
	require.Equal(t, "grok-4.5", gjson.GetBytes(patched, "model").String())
	require.False(t, gjson.GetBytes(patched, "prompt_cache_retention").Exists())
	require.False(t, gjson.GetBytes(patched, "safety_identifier").Exists())
	require.False(t, gjson.GetBytes(patched, "presence_penalty").Exists())
	require.False(t, gjson.GetBytes(patched, "frequencyPenalty").Exists())
	require.False(t, gjson.GetBytes(patched, "stop").Exists())
}

func TestNormalizeGrokMediaModelForEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint GrokMediaEndpoint
		model    string
		want     string
	}{
		{name: "image generation alias", endpoint: GrokMediaEndpointImagesGenerations, model: "grok-imagine", want: "grok-imagine-image-quality"},
		{name: "image edit alias", endpoint: GrokMediaEndpointImagesEdits, model: "grok-imagine", want: "grok-imagine-image-quality"},
		{name: "image quality passthrough", endpoint: GrokMediaEndpointImagesGenerations, model: "grok-imagine-image-quality", want: "grok-imagine-image-quality"},
		{name: "image fast passthrough", endpoint: GrokMediaEndpointImagesGenerations, model: "grok-imagine-image", want: "grok-imagine-image"},
		{name: "video passthrough", endpoint: GrokMediaEndpointVideosGenerations, model: "grok-imagine-video", want: "grok-imagine-video"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, normalizeGrokMediaModelForEndpoint(tt.endpoint, tt.model, false))
		})
	}
}

func TestNormalizeGrokMediaForwardBodyRewritesImagineAlias(t *testing.T) {
	body := []byte(`{"model":"grok-imagine","prompt":"draw a cat"}`)

	normalized, contentType, err := normalizeGrokMediaForwardBody(GrokMediaEndpointImagesGenerations, body, "application/json")

	require.NoError(t, err)
	require.Equal(t, "application/json", contentType)
	require.Equal(t, "grok-imagine-image-quality", gjson.GetBytes(normalized, "model").String())
	require.Equal(t, "draw a cat", gjson.GetBytes(normalized, "prompt").String())
}

func TestParseGrokMediaRequestVideoBillingMetadata(t *testing.T) {
	info := ParseGrokMediaRequest(
		"application/json",
		[]byte(`{"model":"grok-imagine-video-1.5","prompt":"clip","resolution":"1080p","duration":10}`),
	)

	require.Equal(t, "grok-imagine-video-1.5", info.Model)
	require.Equal(t, VideoBillingResolution1080P, info.Resolution)
	require.Equal(t, 10, info.DurationSeconds)

	usage := grokMediaUsageFromResponse(
		GrokMediaEndpointVideosGenerations,
		info,
		[]byte(`{"request_id":"video_req_1"}`),
	)
	require.Equal(t, "video_req_1", usage.ResponseID)
	require.Equal(t, 1, usage.VideoCount)
	require.Equal(t, VideoBillingResolution1080P, usage.VideoResolution)
	require.Equal(t, 10, usage.VideoDurationSeconds)
	require.Equal(t, 1, usage.ImageCount)
	require.Empty(t, usage.ImageSize)
}

func TestOpenAIGatewayService_ForwardGrokResponses_UsesGrokUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"xai_req_1"}},
			Body: io.NopCloser(bytes.NewBufferString(
				`{"id":"resp_1","object":"response","model":"grok-4.5","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`,
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
	require.Equal(t, "grok-4.5", result.UpstreamModel)
	require.Equal(t, "Bearer grok-token", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "https://api.x.ai/v1/responses", upstream.lastReq.URL.String())
	require.Equal(t, "grok-4.5", gjson.GetBytes(upstream.lastBody, "model").String())
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
