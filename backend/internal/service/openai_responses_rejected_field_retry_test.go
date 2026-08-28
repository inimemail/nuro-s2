package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

	t.Run("legacy alias alone cannot trigger replay", func(t *testing.T) {
		retryBody, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(
			http.StatusBadRequest,
			[]byte(`{"max_tokens":2048}`),
			[]byte(`{"error":{"code":"unsupported_parameter","param":"max_output_tokens","message":"Unsupported parameter: max_output_tokens"}}`),
		)
		require.NoError(t, err)
		require.False(t, changed)
		require.Nil(t, retryBody)
	})

	t.Run("canonical rebuild removes compatibility alias", func(t *testing.T) {
		body := []byte(`{"max_tokens":1024,"max_output_tokens":2048,"input":"keep"}`)
		rebuilt, changed, err := RemoveOpenAIResponsesRejectedField(body, "max_output_tokens")
		require.NoError(t, err)
		require.True(t, changed)
		require.False(t, gjson.GetBytes(rebuilt, "max_tokens").Exists())
		require.False(t, gjson.GetBytes(rebuilt, "max_output_tokens").Exists())
		require.Equal(t, "keep", gjson.GetBytes(rebuilt, "input").String())
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

	t.Run("status clears every item of the rejected type only", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"message","status":"keep-other"},{"type":"function_call","status":"remove-1"},{"type":"function_call","status":"remove-2"},{"type":"message","status":"keep-other-2"}]}`)
		retryBody, field, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(
			http.StatusBadRequest,
			body,
			[]byte(`{"error":{"code":"unsupported_parameter","param":"input[1].status","message":"Unsupported parameter: input[1].status"}}`),
		)
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, "input[1].status", field)
		require.False(t, gjson.GetBytes(retryBody, "input.1.status").Exists())
		require.False(t, gjson.GetBytes(retryBody, "input.2.status").Exists())
		require.Equal(t, "keep-other", gjson.GetBytes(retryBody, "input.0.status").String())
		require.Equal(t, "keep-other-2", gjson.GetBytes(retryBody, "input.3.status").String())
	})

	t.Run("status cleanup can be replayed for a failover account", func(t *testing.T) {
		body := []byte(`{"input":[{"type":"message","status":"keep"},{"type":"function_call","status":"remove-1"},{"type":"function_call","status":"remove-2"}]}`)
		retryBody, changed, err := RemoveOpenAIResponsesRejectedField(body, "input[1].status")
		require.NoError(t, err)
		require.True(t, changed)
		require.False(t, gjson.GetBytes(retryBody, "input.1.status").Exists())
		require.False(t, gjson.GetBytes(retryBody, "input.2.status").Exists())
		require.Equal(t, "keep", gjson.GetBytes(retryBody, "input.0.status").String())
	})
}

func TestOpenAIGatewayServiceComposesNamespaceStripWithRejectedFieldRetry(t *testing.T) {
	setGinTestMode()
	body := []byte(`{"model":"gpt-5.5","stream":false,"max_output_tokens":2048,"input":[{"type":"custom_tool_call","namespace":"remove","input":"{}"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
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
	require.Len(t, upstream.bodies, 2)
	for _, forwardedBody := range upstream.bodies {
		require.False(t, gjson.GetBytes(forwardedBody, "input.0.namespace").Exists())
	}
	require.True(t, gjson.GetBytes(upstream.bodies[0], "max_output_tokens").Exists())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "max_output_tokens").Exists())
}

func TestOpenAIResponsesRejectedFieldCacheIsScopedAndExpires(t *testing.T) {
	account := &Account{
		ID: 1001, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://upstream.example/v1"},
	}
	svc := &OpenAIGatewayService{}
	body := []byte(`{"model":"gpt-5.6","max_output_tokens":2048,"input":[{"type":"custom_tool_call","namespace":"tools","status":"completed"}]}`)

	svc.RecordOpenAIResponsesRejectedField(account, "gpt-5.6", string(OpenAIUpstreamTransportHTTPSSE), "max_output_tokens")
	updated, changed, err := svc.ApplyOpenAIResponsesRejectedFieldCache(account, "gpt-5.6", string(OpenAIUpstreamTransportHTTPSSE), body)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(updated, "max_output_tokens").Exists())
	require.True(t, gjson.GetBytes(updated, "input.0.namespace").Exists())

	// A different model and transport must not inherit the remembered capability.
	untouched, changed, err := svc.ApplyOpenAIResponsesRejectedFieldCache(account, "gpt-5.5", string(OpenAIUpstreamTransportResponsesWebsocketV2), body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, string(body), string(untouched))
}

