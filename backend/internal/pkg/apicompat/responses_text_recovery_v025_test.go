package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesRecoveryWithoutCreatedStartsMessage(t *testing.T) {
	item := ResponsesOutput{Type: "message", Status: "completed", Content: []ResponsesContentPart{{Type: "output_text", Text: "hello"}}}
	for _, first := range []ResponsesStreamEvent{
		{Type: "response.output_text.delta", Delta: "hello"},
		{Type: "response.output_text.done", Text: "hello"},
		{Type: "response.output_item.done", Item: &item},
		{Type: "response.completed", Response: &ResponsesResponse{ID: "resp_terminal", Model: "upstream-model", Status: "completed", Output: []ResponsesOutput{item}}},
	} {
		t.Run(first.Type, func(t *testing.T) {
			state := NewResponsesEventToAnthropicState()
			state.Model = "requested-model"
			for _, ignored := range []ResponsesStreamEvent{{Type: "response.in_progress"}, {Type: "response.output_text.delta"}, {Type: "unknown"}} {
				require.Empty(t, ResponsesEventToAnthropicEvents(&ignored, state))
				require.False(t, state.MessageStartSent)
			}
			events := ResponsesEventToAnthropicEvents(&first, state)
			require.NotEmpty(t, events)
			require.Equal(t, "message_start", events[0].Type)
			require.NotEmpty(t, events[0].Message.ID)
			require.Equal(t, "requested-model", events[0].Message.Model)
			id := state.ResponseID
			if first.Response != nil {
				require.Equal(t, first.Response.ID, id)
			}
			require.Empty(t, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.created", Response: &ResponsesResponse{ID: "late-id", Model: "late-model"}}, state))
			require.Equal(t, id, state.ResponseID)
			events = append(events, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.completed", Response: &ResponsesResponse{Status: "completed", Output: []ResponsesOutput{item}, Usage: &ResponsesUsage{InputTokens: 5, OutputTokens: 2}}}, state)...)
			starts, stops, text := 0, 0, ""
			for _, event := range events {
				switch event.Type {
				case "message_start":
					starts++
				case "message_stop":
					stops++
				}
				if event.Delta != nil && event.Delta.Type == "text_delta" {
					text += event.Delta.Text
				}
			}
			require.Equal(t, 1, starts)
			require.Equal(t, 1, stops)
			require.Equal(t, "hello", text)
			require.Equal(t, "message_stop", events[len(events)-1].Type)
			require.Empty(t, FinalizeResponsesAnthropicStream(state))
		})
	}
}

func TestResponsesTerminalTextRecoveryPerPart(t *testing.T) {
	for _, tc := range []struct{ name, delta, final, event, want string }{
		{"partial", "hello", "hello world", "response.completed", "hello world"},
		{"complete", "hello world", "hello world", "response.completed", "hello world"},
		{"mismatch", "hello", "other", "response.completed", "hello"},
		{"failure", "hello", "hello diagnostics", "response.failed", "hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := NewResponsesEventToAnthropicState()
			events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.output_text.delta", Delta: tc.delta}, state)
			events = append(events, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: tc.event, Response: &ResponsesResponse{Output: []ResponsesOutput{{Type: "message", Content: []ResponsesContentPart{{Type: "output_text", Text: tc.final}}}}}}, state)...)
			text := ""
			stops := 0
			for _, e := range events {
				if e.Delta != nil && e.Delta.Type == "text_delta" {
					text += e.Delta.Text
				}
				if e.Type == "message_stop" {
					stops++
				}
			}
			require.Equal(t, tc.want, text)
			require.Equal(t, 1, stops)
			require.Empty(t, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: tc.event}, state))
			require.Empty(t, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.output_text.delta", Delta: "late text"}, state))
		})
	}
	state := NewResponsesEventToAnthropicState()
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.output_text.delta", Delta: "one"}, state)
	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.completed", Response: &ResponsesResponse{Output: []ResponsesOutput{{Type: "message", Content: []ResponsesContentPart{{Type: "output_text", Text: "one!"}, {Type: "output_text", Text: "two"}}}}}}, state)
	text := ""
	for _, e := range events {
		if e.Delta != nil && e.Delta.Type == "text_delta" {
			text += e.Delta.Text
		}
	}
	require.Equal(t, "!two", text)
}

func TestResponsesItemDoneTextIsNotDuplicatedByCompletion(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	item := ResponsesOutput{Type: "message", Status: "completed", Content: []ResponsesContentPart{{Type: "output_text", Text: "hello"}}}
	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.output_item.done", Item: &item}, state)
	events = append(events, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.completed", Response: &ResponsesResponse{Output: []ResponsesOutput{item}}}, state)...)
	text := ""
	for _, event := range events {
		if event.Delta != nil && event.Delta.Type == "text_delta" {
			text += event.Delta.Text
		}
	}
	require.Equal(t, "hello", text)
}
