package service

import (
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func makeHealthTestAccount(id int64, priority int, loadRate int, pool bool) accountWithLoad {
	credentials := map[string]any{}
	if pool {
		credentials["pool_mode"] = true
	}
	return accountWithLoad{
		account: &Account{
			ID:          id,
			Priority:    priority,
			Type:        AccountTypeAPIKey,
			Credentials: credentials,
			Schedulable: true,
			Status:      StatusActive,
		},
		loadInfo: &AccountLoadInfo{
			AccountID: id,
			LoadRate:  loadRate,
		},
	}
}

func reportHealthSamples(stats *accountRuntimeHealthStats, accountID int64, success bool, ttft *int, samples int) {
	for i := 0; i < samples; i++ {
		stats.report(accountID, success, ttft)
	}
}

func withProbeMultiplier(account accountWithLoad, multiplier float64, freshUntil time.Time) accountWithLoad {
	account.account.Extra = map[string]any{
		UpstreamBillingProbeExtraKey: map[string]any{
			"status":      UpstreamBillingProbeStatusOK,
			"fresh_until": freshUntil,
			"data": map[string]any{
				"effective_rate_multiplier": multiplier,
			},
		},
	}
	return account
}

func withConfiguredMultiplier(account accountWithLoad, multiplier float64) accountWithLoad {
	account.account.RateMultiplier = &multiplier
	return account
}

func TestNormalizeAccountSchedulingStrategyDefaultsToStrict(t *testing.T) {
	require.Equal(t, AccountSchedulingStrategyStrictPriority, NormalizeAccountSchedulingStrategy(""))
	require.Equal(t, AccountSchedulingStrategyStrictPriority, NormalizeAccountSchedulingStrategy("invalid"))
	require.Equal(t, AccountSchedulingStrategyHealthFirst, NormalizeAccountSchedulingStrategy(AccountSchedulingStrategyHealthFirst))
	require.Equal(t, AccountSchedulingStrategyHealthCostBalanced, NormalizeAccountSchedulingStrategy(AccountSchedulingStrategyHealthCostBalanced))
	require.True(t, IsAdaptiveHealthSchedulingStrategy(AccountSchedulingStrategyHealthCostBalanced))
	require.False(t, IsAdaptiveHealthSchedulingStrategy(AccountSchedulingStrategyStrictPriority))
}

func TestAdaptiveTTFTSwitchPolicyDefaultsAndGroupOverride(t *testing.T) {
	legacy := adaptiveTTFTSwitchPolicyForGroup(&Group{})
	require.True(t, legacy.enabled)
	require.Equal(t, 60_000.0, legacy.thresholdMs)

	disabled := adaptiveTTFTSwitchPolicyForGroup(&Group{
		AdaptiveTTFTSwitchEnabled:          false,
		AdaptiveTTFTSwitchThresholdSeconds: 30,
	})
	require.False(t, disabled.enabled)
	require.Equal(t, 30_000.0, disabled.thresholdMs)
}

func TestSelectHealthFirstAccountWithLoad_CrossesPriorityForHealthyAccount(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	slow := 2500
	fast := 100
	reportHealthSamples(stats, 1, false, &slow, 3)
	reportHealthSamples(stats, 2, true, &fast, 3)

	selected := selectHealthFirstAccountWithLoad([]accountWithLoad{
		makeHealthTestAccount(1, 1, 0, true),
		makeHealthTestAccount(2, 10, 0, true),
	}, stats, config.GatewaySchedulingConfig{}, false, time.Now(), 0, false)
	require.NotNil(t, selected)
	require.Equal(t, int64(2), selected.account.ID)
}

func TestSelectHealthFirstAccountWithLoad_LowerDeclaredMultiplierWinsWithinHealth(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 100
	reportHealthSamples(stats, 1, true, &fast, 3)
	reportHealthSamples(stats, 2, true, &fast, 3)
	now := time.Now()
	first := withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 1.5, now.Add(time.Hour))
	second := withProbeMultiplier(makeHealthTestAccount(2, 10, 0, true), 0.5, now.Add(time.Hour))

	selected := selectHealthFirstAccountWithLoad([]accountWithLoad{first, second}, stats, config.GatewaySchedulingConfig{}, false, now, 0, false)
	require.NotNil(t, selected)
	require.Equal(t, int64(2), selected.account.ID)
}

func TestSelectHealthFirstAccountWithLoad_CacheHysteresisKeepsSingleTransientFailure(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 120
	// The cached account has a strong history with one transient failure; the
	// competitor remains healthy but should not evict the active cache yet.
	reportHealthSamples(stats, 1, true, &fast, 3)
	stats.report(1, false, &fast)
	reportHealthSamples(stats, 2, true, &fast, 4)
	now := time.Now()
	selected := selectHealthFirstAccountWithLoadForGroup([]accountWithLoad{
		makeHealthTestAccount(1, 1, 0, true),
		makeHealthTestAccount(2, 10, 0, true),
	}, stats, config.GatewaySchedulingConfig{}, false, now, 77, 1, true)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestSelectHealthFirstAccountWithLoad_ColdStartUsesUpstreamMultiplier(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	now := time.Now()
	cheapLowerPriority := withProbeMultiplier(makeHealthTestAccount(2, 10, 0, true), 0.1, now.Add(time.Hour))
	selected := selectHealthFirstAccountWithLoadForGroup([]accountWithLoad{
		makeHealthTestAccount(1, 1, 0, true),
		cheapLowerPriority,
	}, stats, config.GatewaySchedulingConfig{}, false, now, 88, 0, false)
	require.NotNil(t, selected)
	require.Equal(t, int64(2), selected.account.ID)
}

func TestSelectHealthFirstAccountWithLoad_UsesExpiredLastKnownMultiplier(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 100
	reportHealthSamples(stats, 1, true, &fast, 3)
	reportHealthSamples(stats, 2, true, &fast, 3)
	now := time.Now()
	first := withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 2.0, now.Add(-time.Minute))
	second := withProbeMultiplier(makeHealthTestAccount(2, 10, 0, true), 0.5, now.Add(time.Hour))

	selected := selectHealthFirstAccountWithLoad([]accountWithLoad{first, second}, stats, config.GatewaySchedulingConfig{}, false, now, 0, false)
	require.NotNil(t, selected)
	require.Equal(t, int64(2), selected.account.ID)
}

func TestAccountEffectiveUpstreamMultiplierDoesNotFallBackToLocalRate(t *testing.T) {
	now := time.Now()
	local := 1.0
	account := makeHealthTestAccount(1, 1, 0, true).account
	account.Extra = map[string]any{UpstreamBillingProbeEnabledExtraKey: true}
	account.RateMultiplier = &local

	_, ok := accountEffectiveUpstreamMultiplier(account, now)
	require.False(t, ok)
}

func TestAccountEffectiveUpstreamMultiplierKeepsFreshLastKnownAfterProbeFailure(t *testing.T) {
	now := time.Now()
	account := withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 0.07, now.Add(time.Minute)).account
	account.Extra[UpstreamBillingProbeExtraKey].(map[string]any)["status"] = "failed"

	rate, ok := accountEffectiveUpstreamMultiplier(account, now)
	require.True(t, ok)
	require.Equal(t, 0.07, rate)
}

func TestAccountEffectiveUpstreamMultiplierIgnoresSnapshotWhenProbeDisabled(t *testing.T) {
	now := time.Now()
	account := withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 0.07, now.Add(-time.Minute)).account
	account.Extra[UpstreamBillingProbeEnabledExtraKey] = false
	_, ok := accountEffectiveUpstreamMultiplier(account, now)
	require.False(t, ok)
}

