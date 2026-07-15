package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesToChatCompletionsContentFilter(t *testing.T) {
	resp := &ResponsesResponse{
		ID:                "resp_cf",
		Status:            "incomplete",
		IncompleteDetails: &ResponsesIncompleteDetails{Reason: "content_filter"},
		Output: []ResponsesOutput{{
			Type:    "message",
			Content: []ResponsesContentPart{{Type: "output_text", Text: "partial"}},
		}},
		Usage: &ResponsesUsage{InputTokens: 10, OutputTokens: 5},
	}

	chat := ResponsesToChatCompletions(resp, "gpt-5.5")
	require.Len(t, chat.Choices, 1)
	require.Equal(t, "content_filter", chat.Choices[0].FinishReason)
}

func TestResponsesToChatCompletionsStreamingContentFilter(t *testing.T) {
	state := NewResponsesEventToChatState()
	state.ID = "resp_cf"
	state.Model = "gpt-5.5"
	state.SentRole = true

	chunks := ResponsesEventToChatChunks(&ResponsesStreamEvent{
		Type: "response.completed",
		Response: &ResponsesResponse{
			ID:                "resp_cf",
			Status:            "incomplete",
			IncompleteDetails: &ResponsesIncompleteDetails{Reason: "content_filter"},
		},
	}, state)

	found := false
	for _, chunk := range chunks {
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && *choice.FinishReason == "content_filter" {
				found = true
			}
		}
	}
	require.True(t, found)
}
