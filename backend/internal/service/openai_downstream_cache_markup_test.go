package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func downstreamCacheMarkupTestPolicy() OpenAIDownstreamCacheMarkupPolicy {
	return OpenAIDownstreamCacheMarkupPolicy{
		ThresholdTokens:      100000,
		PercentBPS:           1000,
		InputPriceUnits:      100,
		CacheReadPriceUnits:  10,
		OutputPriceUnits:     600,
		LongContextThreshold: 272000,
	}
}

func TestOpenAIDownstreamCacheMarkupAccountConfig(t *testing.T) {
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		account := &Account{Platform: PlatformOpenAI, Type: accountType, Credentials: map[string]any{
			"openai_downstream_cache_markup_enabled":          true,
			"openai_downstream_cache_markup_threshold_tokens": float64(120000),
			"openai_downstream_cache_markup_percent_bps":      float64(750),
		}}
		require.True(t, account.IsOpenAIDownstreamCacheMarkupEnabled())
		require.Equal(t, int64(120000), account.OpenAIDownstreamCacheMarkupThresholdTokens())
		require.Equal(t, int64(750), account.OpenAIDownstreamCacheMarkupPercentBPS())
	}

	disabled := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{}}
	require.False(t, disabled.IsOpenAIDownstreamCacheMarkupEnabled())
	require.Equal(t, int64(OpenAIDownstreamCacheMarkupDefaultThresholdTokens), disabled.OpenAIDownstreamCacheMarkupThresholdTokens())
	require.Zero(t, disabled.OpenAIDownstreamCacheMarkupPercentBPS())

	imagePool := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"pool_mode":                                  true,
		"image_pool_mode":                            true,
		"openai_downstream_cache_markup_enabled":     true,
		"openai_downstream_cache_markup_percent_bps": 1000,
	}}
	require.False(t, imagePool.IsOpenAIDownstreamCacheMarkupEnabled())
}

func TestNormalizeOpenAIDownstreamUsageJSONWithMarkup_ThresholdAndAliases(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":130000,"output_tokens":1000,"total_tokens":131000,"input_tokens_details":{"cached_tokens":120000}}}`)
	got, changed := normalizeOpenAIDownstreamUsageJSONWithMarkup(body, "", downstreamCacheMarkupTestPolicy())
	require.True(t, changed)
	require.Equal(t, int64(134400), gjson.GetBytes(got, "usage.input_tokens").Int())
	require.Equal(t, int64(124000), gjson.GetBytes(got, "usage.input_tokens_details.cached_tokens").Int())
	require.Equal(t, int64(1067), gjson.GetBytes(got, "usage.output_tokens").Int())
	require.Equal(t, int64(135467), gjson.GetBytes(got, "usage.total_tokens").Int())

	below := []byte(`{"usage":{"input_tokens":109999,"output_tokens":10,"total_tokens":110009,"input_tokens_details":{"cached_tokens":99999}}}`)
	unchanged, changed := normalizeOpenAIDownstreamUsageJSONWithMarkup(below, "", downstreamCacheMarkupTestPolicy())
	require.False(t, changed)
	require.Equal(t, below, unchanged)
}

func TestNormalizeOpenAIDownstreamUsageJSONWithMarkup_ZeroPercentIsExactNoop(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":130000,"output_tokens":1000,"total_tokens":131000,"input_tokens_details":{"cached_tokens":120000}}}`)
	policy := downstreamCacheMarkupTestPolicy()
	policy.PercentBPS = 0
	got, changed := normalizeOpenAIDownstreamUsageJSONWithMarkup(body, "", policy)
	require.False(t, changed)
	require.Equal(t, body, got)
}

func TestNormalizeOpenAIDownstreamUsageJSONWithMarkup_UsesRawReadBeforeModeD(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":160000,"output_tokens":1000,"total_tokens":161000,"input_tokens_details":{"cached_tokens":99000,"cache_write_tokens":50000},"cache_creation_input_tokens":50000}}`)
	modeDOnly, changed := normalizeOpenAIDownstreamUsageJSON(body, OpenAIPromptCacheCreationOptimizationModeInput125)
	require.True(t, changed)
	withMarkup, changed := normalizeOpenAIDownstreamUsageJSONWithMarkup(body, OpenAIPromptCacheCreationOptimizationModeInput125, downstreamCacheMarkupTestPolicy())
	require.True(t, changed)
	require.JSONEq(t, string(modeDOnly), string(withMarkup), "Mode D additions must not trigger markup")
}

func TestNormalizeOpenAIResponsesUsageWithMarkup_PreservesLocalCopy(t *testing.T) {
	original := &apicompat.ResponsesUsage{
		InputTokens:  130000,
		OutputTokens: 1000,
		TotalTokens:  131000,
		InputTokensDetails: &apicompat.ResponsesInputTokensDetails{
			CachedTokens: 120000,
		},
	}
	downstream := cloneOpenAIResponsesUsage(original)
	require.True(t, normalizeOpenAIResponsesUsageForDownstreamWithMarkup(downstream, "", downstreamCacheMarkupTestPolicy()))
	require.Equal(t, 130000, original.InputTokens)
	require.Equal(t, 120000, original.InputTokensDetails.CachedTokens)
	require.Equal(t, 1000, original.OutputTokens)
	require.Equal(t, 134400, downstream.InputTokens)
	require.Equal(t, 124000, downstream.InputTokensDetails.CachedTokens)
	require.Equal(t, 1067, downstream.OutputTokens)
}

func TestOpenAIDownstreamCacheMarkupPolicyExcludesMediaIntentAndModels(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"openai_downstream_cache_markup_enabled":     true,
		"openai_downstream_cache_markup_percent_bps": 1000,
	}}
	svc := &OpenAIGatewayService{}
	require.Empty(t, svc.openAIDownstreamCacheMarkupPolicyForContext(context.Background(), account, "gpt-image-2"))
	require.Empty(t, svc.openAIDownstreamCacheMarkupPolicyForContext(
		WithOpenAIExplicitImageGenerationIntent(context.Background(), true), account, "gpt-5.6-sol",
	))
	require.True(t, isOpenAIGPTTextModel("openai/gpt-5.6-sol"))
	require.True(t, isOpenAIGPTTextModel("gpt-4.1"))
	require.True(t, isOpenAIGPTTextModel("gpt-4-vision-preview"))
	require.True(t, isOpenAIGPTTextModel("o3-mini"))
	require.True(t, isOpenAIGPTTextModel("o4-mini"))
	require.False(t, isOpenAIGPTTextModel("gpt-4o-mini-tts"))
	require.False(t, isOpenAIGPTTextModel("codex-image"))
	require.False(t, isOpenAIGPTTextModel("gpt-4o-search-preview"))
	require.False(t, isOpenAIGPTTextModel("deepseek-v4"))
}
