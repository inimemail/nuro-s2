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
	"github.com/stretchr/testify/assert"
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

func TestPatchGrokResponsesBodySanitizesComposerCapabilitiesAndPrivateInput(t *testing.T) {
	body := []byte(`{"model":"grok-composer-2.5-fast","reasoning":{"effort":"high"},"reasoning_effort":"high","reasoningEffort":"high","input":[{"type":"additional_tools","tools":[]},{"type":"message","role":"user","content":"hi"}]}`)

	patched, err := patchGrokResponsesBody(body, "grok-composer-2.5-fast")
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(patched, "reasoning").Exists())
	require.False(t, gjson.GetBytes(patched, "reasoning_effort").Exists())
	require.False(t, gjson.GetBytes(patched, "reasoningEffort").Exists())
	require.Equal(t, 1, len(gjson.GetBytes(patched, "input").Array()))
	require.Equal(t, "message", gjson.GetBytes(patched, "input.0.type").String())
}

func TestPatchGrokResponsesBodyPreservesReasoningForSupportedModel(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","reasoning":{"effort":"high"},"reasoning_effort":"high","input":"hi"}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.5")
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(patched, "reasoning").Exists())
	require.Equal(t, "high", gjson.GetBytes(patched, "reasoning_effort").String())
}

func TestPatchGrokResponsesBodyOnlyRemovesTopLevelExternalWebAccess(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","external_web_access":true,"input":[{"type":"message","role":"user","content":{"external_web_access":"business-data"}}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"external_web_access":{"type":"string"}}}}]}`)

	patched, err := patchGrokResponsesBody(body, "grok-4.5")
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(patched, "external_web_access").Exists())
	assert.Equal(t, "business-data", gjson.GetBytes(patched, "input.0.content.external_web_access").String())
	assert.True(t, gjson.GetBytes(patched, "tools.0.parameters.properties.external_web_access").Exists())
}

func TestGrokRuntimeBlockUsesOpenAICompatibleSchedulerFastPath(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 123, Platform: PlatformGrok, Type: AccountTypeOAuth}

	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))

	svc.ClearAccountSchedulingBlock(account.ID)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestGrokChatResponsesBridgeEligibilityV152(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		want   bool
		reason string
	}{
		{name: "compatible", body: `{"model":"grok","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true},"max_completion_tokens":256}`, want: true},
		{name: "invalid stream", body: `{"model":"grok","messages":[{"role":"user","content":"hi"}],"stream":"yes"}`, reason: "invalid_stream"},
		{name: "unknown stream option", body: `{"model":"grok","messages":[{"role":"user","content":"hi"}],"stream_options":{"extra":true}}`, reason: "unknown_stream_option_extra"},
		{name: "small token limit", body: `{"model":"grok","messages":[{"role":"user","content":"hi"}],"max_tokens":32}`, reason: "unsafe_max_tokens"},
		{name: "conflicting token limits", body: `{"model":"grok","messages":[{"role":"user","content":"hi"}],"max_tokens":256,"max_completion_tokens":256}`, reason: "conflicting_max_tokens"},
		{name: "unsafe message field", body: `{"model":"grok","messages":[{"role":"assistant","content":"hi","tool_calls":[]}]}`, reason: "unsafe_message_field_tool_calls"},
		{name: "empty message", body: `{"model":"grok","messages":[{"role":"assistant","content":""}]}`, reason: "empty_message_content"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := grokChatResponsesBridgeEligibility([]byte(tt.body))
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.reason, reason)
		})
	}
}

func TestGenerateSessionHashUsesGrokConversationHeaderOnlyForGrokGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set(grokConversationIDHeader, "grok-conversation")
	c.Set("api_key", &APIKey{ID: 1, Group: &Group{Platform: PlatformGrok}})

	svc := &OpenAIGatewayService{}
	require.NotEmpty(t, svc.GenerateSessionHash(c, nil))

	c.Set("api_key", &APIKey{ID: 1, Group: &Group{Platform: PlatformOpenAI}})
	require.Empty(t, svc.GenerateSessionHash(c, nil))
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

func TestGrokMediaSuccessResponseIsValid(t *testing.T) {
	tests := []struct {
		name     string
		endpoint GrokMediaEndpoint
		body     string
		want     bool
	}{
		{name: "image URL", endpoint: GrokMediaEndpointImagesGenerations, body: `{"data":[{"url":"https://cdn.example/image.png"}]}`, want: true},
		{name: "image base64", endpoint: GrokMediaEndpointImagesEdits, body: `{"data":[{"b64_json":"aW1hZ2U="}]}`, want: true},
		{name: "empty image output", endpoint: GrokMediaEndpointImagesGenerations, body: `{"data":[]}`},
		{name: "video request", endpoint: GrokMediaEndpointVideosGenerations, body: `{"request_id":"video_req_1"}`, want: true},
		{name: "video request missing ID", endpoint: GrokMediaEndpointVideosGenerations, body: `{"status":"pending"}`},
		{name: "video status pending", endpoint: GrokMediaEndpointVideoStatus, body: `{"status":"pending"}`, want: true},
		{name: "video status done", endpoint: GrokMediaEndpointVideoStatus, body: `{"status":"done","video":{"url":"https://cdn.example/video.mp4"}}`, want: true},
		{name: "diagnostic envelope", endpoint: GrokMediaEndpointVideoStatus, body: `{"message":"private-provider failed"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, grokMediaSuccessResponseIsValid(tt.endpoint, []byte(tt.body)))
		})
	}
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
	require.Equal(t, "https://cli-chat-proxy.grok.com/v1/responses", upstream.lastReq.URL.String())
	require.Equal(t, "grok-4.5", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestOpenAIGatewayService_ForwardGrokResponses_PropagatesStreamTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(bytes.NewBufferString(
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}}\n\n",
			)),
		},
	}
	svc := &OpenAIGatewayService{
		cfg:                   &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		httpUpstream:          upstream,
		codexSnapshotThrottle: newAccountWriteThrottle(time.Minute),
	}
	account := &Account{
		ID:          78,
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "grok-token"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"grok","input":"hi","stream":true}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "response.completed", result.TerminalEventType)
	require.False(t, result.ClientDisconnect)
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.OutputTokens)
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
	require.Equal(t, int64(88), repo.rateLimitedAccountID)
	require.True(t, time.Until(repo.rateLimitResetAt) > 5*time.Second)
}

type openAIGrokAccountRepoStub struct {
	AccountRepository
	rateLimitedAccountID int64
	rateLimitResetAt     time.Time
	tempUnschedAccountID int64
	tempUnschedUntil     time.Time
	tempUnschedReason    string
}

func (r *openAIGrokAccountRepoStub) SetRateLimited(_ context.Context, id int64, resetAt time.Time) error {
	r.rateLimitedAccountID = id
	r.rateLimitResetAt = resetAt
	return nil
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
