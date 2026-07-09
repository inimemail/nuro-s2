//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func newOpenAIOAuthAccountForModelSupportTest() *Account {
	return &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
	}
}

func TestIsModelSupported_OpenAIOAuthEmptyMapping_ServableModels(t *testing.T) {
	account := newOpenAIOAuthAccountForModelSupportTest()

	for _, model := range []string{
		"",
		"gpt-5.4",
		"gpt-5.3-codex",
		"codex-mini-latest",
		"gpt-image-1",
		"claude-sonnet-4-6",
		"gpt-4o",
		"my-custom-alias",
	} {
		require.True(t, account.IsModelSupported(model), "expected %q to remain servable", model)
	}
}

func TestIsModelSupported_OpenAIOAuthEmptyMapping_RejectsForeignModels(t *testing.T) {
	account := newOpenAIOAuthAccountForModelSupportTest()

	for _, model := range []string{
		"deepseek-v4",
		"glm-4.7",
		"kimi-k2",
		"moonshot-v1-128k",
		"gemini-3.0-pro",
		"grok-4",
		"qwen3-max",
		"minimax-m2.5",
		"provider/deepseek-v4",
	} {
		require.False(t, account.IsModelSupported(model), "expected %q to be rejected", model)
	}
}

func TestIsModelSupported_OpenAIOAuthExplicitMappingUnchanged(t *testing.T) {
	account := newOpenAIOAuthAccountForModelSupportTest()
	account.Credentials = map[string]any{
		"model_mapping": map[string]any{"deepseek-v4": "gpt-5.4"},
	}

	require.True(t, account.IsModelSupported("deepseek-v4"))
	require.False(t, account.IsModelSupported("glm-4.7"))
}

func TestIsModelSupported_OpenAIOAuthPassthroughAllowsAll(t *testing.T) {
	account := newOpenAIOAuthAccountForModelSupportTest()
	account.Extra = map[string]any{"openai_passthrough": true}

	require.True(t, account.IsModelSupported("deepseek-v4"))
}

func TestIsModelSupported_OpenAIAPIKeyEmptyMappingAllowsAll(t *testing.T) {
	account := &Account{
		ID:       2,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
	}

	require.True(t, account.IsModelSupported("deepseek-v4"))
	require.True(t, account.IsModelSupported("gpt-5.4"))
}

func TestIsOpenAIOAuthServableModel(t *testing.T) {
	require.True(t, isOpenAIOAuthServableModel("gpt-5.4-high"))
	require.True(t, isOpenAIOAuthServableModel("  gpt-5.3-codex  "))
	require.True(t, isOpenAIOAuthServableModel("DeepThink-x"))
	require.False(t, isOpenAIOAuthServableModel("DeepSeek-V4"))
	require.False(t, isOpenAIOAuthServableModel("qwen3-235b-thinking"))
	require.True(t, isOpenAIOAuthServableModel("deepseekcoder"))
}
