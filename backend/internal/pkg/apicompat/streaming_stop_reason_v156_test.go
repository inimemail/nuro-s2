//go:build unit

package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnthropicStreamingMaxTokensFinalizeMapsToIncompleteWithoutMessageStop(t *testing.T) {
	state := NewAnthropicEventToResponsesState()

	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{
		Type:    "message_start",
		Message: &AnthropicResponse{ID: "msg_test", Model: "claude-opus-4-6", Role: "assistant"},
	}, state)
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{
		Type:  "message_delta",
		Delta: &AnthropicDelta{StopReason: "max_tokens"},
		Usage: &AnthropicUsage{OutputTokens: 4096},
	}, state)

	events := FinalizeAnthropicResponsesStream(state)
	require.Len(t, events, 1)
	require.Equal(t, "response.incomplete", events[0].Type)
	require.NotNil(t, events[0].Response)
	require.Equal(t, "incomplete", events[0].Response.Status)
	require.NotNil(t, events[0].Response.IncompleteDetails)
	require.Equal(t, "max_output_tokens", events[0].Response.IncompleteDetails.Reason)
	require.Empty(t, FinalizeAnthropicResponsesStream(state))
}
