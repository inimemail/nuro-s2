package apicompat

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestV028GPT6SamplingAndIndependentReasoning(t *testing.T) {
	temp := 0.7
	for _, model := range []string{"gpt-6-sol", "openai/gpt-6-luna-high"} {
		for _, effort := range []string{"none", "max", "high"} {
			out, err := ChatCompletionsToResponses(&ChatCompletionsRequest{Model: model, Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}}, ReasoningEffort: effort, Temperature: &temp})
			require.NoError(t, err)
			require.Equal(t, effort, out.Reasoning.Effort)
			require.Equal(t, effort == "none", out.Temperature != nil)
		}
	}
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"gpt-6-sol","reasoning":{"mode":"pro","effort":"none"}}`), &req))
	require.Equal(t, "pro", req.Reasoning.Mode)
	require.Equal(t, "none", req.Reasoning.Effort)
}

func TestV028OpusAdaptiveAndSignedRoundTrip(t *testing.T) {
	temp := 0.5
	for _, block := range []AnthropicContentBlock{{Type: "thinking", Thinking: "", Signature: "provider-signature"}, {Type: "thinking", Thinking: "analysis", Signature: "provider-signature"}, {Type: "redacted_thinking", Data: "opaque-provider-data"}} {
		resp := AnthropicToResponsesResponse(&AnthropicResponse{Model: "claude-opus-5-5", Content: []AnthropicContentBlock{block, {Type: "text", Text: "answer"}}}, "account-A-private-key")
		require.Len(t, resp.Output, 2)
		input, err := json.Marshal(resp.Output)
		require.NoError(t, err)
		req := &ResponsesRequest{Model: "claude-opus-5-5", Input: input, Temperature: &temp, ThinkingScope: "account-A-private-key", Reasoning: &ResponsesReasoning{Effort: "xhigh"}}
		out, err := ResponsesToAnthropicRequest(req)
		require.NoError(t, err)
		require.Equal(t, "adaptive", out.Thinking.Type)
		require.Equal(t, "xhigh", out.OutputConfig.Effort)
		require.Nil(t, out.Temperature)
		var blocks []AnthropicContentBlock
		require.NoError(t, json.Unmarshal(out.Messages[0].Content, &blocks))
		require.Equal(t, block, blocks[0])
		req.ThinkingScope = "account-B-private-key"
		_, err = ResponsesToAnthropicRequest(req)
		require.Error(t, err)
		item := resp.Output[0]
		_, err = decodeAnthropicThinking(item.EncryptedContent, "account-A-private-key", "different-turn-item")
		require.Error(t, err)
		_, err = decodeAnthropicThinking(item.EncryptedContent+"junk", "account-A-private-key", item.ID)
		require.Error(t, err)
	}
	for _, effort := range []string{"none", "minimal", "invalid"} {
		_, err := ResponsesToAnthropicRequest(&ResponsesRequest{Model: "claude-opus-5-5", Input: json.RawMessage(`"hi"`), Reasoning: &ResponsesReasoning{Effort: effort}})
		require.Error(t, err)
	}
	_, err := ResponsesToAnthropicRequest(&ResponsesRequest{Model: "claude-opus-5-5", Input: json.RawMessage(`"hi"`), ToolChoice: json.RawMessage(`"required"`)})
	require.Error(t, err)
	_, err = decodeAnthropicThinking(anthropicThinkingEnvelopePrefix+strings.Repeat("a", 3<<20), "", "")
	require.Error(t, err)
}

func TestV028OpusStreamingSignatureIsNotVisibleText(t *testing.T) {
	state := NewAnthropicEventToResponsesState()
	state.ThinkingScope = "account-scope"
	state.PreserveThinkingSignatures = true
	for _, event := range []*AnthropicStreamEvent{
		{Type: "message_start", Message: &AnthropicResponse{Model: "claude-opus-5-5", ID: "msg_1"}},
		{Type: "content_block_start", ContentBlock: &AnthropicContentBlock{Type: "thinking"}},
		{Type: "content_block_delta", Delta: &AnthropicDelta{Type: "thinking_delta", Thinking: "plan"}},
		{Type: "content_block_delta", Delta: &AnthropicDelta{Type: "signature_delta", Signature: "sig-"}},
		{Type: "content_block_delta", Delta: &AnthropicDelta{Type: "signature_delta", Signature: "part2"}},
		{Type: "content_block_stop"},
	} {
		AnthropicEventToResponsesEvents(event, state)
	}
	require.Len(t, state.Outputs, 1)
	item := state.Outputs[0]
	require.Equal(t, "plan", item.Summary[0].Text)
	block, err := decodeAnthropicThinking(item.EncryptedContent, "account-scope", item.ID)
	require.NoError(t, err)
	require.Equal(t, "sig-part2", block.Signature)
	require.Equal(t, "plan", block.Thinking)
}

func TestV028OversizeThinkingNeverReplaysTruncatedSignature(t *testing.T) {
	state := NewAnthropicEventToResponsesState()
	state.ThinkingScope = "account-scope"
	state.PreserveThinkingSignatures = true
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "content_block_start", ContentBlock: &AnthropicContentBlock{Type: "thinking"}}, state)
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "content_block_delta", Delta: &AnthropicDelta{Type: "signature_delta", Signature: "prefix"}}, state)
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "content_block_delta", Delta: &AnthropicDelta{Type: "signature_delta", Signature: strings.Repeat("x", maxAnthropicThinkingEnvelope)}}, state)
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "content_block_stop"}, state)
	require.Len(t, state.Outputs, 1)
	require.Empty(t, state.Outputs[0].EncryptedContent)
}

func TestV028ThinkingRequiresAccountScope(t *testing.T) {
	block := AnthropicContentBlock{Type: "thinking", Thinking: "plan", Signature: "provider-signature"}
	require.Empty(t, encodeAnthropicThinking(block, "", "item"))
	envelope := encodeAnthropicThinking(block, "account-A", "item")
	require.NotEmpty(t, envelope)
	_, err := decodeAnthropicThinking(envelope, "", "item")
	require.Error(t, err)
	response := AnthropicToResponsesResponse(&AnthropicResponse{Model: "claude-opus-5-5", Content: []AnthropicContentBlock{block}})
	require.Empty(t, response.Output[0].EncryptedContent)
	require.Equal(t, "plan", response.Output[0].Summary[0].Text)
}
