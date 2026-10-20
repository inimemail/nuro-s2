package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAdaptiveStickyCannotUndoFreshErrorOrdering(t *testing.T) {
	now := time.Now()
	policy := defaultAdaptiveTTFTSwitchPolicy()
	sticky := openAIAccountCandidateScore{account: &Account{ID: 1}, errorRate: .19, sampleCount: 8, errorUpdated: now}
	healthy := openAIAccountCandidateScore{account: &Account{ID: 2}, sampleCount: 8, errorUpdated: now}
	pool := []openAIAccountCandidateScore{sticky, healthy}
	preferred := map[int64]struct{}{1: {}, 2: {}}
	for _, balanced := range []bool{false, true} {
		require.False(t, adaptiveOpenAIStickyCanLead(sticky, pool, preferred, balanced, now, policy),
			"even an error-qualified sticky account must not erase a material health advantage")
	}
	sticky.errorRate = .05
	require.True(t, adaptiveOpenAIStickyCanLead(sticky, pool, preferred, false, now, policy), "healthy affinity is preserved")
	sticky.errorRate = 1
	sticky.sampleCount = 1
	require.True(t, adaptiveOpenAIStickyCanLead(sticky, pool, preferred, false, now, policy), "one sample does not replace existing evidence thresholds")
	sticky.sampleCount = 8
	sticky.errorUpdated = now.Add(-policy.sampleFreshness - time.Second)
	require.True(t, adaptiveOpenAIStickyCanLead(sticky, pool, preferred, false, now, policy), "stale errors do not pin recovery")
}

func TestAdaptiveSelectionDoesNotPromoteMateriallyFailingSticky(t *testing.T) {
	for _, strategy := range []string{AccountSchedulingStrategyHealthFirst, AccountSchedulingStrategyHealthCostBalanced} {
		t.Run(strategy, func(t *testing.T) {
			now := time.Now()
			sticky := openAIAccountCandidateScore{
				account:   withOpenAIUpstreamProbeMultiplier(&Account{ID: 1}, .1, now.Add(time.Hour)),
				errorRate: .19, sampleCount: 8, errorUpdated: now,
			}
			healthy := openAIAccountCandidateScore{
				account:     withOpenAIUpstreamProbeMultiplier(&Account{ID: 2}, .1, now.Add(time.Hour)),
				sampleCount: 8, errorUpdated: now,
			}
			scheduler := &defaultOpenAIAccountScheduler{}
			request := OpenAIAccountScheduleRequest{AccountSchedulingStrategy: strategy, StickyAccountID: sticky.account.ID}
			for _, pool := range [][]openAIAccountCandidateScore{{sticky, healthy}, {healthy, sticky}} {
				ordered := scheduler.buildHealthFirstSelectionOrder(pool, request)
				require.Equal(t, healthy.account.ID, ordered[0].account.ID,
					"affinity cannot undo the final selection even when both accounts qualify for the preferred tier")
			}
			sticky.errorRate = .05
			ordered := scheduler.buildHealthFirstSelectionOrder([]openAIAccountCandidateScore{healthy, sticky}, request)
			require.Equal(t, sticky.account.ID, ordered[0].account.ID, "ordinary health jitter preserves affinity")
		})
	}
}
