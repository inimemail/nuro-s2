//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRefreshTokenConsumeRealRedis(t *testing.T) {
	ctx := context.Background()
	rdb := testRedis(t)
	cache := NewRefreshTokenCache(rdb)
	require.NoError(t, cache.StoreRefreshToken(ctx, "atomic", &service.RefreshTokenData{UserID: 7, FamilyID: "fixture"}, time.Minute))
	const callers = 32
	type result struct {
		data *service.RefreshTokenData
		err  error
	}
	results := make(chan result, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			data, err := cache.ConsumeRefreshToken(ctx, "atomic")
			results <- result{data: data, err: err}
		}()
	}
	close(start)
	successes := 0
	for i := 0; i < callers; i++ {
		r := <-results
		if r.err == nil {
			successes++
			require.Equal(t, int64(7), r.data.UserID)
		} else {
			require.ErrorIs(t, r.err, service.ErrRefreshTokenNotFound)
		}
	}
	require.Equal(t, 1, successes)

	require.NoError(t, cache.StoreRefreshToken(ctx, "expired", &service.RefreshTokenData{UserID: 7}, time.Minute))
	require.NoError(t, rdb.PExpire(ctx, refreshTokenKey("expired"), -time.Second).Err())
	_, err := cache.ConsumeRefreshToken(ctx, "expired")
	require.ErrorIs(t, err, service.ErrRefreshTokenNotFound)

	require.NoError(t, rdb.Set(ctx, refreshTokenKey("malformed"), "not JSON", time.Minute).Err())
	_, err = cache.ConsumeRefreshToken(ctx, "malformed")
	require.Error(t, err)
	require.False(t, errors.Is(err, service.ErrRefreshTokenNotFound))
	_, err = cache.ConsumeRefreshToken(ctx, "malformed")
	require.ErrorIs(t, err, service.ErrRefreshTokenNotFound, "malformed data must not remain reusable")
}
