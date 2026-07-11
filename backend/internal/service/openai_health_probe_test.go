package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestConfigureOpenAIResponsesHealthProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := healthProbeRequestBody()

	for _, model := range []string{"gpt-5.4", "gpt-5.5", "o4-mini"} {
		t.Run("valid profile "+model, func(t *testing.T) {
			modelBody := healthProbeRequestBodyForModel(model)
			c, _ := healthProbeTestContext(modelBody, OpenAIHealthProbeProfileResponsesV1)
			enabled, err := ConfigureOpenAIResponsesHealthProbe(c, modelBody, model, false)
			require.NoError(t, err)
			require.True(t, enabled)
			require.True(t, IsOpenAIResponsesHealthProbe(c))
			storedModel, ok := OpenAIResponsesHealthProbeModel(c)
			require.True(t, ok)
			require.Equal(t, model, storedModel)
		})
	}

	t.Run("valid profile without reasoning", func(t *testing.T) {
		modelBody := []byte(`{"model":"gpt-4o","instructions":"Return exactly MONITOR_OK as plain text.","input":"Return exactly MONITOR_OK.","max_output_tokens":512,"stream":false,"store":false}`)
		c, _ := healthProbeTestContext(modelBody, OpenAIHealthProbeProfileResponsesV1)
		enabled, err := ConfigureOpenAIResponsesHealthProbe(c, modelBody, "gpt-4o", false)
		require.NoError(t, err)
		require.True(t, enabled)
	})

	tests := []struct {
		name    string
		profile string
		body    []byte
		model   string
		stream  bool
	}{
		{name: "no profile", body: body, model: "gpt-5.5"},
		{name: "unknown profile", profile: "unknown", body: body, model: "gpt-5.5"},
		{name: "mismatched model", profile: OpenAIHealthProbeProfileResponsesV1, body: body, model: "gpt-5.4"},
		{name: "empty model", profile: OpenAIHealthProbeProfileResponsesV1, body: body},
		{name: "streaming", profile: OpenAIHealthProbeProfileResponsesV1, body: body, model: "gpt-5.5", stream: true},
		{name: "missing stream", profile: OpenAIHealthProbeProfileResponsesV1, body: []byte(`{"model":"gpt-5.5","instructions":"Return exactly MONITOR_OK as plain text.","input":"Return exactly MONITOR_OK.","reasoning":{"effort":"none"},"max_output_tokens":512,"store":false}`), model: "gpt-5.5"},
		{name: "tools", profile: OpenAIHealthProbeProfileResponsesV1, body: []byte(`{"model":"gpt-5.5","instructions":"reply","input":"ok","tools":[{"type":"function","name":"x"}],"stream":false}`), model: "gpt-5.5"},
		{name: "excessive output", profile: OpenAIHealthProbeProfileResponsesV1, body: []byte(`{"model":"gpt-5.5","instructions":"reply","input":"ok","max_output_tokens":2048,"stream":false}`), model: "gpt-5.5"},
		{name: "fractional output", profile: OpenAIHealthProbeProfileResponsesV1, body: []byte(`{"model":"gpt-5.5","instructions":"Return exactly MONITOR_OK as plain text.","input":"Return exactly MONITOR_OK.","reasoning":{"effort":"none"},"max_output_tokens":512.5,"stream":false,"store":false}`), model: "gpt-5.5"},
		{name: "unsupported reasoning", profile: OpenAIHealthProbeProfileResponsesV1, body: []byte(`{"model":"gpt-5.5","instructions":"Return exactly MONITOR_OK as plain text.","input":"Return exactly MONITOR_OK.","reasoning":{"effort":"low"},"max_output_tokens":512,"stream":false,"store":false}`), model: "gpt-5.5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := healthProbeTestContext(tt.body, tt.profile)
			enabled, err := ConfigureOpenAIResponsesHealthProbe(c, tt.body, tt.model, tt.stream)
			if tt.profile == "" {
				require.NoError(t, err)
				require.False(t, enabled)
				return
			}
			require.Error(t, err)
			require.False(t, enabled)
			require.False(t, IsOpenAIResponsesHealthProbe(c))
		})
	}
}

