package repository

import (
	"context"
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
