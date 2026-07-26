package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	opus5InputPricePerToken         = 5e-6
	opus5OutputPricePerToken        = 25e-6
	opus5CacheCreationPricePerToken = 6.25e-6
	opus5CacheReadPricePerToken     = 0.5e-6
)

func TestClaudeOpus5FamilyFallbackDoesNotUseLegacyOpusRates(t *testing.T) {
	svc := NewBillingService(&config.Config{}, &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"claude-opus-4-8": {
			InputCostPerToken: opus5InputPricePerToken, OutputCostPerToken: opus5OutputPricePerToken,
			CacheCreationInputTokenCost: opus5CacheCreationPricePerToken,
			CacheReadInputTokenCost:     opus5CacheReadPricePerToken,
		},
		"claude-opus-4-1":        {InputCostPerToken: 15e-6, OutputCostPerToken: 75e-6},
		"claude-3-opus-20240229": {InputCostPerToken: 15e-6, OutputCostPerToken: 75e-6},
	}})

	for _, model := range []string{"claude-opus-5", "us.anthropic.claude-opus-5-v1"} {
		pricing, err := svc.GetModelPricing(model)
		require.NoError(t, err)
		require.NotNil(t, pricing)
		assert.InDelta(t, opus5InputPricePerToken, pricing.InputPricePerToken, 1e-12)
		assert.InDelta(t, opus5OutputPricePerToken, pricing.OutputPricePerToken, 1e-12)
	}
}

func TestClaudeOpus5HardcodedFallbackPricing(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	for _, tt := range []struct {
		model         string
		input, output float64
	}{
		{model: "claude-opus-5", input: 5e-6, output: 25e-6},
		{model: "us.anthropic.claude-opus-5-v1", input: 5e-6, output: 25e-6},
		{model: "claude-opus-4-8", input: 5e-6, output: 25e-6},
		{model: "claude-opus-4-1-20250805", input: 15e-6, output: 75e-6},
	} {
		t.Run(tt.model, func(t *testing.T) {
			pricing, err := svc.GetModelPricing(tt.model)
			require.NoError(t, err)
			assert.InDelta(t, tt.input, pricing.InputPricePerToken, 1e-12)
			assert.InDelta(t, tt.output, pricing.OutputPricePerToken, 1e-12)
		})
	}
	opus5, err := svc.GetModelPricing("claude-opus-5")
	require.NoError(t, err)
	assert.InDelta(t, opus5CacheCreationPricePerToken, opus5.CacheCreationPricePerToken, 1e-12)
	assert.InDelta(t, opus5CacheReadPricePerToken, opus5.CacheReadPricePerToken, 1e-12)
}

func TestClaudeOpus5BedrockCapabilityGates(t *testing.T) {
	for _, tt := range []struct {
		model                        string
		newer, toolSearch, opus47New bool
	}{
		{model: "claude-opus-5", newer: true, toolSearch: true, opus47New: true},
		{model: "us.anthropic.claude-opus-5-v1", newer: true, toolSearch: true, opus47New: true},
		{model: "claude-sonnet-5", newer: true, toolSearch: true},
		{model: "anthropic.claude-opus-4-1-v1"},
		{model: "us.anthropic.claude-opus-4-8-v1", newer: true, toolSearch: true, opus47New: true},
		{model: "us.anthropic.claude-haiku-4-5-20251001-v1:0", newer: true},
	} {
		t.Run(tt.model, func(t *testing.T) {
			assert.Equal(t, tt.newer, isBedrockClaude45OrNewer(tt.model))
			assert.Equal(t, tt.toolSearch, bedrockModelSupportsToolSearch(tt.model))
			assert.Equal(t, tt.opus47New, isBedrockOpus47OrNewer(tt.model))
		})
	}
}

func TestClaudeOpus5CatalogAndBedrockMapping(t *testing.T) {
	assert.Contains(t, claude.DefaultModelIDs(), "claude-opus-5")
	mapped, ok := domain.DefaultBedrockModelMapping["claude-opus-5"]
	require.True(t, ok)
	assert.Equal(t, "us.anthropic.claude-opus-5-v1", mapped)
}
