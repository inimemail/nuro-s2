package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
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

func TestBuildGrokResponsesRequestAppliesEligibleOverridesWithoutChangingAuthOrSession(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":               "grok-token",
			credKeyHeaderOverrideEnabled: true,
			credKeyHeaderOverrides: map[string]any{
				"user-agent":     "custom-grok-client",
				"x-custom-route": "route-a",
				"authorization":  "Bearer forged",
				"x-grok-conv-id": "static-session",
			},
		},
	}

	req, err := buildGrokResponsesRequest(context.Background(), nil, account, []byte(`{"input":"hi"}`), "grok-token", "server-session", &config.Config{})
	require.NoError(t, err)
	require.Equal(t, "custom-grok-client", req.Header.Get("User-Agent"))
	require.Equal(t, "route-a", getHeaderRaw(req.Header, "x-custom-route"))
	require.Equal(t, "Bearer grok-token", req.Header.Get("Authorization"))
	require.Equal(t, "server-session", req.Header.Get("X-Grok-Conv-Id"))
}

func TestBuildGrokResponsesRequestHeaderOverrideDisabledIsNoOp(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":         "grok-token",
			credKeyHeaderOverrides: map[string]any{"user-agent": "custom-grok-client"},
		},
	}

	req, err := buildGrokResponsesRequest(context.Background(), nil, account, []byte(`{"input":"hi"}`), "grok-token", "server-session", &config.Config{})
	require.NoError(t, err)
	require.NotEqual(t, "custom-grok-client", req.Header.Get("User-Agent"))
	require.Equal(t, "Bearer grok-token", req.Header.Get("Authorization"))
	require.Equal(t, "server-session", req.Header.Get("X-Grok-Conv-Id"))
}

type grokMediaCacheErrorStub struct {
	stubGatewayCache
	err error
}

func (c *grokMediaCacheErrorStub) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	return 0, c.err
}

func TestSelectBoundGrokMediaVideoRequestAccount_DoesNotFallbackOnCacheFailure(t *testing.T) {
	cacheErr := errors.New("redis unavailable")
	svc := &OpenAIGatewayService{cache: &grokMediaCacheErrorStub{err: cacheErr}}

	selection, exact, err := svc.SelectBoundGrokMediaVideoRequestAccount(context.Background(), nil, "video-request-1")
	require.Nil(t, selection)
	require.True(t, exact)
	require.ErrorIs(t, err, cacheErr)
}

func TestSelectBoundGrokMediaVideoRequestAccount_AllowsLegacyFallbackOnCacheMiss(t *testing.T) {
	svc := &OpenAIGatewayService{cache: &grokMediaCacheErrorStub{err: redis.Nil}}

	selection, exact, err := svc.SelectBoundGrokMediaVideoRequestAccount(context.Background(), nil, "video-request-2")
	require.NoError(t, err)
	require.Nil(t, selection)
	require.False(t, exact)
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

func TestSanitizeGrokReasoningNullContent(t *testing.T) {
	body := []byte(`{"input":[{"type":"reasoning","content":null,"encrypted_content":"keep"},{"type":"reasoning","content":"real content"},{"type":"message","content":null}]}`)

	got, err := sanitizeGrokReasoningNullContent(body)

	require.NoError(t, err)
	require.False(t, gjson.GetBytes(got, "input.0.content").Exists())
	require.Equal(t, "keep", gjson.GetBytes(got, "input.0.encrypted_content").String())
	require.Equal(t, "real content", gjson.GetBytes(got, "input.1.content").String())
	require.True(t, gjson.GetBytes(got, "input.2.content").Exists())
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
		{name: "image content", body: `{"model":"grok","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QQ=="}}]}]}`, want: true},
		{name: "text and image content", body: `{"model":"grok","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QQ=="}}]}]}`, want: true},
		{name: "responses image part falls back", body: `{"model":"grok","messages":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,QQ=="}]}]}`, reason: "unsupported_content_part_input_image"},
		{name: "empty image content", body: `{"model":"grok","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,"}}]}]}`, reason: "empty_message_content"},
		{name: "unknown content part", body: `{"model":"grok","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AA=="}}]}]}`, reason: "unsupported_content_part_input_audio"},
		{name: "empty content array", body: `{"model":"grok","messages":[{"role":"user","content":[]}]}`, reason: "empty_message_content"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := grokChatResponsesBridgeEligibility([]byte(tt.body))
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.reason, reason)
		})
	}
}

