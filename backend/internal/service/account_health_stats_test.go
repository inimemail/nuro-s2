package service

import (
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

func TestSelectLayeredAccountWithLoad_SimilarHealthUsesLowerLoad(t *testing.T) {
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
	require.Equal(t, int64(2), selected.account.ID)
}

func TestSelectLayeredAccountWithLoad_UnknownExplorationInSamePriority(t *testing.T) {
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
	require.Equal(t, int64(2), selected.account.ID)
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
