//go:build unit

package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesReadToolWaitsForCompleteSanitizedJSON(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true
	state.ContentBlockOpen = true
	state.CurrentBlockType = "tool_use"
	state.CurrentToolName = "Read"
	state.OutputIndexToBlockIdx = map[int]int{0: 0}

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.function_call_arguments.delta",
		OutputIndex: 0,
		Delta:       `{"file_path":"/tmp/te`,
	}, state)
	require.Empty(t, events)
	require.False(t, state.CurrentToolHadDelta)

	events = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.function_call_arguments.delta",
		OutputIndex: 0,
		Delta:       `st.go","pages":""}`,
	}, state)
	require.Len(t, events, 1)
	require.Equal(t, "content_block_delta", events[0].Type)
	require.Equal(t, "input_json_delta", events[0].Delta.Type)
	require.JSONEq(t, `{"file_path":"/tmp/test.go"}`, events[0].Delta.PartialJSON)

	events = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.function_call_arguments.done",
		OutputIndex: 0,
		Arguments:   `{"file_path":"/tmp/test.go","pages":""}`,
	}, state)
	require.Len(t, events, 1)
	require.Equal(t, "content_block_stop", events[0].Type)
	require.Empty(t, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.function_call_arguments.done",
		OutputIndex: 0,
	}, state))
}
