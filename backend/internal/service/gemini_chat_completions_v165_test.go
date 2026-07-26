package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGeminiResponseToChatCompletionsPreservesSafeInlineImages(t *testing.T) {
	response := map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{
				map[string]any{"text": "rendered:\n"},
				map[string]any{"inlineData": map[string]any{"mimeType": "image/webp", "data": "d2VicA=="}},
			}},
			"finishReason": "STOP",
		}},
	}
	raw, err := json.Marshal(response)
	require.NoError(t, err)
	got, _, err := geminiResponseToChatCompletions(response, "gemini-test", raw, nil)
	require.NoError(t, err)
	require.Len(t, got.Choices, 1)
	var content string
	require.NoError(t, json.Unmarshal(got.Choices[0].Message.Content, &content))
	require.Equal(t, "rendered:\n![image](data:image/webp;base64,d2VicA==)", content)
}

func TestGeminiResponseToChatCompletionsRejectsUnsafeInlineImages(t *testing.T) {
	for _, inline := range []map[string]any{
		{"mimeType": "image/svg+xml", "data": "PHN2Zz48L3N2Zz4="},
		{"mimeType": "image/png; charset=utf-8", "data": "aW1hZ2U="},
		{"mimeType": "image/png", "data": "not-base64"},
	} {
		response := map[string]any{"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{
				map[string]any{"text": "before"},
				map[string]any{"inlineData": inline},
				map[string]any{"text": "after"},
			}},
			"finishReason": "STOP",
		}}}
		raw, err := json.Marshal(response)
		require.NoError(t, err)
		got, _, err := geminiResponseToChatCompletions(response, "gemini-test", raw, nil)
		require.NoError(t, err)
		var content string
		require.NoError(t, json.Unmarshal(got.Choices[0].Message.Content, &content))
		require.Equal(t, "beforeafter", content)
	}
}

func TestGeminiAnthropicMessagesStillOmitsInlineImageMarkdown(t *testing.T) {
	response := map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"parts": []any{
			map[string]any{"text": "before"},
			map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "aW1hZ2U="}},
			map[string]any{"text": "after"},
		}},
		"finishReason": "STOP",
	}}}
	raw, err := json.Marshal(response)
	require.NoError(t, err)
	got, _ := convertGeminiToClaudeMessage(response, "gemini-test", raw, false)
	content, ok := got["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 2)
}
