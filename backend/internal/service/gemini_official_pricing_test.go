package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGemini36BillingTimeAndProviderAcrossEntrypoints(t *testing.T) {
	billing := NewBillingService(nil, nil)
	resolver := NewModelPricingResolver(nil, billing)
	eligible, ineligible := true, false
	tokens := UsageTokens{InputTokens: 1000000, OutputTokens: 1000000, CacheReadTokens: 1000000}
	for _, resolved := range []*ModelPricingResolver{nil, resolver} {
		for _, item := range []struct {
			at       time.Time
			eligible *bool
			want     float64
		}{
			{gemini36LaunchOfferEnd.Add(-time.Nanosecond), &eligible, 4.575},
			{gemini36LaunchOfferEnd, &eligible, 9.15},
			{gemini36LaunchOfferEnd.Add(-time.Hour), &ineligible, 9.15},
		} {
			cost, err := billing.CalculateCostUnified(CostInput{Ctx: context.Background(), Model: "gemini-3.6-flash", Tokens: tokens, RateMultiplier: 1, Resolver: resolved, PricingAt: item.at, GeminiLaunchOfferEligible: item.eligible})
			require.NoError(t, err)
			require.InDelta(t, item.want, cost.TotalCost, 1e-10)
			log := &UsageLog{}
			applyGemini36AccountStatsCost(context.Background(), log, nil, billing, &Account{Platform: PlatformGemini}, 1, "gemini-3.6-flash", tokens, 0, item.at, item.eligible)
			require.NotNil(t, log.AccountStatsCost)
			require.InDelta(t, item.want, *log.AccountStatsCost, 1e-10)
		}
	}
}

func TestGemini36PromotionPreservesOverridesAndSharedCatalog(t *testing.T) {
	price := &ModelPricing{InputPricePerToken: .75e-6, OutputPricePerToken: 2e-6, CacheReadPricePerToken: .02e-6, PricingOverrideInput: true, PricingOverrideOutput: true, PricingOverrideCacheRead: true}
	copy := applyGemini36OfficialPricing("gemini-3.6-flash", price, gemini36LaunchOfferEnd)
	require.Equal(t, *price, *copy)
	require.NotSame(t, price, copy)
	copy.OutputPricePerToken = 100
	require.Equal(t, 2e-6, price.OutputPricePerToken)
	require.Same(t, price, applyGemini36OfficialPricing("gemini-2.5-flash", price, gemini36LaunchOfferEnd))
}

type cacheAuthorizationTestRepo struct {
	GeminiCachedContentRepository
	authorization int64
	barrierCalls  int
}

func (r *cacheAuthorizationTestRepo) AuthorizationRevisionAfter(context.Context, int64) (int64, error) {
	return r.authorization, nil
}
func (r *cacheAuthorizationTestRepo) Barrier(context.Context, int64) (bool, error) {
	r.barrierCalls++
	return true, nil
}

func TestGeminiAuthorizationBarrierSkipsResourceEventsAndAccountAuthTTL(t *testing.T) {
	repo := &cacheAuthorizationTestRepo{}
	s := &GeminiCachedContentService{repo: repo, enabled: true, authBarrier: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, s.WaitAuthorizationBarrier(ctx, 10, true))
	require.Zero(t, repo.barrierCalls, "ordinary cache CRUD must not delay unrelated settings")
	repo.authorization = 11
	require.NoError(t, s.WaitAuthorizationBarrier(ctx, 10, false), "account mutations do not wait for API-key auth TTL")
	require.Equal(t, 1, repo.barrierCalls)
}
