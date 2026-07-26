package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestCellAwareLiveAccountLeaseUsesFixedOpenAICell(t *testing.T) {
	control := miniredis.RunT(t)
	openAI := miniredis.RunT(t)
	anthropic := miniredis.RunT(t)
	controlClient := redis.NewClient(&redis.Options{Addr: control.Addr()})
	openAIClient := redis.NewClient(&redis.Options{Addr: openAI.Addr()})
	legacy := NewConcurrencyCache(controlClient, 1, 60).(*concurrencyCache)

	cacheInterface, err := newCellAwareConcurrencyCache(
		controlClient,
		admissionTestConfig(openAI.Addr(), anthropic.Addr()),
		legacy,
	)
	require.NoError(t, err)
	cache := cacheInterface.(*cellAwareConcurrencyCache)
	t.Cleanup(func() { require.NoError(t, cache.Close()) })

	ctx := context.Background()
	regular, err := cache.AcquireAccountSlotForPlatform(ctx, service.PlatformOpenAI, 610, 1, "regular")
	require.NoError(t, err)
	require.True(t, regular)

	live, err := cache.AcquireLiveAccountLease(ctx, service.PlatformOpenAI, 610, 1, "live", true)
	require.NoError(t, err)
	require.True(t, live)
	require.NoError(t, cache.ReleaseAccountSlot(ctx, 610, "regular"))

	require.Equal(t, int64(1), openAIClient.ZCard(ctx, liveAccountSlotKey(610)).Val())
	require.Equal(t, int64(0), controlClient.ZCard(ctx, liveAccountSlotKey(610)).Val())
	cellID, err := cache.directory.Cell(ctx, 610)
	require.NoError(t, err)
	require.Equal(t, "openai-001", cellID)

	refreshed, err := cache.RefreshLiveAccountLease(ctx, 610, "live")
	require.NoError(t, err)
	require.True(t, refreshed)
	require.NoError(t, cache.ReleaseLiveAccountLease(ctx, 610, "live"))
	require.NoError(t, cache.ReleaseLiveAccountLease(ctx, 610, "live"))
	require.Equal(t, int64(0), openAIClient.ZCard(ctx, liveAccountSlotKey(610)).Val())
}

func TestCellAwareLiveFailureInOneCellDoesNotBlockAnother(t *testing.T) {
	control := miniredis.RunT(t)
	openAIA := miniredis.RunT(t)
	openAIB := miniredis.RunT(t)
	anthropic := miniredis.RunT(t)
	controlClient := redis.NewClient(&redis.Options{Addr: control.Addr()})
	legacy := NewConcurrencyCache(controlClient, 1, 60).(*concurrencyCache)
	cfg := admissionTestConfig(openAIA.Addr(), anthropic.Addr())
	cfg.Gateway.Admission.OpenAICells = fmt.Sprintf(
		"openai-001=redis://%s/0,openai-002=redis://%s/0",
		openAIA.Addr(),
		openAIB.Addr(),
	)

	cacheInterface, err := newCellAwareConcurrencyCache(controlClient, cfg, legacy)
	require.NoError(t, err)
	cache := cacheInterface.(*cellAwareConcurrencyCache)
	t.Cleanup(func() { require.NoError(t, cache.Close()) })

	ctx := context.Background()
	// Even account IDs are frozen to openai-001, odd IDs to openai-002.
	_, err = cache.routeForPlatform(ctx, service.PlatformOpenAI, 620)
	require.NoError(t, err)
	_, err = cache.routeForPlatform(ctx, service.PlatformOpenAI, 621)
	require.NoError(t, err)
	openAIA.Close()

	failed, err := cache.AcquireLiveAccountLease(ctx, service.PlatformOpenAI, 620, 1, "failed-cell", false)
	require.Error(t, err)
	require.False(t, failed)

	acquired, err := cache.AcquireLiveAccountLease(ctx, service.PlatformOpenAI, 621, 1, "healthy-cell", false)
	require.NoError(t, err)
	require.True(t, acquired)
	healthyClient := redis.NewClient(&redis.Options{Addr: openAIB.Addr()})
	require.Equal(t, int64(1), healthyClient.ZCard(ctx, liveAccountSlotKey(621)).Val())
}

func TestCellAwareLiveTenantLeaseReturnsOnlyUnusedEscrowAndNeverOversells(t *testing.T) {
	control := miniredis.RunT(t)
	openAI := miniredis.RunT(t)
	anthropic := miniredis.RunT(t)
	controlClient := redis.NewClient(&redis.Options{Addr: control.Addr()})
	legacy := NewConcurrencyCache(controlClient, 1, 60).(*concurrencyCache)
	cfg := admissionTestConfig(openAI.Addr(), anthropic.Addr())
	cfg.Gateway.Admission.EscrowGrantSize = 4

	cacheInterface, err := newCellAwareConcurrencyCache(controlClient, cfg, legacy)
	require.NoError(t, err)
	cache := cacheInterface.(*cellAwareConcurrencyCache)
	t.Cleanup(func() { require.NoError(t, cache.Close()) })

	ctx := context.Background()
	ordinary, err := cache.AcquireUserSlot(ctx, 700, 8, "ordinary")
	require.NoError(t, err)
	require.True(t, ordinary)
	require.Equal(t, 1, cache.escrow.InUse("user:700"))

	state := cache.escrow.state("user:700")
	state.mu.Lock()
	initialGrants := state.grants
	state.mu.Unlock()
	require.Equal(t, 4, initialGrants)

	firstLive, err := cache.AcquireLiveTenantLease(ctx, 700, 8, 701, "live-1")
	require.NoError(t, err)
	require.True(t, firstLive)
	state.mu.Lock()
	grantsAfterLive := state.grants
	inUseAfterLive := state.inUse
	state.mu.Unlock()
	require.Equal(t, 1, grantsAfterLive)
	require.Equal(t, 1, inUseAfterLive)
	require.Equal(t, "1", controlClient.Get(ctx, cache.escrow.manager.allocatedKey("user:700")).Val())

	type liveResult struct {
		leaseID string
		ok      bool
		err     error
	}
	results := make(chan liveResult, 8)
	var wg sync.WaitGroup
	for index := 2; index <= 9; index++ {
		leaseID := fmt.Sprintf("live-%d", index)
		wg.Add(1)
		go func() {
			defer wg.Done()
			acquired, acquireErr := cache.AcquireLiveTenantLease(ctx, 700, 8, 701, leaseID)
			results <- liveResult{leaseID: leaseID, ok: acquired, err: acquireErr}
		}()
	}
	wg.Wait()
	close(results)
	acquiredLeases := []string{"live-1"}
	for result := range results {
		require.NoError(t, result.err)
		if result.ok {
			acquiredLeases = append(acquiredLeases, result.leaseID)
		}
	}
	require.Len(t, acquiredLeases, 7)

	count, err := cache.GetUserConcurrency(ctx, 700)
	require.NoError(t, err)
	require.Equal(t, 8, count)
	loads, err := cache.GetUsersLoadBatch(ctx, []service.UserWithConcurrency{{ID: 700, MaxConcurrency: 8}})
	require.NoError(t, err)
	require.Equal(t, 8, loads[700].CurrentConcurrency)
	require.Equal(t, 100, loads[700].LoadRate)
	for _, leaseID := range acquiredLeases {
		require.NoError(t, cache.ReleaseLiveTenantLease(ctx, 700, 701, leaseID))
	}
	require.NoError(t, cache.ReleaseLiveTenantLease(ctx, 700, 701, "live-1"))
	require.NoError(t, cache.ReleaseUserSlot(ctx, 700, "ordinary"))
}