func TestOpenAIResponsesRejectedFieldCacheRemovesCategoryAcrossInputItems(t *testing.T) {
	account := &Account{ID: 1002, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc := &OpenAIGatewayService{}
	svc.RecordOpenAIResponsesRejectedField(account, "gpt-5.6", string(OpenAIUpstreamTransportHTTPSSE), "input[1].status")
	body := []byte(`{"model":"gpt-5.6","input":[{"type":"message","status":"completed"},{"type":"message","status":"in_progress"},{"type":"custom_tool_call","status":"completed"}]}`)
	updated, changed, err := svc.ApplyOpenAIResponsesRejectedFieldCache(account, "gpt-5.6", string(OpenAIUpstreamTransportHTTPSSE), body)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(updated, "input.0.status").Exists())
	require.False(t, gjson.GetBytes(updated, "input.1.status").Exists())
	require.False(t, gjson.GetBytes(updated, "input.2.status").Exists())
}

func TestOpenAIResponsesRejectedFieldCacheSeparatesCompactAndPlatforms(t *testing.T) {
	account := &Account{ID: 1003, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	body := []byte(`{"model":"gpt-5.6","max_output_tokens":2048}`)
	responsesScope := OpenAIResponsesRejectedFieldTransportScope(string(OpenAIUpstreamTransportHTTPSSE), false)
	compactScope := OpenAIResponsesRejectedFieldTransportScope(string(OpenAIUpstreamTransportHTTPSSE), true)
	require.NotEqual(t, responsesScope, compactScope)

	svc := &OpenAIGatewayService{}
	svc.RecordOpenAIResponsesRejectedField(account, "gpt-5.6", responsesScope, "max_output_tokens")
	updated, changed, err := svc.ApplyOpenAIResponsesRejectedFieldCache(account, "gpt-5.6", responsesScope, body)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(updated, "max_output_tokens").Exists())

	untouched, changed, err := svc.ApplyOpenAIResponsesRejectedFieldCache(account, "gpt-5.6", compactScope, body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, string(body), string(untouched))

	grok := &Account{ID: account.ID, Platform: PlatformGrok, Type: AccountTypeAPIKey}
	svc.RecordOpenAIResponsesRejectedField(grok, "grok-4", responsesScope, "max_output_tokens")
	untouched, changed, err = svc.ApplyOpenAIResponsesRejectedFieldCache(grok, "grok-4", responsesScope, body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, string(body), string(untouched))
}

func TestOpenAIResponsesRejectedFieldRemotePollIsSingleFlightPerScope(t *testing.T) {
	svc := &OpenAIGatewayService{}
	now := time.Now()
	var claimed atomic.Int64
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if svc.shouldPollOpenAIResponsesRejectedFields("scope", now) {
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int64(1), claimed.Load())
	require.False(t, svc.shouldPollOpenAIResponsesRejectedFields("scope", now.Add(openAIResponsesRejectedFieldRemotePollInterval-time.Millisecond)))
	require.True(t, svc.shouldPollOpenAIResponsesRejectedFields("scope", now.Add(openAIResponsesRejectedFieldRemotePollInterval)))
}

func TestOpenAIResponsesRejectedFieldCleanupRemovesExpiredLowTrafficEntry(t *testing.T) {
	svc := &OpenAIGatewayService{}
	const memoryKey = "scope:max_output_tokens"
	svc.storeOpenAIResponsesRejectedFieldUntil(memoryKey, time.Now().Add(20*time.Millisecond))

	require.Eventually(t, func() bool {
		_, valueExists := svc.openaiResponsesRejectedFieldUntil.Load(memoryKey)
		_, workerExists := svc.openaiResponsesRejectedFieldCleanupScheduled.Load(memoryKey)
		return !valueExists && !workerExists
	}, time.Second, 5*time.Millisecond)
}

func TestOpenAIResponsesRejectedFieldCleanupFollowsExtendedDeadline(t *testing.T) {
	svc := &OpenAIGatewayService{}
	const memoryKey = "scope:input.status"
	firstUntil := time.Now().Add(30 * time.Millisecond)
	extendedUntil := firstUntil.Add(100 * time.Millisecond)
	svc.storeOpenAIResponsesRejectedFieldUntil(memoryKey, firstUntil)
	time.Sleep(10 * time.Millisecond)
	svc.storeOpenAIResponsesRejectedFieldUntil(memoryKey, extendedUntil)

	time.Sleep(time.Until(firstUntil) + 20*time.Millisecond)
	rawUntil, exists := svc.openaiResponsesRejectedFieldUntil.Load(memoryKey)
	require.True(t, exists)
	require.Equal(t, extendedUntil, rawUntil)

	require.Eventually(t, func() bool {
		_, valueExists := svc.openaiResponsesRejectedFieldUntil.Load(memoryKey)
		_, workerExists := svc.openaiResponsesRejectedFieldCleanupScheduled.Load(memoryKey)
		return !valueExists && !workerExists
	}, time.Second, 5*time.Millisecond)
}

func newRejectedFieldTestResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
