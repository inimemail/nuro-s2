package repository

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestGeminiCacheAdmissionFencesAndWindows(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	c := &billingCache{rdb: rdb}
	ctx := t.Context()
	key := "test:gc"
	set := func(gen int64, fields map[string]any) {
		require.NoError(t, c.setGeminiFencedHash(ctx, key, fields, gen, time.Hour))
	}
	set(0, map[string]any{"usage_5h": 1, "window_5h": 10})
	require.False(t, mr.Exists(key+":gc-generation"), "ordinary sites do not create financial fence keys")
	set(2, map[string]any{"usage_5h": 5, "window_5h": 10, "gemini_cache_pending": 2})
	set(1, map[string]any{"usage_5h": 1, "window_5h": 10, "gemini_cache_pending": 0})
	require.Equal(t, "2", mr.HGet(key, "gemini_cache_pending"))
	set(2, map[string]any{"usage_5h": 1, "window_5h": 10})
	require.Equal(t, "5", mr.HGet(key, "usage_5h"), "late snapshot cannot erase ordinary usage")
	set(3, map[string]any{"usage_5h": 0, "window_5h": 20})
	require.Equal(t, "0", mr.HGet(key, "usage_5h"), "a new period must reset old usage")
	set(3, map[string]any{"usage_5h": 99, "window_5h": 10})
	require.Equal(t, "20", mr.HGet(key, "window_5h"), "delayed fills cannot rewind a period")
	require.Equal(t, "0", mr.HGet(key, "usage_5h"), "old period usage cannot leak into the new period")
	set(3, map[string]any{"daily_usage": 7, "daily_window_start": 10, "version": 100})
	set(3, map[string]any{"daily_usage": 1, "daily_window_start": 10, "version": 100})
	require.Equal(t, "7", mr.HGet(key, "daily_usage"))
	set(4, map[string]any{"daily_usage": 0, "daily_window_start": 20, "version": 100})
	require.Equal(t, "0", mr.HGet(key, "daily_usage"), "same-second reset uses the period, not update time")
	set(4, map[string]any{"daily_usage": 2, "daily_window_start": 20, "version": 101})
	set(4, map[string]any{"daily_usage": 1, "daily_window_start": 20, "version": 100})
	require.Equal(t, "2", mr.HGet(key, "daily_usage"))
	set(4, map[string]any{"daily_usage": 99, "daily_window_start": 10, "version": 100})
	require.Equal(t, "20", mr.HGet(key, "daily_window_start"))
	require.Equal(t, "2", mr.HGet(key, "daily_usage"))
	// The balance miss path allows replenishment after explicit invalidation,
	// while same-generation delayed fills cannot restore a prior balance.
	require.NoError(t, c.SetUserBalanceWithGeneration(ctx, 7, 10, 1))
	require.NoError(t, c.SetUserBalanceWithGeneration(ctx, 7, 20, 1))
	value, e := mr.Get(billingBalanceKey(7))
	require.NoError(t, e)
	require.Equal(t, "10", value)
	require.NoError(t, c.SetUserBalanceWithGeneration(ctx, 7, 50, 0))
	value, e = mr.Get(billingBalanceKey(7))
	require.NoError(t, e)
	require.Equal(t, "10", value)
	mr.Del(billingBalanceKey(7))
	require.NoError(t, c.SetUserBalanceWithGeneration(ctx, 7, 20, 1))
	value, e = mr.Get(billingBalanceKey(7))
	require.NoError(t, e)
	require.Equal(t, "20", value)
}

func TestGeminiCacheAuthSnapshotAdvancesGenerationFence(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	c := &apiKeyCache{rdb: rdb}
	set := func(generation int64, used float64) {
		require.NoError(t, c.SetAuthCache(t.Context(), "test-key-hash", &service.APIKeyAuthCacheEntry{Snapshot: &service.APIKeyAuthSnapshot{GeminiCacheGeneration: generation, QuotaUsed: used}}, time.Minute))
	}
	set(0, 0)
	require.False(t, mr.Exists(apiKeyAuthCacheKey("test-key-hash")+":gc-generation"))
	set(2, 0)
	set(1, 99)
	entry, err := c.GetAuthCache(t.Context(), "test-key-hash")
	require.NoError(t, err)
	require.Equal(t, int64(2), entry.Snapshot.GeminiCacheGeneration)
	require.Zero(t, entry.Snapshot.QuotaUsed, "delayed pre-reset snapshot cannot overwrite the new snapshot")
}

