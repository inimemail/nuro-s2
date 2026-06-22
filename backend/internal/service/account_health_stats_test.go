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

func TestFilterByAccountHealthBand_PrefersHealthBeforeLoadInsideSameLayer(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 120
	slow := 2200
	stats.report(1, false, &slow)
	stats.report(1, false, &slow)
	stats.report(1, false, &slow)
	stats.report(2, true, &fast)
	stats.report(2, true, &fast)

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
	stats.report(1, false, &slow)
	stats.report(1, false, &slow)
	stats.report(2, true, &fast)

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
	stats.report(1, false, &slow)
	stats.report(1, false, &slow)
	stats.report(2, true, &fast)

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
	stats.report(1, true, &slow)
	stats.report(2, true, &fast)

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
	stats.report(1, true, &slow)
	stats.report(2, true, &fast)

	selected := selectLayeredAccountWithLoad([]accountWithLoad{
		makeHealthTestAccount(1, 1, 0, true),
		makeHealthTestAccount(2, 100, 0, true),
	}, stats, config.GatewaySchedulingConfig{}, false, time.Now())

	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID)
}

func TestFilterByAccountHealthBand_KeepsLoadBalanceForSimilarHealth(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	ttftA := 100
	ttftB := 130
	stats.report(1, true, &ttftA)
	stats.report(2, true, &ttftB)

	now := time.Now()
	candidates := []accountWithLoad{
		{
			account:  &Account{ID: 1, Priority: 1, Type: AccountTypeAPIKey, LastUsedAt: &now},
			loadInfo: &AccountLoadInfo{AccountID: 1, LoadRate: 40},
		},
		{
			account:  &Account{ID: 2, Priority: 1, Type: AccountTypeAPIKey, LastUsedAt: &now},
			loadInfo: &AccountLoadInfo{AccountID: 2, LoadRate: 0},
		},
	}

	filtered := filterByAccountHealthBand(candidates, stats)
	require.Len(t, filtered, 2)
	loadBalanced := filterByMinLoadRate(filtered)
	require.Len(t, loadBalanced, 1)
	require.Equal(t, int64(2), loadBalanced[0].account.ID)
}

func TestFilterByAccountHealthBand_PenalizesVerySlowTTFTWithoutHardBlocking(t *testing.T) {
	stats := newAccountRuntimeHealthStats()
	fast := 120
	slow := 2600
	stats.report(1, true, &slow)
	stats.report(2, true, &fast)

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
