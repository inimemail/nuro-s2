package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV0221Claude52ModelsAndPrices(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "resources", "model-pricing", "model_prices_and_context_window.json"))
	require.NoError(t, err)
	s := &PricingService{}
	s.pricingData, err = s.parsePricingData(data)
	require.NoError(t, err)
	billing := NewBillingService(&config.Config{}, s)
	fallback := NewBillingService(&config.Config{}, nil)
	for _, base := range []string{"claude-fable-5-2", "claude-mythos-5-2"} {
		for _, model := range []string{base, "anthropic/" + base, "bedrock/us.anthropic." + base + "-v1:0", "vertex_ai/" + base + "@20261110", base + "-20261110-thinking"} {
			require.True(t, claude.IsClaude52(model), model)
			for _, source := range []*BillingService{billing, fallback} {
				price, err := source.GetModelPricing(model)
				require.NoError(t, err)
				require.InDelta(t, 8e-6, price.InputPricePerToken, 1e-15, model)
				require.InDelta(t, 40e-6, price.OutputPricePerToken, 1e-15, model)
				require.InDelta(t, .2e-6, price.CacheReadPricePerToken, 1e-15, model)
			}
			out, err := normalizeOpus55Request([]byte(`{"model":"`+model+`","messages":[]}`), model)
			require.NoError(t, err)
			require.Equal(t, "adaptive", gjson.GetBytes(out, "thinking.type").String())
			require.Equal(t, "high", gjson.GetBytes(out, "output_config.effort").String())
		}
		for _, field := range []string{`"thinking":{"type":"disabled"}`, `"thinking":{"budget_tokens":100}`, `"tool_choice":{"type":"any"}`, `"temperature":0.5`, `"top_p":0.1`, `"top_k":1`, `"temperature":1,"top_p":0.99`, `"output_config":{"effort":"ultra"}`} {
			_, err := normalizeOpus55Request([]byte(`{"model":"`+base+`",`+field+`}`), base)
			require.Error(t, err, field)
		}
	}
	for _, model := range []string{"claude-fable-5-20", "claude-mythos-5-2-unknown", "other/claude-fable-5-2"} {
		require.False(t, claude.IsClaude52(model), model)
	}
	for _, source := range []*BillingService{billing, fallback} {
		price, err := source.GetModelPricing("claude-sonnet-5-5")
		require.NoError(t, err)
		require.InDelta(t, .1e-6, price.CacheReadPricePerToken, 1e-15)
	}
	// Curated OpenAI cards are not part of this price update.
	var catalog map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &catalog))
	require.Contains(t, catalog, "gpt-6.1-sol")
	require.Contains(t, catalog, "gpt-6.1-terra")
}