func TestIsGrokImageGenerationModelV156(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{model: "grok-imagine", want: true},
		{model: "grok-imagine-image-quality", want: true},
		{model: "grok-imagine-edit", want: true},
		{model: "grok-4.5", want: false},
		{model: "grok-composer", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			require.Equal(t, tt.want, isGrokImageGenerationModel(tt.model))
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

func TestGrokMediaEndpointIsVideoMutationRequest(t *testing.T) {
	tests := []struct {
		name     string
		endpoint GrokMediaEndpoint
		want     bool
	}{
		{name: "image generation", endpoint: GrokMediaEndpointImagesGenerations},
		{name: "image edit", endpoint: GrokMediaEndpointImagesEdits},
		{name: "video generation", endpoint: GrokMediaEndpointVideosGenerations, want: true},
		{name: "video edit", endpoint: GrokMediaEndpointVideosEdits, want: true},
		{name: "video extension", endpoint: GrokMediaEndpointVideosExtensions, want: true},
		{name: "video status", endpoint: GrokMediaEndpointVideoStatus},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.endpoint.IsVideoMutationRequest())
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
		{name: "video status failed", endpoint: GrokMediaEndpointVideoStatus, body: `{"request_id":"video_req_1","status":"failed","error":{"message":"private-provider.example failed"}}`, want: true},
		{name: "video status cancelled", endpoint: GrokMediaEndpointVideoStatus, body: `{"request_id":"video_req_1","status":"cancelled","reason":"private-provider"}`, want: true},
		{name: "diagnostic envelope", endpoint: GrokMediaEndpointVideoStatus, body: `{"message":"private-provider failed"}`},
		{name: "HTML error page", endpoint: GrokMediaEndpointVideoStatus, body: `<!DOCTYPE html><title>private.example | 502</title>`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, grokMediaSuccessResponseIsValid(tt.endpoint, []byte(tt.body)))
		})
	}
}

func TestSanitizeGrokMediaUnsuccessfulTerminalResponse(t *testing.T) {
	body := []byte(`{"request_id":"video_req_1","id":"https://private-provider.example/id","status":"failed","created_at":123,"updated_at":"https://private-provider.example/time","error":{"message":"x.ai failed"},"provider":"xai","url":"https://private-provider.example/error"}`)

	got := sanitizeGrokMediaUnsuccessfulTerminalResponse(body, "failed")

	require.JSONEq(t, `{"request_id":"video_req_1","status":"failed","created_at":123}`, string(got))
	require.NotContains(t, string(got), "x.ai")
	require.NotContains(t, string(got), "private-provider")
}

func TestSelectBoundGrokMediaVideoRequestAccountKeepsOriginalAccount(t *testing.T) {
	groupID := int64(12)
	original := Account{ID: 71, Platform: PlatformGrok, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 2, Priority: 9}
	higherPriority := Account{ID: 72, Platform: PlatformGrok, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 2, Priority: 0}
	cache := &stubGatewayCache{}
	svc := &OpenAIGatewayService{
		accountRepo:        stubOpenAIAccountRepo{accounts: []Account{original, higherPriority}},
		cache:              cache,
		concurrencyService: NewConcurrencyService(stubConcurrencyCache{}),
	}
	require.NoError(t, svc.BindGrokMediaVideoRequestAccount(context.Background(), &groupID, "video_req_bound", original.ID))

	selection, bound, err := svc.SelectBoundGrokMediaVideoRequestAccount(context.Background(), &groupID, "video_req_bound")

	require.NoError(t, err)
	require.True(t, bound)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, original.ID, selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
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

func TestOpenAIGatewayService_ForwardGrokMediaOAuthUsesImagineEndpointAndCLIHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"data":[{"url":"https://cdn.example/image.png"}]}`)),
		},
	}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:       76,
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":               "grok-token",
			credKeyHeaderOverrideEnabled: true,
			credKeyHeaderOverrides: map[string]any{
				"user-agent":    "custom-grok-media",
				"x-media-route": "route-b",
				"authorization": "Bearer forged",
			},
		},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)

	result, err := svc.ForwardGrokMedia(
		context.Background(),
		c,
		account,
		GrokMediaEndpointImagesGenerations,
		"",
		[]byte(`{"model":"grok-imagine","prompt":"draw"}`),
		"application/json",
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "https://api.x.ai/v1/images/generations", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer grok-token", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, grokCLIVersion, upstream.lastReq.Header.Get("X-Grok-Client-Version"))
	require.Equal(t, "custom-grok-media", upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, "route-b", getHeaderRaw(upstream.lastReq.Header, "x-media-route"))
}

func TestOpenAIGatewayService_ForwardGrokVideoMutationEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name     string
		endpoint GrokMediaEndpoint
		path     string
	}{
		{name: "edit", endpoint: GrokMediaEndpointVideosEdits, path: "/v1/videos/edits"},
		{name: "extension", endpoint: GrokMediaEndpointVideosExtensions, path: "/v1/videos/extensions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"grok-imagine-video","prompt":"continue","video":{"url":"https://example.com/in.mp4"},"duration":6}`)
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewBufferString(`{"request_id":"video-mutation-123"}`)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{
				ID: 71, Platform: PlatformGrok, Type: AccountTypeAPIKey, Concurrency: 1,
				Credentials: map[string]any{"api_key": "api-key"},
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, bytes.NewReader(body))

			result, err := svc.ForwardGrokMedia(context.Background(), c, account, tt.endpoint, "", body, "application/json")

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, "https://api.x.ai"+tt.path, upstream.lastReq.URL.String())
			require.JSONEq(t, string(body), string(upstream.lastBody))
			require.Equal(t, "video-mutation-123", result.ResponseID)
			require.Equal(t, 1, result.VideoCount)
			require.Equal(t, 6, result.VideoDurationSeconds)
		})
	}
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
	clearRateLimitCalls  int
	clearRateLimitResult bool
	tempUnschedAccountID int64
	tempUnschedUntil     time.Time
	tempUnschedReason    string
}

