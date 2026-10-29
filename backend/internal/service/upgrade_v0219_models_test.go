package service

import (
	"bytes"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUpgradeV0219TerraContracts(t *testing.T) {
	for _, model := range []string{"gpt-6.1-terra", "openai/gpt-6.1-terra-max", "gpt_6.1_terra_none"} {
		require.True(t, openai.IsGPT61TerraModelSpelling(model))
		require.Equal(t, "gpt-6.1-terra", normalizeKnownOpenAICodexModel(model))
		require.Equal(t, "gpt-6.1-terra", NormalizeOpenAICompatRequestedModel(model))
		require.NoError(t, validateNewModelChatReasoningTools(model, "none", false))
		require.Error(t, validateNewModelChatReasoningTools(model, "none", true))
	}
	require.False(t, openai.IsGPT61TerraModelSpelling("gpt-6.1-terra-unknown"))
	require.Error(t, openai.ValidateGPT61SolReasoningEffort("gpt-6.1-sol", "none"))
	require.NoError(t, openai.ValidateGPT61SolReasoningEffort("gpt-6.1-terra", "none"))
	svc := &BillingService{fallbackPrices: map[string]*ModelPricing{}}
	svc.initFallbackPricing()
	p := svc.getFallbackPricing("gpt-6.1-terra-max")
	require.InDelta(t, 1e-6, p.InputPricePerToken, 1e-15)
	require.InDelta(t, 5e-8, p.CacheReadPricePerToken, 1e-15)
	require.False(t, svc.shouldApplySessionLongContextPricing(UsageTokens{InputTokens: 272000}, p))
	require.True(t, svc.shouldApplySessionLongContextPricing(UsageTokens{InputTokens: 272001}, p))
	require.Equal(t, 2.0, p.LongContextInputMultiplier)
	require.Equal(t, 1.5, p.LongContextOutputMultiplier)
	require.Equal(t, "max", normalizeOpenAIReasoningEffortForModel("max", "gpt-6.1-terra"))
	require.Equal(t, "none", normalizeOpenAIReasoningEffortForModel("none", "gpt-6.1-terra"))
	body := []byte(`{"model":"gpt-6.1-terra","reasoning":{"effort":"none"},"temperature":0.5}`)
	out, err := normalizeGPT6ResponsesSampling(body)
	require.NoError(t, err)
	require.True(t, bytes.Equal(body, out))
	p = applyDeepSeekOfficialPeakPricing("deepseek-v4-pro", &ModelPricing{}, time.Unix(0, 0))
	require.InDelta(t, 6.6e-7, p.InputPricePerToken, 1e-15)
	p = applyDeepSeekOfficialPeakPricing("deepseek-v4-flash", &ModelPricing{}, time.Unix(0, 0))
	require.InDelta(t, 1.5e-7, p.InputPricePerToken, 1e-15)
}

func TestUpgradeV0219OpenCodeRolesAndSearchDimensions(t *testing.T) {
	account := &Account{Platform: PlatformOpenCodeGo, Type: AccountTypeAPIKey}
	raw := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"developer","content":"x"},{"role":"assistant","reasoning_content":"real","value":9007199254740993}]}`)
	out, err := normalizeStrictChatRequest(account, "https://opencode.ai/zen/go/v1/chat/completions", raw)
	require.NoError(t, err)
	require.Contains(t, string(out), `"role":"system"`)
	require.Contains(t, string(out), "9007199254740993")
	require.Contains(t, string(out), `"reasoning_content":"real"`)
	raw = bytes.Replace(raw, []byte("deepseek-v4-pro"), []byte("gpt-6-sol"), 1)
	out, err = normalizeStrictChatRequest(account, "https://opencode.ai/zen/go/v1/chat/completions", raw)
	require.NoError(t, err)
	require.Equal(t, raw, out)
	existing := map[string]any{featureKeyWebSearchEmulation: map[string]any{"openai": true, "anthropic": true}}
	incoming := map[string]any{featureKeyWebSearchEmulation: map[string]any{"anthropic": false}, "time_pricing": "keep"}
	merged := mergeWebSearchEmulationDimensions(existing, incoming)
	require.Equal(t, map[string]any{"openai": true, "anthropic": false}, merged[featureKeyWebSearchEmulation])
	require.Equal(t, map[string]any{"anthropic": false}, incoming[featureKeyWebSearchEmulation])
	for _, clear := range []any{nil, map[string]any{}} {
		require.Equal(t, clear, mergeWebSearchEmulationDimensions(existing, map[string]any{featureKeyWebSearchEmulation: clear})[featureKeyWebSearchEmulation])
	}
}

func TestUpgradeV0219ImageCountingAndOptIn(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	observeGeminiImageCount(c, []byte(`{"response":{"candidates":[{"content":{"parts":[{"inline_data":{"mime_type":"image/png","data":"abc"}},{"fileData":{"mimeType":"image/webp","fileUri":"gs://image"}},{"inlineData":{"mimeType":"audio/wav","data":"xyz"}}]}}]}}`))
	require.Equal(t, 2, c.GetInt(geminiObservedImageCountKey))
	observeGeminiImageCount(c, []byte(`{"candidates":[{"content":{"parts":[{"file_data":{"mime_type":"image/png","file_uri":"gs://image"}}]}}]}`))
	require.Equal(t, 2, c.GetInt(geminiObservedImageCountKey))
	resetGeminiResponseSignal(c)
	require.Zero(t, c.GetInt(geminiObservedImageCountKey))
	k := &APIKey{Group: &Group{Platform: PlatformOpenAI, ImageCountBilling: true, AllowImageGeneration: true}}
	require.True(t, imageCountBillingApplies(k, true))
	require.False(t, imageCountBillingApplies(k, false))
	k.Group.ImageCountBilling = false
	require.False(t, imageCountBillingApplies(k, true))
	k.Group.ImageCountBilling = true
	k.Group.Platform = PlatformGemini
	require.False(t, imageCountBillingApplies(k, true))
}
