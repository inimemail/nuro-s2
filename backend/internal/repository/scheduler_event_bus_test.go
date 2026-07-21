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

func TestRedisSchedulerEventBusUsesConcreteInitialCursor(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	bus := NewRedisSchedulerEventBus(client).(*redisSchedulerEventBus)
	require.NoError(t, bus.Publish(context.Background(), service.SchedulerEvent{Type: service.SchedulerEventAccountUpdated, AccountID: 1}))

	lastID, err := bus.latestStreamID(context.Background())
	require.NoError(t, err)
	require.NotEqual(t, "$", lastID)
	require.NotEqual(t, "0-0", lastID)

	require.NoError(t, bus.Publish(context.Background(), service.SchedulerEvent{Type: service.SchedulerEventAccountRuntimeCleared, AccountID: 2, Generation: 1}))
	streams, err := client.XRead(context.Background(), &redis.XReadArgs{
		Streams: []string{schedulerEventStreamKey, lastID},
		Count:   1,
		Block:   time.Second,
	}).Result()
	require.NoError(t, err)
	require.Len(t, streams, 1)
	require.Len(t, streams[0].Messages, 1)
	event, ok := parseRedisSchedulerEvent(streams[0].Messages[0].Values)
	require.True(t, ok)
	require.Equal(t, int64(2), event.AccountID)
	require.Equal(t, int64(1), event.Generation)
}

func TestRedisSchedulerEventBusRuntimeClearGenerationIsMonotonic(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	bus := NewRedisSchedulerEventBus(client).(*redisSchedulerEventBus)

	first, err := bus.AdvanceAccountRuntimeClearGeneration(context.Background(), 9)
	require.NoError(t, err)
	second, err := bus.AdvanceAccountRuntimeClearGeneration(context.Background(), 9)
	require.NoError(t, err)
	loaded, err := bus.LoadAccountRuntimeClearGeneration(context.Background(), 9)
	require.NoError(t, err)
	require.Equal(t, int64(1), first)
	require.Equal(t, int64(2), second)
	require.Equal(t, second, loaded)
}

func TestVersionedAccountCooldownClearPreservesCurrentGeneration(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &concurrencyCache{rdb: client}
	ctx := context.Background()
	accountID := int64(10)

	require.NoError(t, cache.SetAccountCooldownGeneration(ctx, accountID, time.Minute, 2))
	require.NoError(t, cache.ClearAccountCooldownBeforeGeneration(ctx, accountID, 2))
	exists, err := client.Exists(ctx, accountCooldownKey(accountID)).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists)

	require.NoError(t, cache.ClearAccountCooldownBeforeGeneration(ctx, accountID, 3))
	exists, err = client.Exists(ctx, accountCooldownKey(accountID)).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
}

func TestVersionedAccountCooldownClearRemovesLegacyValue(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &concurrencyCache{rdb: client}
	ctx := context.Background()
	accountID := int64(11)

	require.NoError(t, cache.SetAccountCooldown(ctx, accountID, time.Minute))
	require.NoError(t, cache.ClearAccountCooldownBeforeGeneration(ctx, accountID, 1))
	exists, err := client.Exists(ctx, accountCooldownKey(accountID)).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
}
