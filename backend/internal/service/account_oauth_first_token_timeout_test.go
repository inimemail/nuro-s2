package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages_IsolatedFromAPIKeyScalars(t *testing.T) {
	stages, err := NormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages(map[string]any{
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderMsExtraKey:         800,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey: 5000,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderStagesExtraKey: []any{
			map[string]any{"stage": 1, "placeholder_ms": 800, "guard_max_ms": 5000},
			map[string]any{"stage": 2, "placeholder_ms": 3000, "guard_max_ms": 10000},
		},
		openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey:         100000,
		openAIAPIKeyFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey: 100000,
	})
	require.NoError(t, err)
	require.Equal(t, 800, stages[0].PlaceholderMS)
	require.Equal(t, 5000, stages[0].GuardMaxMS)
	require.Equal(t, 3000, stages[1].PlaceholderMS)
}

func TestNormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages_PreservesDefaultFourStages(t *testing.T) {
	stages, err := NormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages(map[string]any{
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderMsExtraKey:         800,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey: 5000,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderStagesExtraKey: []any{
			map[string]any{"stage": 1, "placeholder_ms": 800, "guard_max_ms": 5000},
			map[string]any{"stage": 2, "placeholder_ms": 3000, "guard_max_ms": 10000},
			map[string]any{"stage": 3, "placeholder_ms": 5000, "guard_max_ms": 15000},
			map[string]any{"stage": 4, "placeholder_ms": 10000, "guard_max_ms": 30000},
		},
	})
	require.NoError(t, err)
	require.Equal(t, defaultOpenAIAPIKeyFirstTokenTimeoutPlaceholderStages(), stages)
}

func TestNormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages_RepairsLastGuardAsLegacyStageOne(t *testing.T) {
	stages, err := NormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages(map[string]any{
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderMsExtraKey:         800,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey: 30000,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderStagesExtraKey: []any{
			map[string]any{"stage": 1, "placeholder_ms": 800, "guard_max_ms": 5000},
			map[string]any{"stage": 2, "placeholder_ms": 3000, "guard_max_ms": 10000},
			map[string]any{"stage": 3, "placeholder_ms": 5000, "guard_max_ms": 15000},
			map[string]any{"stage": 4, "placeholder_ms": 10000, "guard_max_ms": 30000},
		},
	})
	require.NoError(t, err)
	require.Equal(t, defaultOpenAIAPIKeyFirstTokenTimeoutPlaceholderStages(), stages)
}

func TestNormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages_PreservesValidLegacyScalarAuthority(t *testing.T) {
	stages, err := NormalizeOpenAIOAuthFirstTokenTimeoutPlaceholderStages(map[string]any{
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderMsExtraKey:         900,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey: 6000,
		openAIOAuthChatGPTFirstTokenTimeoutPlaceholderStagesExtraKey: []map[string]any{
			{"stage": 1, "placeholder_ms": 800, "guard_max_ms": 5000},
			{"stage": 2, "placeholder_ms": 3000, "guard_max_ms": 10000},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 900, stages[0].PlaceholderMS)
	require.Equal(t, 6000, stages[0].GuardMaxMS)
}

func TestOAuthFirstTokenStagesMatchAPIKeyRuntime(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		t.Run(accountType, func(t *testing.T) {
			prefix := "openai_oauth_chatgpt_first_token_timeout_placeholder"
			if accountType == AccountTypeAPIKey {
				prefix = "openai_apikey_first_token_timeout_placeholder"
			}
			account := &Account{ID: 17, Platform: PlatformOpenAI, Type: accountType, Extra: map[string]any{
				prefix + "_enabled":       true,
				prefix + "_ms":            600,
				prefix + "_guard_enabled": true,
				prefix + "_guard_max_ms":  30000,
				prefix + "_stages": []any{
					map[string]any{"stage": 1, "placeholder_ms": 600, "guard_max_ms": 30000},
					map[string]any{"stage": 2, "placeholder_ms": 5000, "guard_max_ms": 90000},
					map[string]any{"stage": 3, "placeholder_ms": 100000, "guard_max_ms": 900000},
				},
			}}
			svc := &OpenAIGatewayService{}
			require.Equal(t, 600, svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.6-sol"))
			for _, sample := range []struct{ latency, want int }{{30000, 600}, {30001, 5000}, {90001, 100000}, {900001, 0}, {500, 600}} {
				svc.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, "gpt-5.6-sol", sample.latency)
				require.Equal(t, sample.want, svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.6-sol"))
			}
			account.Extra[prefix+"_guard_enabled"] = false
			account.Extra[prefix+"_ms"] = 100000
			account.Extra[prefix+"_guard_max_ms"] = 900000
			account.Extra[prefix+"_stages"] = []any{map[string]any{"stage": 1, "placeholder_ms": 100000, "guard_max_ms": 900000}}
			require.Equal(t, 100000, svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.6-sol"), "disabling protection must not truncate a saved stage")
		})
	}
}
