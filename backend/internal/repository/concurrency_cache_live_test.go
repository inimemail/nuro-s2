package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestLiveTenantAndAccountLeasesCountTowardOrdinaryLimits(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	regular := NewConcurrencyCache(client, 15, 900)
	concrete := regular.(*concurrencyCache)
	live, ok := regular.(service.LiveConcurrencyCache)
	require.True(t, ok)
	ctx := context.Background()

	tenantAcquired, err := live.AcquireLiveTenantLease(ctx, 20, 1, 30, "live-lease")
	require.NoError(t, err)
	require.True(t, tenantAcquired)

	regularAccountAcquired, err := regular.AcquireAccountSlot(ctx, 10, 1, "scheduler-slot")
	require.NoError(t, err)
	require.True(t, regularAccountAcquired)
	liveAccountAcquired, err := live.AcquireLiveAccountLease(ctx, service.PlatformOpenAI, 10, 1, "live-lease", true)
	require.NoError(t, err)
	require.True(t, liveAccountAcquired)
	require.NoError(t, regular.ReleaseAccountSlot(ctx, 10, "scheduler-slot"))

	accountCount, err := regular.GetAccountConcurrency(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, accountCount)
	userCount, err := regular.GetUserConcurrency(ctx, 20)
	require.NoError(t, err)
	require.Equal(t, 1, userCount)
	apiKeyCounts, err := concrete.GetAPIKeyConcurrencyBatch(ctx, []int64{30})
	require.NoError(t, err)
	require.Equal(t, 1, apiKeyCounts[30])

	ordinaryAccount, err := regular.AcquireAccountSlot(ctx, 10, 1, "ordinary-account")
	require.NoError(t, err)
	require.False(t, ordinaryAccount)
	ordinaryUser, err := regular.AcquireUserSlot(ctx, 20, 1, "ordinary-user")
	require.NoError(t, err)
	require.False(t, ordinaryUser)

	tenantRefreshed, err := live.RefreshLiveTenantLease(ctx, 20, 30, "live-lease")
	require.NoError(t, err)
	require.True(t, tenantRefreshed)
	accountRefreshed, err := live.RefreshLiveAccountLease(ctx, 10, "live-lease")
	require.NoError(t, err)
	require.True(t, accountRefreshed)

	require.NoError(t, live.ReleaseLiveAccountLease(ctx, 10, "live-lease"))
	require.NoError(t, live.ReleaseLiveTenantLease(ctx, 20, 30, "live-lease"))
	ordinaryAccount, err = regular.AcquireAccountSlot(ctx, 10, 1, "ordinary-after-release")
	require.NoError(t, err)
	require.True(t, ordinaryAccount)
}

func TestLiveLeaseRefreshFailsClosedAfterExpiry(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	regular := NewConcurrencyCache(client, 15, 900)
	live := regular.(service.LiveConcurrencyCache)
	ctx := context.Background()

	tenantAcquired, err := live.AcquireLiveTenantLease(ctx, 20, 2, 30, "expired-live")
	require.NoError(t, err)
	require.True(t, tenantAcquired)
	accountAcquired, err := live.AcquireLiveAccountLease(ctx, service.PlatformOpenAI, 10, 1, "expired-live", false)
	require.NoError(t, err)
	require.True(t, accountAcquired)

	server.FastForward(61 * time.Second)
	tenantRefreshed, err := live.RefreshLiveTenantLease(ctx, 20, 30, "expired-live")
	require.NoError(t, err)
	require.False(t, tenantRefreshed)
	accountRefreshed, err := live.RefreshLiveAccountLease(ctx, 10, "expired-live")
	require.NoError(t, err)
	require.False(t, accountRefreshed)

	ordinaryAccount, err := regular.AcquireAccountSlot(ctx, 10, 1, "ordinary-after-expiry")
	require.NoError(t, err)
	require.True(t, ordinaryAccount)
}

func TestLiveLeaseIdempotentAcquireRefreshesAllTTLs(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	regular := NewConcurrencyCache(client, 15, 900)
	live := regular.(service.LiveConcurrencyCache)
	ctx := context.Background()

	tenantAcquired, err := live.AcquireLiveTenantLease(ctx, 20, 2, 30, "live-retry")
	require.NoError(t, err)
	require.True(t, tenantAcquired)
	accountAcquired, err := live.AcquireLiveAccountLease(ctx, service.PlatformOpenAI, 10, 1, "live-retry", false)
	require.NoError(t, err)
	require.True(t, accountAcquired)

	server.FastForward(50 * time.Second)
	tenantAcquired, err = live.AcquireLiveTenantLease(ctx, 20, 2, 30, "live-retry")
	require.NoError(t, err)
	require.True(t, tenantAcquired)
	accountAcquired, err = live.AcquireLiveAccountLease(ctx, service.PlatformOpenAI, 10, 1, "live-retry", false)
	require.NoError(t, err)
	require.True(t, accountAcquired)

	server.FastForward(20 * time.Second)
	tenantRefreshed, err := live.RefreshLiveTenantLease(ctx, 20, 30, "live-retry")
	require.NoError(t, err)
	require.True(t, tenantRefreshed)
	accountRefreshed, err := live.RefreshLiveAccountLease(ctx, 10, "live-retry")
	require.NoError(t, err)
	require.True(t, accountRefreshed)
}

func TestLiveAccountLeaseReplacementDoesNotOversell(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	regular := NewConcurrencyCache(client, 15, 900)
	live := regular.(service.LiveConcurrencyCache)
	ctx := context.Background()

	first, err := regular.AcquireAccountSlot(ctx, 10, 1, "scheduler-slot")
	require.NoError(t, err)
	require.True(t, first)
	replaced, err := live.AcquireLiveAccountLease(ctx, service.PlatformOpenAI, 10, 1, "live-1", true)
	require.NoError(t, err)
	require.True(t, replaced)
	second, err := live.AcquireLiveAccountLease(ctx, service.PlatformOpenAI, 10, 1, "live-2", false)
	require.NoError(t, err)
	require.False(t, second)
}
