package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestGatewayCachePromptCacheWarmSkipsColdMissUntilFirstHit(t *testing.T) {
	miniRedis := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: miniRedis.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &gatewayCache{rdb: client}
	ctx := context.Background()

	require.NoError(t, cache.RecordOpenAIPromptCacheWarmResult(ctx, 7, "affinity", 19, 1000, 0, time.Hour))
	require.Empty(t, miniRedis.Keys())

	require.NoError(t, cache.RecordOpenAIPromptCacheWarmResult(ctx, 7, "affinity", 19, 1000, 700, time.Hour))
	entries, err := cache.GetOpenAIPromptCacheWarmAccounts(ctx, 7, "affinity")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, int64(19), entries[0].AccountID)
	require.Equal(t, 1, entries[0].Samples)
}

func TestGatewayCachePromptCacheWarmConcurrentUpdatesAreNotLost(t *testing.T) {
	miniRedis := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: miniRedis.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &gatewayCache{rdb: client}

	const requests = 16
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- cache.RecordOpenAIPromptCacheWarmResult(context.Background(), 7, "affinity", 19, 1000, 700, time.Hour)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	entries, err := cache.GetOpenAIPromptCacheWarmAccounts(context.Background(), 7, "affinity")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, requests, entries[0].Samples)
	require.Equal(t, int64(requests*1000), entries[0].InputTokens)
	require.Equal(t, int64(requests*700), entries[0].CachedTokens)
}

func TestGatewayCachePromptCacheWarmAvoidUsesAtomicScript(t *testing.T) {
	miniRedis := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: miniRedis.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &gatewayCache{rdb: client}
	ctx := context.Background()

	require.NoError(t, cache.RecordOpenAIPromptCacheWarmResult(ctx, 7, "affinity", 19, 1000, 700, time.Hour))
	until := time.Now().Add(time.Minute)
	require.NoError(t, cache.AvoidOpenAIPromptCacheWarmAccount(ctx, 7, "affinity", 19, until, time.Hour))
	entries, err := cache.GetOpenAIPromptCacheWarmAccounts(ctx, 7, "affinity")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, until.Unix(), entries[0].AvoidUntil)
}

func TestGatewayCacheStickyClaimDoesNotOverwriteExistingBinding(t *testing.T) {
	miniRedis := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: miniRedis.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &gatewayCache{rdb: client}
	ctx := context.Background()

	claimed, err := cache.SetSessionAccountIDIfAbsent(ctx, 7, "session", 19, time.Hour)
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = cache.SetSessionAccountIDIfAbsent(ctx, 7, "session", 20, time.Hour)
	require.NoError(t, err)
	require.False(t, claimed)
	accountID, err := cache.GetSessionAccountID(ctx, 7, "session")
	require.NoError(t, err)
	require.Equal(t, int64(19), accountID)
}
