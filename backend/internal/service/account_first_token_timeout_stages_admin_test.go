package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeOpenAIAPIKeyFirstTokenTimeoutStagesExtra(t *testing.T) {
	t.Run("syncs stage one into legacy scalars", func(t *testing.T) {
		extra, err := normalizeOpenAIAPIKeyFirstTokenTimeoutStagesExtra(PlatformOpenAI, AccountTypeAPIKey, map[string]any{
			openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey: []any{
				map[string]any{"stage": 1, "placeholder_ms": 1000, "guard_max_ms": 3000},
				map[string]any{"stage": 2, "placeholder_ms": 1750, "guard_max_ms": 5200},
			},
		})
		require.NoError(t, err)
		require.Equal(t, 1000, extra[openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey])
		require.Equal(t, 3000, extra[openAIAPIKeyFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey])
	})

	t.Run("rejects more than ten stages", func(t *testing.T) {
		stages := make([]any, 11)
		for index := range stages {
			stages[index] = map[string]any{"stage": index + 1, "placeholder_ms": 100 + index, "guard_max_ms": 1000 + index}
		}
		_, err := normalizeOpenAIAPIKeyFirstTokenTimeoutStagesExtra(PlatformOpenAI, AccountTypeAPIKey, map[string]any{
			openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey: stages,
		})
		require.Error(t, err)
	})

	t.Run("removes API key stages from other identities", func(t *testing.T) {
		extra, err := normalizeOpenAIAPIKeyFirstTokenTimeoutStagesExtra(PlatformOpenAI, AccountTypeOAuth, map[string]any{
			openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey: []any{},
			"kept": true,
		})
		require.NoError(t, err)
		require.NotContains(t, extra, openAIAPIKeyFirstTokenTimeoutPlaceholderStagesExtraKey)
		require.Equal(t, true, extra["kept"])
	})
}
