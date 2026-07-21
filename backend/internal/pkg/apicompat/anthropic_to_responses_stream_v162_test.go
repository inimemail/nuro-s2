package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnthropicResponsesStreamEmitsContentPartsAndTerminalOutput(t *testing.T) {
	state := NewAnthropicEventToResponsesState()
	events := make([]ResponsesStreamEvent, 0, 8)
	appendEvent := func(event AnthropicStreamEvent) {
		events = append(events, AnthropicEventToResponsesEvents(&event, state)...)
	}

	appendEvent(AnthropicStreamEvent{Type: "message_start", Message: &AnthropicResponse{
		ID: "msg_1", Type: "message", Role: "assistant", Model: "claude-sonnet-4-5",
	}})
	appendEvent(AnthropicStreamEvent{Type: "content_block_start", ContentBlock: &AnthropicContentBlock{Type: "text"}})
	appendEvent(AnthropicStreamEvent{Type: "content_block_delta", Delta: &AnthropicDelta{Type: "text_delta", Text: "hello"}})
	appendEvent(AnthropicStreamEvent{Type: "content_block_stop"})
	appendEvent(AnthropicStreamEvent{Type: "message_delta", Delta: &AnthropicDelta{StopReason: "end_turn"}})
	appendEvent(AnthropicStreamEvent{Type: "message_stop"})

	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	require.Contains(t, types, "response.content_part.added")
	require.Contains(t, types, "response.content_part.done")
	for _, event := range events {
		if event.Type == "response.output_text.done" {
			require.Equal(t, "hello", event.Text)
		}
	}

	terminal := events[len(events)-1]
	require.Equal(t, "response.completed", terminal.Type)
	require.NotNil(t, terminal.Response)
	require.Len(t, terminal.Response.Output, 1)
	require.Equal(t, "hello", terminal.Response.Output[0].Content[0].Text)
}

func TestAnthropicResponsesStreamDonePayloadsAndContentIndexes(t *testing.T) {
	state := NewAnthropicEventToResponsesState()
	events := make([]ResponsesStreamEvent, 0, 20)
	appendEvent := func(event AnthropicStreamEvent) {
		events = append(events, AnthropicEventToResponsesEvents(&event, state)...)
	}
	appendEvent(AnthropicStreamEvent{Type: "message_start", Message: &AnthropicResponse{ID: "msg_2", Model: "claude-sonnet-4-5"}})
	for _, text := range []string{"first", "second"} {
		appendEvent(AnthropicStreamEvent{Type: "content_block_start", ContentBlock: &AnthropicContentBlock{Type: "text"}})
		appendEvent(AnthropicStreamEvent{Type: "content_block_delta", Delta: &AnthropicDelta{Type: "text_delta", Text: text}})
		appendEvent(AnthropicStreamEvent{Type: "content_block_stop"})
	}
	appendEvent(AnthropicStreamEvent{Type: "content_block_start", ContentBlock: &AnthropicContentBlock{Type: "tool_use", ID: "tool_1", Name: "exec"}})
	appendEvent(AnthropicStreamEvent{Type: "content_block_delta", Delta: &AnthropicDelta{Type: "input_json_delta", PartialJSON: `{"cmd":"date"}`}})
	appendEvent(AnthropicStreamEvent{Type: "content_block_stop"})

	var textDone []ResponsesStreamEvent
	var argsDone *ResponsesStreamEvent
	for i := range events {
		event := events[i]
		switch event.Type {
		case "response.output_text.done":
			textDone = append(textDone, event)
		case "response.function_call_arguments.done":
			argsDone = &events[i]
		}
	}
	require.Len(t, textDone, 2)
	require.Equal(t, "first", textDone[0].Text)
	require.Equal(t, 0, textDone[0].ContentIndex)
	require.Equal(t, "second", textDone[1].Text)
	require.Equal(t, 1, textDone[1].ContentIndex)
	require.NotNil(t, argsDone)
	require.Equal(t, `{"cmd":"date"}`, argsDone.Arguments)
}

func TestAnthropicThinkingSignatureRoundTripToResponsesReasoning(t *testing.T) {
	req := &AnthropicRequest{
		Model: "grok-4.5",
		Messages: []AnthropicMessage{{
			Role:    "assistant",
			Content: mustJSONRaw([]AnthropicContentBlock{{Type: "thinking", Thinking: "plan", Signature: "xai-cipher"}, {Type: "text", Text: "done"}}),
		}},
	}
	converted, err := AnthropicToResponses(req)
	require.NoError(t, err)
	var input []ResponsesInputItem
	require.NoError(t, json.Unmarshal(converted.Input, &input))
	require.Equal(t, "reasoning", input[0].Type)
	require.Equal(t, "xai-cipher", input[0].EncryptedContent)

	response := &ResponsesResponse{Output: []ResponsesOutput{{Type: "reasoning", EncryptedContent: "xai-cipher"}}}
	back := ResponsesToAnthropic(response, "grok-4.5")
	require.Equal(t, "thinking", back.Content[0].Type)
	require.Equal(t, "xai-cipher", back.Content[0].Signature)
}

func TestResponsesStreamEmitsThinkingSignatureDeltaBeforeStop(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.created", Response: &ResponsesResponse{ID: "resp_1", Model: "grok-4.5"}}, state)
	events = append(events, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 0, Item: &ResponsesOutput{Type: "reasoning", EncryptedContent: "xai-cipher"}}, state)...)
	events = append(events, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 0, Item: &ResponsesOutput{Type: "reasoning", EncryptedContent: "xai-cipher"}}, state)...)

	sigAt, stopAt := -1, -1
	for i, event := range events {
		if event.Type == "content_block_delta" && event.Delta != nil && event.Delta.Type == "signature_delta" {
			sigAt = i
		}
		if event.Type == "content_block_stop" {
			stopAt = i
		}
	}
	require.GreaterOrEqual(t, sigAt, 0)
	require.Greater(t, stopAt, sigAt)
}

func mustJSONRaw(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
