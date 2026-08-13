package service

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCalculateAudioCostGroupOverrideSemantics(t *testing.T) {
	billing := NewBillingService(nil, nil)
	require.Zero(t, billing.CalculateAudioCostFromUsage(nil, nil, 1).TotalCost)

	zero := 0.0
	cost := billing.CalculateAudioCost("tts", 2, &audioPriceConfig{TTSPerMChars: &zero}, 3)
	require.Zero(t, cost.TotalCost)
	require.Zero(t, cost.ActualCost)

	price := 7.5
	cost = billing.CalculateAudioCost("tts", 2, &audioPriceConfig{TTSPerMChars: &price}, 2)
	require.Equal(t, 15.0, cost.TotalCost)
	require.Equal(t, 30.0, cost.ActualCost)
}

func TestCalculateRecordUsageCostAddsSearchSurchargeToTokens(t *testing.T) {
	pricing := NewPricingService(nil, nil)
	billing := NewBillingService(nil, pricing)
	svc := &GatewayService{billingService: billing}
	price := 20.0
	apiKey := &APIKey{Group: &Group{ID: 1, SearchPricePer1K: &price}}
	result := &ForwardResult{Model: "claude-sonnet-4", SearchCount: 5}
	cost := svc.calculateRecordUsageCost(context.Background(), result, apiKey, result.Model, 1, 1, &recordUsageOpts{})
	require.InDelta(t, 0.1, cost.TotalCost, 1e-9)
}

func TestVideoModelPricesPreferVideo15Family(t *testing.T) {
	prices := NormalizeVideoModelPrices(map[string]map[string]float64{
		"xai/grok-video-1.5": {"720p": 0.42},
	})
	require.Equal(t, 0.42, *LookupVideoModelPrice(prices, "grok-imagine-video-1.5-preview", "720"))
	group := &Group{VideoModelPrices: prices}
	require.Equal(t, 0.42, *group.GetVideoPriceForModel("grok-imagine-video-1.5", "hd"))
}

func TestNormalizeVideoModelPricesDropsNonFiniteValues(t *testing.T) {
	prices := NormalizeVideoModelPrices(map[string]map[string]float64{
		"grok-imagine-video": {
			"480p":  math.NaN(),
			"720p":  math.Inf(1),
			"1080p": 0.5,
		},
	})
	require.Nil(t, LookupVideoModelPrice(prices, "grok-imagine-video", "480p"))
	require.Nil(t, LookupVideoModelPrice(prices, "grok-imagine-video", "720p"))
	require.Equal(t, 0.5, *LookupVideoModelPrice(prices, "grok-imagine-video", "1080p"))
}

func TestCompositeRouteNormalizesNewCapabilityEndpoints(t *testing.T) {
	for _, endpoint := range []string{CompositeRouteEndpointVideos, CompositeRouteEndpointVoice, CompositeRouteEndpointSearch} {
		require.Equal(t, endpoint, normalizeCompositeRouteEndpoint(endpoint))
	}
}
