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

func TestUnlimitedAccountAdmissionHonorsOtherReplicaCooldown(t *testing.T) {
	server := miniredis.RunT(t)
	writer := redis.NewClient(&redis.Options{Addr: server.Addr()})
	reader := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = writer.Close(); _ = reader.Close() })
	cache := NewConcurrencyCache(reader, 15, 900)
	scheduler := service.NewConcurrencyService(cache)
	t.Cleanup(scheduler.StopBackgroundWorkers)
	ctx := context.Background()

	// This process has no local account block. Only the other replica's
	// shared cooldown can prevent admission of an unlimited account.
	require.NoError(t, writer.Set(ctx, accountCooldownKey(71), "1", time.Minute).Err())
	for _, limit := range []int{0, -1} {
		result, err := scheduler.AcquireAccountSlotForPlatform(ctx, service.PlatformOpenAI, 71, limit)
		require.NoError(t, err)
		require.False(t, result.Acquired)
	}
	result, err := scheduler.AcquireFirstAvailableAccountSlot(ctx, []service.AccountSlotCandidate{
		{AccountID: 71, MaxConcurrency: 0}, {AccountID: 72, MaxConcurrency: 1},
	})
	require.NoError(t, err)
	require.True(t, result.Acquired)
	require.Equal(t, int64(72), result.AccountID)
	result.ReleaseFunc()

	server.FastForward(time.Minute)
	unlimited, err := scheduler.AcquireAccountSlotForPlatform(ctx, service.PlatformOpenAI, 71, 0)
	require.NoError(t, err)
	require.True(t, unlimited.Acquired)
	unlimited.ReleaseFunc()
	batch, err := scheduler.AcquireFirstAvailableAccountSlot(ctx, []service.AccountSlotCandidate{{AccountID: 71, MaxConcurrency: -1}})
	require.NoError(t, err)
	require.True(t, batch.Acquired)
	batch.ReleaseFunc()
	require.False(t, server.Exists(accountSlotKey(71)), "unlimited admission must not create or renew account leases")
}