func TestAccountEffectiveUpstreamMultiplierUsesCompactSchedulerObservation(t *testing.T) {
	observed := 1.6
	account := makeHealthTestAccount(1, 1, 0, true).account
	account.UpstreamBillingGuardObservedMultiplier = &observed
	account.Extra = map[string]any{
		UpstreamBillingProbeEnabledExtraKey:      true,
		AdaptiveUpstreamMultiplierFactorExtraKey: 0.1,
	}

	rate, ok := accountEffectiveUpstreamMultiplier(account, time.Now())
	require.True(t, ok)
	require.InDelta(t, 0.16, rate, 1e-9)

	account.Extra[UpstreamBillingProbeEnabledExtraKey] = false
	_, ok = accountEffectiveUpstreamMultiplier(account, time.Now())
	require.False(t, ok, "a stale compact observation must not survive probe disablement")
}

func TestAccountEffectiveUpstreamMultiplierAppliesAdaptiveFactorOnlyToSchedulingValue(t *testing.T) {
	now := time.Now()
	account := withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 1.6, now.Add(time.Minute)).account
	account.Extra[AdaptiveUpstreamMultiplierFactorExtraKey] = 0.1

	rate, ok := accountEffectiveUpstreamMultiplier(account, now)
	require.True(t, ok)
	require.InDelta(t, 0.16, rate, 1e-9)
	require.Equal(t, 1.6, account.Extra[UpstreamBillingProbeExtraKey].(map[string]any)["data"].(map[string]any)["effective_rate_multiplier"])
}

func TestAccountAdaptiveUpstreamMultiplierFactorDefaultsSafely(t *testing.T) {
	account := makeHealthTestAccount(1, 1, 0, true).account
	account.Extra = map[string]any{}
	for _, value := range []any{nil, 0, 100.1, "invalid", math.Inf(1)} {
		if value == nil {
			delete(account.Extra, AdaptiveUpstreamMultiplierFactorExtraKey)
		} else {
			account.Extra[AdaptiveUpstreamMultiplierFactorExtraKey] = value
		}
		require.Equal(t, float64(1), accountAdaptiveUpstreamMultiplierFactor(account))
	}
}

func TestHealthCostBalancedKeepsUnknownHealthCheapestUpstreamEligible(t *testing.T) {
	now := time.Now()
	stats := newAccountRuntimeHealthStats()
	knownTTFT := 250
	reportHealthSamples(stats, 2, true, &knownTTFT, 3)
	cheapUnknown := withProbeMultiplier(makeHealthTestAccount(1, 5, 0, true), 0.07, now.Add(time.Hour))
	expensiveKnown := withProbeMultiplier(makeHealthTestAccount(2, 1, 0, true), 0.2, now.Add(time.Hour))

	selected := selectAdaptiveAccountWithLoadForGroupStrategy([]accountWithLoad{expensiveKnown, cheapUnknown}, stats, config.GatewaySchedulingConfig{}, false, now, 114, 0, false, AccountSchedulingStrategyHealthCostBalanced)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestAdaptiveCoveragePrefersTheAccountWithTheLargestSampleDebt(t *testing.T) {
	now := time.Now()
	stats := newOpenAIAccountRuntimeStats()
	preferred := map[int64]struct{}{1: {}, 2: {}}
	first := openAIAccountCandidateScore{
		account:     makeHealthTestAccount(1, 1, 0, true).account,
		sampleCount: 0,
	}
	second := openAIAccountCandidateScore{
		account:     makeHealthTestAccount(2, 1, 0, true).account,
		sampleCount: 2,
	}

	selected, ok := selectOpenAIUnknownAdaptiveCandidate(
		[]openAIAccountCandidateScore{second, first},
		preferred,
		stats,
		now,
		901,
		0,
		false,
		true,
	)
	require.True(t, ok)
	require.Equal(t, int64(1), selected)
}

func TestHealthCostBalancedDoesNotEscalateForInsufficientTTFTSamples(t *testing.T) {
	for _, sampleCount := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d samples", sampleCount), func(t *testing.T) {
			now := time.Now()
			stats := newAccountRuntimeHealthStats()
			slow := 60_000
			fast := 100
			reportHealthSamples(stats, 1, true, &slow, sampleCount)
			reportHealthSamples(stats, 2, true, &fast, int(accountHealthUnknownMinSamples))
			cheap := withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 1.6, now.Add(time.Hour))
			cheap.account.Extra[AdaptiveUpstreamMultiplierFactorExtraKey] = 0.1
			expensive := withProbeMultiplier(makeHealthTestAccount(2, 1, 0, true), 0.2, now.Add(time.Hour))

			selected := selectAdaptiveAccountWithLoadForGroupStrategy(
				[]accountWithLoad{expensive, cheap},
				stats,
				config.GatewaySchedulingConfig{},
				false,
				now,
				120+int64(sampleCount),
				0,
				false,
				AccountSchedulingStrategyHealthCostBalanced,
			)
			require.NotNil(t, selected)
			require.Equal(t, int64(1), selected.account.ID)
		})
	}
}

func TestHealthCostBalancedKeepsCheapestAccountWithinSameTTFTBand(t *testing.T) {
	now := time.Now()
	stats := newAccountRuntimeHealthStats()
	low := 10_000
	high := 14_000
	reportHealthSamples(stats, 1, true, &low, int(accountHealthUnknownMinSamples))
	reportHealthSamples(stats, 2, true, &high, int(accountHealthUnknownMinSamples))
	cheap := withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 0.16, now.Add(time.Hour))
	expensive := withProbeMultiplier(makeHealthTestAccount(2, 1, 0, true), 0.2, now.Add(time.Hour))

	selected := selectAdaptiveAccountWithLoadForGroupStrategy(
		[]accountWithLoad{expensive, cheap},
		stats,
		config.GatewaySchedulingConfig{},
		false,
		now,
		123,
		0,
		false,
		AccountSchedulingStrategyHealthCostBalanced,
	)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestHealthFirstDoesNotUseInsufficientTTFTSamplesAgainstCheaperAccount(t *testing.T) {
	for _, ttftSampleCount := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d ttft samples", ttftSampleCount), func(t *testing.T) {
			now := time.Now()
			stats := newAccountRuntimeHealthStats()
			slow := 60_000
			fast := 100
			reportHealthSamples(stats, 1, true, &slow, ttftSampleCount)
			reportHealthSamples(stats, 1, true, nil, int(accountHealthUnknownMinSamples)-ttftSampleCount)
			reportHealthSamples(stats, 2, true, &fast, int(accountHealthUnknownMinSamples))
			cheap := withProbeMultiplier(makeHealthTestAccount(1, 10, 0, true), 1.6, now.Add(time.Hour))
			cheap.account.Extra[AdaptiveUpstreamMultiplierFactorExtraKey] = 0.1
			expensive := withProbeMultiplier(makeHealthTestAccount(2, 1, 0, true), 0.2, now.Add(time.Hour))

			selected := selectAdaptiveAccountWithLoadForGroupStrategy(
				[]accountWithLoad{expensive, cheap},
				stats,
				config.GatewaySchedulingConfig{},
				false,
				now,
				130+int64(ttftSampleCount),
				0,
				false,
				AccountSchedulingStrategyHealthFirst,
			)
			require.NotNil(t, selected)
			require.Equal(t, cheap.account.ID, selected.account.ID)
		})
	}
}

