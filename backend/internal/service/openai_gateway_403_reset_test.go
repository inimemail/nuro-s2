package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type openAI403CounterResetStub struct {
	resetCalls []int64
}

func (s *openAI403CounterResetStub) IncrementOpenAI403Count(context.Context, int64, int) (int64, error) {
	return 0, nil
}

func (s *openAI403CounterResetStub) ResetOpenAI403Count(_ context.Context, accountID int64) error {
	s.resetCalls = append(s.resetCalls, accountID)
	return nil
}

func TestOpenAIGatewayServiceRecordUsage_ResetsOpenAI403CounterForZeroUsage(t *testing.T) {
	counter := &openAI403CounterResetStub{}
	rateLimitSvc := NewRateLimitService(nil, nil, nil, nil, nil)
	rateLimitSvc.SetOpenAI403CounterCache(counter)

	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, userRepo, subRepo, nil)
	svc.rateLimitService = rateLimitSvc

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID: "resp_zero_usage_reset_403",
			Model:     "gpt-5.1",
		},
		APIKey:  &APIKey{ID: 1001, Group: &Group{RateMultiplier: 1}},
		User:    &User{ID: 2001},
		Account: &Account{ID: 777, Platform: PlatformOpenAI},
	})

	require.NoError(t, err)
	require.Equal(t, []int64{777}, counter.resetCalls)
	require.Equal(t, 1, usageRepo.calls)
	_, lastUsedScheduled := svc.deferredService.lastUsedUpdates.Load(int64(777))
	require.True(t, lastUsedScheduled)
}

func TestOpenAIGatewayServiceRecordUsage_HealthProbeKeepsBillingButSkipsAccountState(t *testing.T) {
	counter := &openAI403CounterResetStub{}
	rateLimitSvc := NewRateLimitService(nil, nil, nil, nil, nil)
	rateLimitSvc.SetOpenAI403CounterCache(counter)

	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(
		usageRepo,
		billingRepo,
		&openAIRecordUsageUserRepoStub{},
		&openAIRecordUsageSubRepoStub{},
		nil,
	)
	svc.rateLimitService = rateLimitSvc

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID: "resp_health_probe_usage",
			Model:     "gpt-5.5",
		},
		APIKey:      &APIKey{ID: 1002, Group: &Group{RateMultiplier: 1}},
		User:        &User{ID: 2002},
		Account:     &Account{ID: 778, Platform: PlatformOpenAI},
		HealthProbe: true,
	})

	require.NoError(t, err)
	require.Empty(t, counter.resetCalls)
	require.Equal(t, 1, billingRepo.calls)
	require.Equal(t, 1, usageRepo.calls)
	_, lastUsedScheduled := svc.deferredService.lastUsedUpdates.Load(int64(778))
	require.False(t, lastUsedScheduled)
}

func TestOpenAIGatewayServiceRecordUsage_PartialFailureKeepsBillingButSkipsSuccessState(t *testing.T) {
	counter := &openAI403CounterResetStub{}
	rateLimitSvc := NewRateLimitService(nil, nil, nil, nil, nil)
	rateLimitSvc.SetOpenAI403CounterCache(counter)

	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(
		usageRepo,
		billingRepo,
		&openAIRecordUsageUserRepoStub{},
		&openAIRecordUsageSubRepoStub{},
		nil,
	)
	svc.rateLimitService = rateLimitSvc

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID: "resp_partial_failure_usage",
			Model:     "gpt-5.5",
			Usage:     OpenAIUsage{InputTokens: 10, OutputTokens: 2},
		},
		APIKey:                 &APIKey{ID: 1003, Group: &Group{RateMultiplier: 1}},
		User:                   &User{ID: 2003},
		Account:                &Account{ID: 779, Platform: PlatformOpenAI},
		SkipSuccessSideEffects: true,
	})

	require.NoError(t, err)
	require.Empty(t, counter.resetCalls)
	require.Equal(t, 1, billingRepo.calls)
	require.Equal(t, 1, usageRepo.calls)
	_, lastUsedScheduled := svc.deferredService.lastUsedUpdates.Load(int64(779))
	require.False(t, lastUsedScheduled)
}
