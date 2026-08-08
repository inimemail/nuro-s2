package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesToAnthropicRequestDropsInvalidBlocksAndUnmatchedTools(t *testing.T) {
	req := &ResponsesRequest{
		Model: "claude-sonnet-4-5",
		Input: json.RawMessage(`[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"private"}]},
			{"role":"assistant","content":[{"type":"output_text","text":""},{"type":"mystery","text":"bad"},{"type":"output_text","text":"ok"}]},
			{"type":"function_call","call_id":"orphan","name":"never_used","arguments":"{}"},
			{"role":"user","content":[{"type":"input_text","text":"done"}]}
		]`),
	}
	got, err := ResponsesToAnthropicRequest(req)
	require.NoError(t, err)
	var decoded []AnthropicMessage
	require.NoError(t, json.Unmarshal(got.Messages[0].Content, &decoded))
	require.Len(t, got.Messages, 2)
	var assistantBlocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(got.Messages[0].Content, &assistantBlocks))
	require.Len(t, assistantBlocks, 1)
	require.Equal(t, "text", assistantBlocks[0].Type)
	require.Equal(t, "ok", assistantBlocks[0].Text)
}

func TestResponsesToAnthropicRequestKeepsSerializedContentValid(t *testing.T) {
	req := &ResponsesRequest{
		Model: "claude-sonnet-4-5",
		Input: json.RawMessage(`[
			{"role":"assistant","content":{"unexpected":true}},
			{"type":"function_call","call_id":"call_bad","name":"run","arguments":"not-json"},
			{"type":"function_call_output","call_id":"call_bad","output":"ok"}
		]`),
	}

	got, err := ResponsesToAnthropicRequest(req)
	require.NoError(t, err)
	require.Len(t, got.Messages, 2)

	var assistantBlocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(got.Messages[0].Content, &assistantBlocks))
	require.Len(t, assistantBlocks, 1)
	require.Equal(t, "tool_use", assistantBlocks[0].Type)
	require.Equal(t, `{}`, string(assistantBlocks[0].Input))

	var resultBlocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(got.Messages[1].Content, &resultBlocks))
	require.Len(t, resultBlocks, 1)
	require.Equal(t, "tool_result", resultBlocks[0].Type)
}

func TestResponsesToAnthropicRequestDropsMalformedMessageContent(t *testing.T) {
	req := &ResponsesRequest{
		Model: "claude-sonnet-4-5",
		Input: json.RawMessage(`[{"role":"user","content":{"not":"a content block"}}]`),
	}

	got, err := ResponsesToAnthropicRequest(req)
	require.Error(t, err)
	require.Nil(t, got)
}

func TestResponsesToAnthropicRequestRejectsOrphanToolOnlyInput(t *testing.T) {
	req := &ResponsesRequest{
		Model: "claude-sonnet-4-5",
		Input: json.RawMessage(`[{"type":"function_call","call_id":"orphan","name":"run","arguments":"{}"}]`),
	}

	got, err := ResponsesToAnthropicRequest(req)
	require.Error(t, err)
	require.Nil(t, got)
}
