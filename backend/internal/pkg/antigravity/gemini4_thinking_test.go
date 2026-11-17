package antigravity

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestGemini4ThinkingLevelsPreserveDefaultsAndAliases(t *testing.T) {
	require.Nil(t, buildGenerationConfig(&ClaudeRequest{Model: "gemini-4-carbon"}).ThinkingConfig)
	for _, effort := range []string{"low", "medium", "high"} {
		cfg := buildGenerationConfig(&ClaudeRequest{Model: "gemini-4-carbon", MaxTokens: 99999, OutputConfig: &ClaudeOutputConfig{Effort: effort}, Thinking: &ThinkingConfig{Type: "enabled", BudgetTokens: 9999}})
		require.Equal(t, effort, cfg.ThinkingConfig.ThinkingLevel)
		require.Zero(t, cfg.ThinkingConfig.ThinkingBudget)
		require.Equal(t, 65536, cfg.MaxOutputTokens)
	}
	cfg := buildGenerationConfig(&ClaudeRequest{Model: "gemini-4-carbon-low", OutputConfig: &ClaudeOutputConfig{Effort: "high"}, Thinking: &ThinkingConfig{Type: "enabled"}})
	require.Empty(t, cfg.ThinkingConfig.ThinkingLevel)
	require.True(t, cfg.ThinkingConfig.IncludeThoughts)
	disabled := buildGenerationConfig(&ClaudeRequest{Model: "gemini-4-carbon", Thinking: &ThinkingConfig{Type: "disabled"}, OutputConfig: &ClaudeOutputConfig{Effort: "high"}})
	require.Equal(t, "low", disabled.ThinkingConfig.ThinkingLevel)
}

func TestSchemaArrayItemsFallbackDoesNotAttachToObjects(t *testing.T) {
	for _, invalid := range []any{nil, true, []any{}, map[string]any{}} {
		cleaned := CleanJSONSchema(map[string]any{"type": "array", "items": invalid})
		require.Equal(t, map[string]any{"type": "string"}, cleaned["items"])
	}
	cleaned := CleanJSONSchema(map[string]any{"type": "object", "items": map[string]any{"type": "number"}})
	require.NotContains(t, cleaned, "items")
	cleaned = CleanJSONSchema(map[string]any{"type": "array", "items": map[string]any{"type": "number"}})
	require.Equal(t, map[string]any{"type": "number"}, cleaned["items"])
}
