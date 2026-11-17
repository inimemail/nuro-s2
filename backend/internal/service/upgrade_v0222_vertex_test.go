package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestV0222VertexDefaultComponentsAndExplicitZeroStayIndependent(t *testing.T) {
	dir := t.TempDir()
	override := filepath.Join(dir, "override.json")
	require.NoError(t, os.WriteFile(override, []byte(`{"gemini-3.6-flash":{"input_cost_per_token":0,"output_cost_per_image_token":0,"cache_creation_input_token_cost_above_1hr":0,"cache_storage_cost_per_token_per_hour":0}}`), 0600))
	ps := NewPricingService(&config.Config{Pricing: config.PricingConfig{OverrideFile: override}}, nil)
	data, err := ps.parsePricingData([]byte(`{"gemini-3.6-flash":{"input_cost_per_token":1,"output_cost_per_token":2,"cache_read_input_token_cost":3,"cache_creation_input_token_cost":4,"cache_creation_input_token_cost_above_1hr":5,"output_cost_per_image_token":6,"input_cost_per_image_token":7,"cache_read_input_image_token_cost":8}}`))
	require.NoError(t, err)
	ps.pricingData = data
	bs := &BillingService{pricingService: ps}
	base, err := bs.GetModelPricing("gemini-3.6-flash")
	require.NoError(t, err)
	p := applyVertexDefaultTokenPricing(base, 1.1)
	require.Zero(t, p.InputPricePerToken)
	require.Zero(t, p.ImageOutputPricePerToken)
	require.Zero(t, p.CacheCreation1hPrice)
	require.InDelta(t, base.OutputPricePerToken*1.1, p.OutputPricePerToken, 1e-15)
	require.InDelta(t, base.CacheCreationPricePerToken*1.1, p.CacheCreationPricePerToken, 1e-15)
	require.InDelta(t, base.ImageInputPricePerToken*1.1, p.ImageInputPricePerToken, 1e-15)
	require.InDelta(t, base.ImageCacheReadPricePerToken*1.1, p.ImageCacheReadPricePerToken, 1e-15)
	require.NotSame(t, base, p)
	account := &Account{Platform: PlatformGemini, Type: AccountTypeServiceAccount, Credentials: map[string]any{"location": "eu", "project_id": "fixture"}}
	storage, source, err := bs.geminiCacheStoragePriceForAccount("gemini-3.6-flash", time.Now(), account)
	require.NoError(t, err)
	require.Zero(t, storage)
	require.Equal(t, "override_file", source)
	defaultBS := &BillingService{}
	storage, source, err = defaultBS.geminiCacheStoragePriceForAccount("gemini-3.6-flash", time.Now(), account)
	require.NoError(t, err)
	require.InDelta(t, 1.1e-6, storage, 1e-15)
	require.Equal(t, "official_vertex", source)
}

func TestV0222VertexBillingStatsAndBatchUseActualRegion(t *testing.T) {
	const model = "gemini-3.1-flash-image"
	ps := NewPricingService(&config.Config{}, nil)
	ps.pricingData = map[string]*LiteLLMModelPricing{model: {InputCostPerToken: 1e-6, OutputCostPerToken: 2e-6, OutputCostPerImageToken: .1}}
	bs := &BillingService{pricingService: ps}
	resolver := NewModelPricingResolver(nil, bs)
	account := &Account{Platform: PlatformGemini, Type: AccountTypeServiceAccount, Credentials: map[string]any{"location": "eu", "project_id": "fixture"}}
	ctx := context.Background()
	tokens := UsageTokens{InputTokens: 100, OutputTokens: 50}
	input := CostInput{Ctx: ctx, Model: model, Tokens: tokens, RateMultiplier: 1, Resolver: resolver, VertexPriceMultiplier: VertexUpstreamPriceMultiplier(account, model, "us")}
	cost, err := bs.CalculateCostUnified(input)
	require.NoError(t, err)
	require.InDelta(t, .00022, cost.TotalCost, 1e-12)
	log := &UsageLog{}
	applyGemini36AccountStatsCost(ctx, log, nil, bs, account, 1, model, tokens, cost.TotalCost, time.Now(), nil, "us")
	require.NotNil(t, log.AccountStatsCost)
	require.InDelta(t, cost.TotalCost, *log.AccountStatsCost, 1e-12)
	// Explicit channel/group prices are not regionally multiplied.
	for _, source := range []string{PricingSourceChannel, PricingSourceGroup} {
		input.Resolved = &ResolvedPricing{Mode: BillingModeToken, Source: source, BasePricing: &ModelPricing{InputPricePerToken: 1e-6, OutputPricePerToken: 2e-6}}
		cost, err = bs.CalculateCostUnified(input)
		require.NoError(t, err)
		require.InDelta(t, .0002, cost.TotalCost, 1e-12)
	}
	batch := &BatchImageModelPricingResolver{Resolver: resolver}
	require.InDelta(t, 1.1, batch.vertexRegionalUnitMultiplier(ctx, model, account, "us"), 1e-15)
	require.Equal(t, 1.0, batch.vertexRegionalUnitMultiplier(ctx, model, account, "global"))
	ps.pricingData[model].vertexPriceOverrides = map[string]bool{"output_cost_per_image_token": true}
	require.Equal(t, 1.0, batch.vertexRegionalUnitMultiplier(ctx, model, account, "eu"))
	account.Credentials["_gemini_cache_bound_location"] = "global"
	require.Equal(t, 1.0, VertexUpstreamPriceMultiplier(account, model, "eu"))
}

