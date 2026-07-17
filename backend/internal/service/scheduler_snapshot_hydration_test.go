//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type snapshotHydrationCache struct {
	snapshot []*Account
	accounts map[int64]*Account
}

func (c *snapshotHydrationCache) GetSnapshot(ctx context.Context, bucket SchedulerBucket) ([]*Account, bool, error) {
	if c.accounts == nil {
		return c.snapshot, true, nil
	}
	out := make([]*Account, 0, len(c.snapshot))
	for _, account := range c.snapshot {
		if account == nil {
			out = append(out, nil)
			continue
		}
		if fresh, ok := c.accounts[account.ID]; ok {
			out = append(out, fresh)
			continue
		}
		out = append(out, account)
	}
	return out, true, nil
}

func (c *snapshotHydrationCache) SetSnapshot(ctx context.Context, bucket SchedulerBucket, accounts []Account) error {
	return nil
}

func (c *snapshotHydrationCache) GetAccount(ctx context.Context, accountID int64) (*Account, error) {
	if c.accounts == nil {
		return nil, nil
	}
	return c.accounts[accountID], nil
}

func (c *snapshotHydrationCache) SetAccount(ctx context.Context, account *Account) error {
	return nil
}

func (c *snapshotHydrationCache) DeleteAccount(ctx context.Context, accountID int64) error {
	return nil
}

func (c *snapshotHydrationCache) UpdateLastUsed(ctx context.Context, updates map[int64]time.Time) error {
	return nil
}

func (c *snapshotHydrationCache) TryLockBucket(ctx context.Context, bucket SchedulerBucket, ttl time.Duration) (bool, error) {
	return true, nil
}

func (c *snapshotHydrationCache) UnlockBucket(ctx context.Context, bucket SchedulerBucket) error {
	return nil
}

func (c *snapshotHydrationCache) ListBuckets(ctx context.Context) ([]SchedulerBucket, error) {
	return nil, nil
}

func (c *snapshotHydrationCache) GetOutboxWatermark(ctx context.Context) (int64, error) {
	return 0, nil
}

func (c *snapshotHydrationCache) SetOutboxWatermark(ctx context.Context, id int64) error {
	return nil
}

