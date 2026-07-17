package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type schedulerFullRebuildTestCache struct {
	SchedulerCache

	mu        sync.Mutex
	listErr   error
	listCalls int
	lockCalls int
}

type schedulerGroupPolicyRefreshCache struct {
	SchedulerCache
	accounts []Account
}

func (c *schedulerGroupPolicyRefreshCache) SetAccount(_ context.Context, account *Account) error {
	if account != nil {
		c.accounts = append(c.accounts, *account)
	}
	return nil
}

func (c *schedulerGroupPolicyRefreshCache) TryLockBucket(context.Context, SchedulerBucket, time.Duration) (bool, error) {
	return false, nil
}

type schedulerGroupPolicyRefreshRepo struct {
	AccountRepository
	accounts []Account
}

func (r *schedulerGroupPolicyRefreshRepo) ListByGroup(context.Context, int64) ([]Account, error) {
	return r.accounts, nil
}

func (r *schedulerGroupPolicyRefreshRepo) ListActive(context.Context) ([]Account, error) {
	return r.accounts, nil
}

func (c *schedulerGroupPolicyRefreshCache) ListBuckets(context.Context) ([]SchedulerBucket, error) {
	return []SchedulerBucket{{GroupID: 10, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}}, nil
}

func TestSchedulerGroupChangeRefreshesAccountsOutsideSchedulableWindows(t *testing.T) {
	groupID := int64(10)
	limit := 1.25
	cache := &schedulerGroupPolicyRefreshCache{}
	repo := &schedulerGroupPolicyRefreshRepo{accounts: []Account{{
		ID:                          7,
		Status:                      StatusActive,
		RateLimitResetAt:            ptrTimeForSchedulerTest(time.Now().Add(time.Hour)),
		UpstreamBillingGuardEnabled: true,
		AccountGroups: []AccountGroup{{
			GroupID:                           groupID,
			UpstreamBillingGuardMaxMultiplier: &limit,
		}},
	}}}
	svc := NewSchedulerSnapshotService(cache, nil, repo, nil, nil, nil)

	require.NoError(t, svc.handleGroupEvent(context.Background(), &groupID, nil))
	require.Len(t, cache.accounts, 1)
	require.Equal(t, int64(7), cache.accounts[0].ID)
	require.NotNil(t, cache.accounts[0].AccountGroups[0].UpstreamBillingGuardMaxMultiplier)
	require.Equal(t, limit, *cache.accounts[0].AccountGroups[0].UpstreamBillingGuardMaxMultiplier)
}

func TestSchedulerInitialRebuildRefreshesAccountsOutsideSchedulableWindows(t *testing.T) {
	groupID := int64(10)
	limit := 1.25
	cache := &schedulerGroupPolicyRefreshCache{}
	repo := &schedulerGroupPolicyRefreshRepo{accounts: []Account{{
		ID:                          7,
		Status:                      StatusActive,
		RateLimitResetAt:            ptrTimeForSchedulerTest(time.Now().Add(time.Hour)),
		UpstreamBillingGuardEnabled: true,
		AccountGroups: []AccountGroup{{
			GroupID:                           groupID,
			UpstreamBillingGuardMaxMultiplier: &limit,
		}},
	}}}
	svc := NewSchedulerSnapshotService(cache, nil, repo, nil, nil, nil)

	svc.runInitialRebuild()

	require.Len(t, cache.accounts, 1)
	require.Equal(t, int64(7), cache.accounts[0].ID)
	require.Equal(t, limit, *cache.accounts[0].AccountGroups[0].UpstreamBillingGuardMaxMultiplier)
}

func TestSchedulerMarkedFullRebuildRefreshesAccountsOutsideSchedulableWindows(t *testing.T) {
	cache := &schedulerGroupPolicyRefreshCache{}
	repo := &schedulerGroupPolicyRefreshRepo{accounts: []Account{{
		ID:            8,
		Status:        StatusActive,
		OverloadUntil: ptrTimeForSchedulerTest(time.Now().Add(time.Hour)),
		AccountGroups: []AccountGroup{{GroupID: 10}},
	}}}
	svc := NewSchedulerSnapshotService(cache, nil, repo, nil, nil, nil)

	err := svc.handleOutboxEvent(context.Background(), SchedulerOutboxEvent{
		EventType: SchedulerOutboxEventFullRebuild,
		Payload:   map[string]any{"refresh_account_metadata": true},
	}, nil)

	require.NoError(t, err)
	require.Len(t, cache.accounts, 1)
	require.Equal(t, int64(8), cache.accounts[0].ID)
}