func TestAdaptivePolicyTierDoesNotUseAvailableHigherCostWhileCheapTierIsFull(t *testing.T) {
	now := time.Now()
	stats := newAccountRuntimeHealthStats()
	ttft := 200
	reportHealthSamples(stats, 1, true, &ttft, 3)
	reportHealthSamples(stats, 2, true, &ttft, 3)
	cheapFull := withProbeMultiplier(makeHealthTestAccount(1, 1, 100, true), 0.07, now.Add(time.Hour))
	expensiveAvailable := withProbeMultiplier(makeHealthTestAccount(2, 1, 0, true), 0.2, now.Add(time.Hour))

	filtered := filterAdaptiveAccountsToPolicyTier(
		[]accountWithLoad{cheapFull, expensiveAvailable},
		[]accountWithLoad{expensiveAvailable},
		stats,
		AccountSchedulingStrategyHealthCostBalanced,
		now,
		nil,
	)
	require.Empty(t, filtered)
}

func TestAdaptiveHistoryKeepsP90AfterRuntimeWarmup(t *testing.T) {
	now := time.Now()
	stats := newAccountRuntimeHealthStats()
	runtimeTTFT := 300
	reportHealthSamples(stats, 1, true, &runtimeTTFT, 3)
	items := []accountWithLoad{makeHealthTestAccount(1, 1, 0, true)}
	candidates := buildAccountHealthCandidatesWithHistory(items, stats, map[int64]AccountTTFTHistory{
		1: {AccountID: 1, SampleCount: 20, P50Ms: 800, P90Ms: 1400, LatestAt: now},
	})
	require.Len(t, candidates, 1)
	require.Equal(t, float64(runtimeTTFT), candidates[0].ttft)
	require.Equal(t, 1400.0, candidates[0].ttftP90)
}

func TestAdaptiveSeriouslySlowRequiresTrustedSamplesForEntireTier(t *testing.T) {
	profiles := []adaptiveAccountHealthProfile{
		{account: &Account{ID: 1}, p50: 60_000, p90: 70_000, hasTTFT: true, ttftSamples: 2},
		{account: &Account{ID: 2}, p50: 100, p90: 150, hasTTFT: true, ttftSamples: 3},
	}
	require.False(t, adaptiveTierIsSeriouslySlow(profiles, []int{0}, []int{0, 1}))
}

func TestAdaptiveTTFTBands(t *testing.T) {
	tests := []struct {
		ms   float64
		band int
	}{
		{ms: 0, band: 0},
		{ms: 15_000, band: 0},
		{ms: 15_001, band: 1},
		{ms: 30_000, band: 1},
		{ms: 30_001, band: 2},
		{ms: 60_000, band: 2},
		{ms: 60_001, band: 3},
	}
	for _, test := range tests {
		require.Equal(t, test.band, adaptiveTTFTBand(test.ms))
	}
}

func TestAdaptiveTTFTUsesP90OnlyWhenP50IsInSameBand(t *testing.T) {
	require.False(t, adaptiveTTFTMateriallySlowerThan(10_000, 70_000, 20_000, 20_000))
	require.True(t, adaptiveTTFTMateriallySlowerThan(20_000, 20_000, 10_000, 70_000))
	require.True(t, adaptiveTTFTMateriallySlowerThan(10_000, 70_000, 12_000, 10_000))
}

func TestFilterByAccountHealthBand_PrefersHealthBeforeLoadInsideSameLayer(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 120
	slow := 2200
	reportHealthSamples(stats, 1, false, &slow, 3)
	reportHealthSamples(stats, 2, true, &fast, 3)

	candidates := []accountWithLoad{
		makeHealthTestAccount(1, 1, 0, true),
		makeHealthTestAccount(2, 1, 60, true),
	}

	filtered := filterByAccountHealthBand(candidates, stats)
	require.Len(t, filtered, 1)
	require.Equal(t, int64(2), filtered[0].account.ID)
}

func TestLayeredSelectionHealthBandDoesNotCrossPriority(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 100
	slow := 2000
	reportHealthSamples(stats, 1, false, &slow, 3)
	reportHealthSamples(stats, 2, true, &fast, 3)

	accounts := []accountWithLoad{
		makeHealthTestAccount(1, 1, 0, true),
		makeHealthTestAccount(2, 100, 0, true),
	}

	step1 := filterByMinPriority(accounts)
	require.Len(t, step1, 1)
	step2 := filterByAccountHealthBand(step1, stats)
	require.Len(t, step2, 1)
	require.Equal(t, int64(1), step2[0].account.ID)
}

func TestLayeredSelectionHealthBandKeepsNonPoolBeforePool(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 90
	slow := 2400
	reportHealthSamples(stats, 1, false, &slow, 3)
	reportHealthSamples(stats, 2, true, &fast, 3)

	accounts := []accountWithLoad{
		makeHealthTestAccount(1, 1, 0, false),
		makeHealthTestAccount(2, 1, 0, true),
	}

	step1 := filterByMinPriority(accounts)
	step2 := filterByNonPoolModeIfPresent(step1)
	require.Len(t, step2, 1)
	require.Equal(t, int64(1), step2[0].account.ID)

	step3 := filterByAccountHealthBand(step2, stats)
	require.Len(t, step3, 1)
	require.Equal(t, int64(1), step3[0].account.ID)
}

func TestSelectLayeredAccountWithLoad_UsesHealthBand(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 120
	slow := 2600
	reportHealthSamples(stats, 1, true, &slow, 3)
	reportHealthSamples(stats, 2, true, &fast, 3)

	selected := selectLayeredAccountWithLoad([]accountWithLoad{
		makeHealthTestAccount(1, 1, 0, true),
		makeHealthTestAccount(2, 1, 70, true),
	}, stats, config.GatewaySchedulingConfig{}, false, time.Now())

	require.NotNil(t, selected)
	require.Equal(t, int64(2), selected.account.ID)
}

