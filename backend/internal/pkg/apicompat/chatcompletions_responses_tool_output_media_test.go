package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func convertToolOutputMediaForTest(t *testing.T, input string) []ChatMessage {
	t.Helper()
	messages, err := responsesInputToChatMessages("", json.RawMessage(input))
	require.NoError(t, err)
	assertChatInvariants(t, messages)
	return messages
}

func toolOutputContentForTest(t *testing.T, message ChatMessage) string {
	t.Helper()
	var content string
	require.Equal(t, "tool", message.Role)
	require.NoError(t, json.Unmarshal(message.Content, &content))
	return content
}

func toolOutputPartsForTest(t *testing.T, message ChatMessage) []ChatContentPart {
	t.Helper()
	var parts []ChatContentPart
	require.NoError(t, json.Unmarshal(message.Content, &parts))
	return parts
}

func TestResponsesToolOutputMediaParallelBatchUsesCallOrder(t *testing.T) {
	messages := convertToolOutputMediaForTest(t, `[
		{"type":"function_call","call_id":"call_A","name":"view_image","arguments":"{}"},
		{"type":"function_call","call_id":"call_B","name":"view_image","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_B","output":[{"type":"input_image","image_url":{"url":"https://example.com/b.png"}}]},
		{"type":"function_call_output","call_id":"call_A","output":[{"type":"input_image","image_url":{"url":"https://example.com/a.png"}}]}
	]`)

	require.Len(t, messages, 4)
	require.Equal(t, []string{"assistant", "tool", "tool", "user"}, chatMessageRoles(messages))
	require.Equal(t, "call_A", messages[1].ToolCallID)
	require.Equal(t, "call_B", messages[2].ToolCallID)
	parts := toolOutputPartsForTest(t, messages[3])
	require.Len(t, parts, 4)
	require.Equal(t, "[Tool output media for call call_A]", parts[0].Text)
	require.Equal(t, "https://example.com/a.png", parts[1].ImageURL.URL)
	require.Equal(t, "[Tool output media for call call_B]", parts[2].Text)
	require.Equal(t, "https://example.com/b.png", parts[3].ImageURL.URL)
}

func TestResponsesToolOutputMediaDuplicateCallIDIsLastWins(t *testing.T) {
	messages := convertToolOutputMediaForTest(t, `[
		{"type":"function_call","call_id":"call_image","name":"view_image","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_image","output":[{"type":"input_image","image_url":"https://example.com/first.png"}]},
		{"type":"function_call_output","call_id":"call_image","output":[{"type":"input_image","image_url":"https://example.com/last.png"}]}
	]`)

	require.Len(t, messages, 3)
	require.NotContains(t, string(messages[1].Content), "first.png")
	require.NotContains(t, string(messages[2].Content), "first.png")
	require.Contains(t, string(messages[2].Content), "last.png")

	messages = convertToolOutputMediaForTest(t, `[
		{"type":"function_call","call_id":"call_image","name":"view_image","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_image","output":[{"type":"input_image","image_url":"data:image/png;base64,AQID"}]},
		{"type":"function_call_output","call_id":"call_image","output":"latest text"}
	]`)
	require.Len(t, messages, 2)
	require.Equal(t, "latest text", toolOutputContentForTest(t, messages[1]))
}

func TestResponsesToolOutputMediaPreservesMediaFreeOutput(t *testing.T) {
	input := `[
		{"type":"function_call","call_id":"call_text","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_text","output":{"type":"result","score":0.9,"large":9007199254740993}}
	]`
	messages := convertToolOutputMediaForTest(t, input)
	require.Len(t, messages, 2)
	require.JSONEq(t, `{"type":"result","score":0.9,"large":9007199254740993}`, toolOutputContentForTest(t, messages[1]))

	input = `[
		{"type":"function_call","call_id":"call_text","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_text","output":"prefix data:image/png;base64,AQID suffix"}
	]`
	messages = convertToolOutputMediaForTest(t, input)
	require.Len(t, messages, 2)
	require.Equal(t, "prefix data:image/png;base64,AQID suffix", toolOutputContentForTest(t, messages[1]))
}

func TestResponsesToolOutputMediaNeverLeaksIntoToolContent(t *testing.T) {
	messages := convertToolOutputMediaForTest(t, `[
		{"type":"function_call","call_id":"call_image","name":"view_image","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_image","output":{"status":"ok","content":[{"type":"input_image","image_url":"data:image/png;base64,AQID"}],"large":9007199254740993}}
	]`)

	require.Len(t, messages, 3)
	toolContent := toolOutputContentForTest(t, messages[1])
	require.NotContains(t, toolContent, "data:image/")
	require.Contains(t, toolContent, "9007199254740993")
	require.Contains(t, string(messages[2].Content), "data:image/png;base64,AQID")
}