func ptrTimeForSchedulerTest(value time.Time) *time.Time {
	return &value
}

func (c *schedulerFullRebuildTestCache) ListBuckets(context.Context) ([]SchedulerBucket, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listCalls++
	return nil, c.listErr
}

func (c *schedulerFullRebuildTestCache) TryLockBucket(context.Context, SchedulerBucket, time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lockCalls++
	return false, nil
}

func TestSchedulerSnapshotServiceFullRebuildCoalescesConcurrentRequestsIntoTrailingRun(t *testing.T) {
	svc := &SchedulerSnapshotService{}
	wantTrailingErr := errors.New("trailing rebuild failed")
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseFirst) })
	}
	defer release()

	var calls atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32
	run := func() error {
		call := calls.Add(1)
		currentActive := active.Add(1)
		defer active.Add(-1)
		for {
			previousMax := maxActive.Load()
			if currentActive <= previousMax || maxActive.CompareAndSwap(previousMax, currentActive) {
				break
			}
		}
		if call == 1 {
			close(firstStarted)
			<-releaseFirst
			return nil
		}
		return wantTrailingErr
	}

	firstResult := make(chan error, 1)
	go func() {
		firstResult <- svc.coalesceFullRebuild(run)
	}()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first rebuild did not start")
	}

	const followers = 20
	followerResults := make(chan error, followers)
	for range followers {
		go func() {
			followerResults <- svc.coalesceFullRebuild(run)
		}()
	}

	require.Eventually(t, func() bool {
		requested, _ := schedulerFullRebuildState(svc)
		return requested == followers+1
	}, time.Second, time.Millisecond)
	release()

	require.NoError(t, <-firstResult)
	for range followers {
		require.ErrorIs(t, <-followerResults, wantTrailingErr)
	}
	require.EqualValues(t, 2, calls.Load())
	require.EqualValues(t, 1, maxActive.Load())
	requested, completed := schedulerFullRebuildState(svc)
	require.EqualValues(t, followers+1, requested)
	require.Equal(t, requested, completed)
}

func TestSchedulerSnapshotServiceFullRebuildRunsAgainForSequentialRequest(t *testing.T) {
	svc := &SchedulerSnapshotService{}
	wantSecondErr := errors.New("second rebuild failed")
	var calls atomic.Int32
	run := func() error {
		if calls.Add(1) == 2 {
			return wantSecondErr
		}
		return nil
	}

	require.NoError(t, svc.coalesceFullRebuild(run))
	require.ErrorIs(t, svc.coalesceFullRebuild(run), wantSecondErr)
	require.EqualValues(t, 2, calls.Load())
	requested, completed := schedulerFullRebuildState(svc)
	require.EqualValues(t, 2, requested)
	require.Equal(t, requested, completed)
}

func TestSchedulerSnapshotServiceInitialFullRebuildFallsBackWhenListBucketsFails(t *testing.T) {
	cache := &schedulerFullRebuildTestCache{listErr: errors.New("list buckets failed")}
	svc := NewSchedulerSnapshotService(cache, nil, nil, nil, nil, nil)

	svc.runInitialRebuild()

	cache.mu.Lock()
	listCalls := cache.listCalls
	lockCalls := cache.lockCalls
	cache.mu.Unlock()
	require.Equal(t, 1, listCalls)
	require.Positive(t, lockCalls)
	requested, completed := schedulerFullRebuildState(svc)
	require.EqualValues(t, 1, requested)
	require.Equal(t, requested, completed)
}

func schedulerFullRebuildState(svc *SchedulerSnapshotService) (requested uint64, completed uint64) {
	svc.fullRebuildStateMu.Lock()
	defer svc.fullRebuildStateMu.Unlock()
	return svc.fullRebuildRequested, svc.fullRebuildCompleted
}
