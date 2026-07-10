package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCalculateCostUnified_TokenIntervalsIncludeCacheCreationInContext(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	maxTokens := 100
	lowInputPrice := 10.0
	highInputPrice := 20.0
	resolved := &ResolvedPricing{
		Mode:        BillingModeToken,
		BasePricing: &ModelPricing{InputPricePerToken: 1.0},
		Intervals: []PricingInterval{
			{MinTokens: 0, MaxTokens: &maxTokens, InputPrice: &lowInputPrice},
			{MinTokens: maxTokens, InputPrice: &highInputPrice},
		},
	}

	cost, err := svc.CalculateCostUnified(CostInput{
		Ctx:            context.Background(),
		Model:          "custom-interval-model",
		Tokens:         UsageTokens{InputTokens: 50, CacheCreationTokens: 30, CacheReadTokens: 30},
		RateMultiplier: 1,
		Resolver:       &ModelPricingResolver{},
		Resolved:       resolved,
	})

	require.NoError(t, err)
	require.InDelta(t, 50*highInputPrice, cost.InputCost, 1e-12)
}

func TestCalculateCostUnified_RequestTiersIncludeCacheCreationInContext(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	maxTokens := 100
	lowRequestPrice := 10.0
	highRequestPrice := 20.0
	resolved := &ResolvedPricing{
		Mode: BillingModePerRequest,
		RequestTiers: []PricingInterval{
			{MinTokens: 0, MaxTokens: &maxTokens, PerRequestPrice: &lowRequestPrice},
			{MinTokens: maxTokens, PerRequestPrice: &highRequestPrice},
		},
	}

	cost, err := svc.CalculateCostUnified(CostInput{
		Ctx:            context.Background(),
		Model:          "custom-request-tier-model",
		Tokens:         UsageTokens{InputTokens: 50, CacheCreationTokens: 30, CacheReadTokens: 30},
		RateMultiplier: 1,
		Resolver:       &ModelPricingResolver{},
		Resolved:       resolved,
	})

	require.NoError(t, err)
	require.InDelta(t, highRequestPrice, cost.TotalCost, 1e-12)
}