func TestV0222VertexPriorityOverridesAreIndependentFromStandardPrices(t *testing.T) {
	dir := t.TempDir()
	override := filepath.Join(dir, "override.json")
	require.NoError(t, os.WriteFile(override, []byte(`{"fixture":{"input_cost_per_token_priority":7,"output_cost_per_token_priority":0,"cache_read_input_token_cost":0}}`), 0600))
	ps := NewPricingService(&config.Config{Pricing: config.PricingConfig{OverrideFile: override}}, nil)
	data, err := ps.parsePricingData([]byte(`{"fixture":{"input_cost_per_token":1,"input_cost_per_token_priority":2,"output_cost_per_token":3,"output_cost_per_token_priority":4,"cache_read_input_token_cost":5,"cache_read_input_token_cost_priority":6}}`))
	require.NoError(t, err)
	ps.pricingData = data
	bs := &BillingService{pricingService: ps}
	base, err := bs.GetModelPricing("fixture")
	require.NoError(t, err)
	price := applyVertexDefaultTokenPricing(base, 1.1)
	require.InDelta(t, 1.1, price.InputPricePerToken, 1e-15)
	require.Equal(t, 7.0, price.InputPricePerTokenPriority)
	require.InDelta(t, 3.3, price.OutputPricePerToken, 1e-15)
	require.Zero(t, price.OutputPricePerTokenPriority)
	require.Zero(t, price.CacheReadPricePerToken)
	require.InDelta(t, 6.6, price.CacheReadPricePerTokenPriority, 1e-15)
	require.Equal(t, 6.0, base.CacheReadPricePerTokenPriority)
}

func TestV0222Gemini4DatePricingPreservesPriorityOverrides(t *testing.T) {
	for _, model := range []string{"gemini-4-carbon", "gemini-4-hydrogen"} {
		t.Run(model, func(t *testing.T) {
			override := filepath.Join(t.TempDir(), "override.json")
			require.NoError(t, os.WriteFile(override, []byte(`{"`+model+`":{"input_cost_per_token_priority":0.000007,"output_cost_per_token_priority":0.000011,"cache_read_input_token_cost_priority":0.000001}}`), 0600))
			ps := NewPricingService(&config.Config{Pricing: config.PricingConfig{OverrideFile: override}}, nil)
			data, err := ps.parsePricingData([]byte(`{"` + model + `":{"input_cost_per_token":1,"output_cost_per_token":2,"cache_read_input_token_cost":3}}`))
			require.NoError(t, err)
			ps.pricingData = data
			bs := &BillingService{pricingService: ps}
			base, err := bs.GetModelPricing(model)
			require.NoError(t, err)
			for _, at := range []time.Time{gemini4CarbonStandardPriceStart.Add(-time.Nanosecond), gemini4CarbonStandardPriceStart} {
				price := applyGemini4OfficialPricing(model, base, at)
				require.Equal(t, 7e-6, price.InputPricePerTokenPriority)
				require.Equal(t, 11e-6, price.OutputPricePerTokenPriority)
				require.Equal(t, 1e-6, price.CacheReadPricePerTokenPriority)
				cost, err := bs.CalculateCostUnified(CostInput{Ctx: context.Background(), Model: model, Tokens: UsageTokens{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 20}, RateMultiplier: 1, ServiceTier: "priority", PricingAt: at, Resolver: NewModelPricingResolver(nil, bs)})
				require.NoError(t, err)
				require.InDelta(t, 100*7e-6+50*11e-6+20*1e-6, cost.TotalCost, 1e-15)
			}
			require.Equal(t, 7e-6, base.InputPricePerTokenPriority)
		})
	}
	// Explicit zero must survive the date policy too. This does not change the
	// existing billing engine's interpretation of a zero priority component.
	price := &ModelPricing{InputPricePerTokenPriority: 0, vertexPriceOverrides: map[string]bool{"input_cost_per_token_priority": true}}
	require.Zero(t, applyGemini4OfficialPricing("gemini-4-carbon", price, gemini4CarbonStandardPriceStart).InputPricePerTokenPriority)
}
