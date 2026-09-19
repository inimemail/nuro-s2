//go:build unit

package repository

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestV027SeedanceQuotaOutboxRetryDoesNotDoubleCount(t *testing.T) {
	c, mr := newMiniRedisCache(t)
	ctx := context.Background()
	limit := 100.0
	require.Error(t, c.ApplySeedancePlatformQuota(ctx, "task", 1, "openai", 2, time.Minute))
	require.NoError(t, c.SetUserPlatformQuotaCache(ctx, 1, "openai", &service.UserPlatformQuotaCacheEntry{SchemaVersion: service.UserPlatformQuotaCacheSchemaV1, DailyLimitUSD: &limit, DailyUsageUSD: 10}, time.Minute))
	for i := 0; i < 3; i++ {
		require.NoError(t, c.ApplySeedancePlatformQuota(ctx, "task", 1, "openai", 2, time.Minute))
	}
	entry, ok, err := c.GetUserPlatformQuotaCache(ctx, 1, "openai")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 12.0, entry.DailyUsageUSD)
	require.True(t, mr.Exists("billing:seedance-quota:task"))
	require.True(t, mr.Exists(userPlatformQuotaDirtySetKey()))
}
