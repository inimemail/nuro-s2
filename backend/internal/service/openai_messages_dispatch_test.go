package service

import "testing"

import "github.com/stretchr/testify/require"

func TestNormalizeOpenAIMessagesDispatchModelConfig(t *testing.T) {
	t.Parallel()

	cfg := normalizeOpenAIMessagesDispatchModelConfig(OpenAIMessagesDispatchModelConfig{
		OpusMappedModel:   " gpt-5.4-high ",
		SonnetMappedModel: "gpt-5.3-codex",
		HaikuMappedModel:  " gpt-5.4-mini-medium ",
		ExactModelMappings: map[string]string{
			" claude-sonnet-4-5-20250929 ": " gpt-5.2-high ",
			"":                             "gpt-5.4",
			"claude-opus-4-6":              " ",
		},
	})

	require.Equal(t, "gpt-5.4", cfg.OpusMappedModel)
	require.Equal(t, "gpt-5.3-codex", cfg.SonnetMappedModel)
	require.Equal(t, "gpt-5.4-mini", cfg.HaikuMappedModel)
	require.Equal(t, map[string]string{
		"claude-sonnet-4-5-20250929": "gpt-5.2",
	}, cfg.ExactModelMappings)
}

func TestGroupResolveMessagesDispatchModel_GrokMapsClaudeFamilyToGrok(t *testing.T) {
	t.Parallel()

	group := &Group{Platform: PlatformGrok}

	require.Equal(t, "grok-4.5", group.ResolveMessagesDispatchModel("claude-sonnet-4-5"))
	require.Equal(t, "grok-4.5", group.ResolveMessagesDispatchModel("claude-opus-4-6"))
	require.Equal(t, "grok-4.5", group.ResolveMessagesDispatchModel("claude-haiku-4-5"))
	require.Empty(t, group.ResolveMessagesDispatchModel("grok"))
	require.Empty(t, group.ResolveMessagesDispatchModel("gpt-5.3-codex"))
}

func TestSanitizeGroupMessagesDispatchFields_GrokPreservesDispatchOnly(t *testing.T) {
	t.Parallel()

	group := &Group{
		Platform:                           PlatformGrok,
		AllowMessagesDispatch:              true,
		DefaultMappedModel:                 "custom-model",
		StrictModelPriorityOnModelMismatch: true,
		MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
			SonnetMappedModel: "custom-model",
		},
	}

	sanitizeGroupMessagesDispatchFields(group)

	require.True(t, group.AllowMessagesDispatch)
	require.Empty(t, group.DefaultMappedModel)
	require.False(t, group.StrictModelPriorityOnModelMismatch)
	require.Equal(t, OpenAIMessagesDispatchModelConfig{}, group.MessagesDispatchModelConfig)
}
