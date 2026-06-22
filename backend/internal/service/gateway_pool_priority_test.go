package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func (r schedulerTestOpenAIAccountRepo) ListSchedulableByPlatforms(ctx context.Context, platforms []string) ([]Account, error) {
	platformSet := make(map[string]struct{}, len(platforms))
	for _, platform := range platforms {
		platformSet[platform] = struct{}{}
	}
	var result []Account
	for _, acc := range r.accounts {
		if _, ok := platformSet[acc.Platform]; ok && acc.IsSchedulable() {
			result = append(result, acc)
		}
	}
	return result, nil
}

func (r schedulerTestOpenAIAccountRepo) ListSchedulableByGroupIDAndPlatforms(ctx context.Context, groupID int64, platforms []string) ([]Account, error) {
	accounts, err := r.ListSchedulableByPlatforms(ctx, platforms)
	if err != nil {
		return nil, err
	}
	result := make([]Account, 0, len(accounts))
	for _, acc := range accounts {
		if openAIStickyAccountMatchesGroup(&acc, &groupID) {
			result = append(result, acc)
		}
	}
	return result, nil
}

func (r schedulerTestOpenAIAccountRepo) ListSchedulableUngroupedByPlatforms(ctx context.Context, platforms []string) ([]Account, error) {
	accounts, err := r.ListSchedulableByPlatforms(ctx, platforms)
	if err != nil {
		return nil, err
	}
	result := make([]Account, 0, len(accounts))
	for _, acc := range accounts {
		if openAIStickyAccountMatchesGroup(&acc, nil) {
			result = append(result, acc)
		}
	}
	return result, nil
}

func TestGatewayService_SelectAccountWithLoadAwareness_SamePriorityPrefersNonPoolBeforePoolLoad(t *testing.T) {
	ctx := context.Background()
	accounts := []Account{
		{
			ID:          6101,
			Platform:    PlatformAnthropic,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    1,
			Credentials: map[string]any{"pool_mode": true},
		},
		{
			ID:          6102,
			Platform:    PlatformAnthropic,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    1,
		},
		{
			ID:          6103,
			Platform:    PlatformAnthropic,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    3,
		},
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &GatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:       &schedulerTestGatewayCache{},
		cfg:         cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				6101: {AccountID: 6101, LoadRate: 0, WaitingCount: 0},
				6102: {AccountID: 6102, LoadRate: 95, WaitingCount: 9},
				6103: {AccountID: 6103, LoadRate: 0, WaitingCount: 0},
			},
		}),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "", "claude-sonnet-4-5", nil, "", 0)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(6102), selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestGatewayService_SelectAccountWithLoadAwareness_PoolSoftCooldownFallsForwardToNextPriority(t *testing.T) {
	ctx := context.Background()
	pool := Account{
		ID:          6111,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    1,
		Credentials: map[string]any{"pool_mode": true},
	}
	nextPriority := Account{
		ID:          6112,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    3,
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &GatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{pool, nextPriority}},
		cache:       &schedulerTestGatewayCache{},
		cfg:         cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				pool.ID:         {AccountID: pool.ID, LoadRate: 0, WaitingCount: 0},
				nextPriority.ID: {AccountID: nextPriority.ID, LoadRate: 0, WaitingCount: 0},
			},
		}),
	}
	svc.anthropicPoolSoftCooldownUntil.Store(pool.ID, time.Now().Add(time.Minute))

	selection, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "", "claude-sonnet-4-5", nil, "", 0)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, nextPriority.ID, selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}