func (r *openAIGrokAccountRepoStub) ClearRateLimitIfObserved(
	_ context.Context,
	_ int64,
	_, _ time.Time,
) (bool, error) {
	r.clearRateLimitCalls++
	return r.clearRateLimitResult, nil
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

func TestGrokSuccessfulResponseClearsObservedRuntimeRateLimit(t *testing.T) {
	now := time.Now()
	resetAt := now.Add(10 * time.Minute)
	repo := &openAIGrokAccountRepoStub{clearRateLimitResult: true}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{
		ID: 901, Platform: PlatformGrok, Type: AccountTypeOAuth,
		RateLimitedAt: &now, RateLimitResetAt: &resetAt,
	}
	svc.BlockAccountScheduling(account, resetAt, "429")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))

	svc.updateGrokUsageFromResponse(context.Background(), account, nil, http.StatusOK)

	require.Equal(t, 1, repo.clearRateLimitCalls)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestGrokSuccessfulResponseKeepsActiveTemporaryRuntimeBlock(t *testing.T) {
	now := time.Now()
	resetAt := now.Add(10 * time.Minute)
	tempUntil := now.Add(20 * time.Minute)
	repo := &openAIGrokAccountRepoStub{clearRateLimitResult: true}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{
		ID: 902, Platform: PlatformGrok, Type: AccountTypeOAuth,
		RateLimitedAt: &now, RateLimitResetAt: &resetAt,
		TempUnschedulableUntil: &tempUntil,
	}
	svc.BlockAccountScheduling(account, tempUntil, "temporary_error")

	svc.updateGrokUsageFromResponse(context.Background(), account, nil, http.StatusOK)

	require.Equal(t, 1, repo.clearRateLimitCalls)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestGrokSuccessfulResponseKeepsNewerConcurrentRateLimitBlock(t *testing.T) {
	now := time.Now()
	observedResetAt := now.Add(10 * time.Minute)
	newResetAt := now.Add(30 * time.Minute)
	repo := &openAIGrokAccountRepoStub{clearRateLimitResult: true}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{
		ID: 904, Platform: PlatformGrok, Type: AccountTypeOAuth,
		RateLimitedAt: &now, RateLimitResetAt: &observedResetAt,
	}
	svc.BlockAccountScheduling(account, newResetAt, "newer_429")

	svc.updateGrokUsageFromResponse(context.Background(), account, nil, http.StatusOK)

	require.Equal(t, 1, repo.clearRateLimitCalls)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	actualResetAt, ok := value.(time.Time)
	require.True(t, ok)
	require.WithinDuration(t, newResetAt, actualResetAt, time.Second)
}

func TestGrokSuccessfulResponseKeepsRuntimeBlockWhenRateLimitCASLoses(t *testing.T) {
	now := time.Now()
	resetAt := now.Add(10 * time.Minute)
	repo := &openAIGrokAccountRepoStub{clearRateLimitResult: false}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{
		ID: 903, Platform: PlatformGrok, Type: AccountTypeOAuth,
		RateLimitedAt: &now, RateLimitResetAt: &resetAt,
	}
	svc.BlockAccountScheduling(account, resetAt, "429")

	svc.updateGrokUsageFromResponse(context.Background(), account, nil, http.StatusOK)

	require.Equal(t, 1, repo.clearRateLimitCalls)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}
