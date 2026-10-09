package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestV0215ChatRolesAndLegacyFunctionFIFO(t *testing.T) {
	req := &ChatCompletionsRequest{Model: "gpt-5", ToolChoice: json.RawMessage(`{"type":"function","function":{"name":" search "}}`), Messages: []ChatMessage{
		{Role: "developer", Content: json.RawMessage(`"policy"`)},
		{Role: "assistant", ToolCalls: []ChatToolCall{{ID: "call_legacy_1", Function: ChatFunctionCall{Name: "modern", Arguments: "{}"}}}},
		{Role: "assistant", FunctionCall: &ChatFunctionCall{Name: "search", Arguments: "{}"}},
		{Role: "assistant", FunctionCall: &ChatFunctionCall{Name: "search", Arguments: `{"q":2}`}},
		{Role: "function", Name: "search", Content: json.RawMessage(`"one"`)},
		{Role: "function", Name: "search", Content: json.RawMessage(`"two"`)},
	}}
	out, err := ChatCompletionsToResponses(req)
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"function","name":"search"}`, string(out.ToolChoice))
	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(out.Input, &items))
	require.Equal(t, "developer", items[0].Role)
	require.Equal(t, "call_legacy_1", items[1].CallID)
	require.Equal(t, "call_legacy_2", items[2].CallID)
	require.Equal(t, "call_legacy_3", items[3].CallID)
	require.Equal(t, items[2].CallID, items[4].CallID)
	require.Equal(t, items[3].CallID, items[5].CallID)
}

func TestV0215RefusalKeepsUsageAndFinalizesOnce(t *testing.T) {
	response := &ResponsesResponse{Status: "completed", Output: []ResponsesOutput{{Type: "message", Content: []ResponsesContentPart{{Type: "refusal", Refusal: "denied"}}}}, Usage: &ResponsesUsage{InputTokens: 10, OutputTokens: 2}}
	converted := ResponsesToChatCompletions(response, "gpt-5")
	require.Equal(t, "denied", converted.Choices[0].Message.Refusal)
	require.Equal(t, "content_filter", converted.Choices[0].FinishReason)
	require.Equal(t, 10, converted.Usage.PromptTokens)
	state := NewResponsesEventToChatState()
	state.IncludeUsage = true
	event := &ResponsesStreamEvent{Type: "response.completed", Response: response}
	chunks := ResponsesEventToChatChunks(event, state)
	require.Len(t, chunks, 3)
	require.Equal(t, "denied", *chunks[0].Choices[0].Delta.Refusal)
	require.Equal(t, "content_filter", *chunks[1].Choices[0].FinishReason)
	require.Equal(t, 10, chunks[2].Usage.PromptTokens)
	require.Empty(t, ResponsesEventToChatChunks(event, state))
	require.Empty(t, FinalizeResponsesChatStream(state))
}

func TestV0215ThinkingHasRequiredEmptySignature(t *testing.T) {
	raw, err := json.Marshal(AnthropicContentBlock{Type: "thinking"})
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"thinking","thinking":"","signature":""}`, string(raw))
	raw, err = json.Marshal(AnthropicContentBlock{Type: "text", Text: "hello"})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "signature")
}
