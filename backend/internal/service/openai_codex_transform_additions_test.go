package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnsureCodexReasoningInclude(t *testing.T) {
	body := map[string]any{"reasoning": map[string]any{"effort": "medium"}}
	require.True(t, ensureCodexReasoningInclude(body))
	require.Equal(t, []any{"reasoning.encrypted_content"}, body["include"])
	require.False(t, ensureCodexReasoningInclude(body))

	body2 := map[string]any{}
	require.False(t, ensureCodexReasoningInclude(body2))
	_, ok := body2["include"]
	require.False(t, ok)

	body3 := map[string]any{
		"reasoning": map[string]any{"effort": "high"},
		"include":   []any{"foo"},
	}
	require.True(t, ensureCodexReasoningInclude(body3))
	require.Equal(t, []any{"foo", "reasoning.encrypted_content"}, body3["include"])
}

func TestApplyCodexClientMetadata(t *testing.T) {
	acc := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_device_id": "dev-xyz"}}

	body := map[string]any{}
	require.True(t, applyCodexClientMetadata(body, acc))
	cm, ok := body["client_metadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "dev-xyz", cm["x-codex-installation-id"])
	require.False(t, applyCodexClientMetadata(body, acc))

	body2 := map[string]any{}
	require.False(t, applyCodexClientMetadata(body2, &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}))
	_, ok = body2["client_metadata"]
	require.False(t, ok)

	body3 := map[string]any{"client_metadata": map[string]any{"x-codex-turn-metadata": "t"}}
	require.True(t, applyCodexClientMetadata(body3, acc))
	cm3, _ := body3["client_metadata"].(map[string]any)
	require.Equal(t, "t", cm3["x-codex-turn-metadata"])
	require.Equal(t, "dev-xyz", cm3["x-codex-installation-id"])
}

func TestDefaultCodexSynthInstructionsModelAware(t *testing.T) {
	require.True(t, strings.Contains(defaultCodexSynthInstructions("gpt-5-codex"), "You are Codex, based on GPT-5"))
	require.True(t, strings.Contains(defaultCodexSynthInstructions("gpt-5.2"), "You are GPT-5.2 running in the Codex CLI"))
	require.True(t, strings.Contains(defaultCodexSynthInstructions("gpt-5.1"), "You are GPT-5.1 running in the Codex CLI"))
}
