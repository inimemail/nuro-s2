package service

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"strings"
	"testing"
	"time"
)

func TestV028SchemaNullRequiredBoundaries(t *testing.T) {
	body := []byte(`{"tools":[{"parameters":{"required":null,"properties":{"required":{"type":"string"},"nested":{"required":null,"properties":{"x":{"type":"number"}}}}}}],"input":[{"content":"required:null","required":null}]}`)
	out, changed, err := sanitizeOpenAIResponsesToolParameterTypes(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "[]", gjson.GetBytes(out, "tools.0.parameters.required").Raw)
	require.Equal(t, "string", gjson.GetBytes(out, "tools.0.parameters.properties.required.type").String())
	require.Equal(t, "[]", gjson.GetBytes(out, "tools.0.parameters.properties.nested.required").Raw)
	require.Equal(t, "null", gjson.GetBytes(out, "input.0.required").Raw)
	out2, changed, err := sanitizeOpenAIResponsesToolParameterTypes(out)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, out, out2)
}

func TestV028OversizedItemIDs(t *testing.T) {
	for _, typ := range []string{"message", "reasoning", "function_call"} {
		require.True(t, shouldStripOpenAIResponsesInputItemID(typ, strings.Repeat("a", 65)))
	}
	require.False(t, shouldStripOpenAIResponsesInputItemID("function_call_output", strings.Repeat("a", 65)))
	require.False(t, shouldStripOpenAIResponsesInputItemID("message", "msg"+strings.Repeat("a", 61)))
}

func TestV028ReminderAndTrailingSystemBoundaries(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"<system-reminder>blocked words</system-reminder>"},{"role":"system","content":"system"}]}`)
	require.Contains(t, extractContentModerationKeywordText(ContentModerationProtocolAnthropicMessages, body), "blocked words")
	require.Empty(t, ExtractContentModerationText(ContentModerationProtocolAnthropicMessages, body))
	body = []byte(`{"messages":[{"role":"user","content":"blocked words"},{"role":"assistant","content":"tool"},{"role":"system","content":"system"}]}`)
	require.Empty(t, extractContentModerationKeywordText(ContentModerationProtocolAnthropicMessages, body))
}

func TestV028VertexRetryInfo(t *testing.T) {
	for _, tc := range []struct {
		delay    string
		min, max int64
	}{{"1.5s", 1, 3}, {"36000s", 899, 901}} {
		before := time.Now().Unix()
		reset := ParseGeminiRateLimitResetTime([]byte(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"` + tc.delay + `"}]}}`))
		require.NotNil(t, reset)
		require.GreaterOrEqual(t, *reset-before, tc.min)
		require.LessOrEqual(t, *reset-before, tc.max)
	}
	require.Nil(t, ParseGeminiRateLimitResetTime([]byte(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"-3s"}]}}`)))
}

func TestV028CodexMetadataASCII(t *testing.T) {
	original := map[string]any{"name": "中文😀\u007f", "id": "stable"}
	raw, err := marshalCodexTurnMetadata(original)
	require.NoError(t, err)
	for _, b := range raw {
		require.Less(t, b, byte(127))
	}
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, original, decoded)
}

func TestV028Opus55TestPayload(t *testing.T) {
	payload, err := createTestPayload("claude-opus-5-5")
	require.NoError(t, err)
	require.NotContains(t, payload, "temperature")
	require.Equal(t, map[string]any{"type": "adaptive"}, payload["thinking"])
	old, err := createTestPayload("claude-sonnet-4-6")
	require.NoError(t, err)
	require.Equal(t, 1, old["temperature"])
}

func TestV028SSEStatusValidation(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{{`429`, 429}, {`"429"`, 429}, {`401`, 401}, {`429.5`, 502}, {`true`, 502}, {`"429suffix"`, 502}, {`200`, 502}} {
		require.Equal(t, tc.want, openAIStreamFailedEventSemanticStatus([]byte(`{"error":{"status":`+tc.value+`}}`), ""))
	}
}