func TestOpenAIHealthProbeEmptyJSONTriggersRequestLocalFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"id":"resp_empty","object":"response","model":"gpt-5.5","output":[{"type":"reasoning","summary":[]}],"usage":{"input_tokens":12,"output_tokens":4,"total_tokens":16}}`)
	c, recorder := configuredHealthProbeContext(t)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"req-empty"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	account := &Account{ID: 91, Name: "probe-account", Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	service := &OpenAIGatewayService{cfg: &config.Config{}}

	result, err := service.handleNonStreamingResponse(context.Background(), response, c, account, "gpt-5.5", "gpt-5.5")
	require.Nil(t, result)
	require.Error(t, err)
	require.Empty(t, recorder.Body.String())

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.True(t, failoverErr.SkipPoolSoftCooldown)
	require.True(t, failoverErr.SkipPromptCacheAvoidance)
	require.True(t, failoverErr.SkipStickySessionEviction)
	require.True(t, failoverErr.SkipSchedulePenalty)
	require.Equal(t, "gpt-5.5", failoverErr.ProbeModel)
	require.True(t, IsOpenAIHealthProbeEmptyErrorBody(failoverErr.ResponseBody))
	require.Equal(t, "req-empty", failoverErr.ResponseHeaders.Get("x-request-id"))
}

func TestOpenAIHealthProbeEmptySSEResponseTriggersFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, recorder := configuredHealthProbeContext(t)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.reasoning_summary_text.delta","delta":"thinking"}`,
			`data: {"type":"response.completed","response":{"id":"resp_empty_sse","object":"response","model":"gpt-5.5","output":[{"type":"reasoning","summary":[]}],"usage":{"input_tokens":12,"output_tokens":4,"total_tokens":16}}}`,
			`data: [DONE]`,
		}, "\n"))),
	}
	service := &OpenAIGatewayService{cfg: &config.Config{}}

	result, err := service.handleNonStreamingResponse(context.Background(), response, c, &Account{ID: 92, Platform: PlatformOpenAI}, "gpt-5.5", "gpt-5.5")
	require.Nil(t, result)
	require.Error(t, err)
	require.Empty(t, recorder.Body.String())
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.True(t, IsOpenAIHealthProbeEmptyErrorBody(failoverErr.ResponseBody))
}

func TestOpenAIHealthProbeNonEmptyResponsePassesThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, recorder := configuredHealthProbeContext(t)
	body := []byte(`{"id":"resp_ok","object":"response","model":"gpt-5.5","output":[{"type":"reasoning","summary":[]},{"type":"message","content":[{"type":"output_text","text":"MONITOR_OK"}]}],"usage":{"input_tokens":12,"output_tokens":2,"total_tokens":14}}`)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	service := &OpenAIGatewayService{cfg: &config.Config{}}

	result, err := service.handleNonStreamingResponse(context.Background(), response, c, &Account{ID: 93, Platform: PlatformOpenAI}, "gpt-5.5", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, recorder.Body.String(), "MONITOR_OK")
}

func TestOpenAIHealthProbeMarkerIsNotForwardedUpstream(t *testing.T) {
	header := strings.ToLower(OpenAIHealthProbeHeader)
	require.False(t, openaiAllowedHeaders[header])
	require.False(t, openaiPassthroughAllowedHeaders[header])
}

func TestOpenAIHealthProbeUpstreamContextKeepsCancellation(t *testing.T) {
	c, _ := configuredHealthProbeContext(t)
	parent, cancel := context.WithCancel(context.Background())
	upstream, release := openAIUpstreamRequestContext(parent, c)
	release()
	cancel()
	require.ErrorIs(t, upstream.Err(), context.Canceled)
}

