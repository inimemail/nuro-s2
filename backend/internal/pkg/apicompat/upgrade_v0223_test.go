package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV0223AnthropicUsagePresenceAndStreamingTTL(t *testing.T) {
	for _, raw := range []string{`{}`, `{"cache_creation_input_tokens":null}`, `{"cache_creation_input_tokens":0}`, `{"cache_creation":{"ephemeral_1h_input_tokens":12}}`} {
		var usage AnthropicUsage
		require.NoError(t, json.Unmarshal([]byte(raw), &usage))
		out, err := json.Marshal(usage)
		require.NoError(t, err)
		require.Equal(t, gjson.Get(raw, "cache_creation_input_tokens").Type != gjson.Null && gjson.Get(raw, "cache_creation_input_tokens").Exists(), gjson.GetBytes(out, "cache_creation_input_tokens").Exists())
	}
	state := NewAnthropicEventToResponsesState()
	for _, raw := range []string{
		`{"type":"message_start","message":{"id":"m","usage":{"cache_creation":{"ephemeral_5m_input_tokens":7}}}}`,
		`{"type":"message_delta","usage":{"cache_creation":{"ephemeral_1h_input_tokens":13}}}`,
		`{"type":"message_delta","usage":{"cache_creation":{"ephemeral_5m_input_tokens":0}}}`,
	} {
		var event AnthropicStreamEvent
		require.NoError(t, json.Unmarshal([]byte(raw), &event))
		AnthropicEventToResponsesEvents(&event, state)
	}
	require.Equal(t, 13, state.CacheCreationInputTokens)
	events := AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "message_stop"}, state)
	require.Equal(t, 13, events[len(events)-1].Response.Usage.InputTokens)
	zero, err := json.Marshal(anthropicUsageFromResponsesUsage(nil))
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(zero, "cache_creation_input_tokens").Exists())
	require.True(t, gjson.GetBytes(zero, "cache_read_input_tokens").Exists())
}
func TestV0223ChatCacheWriteZeroAndNull(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{`{"cache_write_tokens":0,"cache_creation_tokens":10}`, 0},
		{`{"cache_write_tokens":null,"cache_creation_tokens":10}`, 10},
		{`{"cache_creation_tokens":10}`, 10},
	} {
		var details ChatTokenDetails
		require.NoError(t, json.Unmarshal([]byte(tc.raw), &details))
		usage := ChatUsageToResponsesUsage(&ChatUsage{PromptTokens: 20, PromptTokensDetails: &details})
		require.Equal(t, tc.want, usage.CacheCreationInputTokens)
	}
}

func TestV0223AnthropicTTLOverflowDoesNotBecomeZeroUsage(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	one := 1
	usage := AnthropicUsage{CacheCreation: &AnthropicCacheCreation{Ephemeral5mInputTokens: &maxInt, Ephemeral1hInputTokens: &one}}
	response := AnthropicToResponsesResponse(&AnthropicResponse{Usage: usage})
	require.Nil(t, response.Usage)
	state := NewAnthropicEventToResponsesState()
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "message_start", Message: &AnthropicResponse{Usage: usage}}, state)
	events := AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "message_stop"}, state)
	require.Nil(t, events[len(events)-1].Response.Usage)
}

func TestV0223ChatUsageOverflowDoesNotBecomeNegativeTotal(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	require.Nil(t, ChatUsageToResponsesUsage(&ChatUsage{PromptTokens: maxInt, CompletionTokens: 1}))
	valid := ChatUsageToResponsesUsage(&ChatUsage{PromptTokens: maxInt - 1, CompletionTokens: 1})
	require.Equal(t, maxInt, valid.TotalTokens)
}

func TestV0223TypedAnthropicUsageKeepsValidFieldsBesideInvalidCounts(t *testing.T) {
	for _, field := range []string{
		`"input_tokens":1.5`, `"cache_creation_input_tokens":-1`, `"cache_read_input_tokens":true`,
		`"prompt_tokens":9223372036854775808`, `"cached_tokens":[]`,
		`"prompt_tokens_details":{"cached_tokens":{}}`, `"prompt_cache_hit_tokens":"bad"`,
		`"prompt_cache_miss_tokens":1e999`, `"cache_creation":{"ephemeral_5m_input_tokens":false}`,
	} {
		t.Run(field, func(t *testing.T) {
			var event AnthropicStreamEvent
			require.NoError(t, json.Unmarshal([]byte(`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":"2",`+field+`}}`), &event))
			require.True(t, event.Usage.UsageInvalid)
			require.Equal(t, 2, event.Usage.OutputTokens)
			require.Equal(t, "max_tokens", event.Delta.StopReason)
			wire, err := json.Marshal(event)
			require.NoError(t, err)
			require.NotContains(t, string(wire), "UsageInvalid")
			state := NewAnthropicEventToResponsesState()
			AnthropicEventToResponsesEvents(&event, state)
			terminal := AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "message_stop"}, state)
			require.Nil(t, terminal[len(terminal)-1].Response.Usage)
			require.Equal(t, "incomplete", terminal[len(terminal)-1].Response.Status)
		})
	}
	var usage AnthropicUsage
	require.NoError(t, json.Unmarshal([]byte(`{"input_tokens":1e2,"output_tokens":"2","cache_creation":{"ephemeral_5m_input_tokens":"7","ephemeral_1h_input_tokens":1.3e1},"prompt_tokens_details":{"cached_tokens":"30"}}`), &usage))
	require.False(t, usage.UsageInvalid)
	require.Equal(t, 100, usage.InputTokens)
	require.Equal(t, 7, *usage.CacheCreation.Ephemeral5mInputTokens)
	require.Equal(t, 13, *usage.CacheCreation.Ephemeral1hInputTokens)
	require.Equal(t, 30, usage.PromptTokensDetails.CachedTokens)
	for _, raw := range []string{`{"input_tokens":`, `{"cache_creation":[]}`, `{"prompt_tokens_details":true}`} {
		require.Error(t, json.Unmarshal([]byte(raw), &usage), "keep malformed structure errors")
	}
}