func TestGeminiCacheUsageResetWithinSameWindow(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	c := &billingCache{rdb: rdb}
	for _, pair := range [][2]string{{"usage_5h", "window_5h"}, {"daily_usage", "daily_window_start"}} {
		t.Run(pair[0], func(t *testing.T) {
			key := "test:reset:" + pair[0]
			set := func(gen int64, usage int) {
				require.NoError(t, c.setGeminiFencedHash(t.Context(), key, map[string]any{
					pair[0]: usage, pair[1]: 100, "gemini_cache_generation": gen, "gemini_cache_pending": 2,
				}, gen, time.Hour))
			}
			set(1, 9)
			// A late refill may arrive after invalidation but before the reset
			// snapshot. Both window starts can serialize to the same second.
			mr.Del(key)
			set(1, 9)
			require.NoError(t, c.invalidateGeminiFencedHash(t.Context(), key, 2))
			set(1, 9)
			require.False(t, mr.Exists(key), "old refill is fenced before the reset snapshot arrives")
			set(2, 0)
			require.Equal(t, "0", mr.HGet(key, pair[0]), "new generation must permit an explicit usage reset")
			require.Equal(t, "2", mr.HGet(key, "gemini_cache_pending"))
			set(1, 9)
			require.Equal(t, "0", mr.HGet(key, pair[0]), "old generation cannot undo the reset")
			set(2, 3)
			set(2, 0)
			require.Equal(t, "3", mr.HGet(key, pair[0]), "same-generation refill still preserves subsequent usage")
			set(3, 1)
			require.Equal(t, "3", mr.HGet(key, pair[0]), "normal financial generation cannot lower concurrent usage")
			require.NoError(t, c.invalidateGeminiFencedHash(t.Context(), key, 2))
			require.Equal(t, "3", mr.HGet(key, pair[0]), "late invalidation cannot delete a newer generation")
		})
	}
	key := "test:platform-generation"
	for _, snapshot := range []struct {
		generation int64
		usage      int
	}{{1, 9}, {2, 1}} {
		require.NoError(t, c.setGeminiFencedHash(t.Context(), key, map[string]any{
			"schema_version": 1, "daily_usage": snapshot.usage, "daily_window_start": 100,
			"gemini_cache_generation": snapshot.generation,
		}, snapshot.generation, time.Hour))
	}
	require.Equal(t, "9", mr.HGet(key, "daily_usage"), "financial generation must not lower Redis-authoritative platform usage")
}

func TestGeminiCacheResetInvalidationCacheKeys(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	c := &billingCache{rdb: rdb}
	require.NoError(t, c.SetSubscriptionCache(t.Context(), 1, 2, &service.SubscriptionCacheData{Status: "active", GeminiCacheGeneration: 1, DailyUsage: 9}))
	require.NoError(t, c.InvalidateSubscriptionCacheWithGeneration(t.Context(), 1, 2, 2))
	require.False(t, mr.Exists(billingSubKey(1, 2)))
	generation, err := mr.Get(billingSubKey(1, 2) + ":gc-generation")
	require.NoError(t, err)
	require.Equal(t, "2", generation)
	require.NoError(t, c.SetAPIKeyRateLimit(t.Context(), 3, &service.APIKeyRateLimitCacheData{GeminiCacheGeneration: 1, Usage5h: 9}))
	require.NoError(t, c.InvalidateAPIKeyRateLimitWithGeneration(t.Context(), 3, 2))
	require.False(t, mr.Exists(billingRateLimitKey(3)))
	generation, err = mr.Get(billingRateLimitKey(3) + ":gc-generation")
	require.NoError(t, err)
	require.Equal(t, "2", generation)
}