func TestOpenAIHealthProbeSessionIsRequestLocalAndCleaned(t *testing.T) {
	first := NewOpenAIHealthProbeSessionHash()
	second := NewOpenAIHealthProbeSessionHash()
	require.NotEqual(t, first, second)
	require.True(t, IsOpenAIHealthProbeSessionHash(first))
	require.False(t, IsOpenAIPromptCacheBoostAffinitySessionHash(first))

	cache := &stubGatewayCache{sessionBindings: map[string]int64{"openai:" + first: 91, "openai:business-session": 92}}
	service := &OpenAIGatewayService{cache: cache}
	service.ReleaseOpenAIHealthProbeSession(context.Background(), nil, first)
	require.NotContains(t, cache.sessionBindings, "openai:"+first)
	require.Equal(t, int64(92), cache.sessionBindings["openai:business-session"])

	service.ReleaseOpenAIHealthProbeSession(context.Background(), nil, "business-session")
	require.Equal(t, int64(92), cache.sessionBindings["openai:business-session"])
}

func TestIsolateOpenAIHealthProbeFailoverOnlyChangesMarkedRequests(t *testing.T) {
	marked, _ := configuredHealthProbeContext(t)
	markedErr := &UpstreamFailoverError{StatusCode: http.StatusTooManyRequests}
	IsolateOpenAIHealthProbeFailover(marked, markedErr)
	require.True(t, markedErr.SkipPoolSoftCooldown)
	require.True(t, markedErr.SkipPromptCacheAvoidance)
	require.True(t, markedErr.SkipStickySessionEviction)
	require.True(t, markedErr.SkipSchedulePenalty)

	unmarked, _ := healthProbeTestContext(healthProbeRequestBody(), "")
	unmarkedErr := &UpstreamFailoverError{StatusCode: http.StatusTooManyRequests}
	IsolateOpenAIHealthProbeFailover(unmarked, unmarkedErr)
	require.False(t, unmarkedErr.SkipPoolSoftCooldown)
	require.False(t, unmarkedErr.SkipPromptCacheAvoidance)
	require.False(t, unmarkedErr.SkipStickySessionEviction)
	require.False(t, unmarkedErr.SkipSchedulePenalty)
}

func TestOpenAIHealthProbeDoesNotChangeUnmarkedEmptyResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := healthProbeRequestBody()
	c, recorder := healthProbeTestContext(body, "")
	responseBody := []byte(`{"id":"resp_empty","object":"response","model":"gpt-5.5","output":[],"usage":{"input_tokens":4,"output_tokens":0,"total_tokens":4}}`)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(responseBody)),
	}
	service := &OpenAIGatewayService{cfg: &config.Config{}}

	result, err := service.handleNonStreamingResponse(context.Background(), response, c, &Account{ID: 94, Platform: PlatformOpenAI}, "gpt-5.5", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, recorder.Body.String(), `"output":[]`)
}

func configuredHealthProbeContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	body := healthProbeRequestBody()
	c, recorder := healthProbeTestContext(body, OpenAIHealthProbeProfileResponsesV1)
	enabled, err := ConfigureOpenAIResponsesHealthProbe(c, body, "gpt-5.5", false)
	require.NoError(t, err)
	require.True(t, enabled)
	return c, recorder
}

func healthProbeTestContext(body []byte, profile string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if profile != "" {
		c.Request.Header.Set(OpenAIHealthProbeHeader, profile)
	}
	return c, recorder
}

func healthProbeRequestBody() []byte {
	return healthProbeRequestBodyForModel("gpt-5.5")
}

func healthProbeRequestBodyForModel(model string) []byte {
	return []byte(`{"model":"` + model + `","instructions":"Return exactly MONITOR_OK as plain text.","input":"Return exactly MONITOR_OK.","reasoning":{"effort":"none"},"max_output_tokens":512,"stream":false,"store":false}`)
}
