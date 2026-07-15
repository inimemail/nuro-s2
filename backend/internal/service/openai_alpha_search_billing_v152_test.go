package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestV152WebSearchBillingUsesBaseMultiplier(t *testing.T) {
	groupID := int64(11)
	apiKey := &APIKey{
		ID:      1,
		GroupID: &groupID,
		Group:   &Group{ID: groupID, Platform: PlatformOpenAI},
	}
	result := &OpenAIForwardResult{Model: "search-model", WebSearchCalls: 1}
	svc := &OpenAIGatewayService{billingService: &BillingService{}}

	cost, err := svc.calculateOpenAIRecordUsageCost(
		context.Background(), result, apiKey, []string{"search-model"},
		3.0, 1.0, 1.0, 2.0, UsageTokens{}, "", false,
	)
	require.NoError(t, err)
	require.InDelta(t, 0.01, cost.TotalCost, 1e-12)
	require.InDelta(t, 0.02, cost.ActualCost, 1e-12)

	free := 0.0
	apiKey.Group.WebSearchPricePerCall = &free
	cost, err = svc.calculateOpenAIRecordUsageCost(
		context.Background(), result, apiKey, []string{"search-model"},
		3.0, 1.0, 1.0, 2.0, UsageTokens{}, "", false,
	)
	require.NoError(t, err)
	require.Zero(t, cost.TotalCost)
	require.Zero(t, cost.ActualCost)
}
