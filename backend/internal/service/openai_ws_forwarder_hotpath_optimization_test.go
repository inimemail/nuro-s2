package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestParseOpenAIWSEventEnvelope(t *testing.T) {
	eventType, responseID, response := parseOpenAIWSEventEnvelope([]byte(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.1"}}`))
	require.Equal(t, "response.completed", eventType)
	require.Equal(t, "resp_1", responseID)
	require.True(t, response.Exists())
	require.Equal(t, `{"id":"resp_1","model":"gpt-5.1"}`, response.Raw)

	eventType, responseID, response = parseOpenAIWSEventEnvelope([]byte(`{"type":"response.delta","id":"evt_1"}`))
	require.Equal(t, "response.delta", eventType)
	require.Equal(t, "evt_1", responseID)
	require.False(t, response.Exists())
}

func TestParseOpenAIWSResponseUsageFromCompletedEvent(t *testing.T) {
	usage := &OpenAIUsage{}
	parseOpenAIWSResponseUsageFromCompletedEvent(
		[]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":3}}}}`),
		usage,
	)
	require.Equal(t, 11, usage.InputTokens)
	require.Equal(t, 7, usage.OutputTokens)
	require.Equal(t, 3, usage.CacheReadInputTokens)

	parseOpenAIWSResponseUsageFromCompletedEvent(
		[]byte(`{"type":"response.completed","response":{"usage":{"prompt_tokens":19,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4}}}}`),
		usage,
	)
	require.Equal(t, 19, usage.InputTokens)
	require.Equal(t, 5, usage.OutputTokens)
	require.Equal(t, 4, usage.CacheReadInputTokens)
}

func TestOpenAIWSEventShouldParseUsageTerminalEvents(t *testing.T) {
	t.Parallel()

	for _, eventType := range []string{
		"response.completed",
		"response.done",
		"response.failed",
		"response.incomplete",
		"response.cancelled",
		"response.canceled",
	} {
		require.True(t, openAIWSEventShouldParseUsage(eventType), eventType)
		require.True(t, openAIWSEventShouldParseUsage("  "+eventType+"  "), eventType)
	}
	require.False(t, openAIWSEventShouldParseUsage("response.output_text.delta"))
	require.False(t, openAIWSEventShouldParseUsage(""))
}

func TestOpenAIWSErrorEventHelpers_ConsistentWithWrapper(t *testing.T) {
	message := []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"invalid_request","message":"invalid input"}}`)
	codeRaw, errTypeRaw, errMsgRaw := parseOpenAIWSErrorEventFields(message)

	wrappedReason, wrappedRecoverable := classifyOpenAIWSErrorEvent(message)
	rawReason, rawRecoverable := classifyOpenAIWSErrorEventFromRaw(codeRaw, errTypeRaw, errMsgRaw)
	require.Equal(t, wrappedReason, rawReason)
	require.Equal(t, wrappedRecoverable, rawRecoverable)

	wrappedStatus := openAIWSErrorHTTPStatus(message)
	rawStatus := openAIWSErrorHTTPStatusFromRaw(codeRaw, errTypeRaw)
	require.Equal(t, wrappedStatus, rawStatus)
	require.Equal(t, http.StatusBadRequest, rawStatus)

	wrappedCode, wrappedType, wrappedMsg := summarizeOpenAIWSErrorEventFields(message)
	rawCode, rawType, rawMsg := summarizeOpenAIWSErrorEventFieldsFromRaw(codeRaw, errTypeRaw, errMsgRaw)
	require.Equal(t, wrappedCode, rawCode)
	require.Equal(t, wrappedType, rawType)
	require.Equal(t, wrappedMsg, rawMsg)
}

func TestSanitizeOpenAIWSErrorEventForClient(t *testing.T) {
	t.Parallel()

	t.Run("error event removes endpoint and unknown diagnostics", func(t *testing.T) {
		payload := []byte(`{"type":"error","error":{"type":"server_error","message":"<!DOCTYPE html><title>xiaobaishu.org | 502</title>","upstream_host":"xiaobaishu.org"},"debug_url":"https://www.cloudflare.com/5xx"}`)
		sanitized := sanitizeOpenAIWSErrorEventForClient(payload, "error", false)
		require.JSONEq(t, `{"type":"error","error":{"type":"server_error","message":"Upstream request failed"}}`, string(sanitized))
		require.NotContains(t, string(sanitized), "xiaobaishu.org")
		require.NotContains(t, string(sanitized), "cloudflare.com")
		require.NotContains(t, string(sanitized), "DOCTYPE")
	})

	t.Run("response failed preserves safe cyber policy fields", func(t *testing.T) {
		payload := []byte(`{"type":"response.failed","sequence_number":9,"response":{"id":"resp_cyber","object":"response","status":"failed","model":"gpt-5.1","error":{"type":"safety_error","code":"cyber_policy","message":"This request has been flagged for potentially high-risk cyber activity.","upstream_host":"private.example"},"usage":{"input_tokens":10},"output":[{"private_url":"https://private.example/result"}]}}`)
		sanitized := sanitizeOpenAIWSErrorEventForClient(payload, "response.failed", false)
		require.Equal(t, "resp_cyber", gjson.GetBytes(sanitized, "response.id").String())
		require.Equal(t, "failed", gjson.GetBytes(sanitized, "response.status").String())
		require.Equal(t, "safety_error", gjson.GetBytes(sanitized, "response.error.type").String())
		require.Equal(t, "cyber_policy", gjson.GetBytes(sanitized, "response.error.code").String())
		require.Contains(t, gjson.GetBytes(sanitized, "response.error.message").String(), "high-risk cyber activity")
		require.False(t, gjson.GetBytes(sanitized, "response.usage").Exists())
		require.False(t, gjson.GetBytes(sanitized, "response.output").Exists())
		require.NotContains(t, string(sanitized), "private.example")
	})

	t.Run("success event stays byte for byte unchanged", func(t *testing.T) {
		payload := []byte(`{"type":"response.completed","response":{"id":"resp_ok","output":[{"type":"message","content":[]}]}}`)
		require.Equal(t, payload, sanitizeOpenAIWSErrorEventForClient(payload, "response.completed", true))
	})
}

func TestNormalizeOpenAIWSUpstreamEventForSafety(t *testing.T) {
	t.Run("normal completed remains completed", func(t *testing.T) {
		payload := []byte(`{"type":"response.completed","response":{"id":"resp_ok","status":"completed","output":[]}}`)
		normalized, eventType, unsafe := normalizeOpenAIWSUpstreamEventForSafety(payload, "response.completed")
		require.False(t, unsafe)
		require.Equal(t, "response.completed", eventType)
		require.Equal(t, payload, normalized)
	})

	t.Run("diagnostic completed becomes safe failure", func(t *testing.T) {
		payload := []byte(`{"type":"response.completed","response":{"id":"resp_bad","status":"completed","provider":"private-provider.example"}}`)
		normalized, eventType, unsafe := normalizeOpenAIWSUpstreamEventForSafety(payload, "response.completed")
		require.True(t, unsafe)
		require.Equal(t, "response.failed", eventType)
		require.Contains(t, string(normalized), safeUpstreamErrorMessage)
		require.NotContains(t, string(normalized), "private-provider")
	})

	t.Run("invalid frame becomes safe failure", func(t *testing.T) {
		normalized, eventType, unsafe := normalizeOpenAIWSUpstreamEventForSafety([]byte(`<!DOCTYPE html><title>private.example</title>`), "")
		require.True(t, unsafe)
		require.Equal(t, "response.failed", eventType)
		require.Contains(t, string(normalized), safeUpstreamErrorMessage)
		require.NotContains(t, string(normalized), "private.example")
	})

	require.True(t, isOpenAIWSSuccessTerminalEvent("response.completed"))
	require.True(t, isOpenAIWSSuccessTerminalEvent("response.done"))
	require.False(t, isOpenAIWSSuccessTerminalEvent("response.failed"))
	require.False(t, isOpenAIWSSuccessTerminalEvent("response.incomplete"))
	require.False(t, isOpenAIWSSuccessTerminalEvent("response.cancelled"))
}

func TestBuildOpenAIWSHTTPBridgeErrorEventDoesNotExposeUpstreamIdentity(t *testing.T) {
	payload := buildOpenAIWSHTTPBridgeErrorEvent(
		http.StatusBadGateway,
		"xAI proxy https://xiaobaishu.org failed via cloudflare",
	)

	require.Contains(t, string(payload), safeUpstreamErrorMessage)
	require.NotContains(t, string(payload), "xAI")
	require.NotContains(t, string(payload), "xiaobaishu.org")
	require.NotContains(t, string(payload), "cloudflare")
}

func TestOpenAIWSMessageLikelyContainsToolCalls(t *testing.T) {
	require.False(t, openAIWSMessageLikelyContainsToolCalls([]byte(`{"type":"response.output_text.delta","delta":"hello"}`)))
	require.True(t, openAIWSMessageLikelyContainsToolCalls([]byte(`{"type":"response.output_item.added","item":{"tool_calls":[{"id":"tc1"}]}}`)))
	require.True(t, openAIWSMessageLikelyContainsToolCalls([]byte(`{"type":"response.output_item.added","item":{"type":"function_call"}}`)))
}

func TestReplaceOpenAIWSMessageModel_OptimizedStillCorrect(t *testing.T) {
	noModel := []byte(`{"type":"response.output_text.delta","delta":"hello"}`)
	require.Equal(t, string(noModel), string(replaceOpenAIWSMessageModel(noModel, "gpt-5.1", "custom-model")))

	rootOnly := []byte(`{"type":"response.created","model":"gpt-5.1"}`)
	require.Equal(t, `{"type":"response.created","model":"custom-model"}`, string(replaceOpenAIWSMessageModel(rootOnly, "gpt-5.1", "custom-model")))

	responseOnly := []byte(`{"type":"response.completed","response":{"model":"gpt-5.1"}}`)
	require.Equal(t, `{"type":"response.completed","response":{"model":"custom-model"}}`, string(replaceOpenAIWSMessageModel(responseOnly, "gpt-5.1", "custom-model")))

	both := []byte(`{"model":"gpt-5.1","response":{"model":"gpt-5.1"}}`)
	require.Equal(t, `{"model":"custom-model","response":{"model":"custom-model"}}`, string(replaceOpenAIWSMessageModel(both, "gpt-5.1", "custom-model")))
}