func TestSelectLayeredAccountWithLoad_HealthDoesNotCrossPriority(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 120
	slow := 2600
	reportHealthSamples(stats, 1, true, &slow, 3)
	reportHealthSamples(stats, 2, true, &fast, 3)

	selected := selectLayeredAccountWithLoad([]accountWithLoad{
		makeHealthTestAccount(1, 1, 0, true),
		makeHealthTestAccount(2, 100, 0, true),
	}, stats, config.GatewaySchedulingConfig{}, false, time.Now())

	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestSelectLayeredAccountWithLoad_SimilarHealthUsesLRU(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	ttftA := 100
	ttftB := 130
	reportHealthSamples(stats, 1, true, &ttftA, 3)
	reportHealthSamples(stats, 2, true, &ttftB, 3)

	now := time.Now()
	earlier := now.Add(-time.Hour)
	candidates := []accountWithLoad{
		{
			account:  &Account{ID: 1, Priority: 1, Type: AccountTypeAPIKey, LastUsedAt: &earlier},
			loadInfo: &AccountLoadInfo{AccountID: 1, LoadRate: 40},
		},
		{
			account:  &Account{ID: 2, Priority: 1, Type: AccountTypeAPIKey, LastUsedAt: &now},
			loadInfo: &AccountLoadInfo{AccountID: 2, LoadRate: 0},
		},
	}

	filtered := filterByAccountHealthBand(candidates, stats)
	require.Len(t, filtered, 2)
	selected := selectLayeredAccountWithLoad(candidates, stats, config.GatewaySchedulingConfig{}, false, now)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestSelectLayeredAccountWithLoad_SecondsLevelSimilarHealthUsesLRU(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	faster := 3000
	slower := 4000
	reportHealthSamples(stats, 1, true, &faster, 3)
	reportHealthSamples(stats, 2, true, &slower, 3)

	now := time.Now()
	earlier := now.Add(-time.Hour)
	candidates := []accountWithLoad{
		{account: &Account{ID: 1, Priority: 1, Type: AccountTypeAPIKey, LastUsedAt: &earlier}, loadInfo: &AccountLoadInfo{AccountID: 1, LoadRate: 80}},
		{account: &Account{ID: 2, Priority: 1, Type: AccountTypeAPIKey, LastUsedAt: &now}, loadInfo: &AccountLoadInfo{AccountID: 2, LoadRate: 0}},
	}

	filtered := filterByAccountHealthBand(candidates, stats)
	require.Len(t, filtered, 2)
	selected := selectLayeredAccountWithLoad(candidates, stats, config.GatewaySchedulingConfig{}, false, now)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestSelectLayeredAccountWithLoad_TensOfSecondsStillLosesToHealthy(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 3000
	verySlow := 10000
	reportHealthSamples(stats, 1, true, &fast, 3)
	reportHealthSamples(stats, 2, true, &verySlow, 3)

	now := time.Now()
	candidates := []accountWithLoad{
		{account: &Account{ID: 1, Priority: 1, Type: AccountTypeAPIKey}, loadInfo: &AccountLoadInfo{AccountID: 1, LoadRate: 80}},
		{account: &Account{ID: 2, Priority: 1, Type: AccountTypeAPIKey}, loadInfo: &AccountLoadInfo{AccountID: 2, LoadRate: 0}},
	}

	filtered := filterByAccountHealthBand(candidates, stats)
	require.Len(t, filtered, 1)
	require.Equal(t, int64(1), filtered[0].account.ID)
	selected := selectLayeredAccountWithLoad(candidates, stats, config.GatewaySchedulingConfig{}, false, now)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestAccountRuntimeHealthScore_NoTTFTDoesNotExceedUnknown(t *testing.T) {
	require.Equal(t, accountHealthUnknownScore, accountRuntimeHealthScore(0, 0, false, 0, 0, false))
	require.Equal(t, accountHealthUnknownScore, accountRuntimeHealthScore(0.01, 0, false, 0, 0, false))
	require.Less(t, accountRuntimeHealthScore(0.5, 0, false, 0, 0, false), accountHealthUnknownScore)
}

func TestSelectLayeredAccountWithLoad_DoesNotExploreUnknownInMainPath(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 120
	reportHealthSamples(stats, 1, true, &fast, 3)
	now := time.Now()
	candidates := []accountWithLoad{
		{account: &Account{ID: 1, Priority: 1, Type: AccountTypeAPIKey}, loadInfo: &AccountLoadInfo{AccountID: 1, LoadRate: 0}},
		{account: &Account{ID: 2, Priority: 1, Type: AccountTypeAPIKey}, loadInfo: &AccountLoadInfo{AccountID: 2, LoadRate: 0}},
	}

	for i := 0; i < int(accountHealthUnknownExploreEvery)-1; i++ {
		selected := selectLayeredAccountWithLoad(candidates, stats, config.GatewaySchedulingConfig{}, false, now)
		require.NotNil(t, selected)
		require.Equal(t, int64(1), selected.account.ID)
	}
	selected := selectLayeredAccountWithLoad(candidates, stats, config.GatewaySchedulingConfig{}, false, now)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestSelectLayeredAccountWithLoad_BusinessSamplesWithoutTTFTAreKnown(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 120
	reportHealthSamples(stats, 1, true, &fast, 3)
	reportHealthSamples(stats, 2, true, nil, 3)

	_, _, hasTTFT, found, sampleCount, ttftSampleCount, _ := stats.snapshotWithMeta(2)
	require.True(t, found)
	require.False(t, hasTTFT)
	require.Equal(t, int64(3), sampleCount)
	require.Equal(t, int64(0), ttftSampleCount)

	now := time.Now()
	candidates := []accountWithLoad{
		{account: &Account{ID: 1, Priority: 1, Type: AccountTypeAPIKey}, loadInfo: &AccountLoadInfo{AccountID: 1, LoadRate: 0}},
		{account: &Account{ID: 2, Priority: 1, Type: AccountTypeAPIKey}, loadInfo: &AccountLoadInfo{AccountID: 2, LoadRate: 0}},
	}
	unknownOnly := buildAccountHealthCandidates(candidates[1:], stats)
	require.True(t, hasKnownAccountHealthSample(unknownOnly))

	stats.selectionCounter.Store(accountHealthUnknownExploreEvery - 1)
	selected := selectLayeredAccountWithLoad(candidates, stats, config.GatewaySchedulingConfig{}, false, now)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestAdaptiveStrategiesPreferExactLowestCostWithinSameTTFTBand(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	ttftA, ttftB, ttftC := 500, 200, 100
	reportHealthSamples(stats, 1, true, &ttftA, 3)
	reportHealthSamples(stats, 2, true, &ttftB, 3)
	reportHealthSamples(stats, 3, true, &ttftC, 3)
	now := time.Now()
	a := withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 0.5, now.Add(time.Hour))
	b := withProbeMultiplier(makeHealthTestAccount(2, 10, 0, true), 0.6, now.Add(time.Hour))
	c := withProbeMultiplier(makeHealthTestAccount(3, 20, 0, true), 1.5, now.Add(time.Hour))

	selected := selectAdaptiveAccountWithLoadForGroupStrategy([]accountWithLoad{a, b, c}, stats, config.GatewaySchedulingConfig{}, false, now, 101, 0, false, AccountSchedulingStrategyHealthCostBalanced)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)

	selectedLeading := selectAdaptiveAccountWithLoadForGroupStrategy([]accountWithLoad{a, b, c}, stats, config.GatewaySchedulingConfig{}, false, now, 102, 0, false, AccountSchedulingStrategyHealthFirst)
	require.NotNil(t, selectedLeading)
	require.Equal(t, int64(1), selectedLeading.account.ID)
}

func TestHealthCostBalancedEscalatesWhenCheapestTierIsSeriouslySlow(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	verySlow, fast := 60_000, 300
	reportHealthSamples(stats, 1, true, &verySlow, 3)
	reportHealthSamples(stats, 2, true, &fast, 3)
	now := time.Now()
	cheap := withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 0.5, now.Add(time.Hour))
	usable := withProbeMultiplier(makeHealthTestAccount(2, 2, 0, true), 0.7, now.Add(time.Hour))
	selected := selectAdaptiveAccountWithLoadForGroupStrategy([]accountWithLoad{cheap, usable}, stats, config.GatewaySchedulingConfig{}, false, now, 106, 0, false, AccountSchedulingStrategyHealthCostBalanced)
	require.NotNil(t, selected)
	require.Equal(t, int64(2), selected.account.ID)
}

func TestHealthCostBalancedTreatsSustainedModerateRatioWithLargeGapAsSeriouslySlow(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	slow, fast := 30_000, 1000
	reportHealthSamples(stats, 1, true, &slow, 3)
	reportHealthSamples(stats, 2, true, &fast, 3)
	now := time.Now()
	cheap := withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 0.5, now.Add(time.Hour))
	usable := withProbeMultiplier(makeHealthTestAccount(2, 2, 0, true), 0.7, now.Add(time.Hour))
	selected := selectAdaptiveAccountWithLoadForGroupStrategyWithPolicyAndHistory(
		[]accountWithLoad{cheap, usable}, stats, config.GatewaySchedulingConfig{}, false, now, 112, 0, false,
		AccountSchedulingStrategyHealthCostBalanced, true, nil,
		adaptiveTTFTSwitchPolicyFromValues(true, 30),
	)
	require.NotNil(t, selected)
	require.Equal(t, int64(2), selected.account.ID)
}

func TestHealthCostBalancedTTFTSwitchUsesConfiguredThresholdAndExactTiers(t *testing.T) {
	now := time.Now()
	makeProfile := func(id int64, rate, p50 float64, samples int64) adaptiveAccountHealthProfile {
		item := withProbeMultiplier(makeHealthTestAccount(id, 1, 0, true), rate, now.Add(time.Hour))
		return adaptiveAccountHealthProfile{account: item.account, p50: p50, hasTTFT: samples > 0, ttftSamples: samples, errorSamples: 3}
	}

	t.Run("default 60 seconds keeps a usable cheap tier", func(t *testing.T) {
		profiles := []adaptiveAccountHealthProfile{
			makeProfile(1, 0.05, 45_000, 3),
			makeProfile(2, 0.06, 5_000, 3),
		}
		indexes := adaptiveAccountPolicyIndexesWithTTFTPolicy(profiles, true, now, defaultAdaptiveTTFTSwitchPolicy())
		require.Equal(t, []int{0}, indexes)
	})

	t.Run("custom 30 seconds advances to the next unknown exact tier", func(t *testing.T) {
		profiles := []adaptiveAccountHealthProfile{
			makeProfile(1, 0.05, 45_000, 3),
			makeProfile(2, 0.06, 0, 0),
			makeProfile(3, 0.07, 5_000, 3),
		}
		indexes := adaptiveAccountPolicyIndexesWithTTFTPolicy(profiles, true, now, adaptiveTTFTSwitchPolicyFromValues(true, 30))
		require.Equal(t, []int{1}, indexes)
	})

	t.Run("known equally slow expensive tier is skipped for a useful tier", func(t *testing.T) {
		profiles := []adaptiveAccountHealthProfile{
			makeProfile(1, 0.05, 70_000, 3),
			makeProfile(2, 0.06, 68_000, 3),
			makeProfile(3, 0.07, 20_000, 3),
			makeProfile(4, 0.16, 5_000, 3),
		}
		indexes := adaptiveAccountPolicyIndexesWithTTFTPolicy(profiles, true, now, defaultAdaptiveTTFTSwitchPolicy())
		require.Equal(t, []int{2}, indexes)
	})

	t.Run("disabled switch always keeps the cheapest exact tier", func(t *testing.T) {
		profiles := []adaptiveAccountHealthProfile{
			makeProfile(1, 0.05, 120_000, 3),
			makeProfile(2, 0.06, 5_000, 3),
		}
		indexes := adaptiveAccountPolicyIndexesWithTTFTPolicy(profiles, true, now, adaptiveTTFTSwitchPolicyFromValues(false, 60))
		require.Equal(t, []int{0}, indexes)
	})
}

func TestHealthFirstTTFTThresholdDoesNotPreferSpeedBelowLimit(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	now := time.Now()
	cheapTTFT, expensiveTTFT := 45_000, 5_000
	reportHealthSamples(stats, 1, true, &cheapTTFT, 3)
	reportHealthSamples(stats, 2, true, &expensiveTTFT, 3)
	cheap := withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 0.05, now.Add(time.Hour))
	expensive := withProbeMultiplier(makeHealthTestAccount(2, 1, 0, true), 0.20, now.Add(time.Hour))

	selected := selectAdaptiveAccountWithLoadForGroupStrategyWithPolicyAndHistory(
		[]accountWithLoad{expensive, cheap}, stats, config.GatewaySchedulingConfig{}, false, now, 140, 0, false,
		AccountSchedulingStrategyHealthFirst, true, nil, defaultAdaptiveTTFTSwitchPolicy(),
	)
	require.NotNil(t, selected)
	require.Equal(t, cheap.account.ID, selected.account.ID)

	selected = selectAdaptiveAccountWithLoadForGroupStrategyWithPolicyAndHistory(
		[]accountWithLoad{expensive, cheap}, stats, config.GatewaySchedulingConfig{}, false, now, 141, 0, false,
		AccountSchedulingStrategyHealthFirst, true, nil, adaptiveTTFTSwitchPolicyFromValues(true, 30),
	)
	require.NotNil(t, selected)
	require.Equal(t, expensive.account.ID, selected.account.ID)
}

func TestAdaptiveHealthFirstSelectionWithIncompleteHealthIsStable(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	ttft := 500
	reportHealthSamples(stats, 1, true, &ttft, 3)
	stats.report(2, true, &ttft)
	stats.report(2, false, &ttft)
	stats.report(2, true, &ttft)
	reportHealthSamples(stats, 3, true, &ttft, 1)
	now := time.Now()
	accounts := map[int64]accountWithLoad{
		1: withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 0.20, now.Add(time.Hour)),
		2: withProbeMultiplier(makeHealthTestAccount(2, 1, 0, true), 0.05, now.Add(time.Hour)),
		3: withProbeMultiplier(makeHealthTestAccount(3, 1, 0, true), 0.10, now.Add(time.Hour)),
	}
	permutations := [][]int64{
		{1, 2, 3}, {1, 3, 2}, {2, 1, 3},
		{2, 3, 1}, {3, 1, 2}, {3, 2, 1},
	}

	for _, order := range permutations {
		candidates := []accountWithLoad{accounts[order[0]], accounts[order[1]], accounts[order[2]]}
		selected := selectAdaptiveAccountWithLoadForGroupStrategyWithPolicyAndHistory(
			candidates, stats, config.GatewaySchedulingConfig{}, false, now, 142, 0, false,
			AccountSchedulingStrategyHealthFirst, false, nil, defaultAdaptiveTTFTSwitchPolicy(),
		)
		require.NotNil(t, selected)
		// Account 3 has incomplete evidence and is therefore default-healthy. It
		// ties account 1 on health and wins their multiplier tie-break, while the
		// known 16% error account must not become health-first merely by being cheap.
		require.Equal(t, int64(3), selected.account.ID, "input order %v", order)
	}
}

