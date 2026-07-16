package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeOpenAIResponsesLiteTools_EnsuresAllTurns(t *testing.T) {
	req := map[string]any{"model": "gpt-5.5", "input": "hello"}
	changed, err := normalizeOpenAIResponsesLiteTools(req)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "all_turns", req["reasoning"].(map[string]any)["context"])
}

func TestNormalizeOpenAIResponsesLiteTools_MovesNamespaceAndKeepsFunction(t *testing.T) {
	req := map[string]any{
		"tools": []any{
			map[string]any{"type": "namespace", "name": "image_gen"},
			map[string]any{"type": "function", "name": "lookup"},
		},
		"input": "hello",
	}
	changed, err := normalizeOpenAIResponsesLiteTools(req)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, req["tools"], 1)
	input := req["input"].([]any)
	require.Equal(t, "additional_tools", input[1].(map[string]any)["type"])
	require.Equal(t, "all_turns", req["reasoning"].(map[string]any)["context"])
}

func TestNormalizeOpenAIResponsesLiteTools_RejectsNonObjectReasoning(t *testing.T) {
	_, err := normalizeOpenAIResponsesLiteTools(map[string]any{"reasoning": "high"})
	require.Error(t, err)
}
