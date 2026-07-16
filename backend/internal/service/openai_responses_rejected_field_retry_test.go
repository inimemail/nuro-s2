package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIResponsesRejectedFieldRetryStateIsLazyAndBounded(t *testing.T) {
	state := &openAIResponsesRejectedFieldRetryState{}
	require.Nil(t, state.seenBodyHashes)
	current := []byte(`{"model":"gpt-5.5"}`)
	for attempt := 0; attempt < maxOpenAIResponsesRejectedFieldRetries; attempt++ {
		next := []byte(fmt.Sprintf(`{"model":"gpt-5.5","attempt":%d}`, attempt))
		require.True(t, state.Allow(current, next))
		require.False(t, state.Allow(current, next))
		current = next
	}
	require.False(t, state.Allow(current, []byte(`{"model":"gpt-5.5","overflow":true}`)))
}

func TestNormalizeOpenAIResponsesRejectedFieldRetryBodyIsExact(t *testing.T) {
	t.Run("top level max output", func(t *testing.T) {
		body := []byte(`{"max_output_tokens":2048,"input":[{"content":{"max_output_tokens":"keep"}}]}`)
		retryBody, field, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(
			http.StatusBadRequest,
			body,
			[]byte(`{"error":{"code":"unsupported_parameter","param":"max_output_tokens","message":"Unsupported parameter: max_output_tokens"}}`),
		)
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, "max_output_tokens", field)
		require.False(t, gjson.GetBytes(retryBody, "max_output_tokens").Exists())
		require.Equal(t, "keep", gjson.GetBytes(retryBody, "input.0.content.max_output_tokens").String())
	})

	t.Run("tool namespace only", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"message","namespace":"keep"},{"type":"custom_tool_call","namespace":"remove"}]}`)
		retryBody, field, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(
			http.StatusBadRequest,
			body,
			[]byte(`{"error":{"code":"unknown_parameter","param":"input[1].namespace","message":"Unknown parameter: input[1].namespace"}}`),
		)
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, "input[1].namespace", field)
		require.Equal(t, "keep", gjson.GetBytes(retryBody, "input.0.namespace").String())
		require.False(t, gjson.GetBytes(retryBody, "input.1.namespace").Exists())
	})

	t.Run("ambiguous validation error", func(t *testing.T) {
		retryBody, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(
			http.StatusBadRequest,
			[]byte(`{"max_output_tokens":2048}`),
			[]byte(`{"error":{"code":"invalid_request_error","param":"max_output_tokens","message":"max_output_tokens must be positive"}}`),
		)
		require.NoError(t, err)
		require.False(t, changed)
		require.Nil(t, retryBody)
	})

	t.Run("websocket response failed envelope", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"custom_tool_call","namespace":"remove"}]}`)
		retryBody, field, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(
			http.StatusBadRequest,
			body,
			[]byte(`{"type":"response.failed","response":{"error":{"code":"unknown_parameter","param":"input[0].namespace","message":"Unknown parameter: input[0].namespace"}}}`),
		)
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, "input[0].namespace", field)
		require.False(t, gjson.GetBytes(retryBody, "input.0.namespace").Exists())
	})
}

func TestOpenAIGatewayServiceRetriesExplicitRejectedResponsesFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","stream":false,"max_output_tokens":2048,"input":[{"type":"custom_tool_call","namespace":"remove","input":"{}"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newRejectedFieldTestResponse(http.StatusBadRequest, `{"error":{"code":"unknown_parameter","param":"input[0].namespace","message":"Unknown parameter: input[0].namespace"}}`),
		newRejectedFieldTestResponse(http.StatusBadRequest, `{"error":{"code":"unsupported_parameter","param":"max_output_tokens","message":"Unsupported parameter: max_output_tokens"}}`),
		newRejectedFieldTestResponse(http.StatusOK, `{"output":[],"usage":{"input_tokens":1,"output_tokens":1}}`),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID: 99, Name: "responses", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.openai.com"},
		Extra:       map[string]any{openai_compat.ExtraKeyResponsesSupported: true}, Status: StatusActive, Schedulable: true,
	}
	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 3)
	require.False(t, gjson.GetBytes(upstream.bodies[1], "input.0.namespace").Exists())
	require.True(t, gjson.GetBytes(upstream.bodies[1], "max_output_tokens").Exists())
	require.False(t, gjson.GetBytes(upstream.bodies[2], "max_output_tokens").Exists())
}

func newRejectedFieldTestResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