func TestAdaptiveSelectionUsesLastKnownUpstreamMultiplierAfterExpiry(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	ttft := 200
	reportHealthSamples(stats, 1, true, &ttft, 3)
	reportHealthSamples(stats, 2, true, &ttft, 3)
	now := time.Now()
	localOneA := withConfiguredMultiplier(makeHealthTestAccount(1, 1, 0, true), 1)
	localOneB := withConfiguredMultiplier(makeHealthTestAccount(2, 2, 0, true), 1)
	upstreamCheap := withProbeMultiplier(localOneA, 0.07, now.Add(time.Hour))
	upstreamExpensive := withProbeMultiplier(localOneB, 0.2, now.Add(time.Hour))
	selected := selectAdaptiveAccountWithLoadForGroupStrategy([]accountWithLoad{upstreamExpensive, upstreamCheap}, stats, config.GatewaySchedulingConfig{}, false, now, 107, 0, false, AccountSchedulingStrategyHealthCostBalanced)
	require.Equal(t, int64(1), selected.account.ID)

	observedExpensive := withProbeMultiplier(localOneA, 2.0, now.Add(time.Hour))
	selected = selectAdaptiveAccountWithLoadForGroupStrategy([]accountWithLoad{observedExpensive, upstreamExpensive}, stats, config.GatewaySchedulingConfig{}, false, now, 108, 0, false, AccountSchedulingStrategyHealthCostBalanced)
	require.Equal(t, int64(2), selected.account.ID)

	// An expired but previously trusted low multiplier remains part of cost
	// ordering; it must not disappear merely because the next probe is due.
	expiredCheap := withProbeMultiplier(localOneA, 0.07, now.Add(-time.Minute)).account
	expiredExpensive := withProbeMultiplier(localOneB, 0.2, now.Add(time.Hour)).account
	rate, known := accountEffectiveUpstreamMultiplier(expiredCheap, now)
	require.True(t, known)
	require.Equal(t, 0.07, rate)
	selected = selectAdaptiveAccountWithLoadForGroupStrategy([]accountWithLoad{{account: expiredExpensive}, {account: expiredCheap}}, stats, config.GatewaySchedulingConfig{}, false, now, 1081, 0, false, AccountSchedulingStrategyHealthCostBalanced)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestAdaptiveSelectionHistoryMakesAccountKnownAndBlocksStickyCostEscape(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	now := time.Now()
	history := map[int64]AccountTTFTHistory{
		1: {AccountID: 1, SampleCount: 12, P50Ms: 500, P90Ms: 800, LatestAt: now},
		2: {AccountID: 2, SampleCount: 12, P50Ms: 100, P90Ms: 180, LatestAt: now},
	}
	cheap := withProbeMultiplier(makeHealthTestAccount(1, 5, 0, true), 0.5, now.Add(time.Hour))
	expensiveSticky := withProbeMultiplier(makeHealthTestAccount(2, 1, 0, true), 1.5, now.Add(time.Hour))
	selected := selectAdaptiveAccountWithLoadForGroupStrategyWithHistory([]accountWithLoad{cheap, expensiveSticky}, stats, config.GatewaySchedulingConfig{}, false, now, 109, 2, true, AccountSchedulingStrategyHealthCostBalanced, history)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestAdaptiveSelectionReturnsToCheaperStickyAfterMultiplierExpiry(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	now := time.Now()
	ttft := 200
	reportHealthSamples(stats, 1, true, &ttft, 3)
	reportHealthSamples(stats, 2, true, &ttft, 3)
	cheap := withProbeMultiplier(makeHealthTestAccount(1, 5, 0, true), 0.07, now.Add(-time.Minute))
	expensiveSticky := withProbeMultiplier(makeHealthTestAccount(2, 1, 0, true), 0.12, now.Add(time.Hour))
	selected := selectAdaptiveAccountWithLoadForGroupStrategy(
		[]accountWithLoad{expensiveSticky, cheap},
		stats,
		config.GatewaySchedulingConfig{},
		false,
		now,
		116,
		expensiveSticky.account.ID,
		true,
		AccountSchedulingStrategyHealthCostBalanced,
	)
	require.NotNil(t, selected)
	require.Equal(t, cheap.account.ID, selected.account.ID)
}

func TestHealthCostBalancedKeepsExactMultiplierPriority(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	now := time.Now()
	ttft := 200
	for _, accountID := range []int64{1, 2, 3} {
		reportHealthSamples(stats, accountID, true, &ttft, 3)
	}
	best := withProbeMultiplier(makeHealthTestAccount(1, 10, 0, true), 0.05, now.Add(time.Hour))
	second := withProbeMultiplier(makeHealthTestAccount(2, 1, 0, true), 0.06, now.Add(time.Hour))
	third := withProbeMultiplier(makeHealthTestAccount(3, 1, 0, true), 0.07, now.Add(time.Hour))
	selected := selectAdaptiveAccountWithLoadForGroupStrategy(
		[]accountWithLoad{third, second, best},
		stats,
		config.GatewaySchedulingConfig{},
		false,
		now,
		117,
		0,
		false,
		AccountSchedulingStrategyHealthCostBalanced,
	)
	require.NotNil(t, selected)
	require.Equal(t, best.account.ID, selected.account.ID)
}

func TestHealthFirstKeepsStickyWithinCostBandAgainstUnknown(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	now := time.Now()
	cheapUnknown := withProbeMultiplier(makeHealthTestAccount(1141, 1, 0, true), 0.07, now.Add(time.Hour))
	stickyMeasured := withProbeMultiplier(makeHealthTestAccount(1142, 1, 0, true), 0.08, now.Add(time.Hour))
	ttft := 150
	reportHealthSamples(stats, stickyMeasured.account.ID, true, &ttft, int(accountHealthUnknownMinSamples))

	selected := selectAdaptiveAccountWithLoadForGroupStrategy(
		[]accountWithLoad{stickyMeasured, cheapUnknown},
		stats,
		config.GatewaySchedulingConfig{},
		false,
		now,
		114,
		stickyMeasured.account.ID,
		true,
		AccountSchedulingStrategyHealthFirst,
	)
	require.NotNil(t, selected)
	require.Equal(t, stickyMeasured.account.ID, selected.account.ID)
}

func TestHealthFirstDoesNotKeepStickyAccountAfterItBecomesUnhealthy(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	now := time.Now()
	sticky := withProbeMultiplier(makeHealthTestAccount(1143, 1, 0, true), 0.08, now.Add(time.Hour))
	healthy := withProbeMultiplier(makeHealthTestAccount(1144, 1, 0, true), 0.08, now.Add(time.Hour))
	fast := 150
	reportHealthSamples(stats, healthy.account.ID, true, &fast, int(accountHealthUnknownMinSamples))
	reportHealthSamples(stats, sticky.account.ID, false, nil, int(accountHealthUnknownMinSamples))

	selected := selectAdaptiveAccountWithLoadForGroupStrategy(
		[]accountWithLoad{sticky, healthy},
		stats,
		config.GatewaySchedulingConfig{},
		false,
		now,
		115,
		sticky.account.ID,
		true,
		AccountSchedulingStrategyHealthFirst,
	)
	require.NotNil(t, selected)
	require.Equal(t, healthy.account.ID, selected.account.ID)
}

func TestAdaptiveSameHealthFirstCostBand(t *testing.T) {
	require.True(t, adaptiveSameHealthFirstCostBand(0, 0))
	require.False(t, adaptiveSameHealthFirstCostBand(0, 0.01))
	require.True(t, adaptiveSameHealthFirstCostBand(0.07, 0.08))
	require.False(t, adaptiveSameHealthFirstCostBand(0.07, 0.2))
}

func TestAdaptiveSelectionHistoryDoesNotBreakStickyFromP90Alone(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	now := time.Now()
	history := map[int64]AccountTTFTHistory{
		1: {AccountID: 1, SampleCount: 12, P50Ms: 300, P90Ms: 70_000, LatestAt: now},
		2: {AccountID: 2, SampleCount: 12, P50Ms: 350, P90Ms: 500, LatestAt: now},
	}
	sticky := withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 0.5, now.Add(time.Hour))
	stable := withProbeMultiplier(makeHealthTestAccount(2, 2, 0, true), 0.5, now.Add(time.Hour))
	selected := selectAdaptiveAccountWithLoadForGroupStrategyWithHistory([]accountWithLoad{sticky, stable}, stats, config.GatewaySchedulingConfig{}, false, now, 113, 1, true, AccountSchedulingStrategyHealthFirst, history)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestAdaptiveSelectionHistoryKeepsStickyAccountWithInsufficientTTFTSamples(t *testing.T) {
	now := time.Now()
	sticky := withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 0.16, now.Add(time.Hour))
	healthy := withProbeMultiplier(makeHealthTestAccount(2, 1, 0, true), 0.16, now.Add(time.Hour))
	history := map[int64]AccountTTFTHistory{
		1: {AccountID: 1, SampleCount: 2, P50Ms: 60_000, P90Ms: 60_000, LatestAt: now},
		2: {AccountID: 2, SampleCount: 3, P50Ms: 100, P90Ms: 150, LatestAt: now},
	}

	selected := selectAdaptiveAccountWithLoadForGroupStrategyWithHistory(
		[]accountWithLoad{sticky, healthy},
		newAccountRuntimeHealthStats(),
		config.GatewaySchedulingConfig{},
		false,
		now,
		124,
		sticky.account.ID,
		true,
		AccountSchedulingStrategyHealthFirst,
		history,
	)
	require.NotNil(t, selected)
	require.Equal(t, sticky.account.ID, selected.account.ID)
}

func TestAdaptiveHistoryDoesNotLeakIntoStrictPrioritySelection(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	now := time.Now()
	first := withConfiguredMultiplier(makeHealthTestAccount(1, 1, 0, true), 1.0)
	second := withConfiguredMultiplier(makeHealthTestAccount(2, 1, 0, true), 1.0)
	older, newer := now.Add(-time.Minute), now
	first.account.LastUsedAt = &older
	second.account.LastUsedAt = &newer
	accounts := []accountWithLoad{first, second}

	adaptive := selectAdaptiveAccountWithLoadForGroupStrategyWithHistory(accounts, stats, config.GatewaySchedulingConfig{}, false, now, 110, 0, false, AccountSchedulingStrategyHealthFirst, map[int64]AccountTTFTHistory{
		1: {AccountID: 1, SampleCount: 10, P50Ms: 60_000, P90Ms: 70_000, LatestAt: now},
		2: {AccountID: 2, SampleCount: 10, P50Ms: 100, P90Ms: 180, LatestAt: now},
	})
	require.NotNil(t, adaptive)
	require.Equal(t, int64(2), adaptive.account.ID)

	strict := selectLayeredAccountWithLoad(accounts, stats, config.GatewaySchedulingConfig{}, false, now)
	require.NotNil(t, strict)
	require.Equal(t, int64(1), strict.account.ID)
}

func TestAdaptiveFallbackOrderingUsesHistory(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	now := time.Now()
	cheap := withProbeMultiplier(makeHealthTestAccount(1, 5, 100, true), 0.5, now.Add(time.Hour)).account
	expensive := withProbeMultiplier(makeHealthTestAccount(2, 1, 100, true), 1.5, now.Add(time.Hour)).account
	accounts := []*Account{expensive, cheap}
	history := map[int64]AccountTTFTHistory{
		cheap.ID:     {AccountID: cheap.ID, SampleCount: 20, P50Ms: 500, P90Ms: 800, LatestAt: now},
		expensive.ID: {AccountID: expensive.ID, SampleCount: 20, P50Ms: 100, P90Ms: 180, LatestAt: now},
	}
	svc := &GatewayService{}

	svc.sortAdaptiveCandidatesForFallbackWithHistory(accounts, stats, config.GatewaySchedulingConfig{}, false, 111, AccountSchedulingStrategyHealthCostBalanced, history)
	require.Equal(t, cheap.ID, accounts[0].ID)

	strict := selectLayeredAccount([]*Account{expensive, cheap}, stats, config.GatewaySchedulingConfig{}, false, now)
	require.Equal(t, expensive.ID, strict.ID)
}

func TestSelectHealthCostBalancedAccountWithLoad_PreferSoonestResetKeepsWideHealthBand(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 100
	acceptable := 220
	reportHealthSamples(stats, 1, true, &fast, 3)
	reportHealthSamples(stats, 2, true, &acceptable, 3)
	now := time.Now()
	leadingReset := now.Add(20 * time.Minute)
	cheaperReset := now.Add(2 * time.Minute)
	leading := withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 1.5, leadingReset)
	cheaper := withProbeMultiplier(makeHealthTestAccount(2, 10, 0, true), 0.5, cheaperReset)

	selected := selectAdaptiveAccountWithLoadForGroupStrategy(
		[]accountWithLoad{{account: leading.account}, {account: cheaper.account}},
		stats,
		config.GatewaySchedulingConfig{PreferSoonestReset: true},
		false,
		now,
		105,
		0,
		false,
		AccountSchedulingStrategyHealthCostBalanced,
	)
	require.NotNil(t, selected)
	require.Equal(t, int64(2), selected.account.ID)
}

