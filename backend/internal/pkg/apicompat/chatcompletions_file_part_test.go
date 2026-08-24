package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatCompletionsFilePartBecomesResponsesInputFile(t *testing.T) {
	content, err := json.Marshal([]ChatContentPart{{Type: "file", File: &ChatFile{Filename: "report.pdf", FileID: "file_123"}}})
	require.NoError(t, err)
	request, err := ChatCompletionsToResponses(&ChatCompletionsRequest{Model: "gpt-5.5", Messages: []ChatMessage{{Role: "user", Content: content}}})
	require.NoError(t, err)
	require.Contains(t, string(request.Input), `"type":"input_file"`)
	require.Contains(t, string(request.Input), `"file_id":"file_123"`)
}

func TestChatFunctionCallOmitsEmptyStreamedName(t *testing.T) {
	encoded, err := json.Marshal(ChatFunctionCall{Arguments: `{"city":"Shanghai"}`})
	require.NoError(t, err)
	require.NotContains(t, string(encoded), `"name"`)
}

func TestValidateToolCallArgumentsRejectsTruncatedJSON(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("gpt-5.5")
	index := 0
	chunk := ChatCompletionsChunk{Choices: []ChatChunkChoice{{Delta: ChatDelta{ToolCalls: []ChatToolCall{{Index: &index, ID: "call_1", Function: ChatFunctionCall{Name: "weather", Arguments: `{"city":`}}}}}}}
	ChatCompletionsChunkToResponsesEvents(&chunk, state)
	require.Error(t, state.ValidateToolCallArguments())
}