func TestSchedulerSnapshotLocalSnapshot_IgnoresOwnSnapshotUpdatedEvent(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LocalSnapshotEnabled = true
	cfg.Gateway.Scheduling.LocalSnapshotTTLMS = 1000
	cfg.Gateway.Scheduling.LocalSnapshotMaxKeys = 16
	cfg.Gateway.Scheduling.EventBusEnabled = true
	cfg.Gateway.Scheduling.EventBusBackend = "local"
	svc := NewSchedulerSnapshotService(&snapshotHydrationCache{}, nil, nil, nil, cfg, nil)
	bucket := SchedulerBucket{GroupID: 1, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
	accounts := []Account{{
		ID:          11,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
	}}
	svc.storeLocalSnapshot(bucket, accounts)

	svc.handleSchedulerEvent(context.Background(), SchedulerEvent{
		Type:   SchedulerEventSnapshotUpdated,
		Bucket: bucket,
		Source: svc.eventSource,
	})
	got, hit := svc.localSnapshot.Get(bucket, time.Now())
	if !hit || len(got) != 1 {
		t.Fatalf("expected own snapshot_updated event to keep local snapshot, hit=%v len=%d", hit, len(got))
	}

	svc.handleSchedulerEvent(context.Background(), SchedulerEvent{
		Type:   SchedulerEventSnapshotUpdated,
		Bucket: bucket,
		Source: "other-instance",
	})
	if _, hit = svc.localSnapshot.Get(bucket, time.Now()); hit {
		t.Fatal("expected remote snapshot_updated event to invalidate local snapshot")
	}
}

func TestSchedulerLocalSnapshot_ClonesMutableAccountFields(t *testing.T) {
	snapshot := NewSchedulerLocalSnapshot(config.GatewaySchedulingConfig{
		LocalSnapshotEnabled: true,
		LocalSnapshotTTLMS:   1000,
		LocalSnapshotMaxKeys: 16,
	})
	bucket := SchedulerBucket{GroupID: 1, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
	observed := 2.5
	evaluatedAt := time.Now().UTC()
	accounts := []Account{{
		ID:          12,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"api_key": "old",
			"nested":  map[string]any{"flag": "old"},
		},
		GroupIDs:                               []int64{1},
		UpstreamBillingGuardObservedMultiplier: &observed,
		UpstreamBillingGuardEvaluatedAt:        &evaluatedAt,
	}}
	snapshot.Set(bucket, accounts, time.Now())
	accounts[0].Credentials["api_key"] = "mutated"
	accounts[0].Credentials["nested"].(map[string]any)["flag"] = "mutated"
	accounts[0].GroupIDs[0] = 99
	observed = 9
	evaluatedAt = evaluatedAt.Add(time.Hour)

	got, hit := snapshot.Get(bucket, time.Now())
	if !hit || len(got) != 1 {
		t.Fatalf("expected local snapshot hit, hit=%v len=%d", hit, len(got))
	}
	if got[0].Credentials["api_key"] != "old" {
		t.Fatalf("expected credentials clone, got %v", got[0].Credentials["api_key"])
	}
	if got[0].Credentials["nested"].(map[string]any)["flag"] != "old" {
		t.Fatalf("expected nested credentials clone, got %v", got[0].Credentials["nested"])
	}
	if got[0].GroupIDs[0] != 1 {
		t.Fatalf("expected group ids clone, got %v", got[0].GroupIDs)
	}
	if got[0].UpstreamBillingGuardObservedMultiplier == nil || *got[0].UpstreamBillingGuardObservedMultiplier != 2.5 {
		t.Fatalf("expected billing guard multiplier clone, got %v", got[0].UpstreamBillingGuardObservedMultiplier)
	}
	if got[0].UpstreamBillingGuardEvaluatedAt == nil || !got[0].UpstreamBillingGuardEvaluatedAt.Equal(evaluatedAt.Add(-time.Hour)) {
		t.Fatalf("expected billing guard evaluation time clone, got %v", got[0].UpstreamBillingGuardEvaluatedAt)
	}
}

func TestOpenAISelectAccountWithLoadAwareness_HydratesSelectedAccountFromSchedulerSnapshot(t *testing.T) {
	cache := &snapshotHydrationCache{
		snapshot: []*Account{
			{
				ID:          1,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				GroupIDs:    []int64{2},
				Credentials: map[string]any{
					"model_mapping": map[string]any{
						"gpt-4": "gpt-4",
					},
				},
			},
		},
		accounts: map[int64]*Account{
			1: {
				ID:          1,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				GroupIDs:    []int64{2},
				Credentials: map[string]any{
					"api_key":       "sk-live",
					"model_mapping": map[string]any{"gpt-4": "gpt-4"},
				},
			},
		},
	}

	schedulerSnapshot := NewSchedulerSnapshotService(cache, nil, nil, nil, nil, nil)
	groupID := int64(2)
	svc := &OpenAIGatewayService{
		schedulerSnapshot: schedulerSnapshot,
		cache:             &stubGatewayCache{},
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.Account == nil {
		t.Fatalf("expected selected account")
	}
	if got := selection.Account.GetOpenAIApiKey(); got != "sk-live" {
		t.Fatalf("expected hydrated api key, got %q", got)
	}
}

func TestSchedulerSnapshotListSchedulableAccounts_FiltersStaleGroupMembership(t *testing.T) {
	cache := &snapshotHydrationCache{
		snapshot: []*Account{
			{
				ID:          1,
				Platform:    PlatformOpenAI,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				GroupIDs:    []int64{10},
			},
		},
		accounts: map[int64]*Account{
			1: {
				ID:          1,
				Platform:    PlatformOpenAI,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				GroupIDs:    nil,
			},
		},
	}

	schedulerSnapshot := NewSchedulerSnapshotService(cache, nil, nil, nil, nil, nil)
	groupID := int64(10)

	accounts, _, err := schedulerSnapshot.ListSchedulableAccounts(context.Background(), &groupID, PlatformOpenAI, false)
	if err != nil {
		t.Fatalf("ListSchedulableAccounts error: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("expected stale group member to be filtered, got %d accounts", len(accounts))
	}
}

func TestSchedulerSnapshotListSchedulableAccounts_AppliesBindingScopedBillingGuard(t *testing.T) {
	observed := 2.0
	lowLimit := 1.0
	highLimit := 3.0
	cache := &snapshotHydrationCache{snapshot: []*Account{{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true,
		Extra:                                  map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		UpstreamBillingGuardObservedMultiplier: &observed,
		GroupIDs:                               []int64{10, 20},
		AccountGroups: []AccountGroup{
			{GroupID: 10, UpstreamBillingGuardMaxMultiplier: &lowLimit},
			{GroupID: 20, UpstreamBillingGuardMaxMultiplier: &highLimit},
		},
	}}}
	schedulerSnapshot := NewSchedulerSnapshotService(cache, nil, nil, nil, nil, nil)

	group10 := int64(10)
	accounts, _, err := schedulerSnapshot.ListSchedulableAccounts(context.Background(), &group10, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Len(t, accounts, 1, "guarded accounts stay in the bucket so sticky affinity is preserved")
	require.True(t, accounts[0].UpstreamBillingGuardGroupBlocked)
	require.False(t, accounts[0].IsSchedulable())

	group20 := int64(20)
	accounts, _, err = schedulerSnapshot.ListSchedulableAccounts(context.Background(), &group20, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.False(t, accounts[0].UpstreamBillingGuardGroupBlocked)
	require.True(t, accounts[0].IsSchedulable())
}

func TestOpenAINewAcquiredSelectionResult_ReleasesSlotWhenHydrationFails(t *testing.T) {
	cache := &snapshotHydrationCache{
		accounts: map[int64]*Account{},
	}
	schedulerSnapshot := NewSchedulerSnapshotService(cache, nil, stubOpenAIAccountRepo{}, nil, nil, nil)
	svc := &OpenAIGatewayService{
		schedulerSnapshot: schedulerSnapshot,
	}
	releaseCalls := 0

	selection, err := svc.newAcquiredSelectionResult(context.Background(), &Account{ID: 1001}, func() {
		releaseCalls++
	})

	if err == nil {
		t.Fatalf("expected hydration error")
	}
	if selection != nil {
		t.Fatalf("expected nil selection on hydration error")
	}
	if releaseCalls != 1 {
		t.Fatalf("expected release to be called once, got %d", releaseCalls)
	}
}

func TestGatewayNewAcquiredSelectionResult_ReleasesSlotWhenHydrationFails(t *testing.T) {
	cache := &snapshotHydrationCache{
		accounts: map[int64]*Account{},
	}
	schedulerSnapshot := NewSchedulerSnapshotService(cache, nil, stubOpenAIAccountRepo{}, nil, nil, nil)
	svc := &GatewayService{
		schedulerSnapshot: schedulerSnapshot,
	}
	releaseCalls := 0

	selection, err := svc.newAcquiredSelectionResult(context.Background(), &Account{ID: 1001}, func() {
		releaseCalls++
	})

	if err == nil {
		t.Fatal("expected hydration error")
	}
	if selection != nil {
		t.Fatal("expected nil selection on hydration error")
	}
	if releaseCalls != 1 {
		t.Fatalf("expected release to be called once, got %d", releaseCalls)
	}
}

func TestGatewaySelectAccountWithLoadAwareness_HydratesSelectedAccountFromSchedulerSnapshot(t *testing.T) {
	cache := &snapshotHydrationCache{
		snapshot: []*Account{
			{
				ID:          9,
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
			},
		},
		accounts: map[int64]*Account{
			9: {
				ID:          9,
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				Credentials: map[string]any{
					"api_key": "anthropic-live-key",
				},
			},
		},
	}

	schedulerSnapshot := NewSchedulerSnapshotService(cache, nil, nil, nil, nil, nil)
	svc := &GatewayService{
		schedulerSnapshot: schedulerSnapshot,
		cache:             &mockGatewayCacheForPlatform{},
		cfg:               testConfig(),
	}

	result, err := svc.SelectAccountWithLoadAwareness(context.Background(), nil, "", "claude-3-5-sonnet-20241022", nil, "", 0)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if result == nil || result.Account == nil {
		t.Fatalf("expected selected account")
	}
	if got := result.Account.GetCredential("api_key"); got != "anthropic-live-key" {
		t.Fatalf("expected hydrated api key, got %q", got)
	}
}

func TestGatewaySelectAccountWithLoadAwareness_StickyMovedGroupClearsSession(t *testing.T) {
	groupID := int64(1)
	sessionHash := "gateway-session-moved-group"
	cache := &snapshotHydrationCache{
		snapshot: []*Account{
			{
				ID:          9,
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				GroupIDs:    []int64{groupID},
			},
			{
				ID:          10,
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    2,
				GroupIDs:    []int64{groupID},
			},
		},
		accounts: map[int64]*Account{
			9: {
				ID:          9,
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				GroupIDs:    []int64{2},
			},
			10: {
				ID:          10,
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    2,
				GroupIDs:    []int64{groupID},
				Credentials: map[string]any{"api_key": "current-group-key"},
			},
		},
	}
	gatewayCache := &mockGatewayCacheForPlatform{
		sessionBindings: map[string]int64{sessionHash: 9},
	}

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{*cache.accounts[9], *cache.accounts[10]},
		accountsByID: map[int64]*Account{
			9:  cache.accounts[9],
			10: cache.accounts[10],
		},
	}
	schedulerSnapshot := NewSchedulerSnapshotService(cache, nil, repo, nil, nil, nil)
	svc := &GatewayService{
		accountRepo:       repo,
		groupRepo:         &mockGroupRepoForGateway{groups: map[int64]*Group{groupID: {ID: groupID, Platform: PlatformAnthropic, Status: StatusActive}}},
		schedulerSnapshot: schedulerSnapshot,
		cache:             gatewayCache,
		cfg:               testConfig(),
		concurrencyService: NewConcurrencyService(&mockConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				9:  {AccountID: 9, LoadRate: 0},
				10: {AccountID: 10, LoadRate: 0},
			},
			acquireResults: map[int64]bool{
				9:  true,
				10: true,
			},
		}),
	}

	result, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, sessionHash, "claude-3-5-sonnet-20241022", nil, "", 0)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Account)
	require.Equal(t, int64(10), result.Account.ID)
	require.Equal(t, 1, gatewayCache.deletedSessions[sessionHash])
	require.Equal(t, int64(10), gatewayCache.sessionBindings[sessionHash])
	if result.ReleaseFunc != nil {
		result.ReleaseFunc()
	}
}