func TestAdaptiveSelection_WarmingUpRotatesSampleStarvedAccounts(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 100
	reportHealthSamples(stats, 1, true, &fast, 3)
	accounts := []accountWithLoad{
		withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 0.5, time.Now().Add(time.Hour)),
		withProbeMultiplier(makeHealthTestAccount(2, 1, 0, true), 0.5, time.Now().Add(time.Hour)),
		withProbeMultiplier(makeHealthTestAccount(3, 1, 0, true), 0.5, time.Now().Add(time.Hour)),
	}
	now := time.Now()
	// The counter is per-group and starts at zero; drive it to the next turn.
	value, _ := stats.warmingUpCounter.LoadOrStore(int64(103), &atomic.Uint64{})
	value.(*atomic.Uint64).Store(9)
	first := selectAdaptiveAccountWithLoadForGroupStrategy(accounts, stats, config.GatewaySchedulingConfig{}, false, now, 103, 0, false, AccountSchedulingStrategyHealthFirst)
	require.NotNil(t, first)
	require.Equal(t, int64(2), first.account.ID)

	value.(*atomic.Uint64).Store(19)
	second := selectAdaptiveAccountWithLoadForGroupStrategy(accounts, stats, config.GatewaySchedulingConfig{}, false, now.Add(10*time.Second), 103, 0, false, AccountSchedulingStrategyHealthFirst)
	require.NotNil(t, second)
	require.Equal(t, int64(3), second.account.ID)

	// Active affinity suppresses warm-up so continuation/sticky semantics remain intact.
	value.(*atomic.Uint64).Store(29)
	affined := selectAdaptiveAccountWithLoadForGroupStrategy(accounts, stats, config.GatewaySchedulingConfig{}, false, now.Add(4*time.Minute), 103, 1, true, AccountSchedulingStrategyHealthFirst)
	require.NotNil(t, affined)
	require.Equal(t, int64(1), affined.account.ID)
}

