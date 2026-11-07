package service

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAntigravityUsagePresentationIsolation(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	flag := true
	used := 42.0
	snapshot := &UsageInfo{
		UpdatedAt:               &reset,
		AntigravityQuotaGroups:  []*AntigravityQuotaGroup{{Key: "argon", Windows: []*AntigravityQuotaWindow{{Window: "weekly", Utilization: &used, ResetsAt: &reset}}}},
		AntigravityQuotaSources: map[string]*AntigravityQuotaSource{"summary": {UpdatedAt: &reset}},
		FiveHour:                &UsageProgress{ResetsAt: &reset, RemainingSeconds: 777, WindowStats: &WindowStats{Requests: 3}},
		AntigravityQuota:        map[string]*AntigravityModelQuota{"a": {Utilization: 20}},
		AntigravityQuotaDetails: map[string]*AntigravityModelDetail{"a": {SupportsImages: &flag, SupportedMimeTypes: map[string]bool{"image/png": true}}},
		AICredits:               []AICredit{{Amount: 10}}, ModelForwardingRules: map[string]string{"a": "b"},
	}
	before, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 100 {
				view := cloneAntigravityUsageForPresentation(snapshot)
				_, _ = json.Marshal(snapshot)
				_, _ = json.Marshal(view)
				*view.AntigravityQuotaGroups[0].Windows[0].Utilization = 9
				*view.AntigravityQuotaGroups[0].Windows[0].ResetsAt = time.Time{}
				*view.AntigravityQuotaSources["summary"].UpdatedAt = time.Time{}
				view.AntigravityQuotaGroups[0].Key = "other"
				view.FiveHour.RemainingSeconds = -1
				view.FiveHour.WindowStats.Requests = 9
				*view.FiveHour.ResetsAt = time.Time{}
				*view.UpdatedAt = time.Time{}
				view.AntigravityQuota["a"].Utilization = 100
				*view.AntigravityQuotaDetails["a"].SupportsImages = false
				view.AntigravityQuotaDetails["a"].SupportedMimeTypes["image/png"] = false
				view.AICredits[0].Amount = 0
				view.ModelForwardingRules["a"] = "c"
			}
		})
	}
	wg.Wait()
	after, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
	past := time.Now().Add(-time.Hour)
	require.Zero(t, cloneAntigravityUsageForPresentation(&UsageInfo{FiveHour: &UsageProgress{ResetsAt: &past}}).FiveHour.RemainingSeconds)
}