func TestAdaptiveSelection_GenericRecoveryKeepsExistingDelay(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 100
	slow := 2_000
	reportHealthSamples(stats, 1, true, &fast, int(accountHealthUnknownMinSamples))
	reportHealthSamples(stats, 2, false, &slow, int(accountHealthUnknownMinSamples))
	now := time.Now()
	accounts := []accountWithLoad{
		withProbeMultiplier(makeHealthTestAccount(1, 1, 0, true), 0.5, now.Add(time.Hour)),
		withProbeMultiplier(makeHealthTestAccount(2, 1, 0, true), 0.5, now.Add(time.Hour)),
	}
	value, _ := stats.healthFirstCounter.LoadOrStore(int64(105), &atomic.Uint64{})
	value.(*atomic.Uint64).Store(accountHealthUnknownExploreEvery - 1)

	selected := selectAdaptiveAccountWithLoadForGroupStrategy(accounts, stats, config.GatewaySchedulingConfig{}, false, now, 105, 0, false, AccountSchedulingStrategyHealthFirst)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestAdaptiveSelection_FallbackOrderingDoesNotMarkWarmingUp(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	accounts := []accountWithLoad{
		makeHealthTestAccount(1, 1, 0, true),
		makeHealthTestAccount(2, 1, 0, true),
	}
	for i := 0; i < 10; i++ {
		selected := selectAdaptiveAccountWithLoadForGroupStrategyWarmup(accounts, stats, config.GatewaySchedulingConfig{}, false, time.Now(), 104, 0, false, AccountSchedulingStrategyHealthFirst, false)
		require.NotNil(t, selected)
	}
	value, ok := stats.warmingUpCounter.Load(int64(104))
	require.False(t, ok)
	require.Nil(t, value)
}

func TestSelectLayeredAccountWithLoad_NilTTFTFailuresBecomeDegraded(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 120
	reportHealthSamples(stats, 1, true, &fast, 3)
	reportHealthSamples(stats, 2, false, nil, 3)

	now := time.Now()
	candidates := []accountWithLoad{
		{account: &Account{ID: 1, Priority: 1, Type: AccountTypeAPIKey}, loadInfo: &AccountLoadInfo{AccountID: 1, LoadRate: 80}},
		{account: &Account{ID: 2, Priority: 1, Type: AccountTypeAPIKey}, loadInfo: &AccountLoadInfo{AccountID: 2, LoadRate: 0}},
	}
	healthCandidates := buildAccountHealthCandidates(candidates, stats)
	require.True(t, hasKnownAccountHealthSample(healthCandidates[1:]))
	require.Less(t, healthCandidates[1].score, accountHealthUnknownScore)

	stats.selectionCounter.Store(accountHealthUnknownExploreEvery - 1)
	require.Nil(t, selectUnknownExplorationCandidate(healthCandidates, stats, now, false))

	healthCandidates[1].lastUpdated = now.Add(-accountHealthDegradedRecoveryDelay - time.Second)
	selectedProbe := selectDegradedRecoveryCandidate(healthCandidates, stats, now, bestAccountHealthScore(healthCandidates), false)
	require.NotNil(t, selectedProbe)
	require.Equal(t, int64(2), selectedProbe.account.ID)

	selected := selectLayeredAccountWithLoad(candidates, stats, config.GatewaySchedulingConfig{}, false, now)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestSelectLayeredAccountWithLoad_UnknownExplorationDoesNotCrossPriority(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 120
	reportHealthSamples(stats, 1, true, &fast, 3)
	now := time.Now()
	candidates := []accountWithLoad{
		{account: &Account{ID: 1, Priority: 1, Type: AccountTypeAPIKey}, loadInfo: &AccountLoadInfo{AccountID: 1, LoadRate: 0}},
		{account: &Account{ID: 2, Priority: 10, Type: AccountTypeAPIKey}, loadInfo: &AccountLoadInfo{AccountID: 2, LoadRate: 0}},
	}

	for i := 0; i < int(accountHealthUnknownExploreEvery); i++ {
		selected := selectLayeredAccountWithLoad(candidates, stats, config.GatewaySchedulingConfig{}, false, now)
		require.NotNil(t, selected)
		require.Equal(t, int64(1), selected.account.ID)
	}
}

func TestSelectLayeredAccountWithLoad_UnavailableTopAccountUsesSamePriorityHealth(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 120
	slow := 2400
	reportHealthSamples(stats, 2, false, &slow, 3)
	reportHealthSamples(stats, 3, true, &fast, 3)
	reportHealthSamples(stats, 4, true, &fast, 3)
	now := time.Now()

	// Account 1 represents the originally-best top-priority account after the
	// caller has already filtered it out for soft cooldown/runtime block.
	candidates := []accountWithLoad{
		{account: &Account{ID: 2, Priority: 1, Type: AccountTypeAPIKey}, loadInfo: &AccountLoadInfo{AccountID: 2, LoadRate: 0}},
		{account: &Account{ID: 3, Priority: 1, Type: AccountTypeAPIKey}, loadInfo: &AccountLoadInfo{AccountID: 3, LoadRate: 30}},
		{account: &Account{ID: 4, Priority: 10, Type: AccountTypeAPIKey}, loadInfo: &AccountLoadInfo{AccountID: 4, LoadRate: 0}},
	}

	selected := selectLayeredAccountWithLoad(candidates, stats, config.GatewaySchedulingConfig{}, false, now)
	require.NotNil(t, selected)
	require.Equal(t, int64(3), selected.account.ID)
}

func TestSelectLayeredAccountWithLoad_MissingLoadInfoDoesNotBeatKnownLoad(t *testing.T) {
	now := time.Now()
	older := now.Add(-time.Hour)
	candidates := []accountWithLoad{
		{account: &Account{ID: 1, Priority: 1, Type: AccountTypeAPIKey, LastUsedAt: &older}, loadInfo: &AccountLoadInfo{AccountID: 1, LoadRate: 50}},
		{account: &Account{ID: 2, Priority: 1, Type: AccountTypeAPIKey, LastUsedAt: &now}, loadInfo: &AccountLoadInfo{AccountID: 2, LoadRate: 0}, loadInfoMissing: true},
	}

	selected := selectLayeredAccountWithLoad(candidates, nil, config.GatewaySchedulingConfig{}, false, now)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestSortCandidatesForFallback_DoesNotTriggerUnknownExploration(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 120
	reportHealthSamples(stats, 1, true, &fast, 3)
	stats.selectionCounter.Store(accountHealthUnknownExploreEvery - 1)

	accounts := []*Account{
		{ID: 1, Priority: 1, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true},
		{ID: 2, Priority: 1, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true},
	}

	svc := &GatewayService{}
	svc.sortCandidatesForFallback(accounts, stats, config.GatewaySchedulingConfig{}, false)

	require.Equal(t, int64(1), accounts[0].ID)
	require.Equal(t, accountHealthUnknownExploreEvery-1, stats.selectionCounter.Load())
}

func TestFilterByAccountHealthBand_PenalizesVerySlowTTFTWithoutHardBlocking(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 120
	slow := 2600
	reportHealthSamples(stats, 1, true, &slow, 3)
	reportHealthSamples(stats, 2, true, &fast, 3)

	candidates := []accountWithLoad{
		makeHealthTestAccount(1, 1, 0, true),
		makeHealthTestAccount(2, 1, 50, true),
	}

	filtered := filterByAccountHealthBand(candidates, stats)
	require.Len(t, filtered, 1)
	require.Equal(t, int64(2), filtered[0].account.ID)

	// 不是硬禁用：如果同层只剩这个慢账号，它仍然可被选择。
	onlySlow := filterByAccountHealthBand(candidates[:1], stats)
	require.Len(t, onlySlow, 1)
	require.Equal(t, int64(1), onlySlow[0].account.ID)
}

func TestGatewayServiceReportAccountScheduleResult_LazilyInitializesStats(t *testing.T) {
	svc := &GatewayService{}
	ttft := 180

	svc.ReportAccountScheduleResult(&Account{ID: 42, Platform: PlatformAnthropic}, true, &ttft)

	stats := svc.accountHealthStats.Load()
	require.NotNil(t, stats)
	errorRate, recordedTTFT, hasTTFT, found := stats.snapshot(42)
	require.True(t, found)
	require.True(t, hasTTFT)
	require.Equal(t, 0.0, errorRate)
	require.Equal(t, 180.0, recordedTTFT)
}

func TestGatewayServiceReportAccountScheduleResult_SkipsOpenAI(t *testing.T) {
	svc := &GatewayService{}
	ttft := 180

	svc.ReportAccountScheduleResult(&Account{ID: 42, Platform: PlatformOpenAI}, true, &ttft)

	require.Nil(t, svc.accountHealthStats.Load())
}

func TestAdaptiveHealthFreshnessExpiresErrorAndTTFTToUnknown(t *testing.T) {
	now := time.Now()
	profiles := []adaptiveAccountHealthProfile{{
		errorRate: 1, errorSamples: 5, p50: 90_000, p90: 100_000, hasTTFT: true, ttftSamples: 5,
		errorUpdated: now.Add(-16 * time.Minute), ttftUpdated: now.Add(-16 * time.Minute),
	}}
	fresh := freshAdaptiveHealthProfiles(profiles, now, adaptiveTTFTSwitchPolicy{enabled: true, thresholdMs: 60_000, sampleFreshness: 15 * time.Minute})
	require.Zero(t, fresh[0].errorSamples)
	require.Zero(t, fresh[0].errorRate)
	require.False(t, fresh[0].hasTTFT)
	require.Zero(t, fresh[0].ttftSamples)
}

func TestAccountRuntimeHealthResetPreservesTTFT(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	ttft := 420
	reportHealthSamples(stats, 42, false, &ttft, 3)
	stats.resetErrorHealth(42)

	errorRate, gotTTFT, hasTTFT, found, samples, ttftSamples, _ := stats.snapshotWithMeta(42)
	require.True(t, found)
	require.Zero(t, errorRate)
	require.Zero(t, samples)
	require.True(t, hasTTFT)
	require.Equal(t, float64(ttft), gotTTFT)
	require.EqualValues(t, 3, ttftSamples)
}
