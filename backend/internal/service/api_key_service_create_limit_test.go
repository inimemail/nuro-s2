//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type createCounterCacheStub struct {
	apiKeyCacheStub
	count        int64
	increments   int
	decrements   int
	incrementErr error
}

func (s *createCounterCacheStub) IncrementCreateCount(context.Context, int64, time.Duration) (int64, error) {
	s.increments++
	if s.incrementErr != nil {
		return 0, s.incrementErr
	}
	s.count++
	return s.count, nil
}

func (s *createCounterCacheStub) DecrementCreateCount(context.Context, int64) error {
	s.decrements++
	if s.count > 0 {
		s.count--
	}
	return nil
}

func TestAPIKeyServiceCreateLimits_ActiveCount(t *testing.T) {
	repo := &apiKeyRepoStub{countByUserID: func(context.Context, int64) (int64, error) { return 2, nil }}
	svc := &APIKeyService{apiKeyRepo: repo, cfg: &config.Config{APIKeyCreate: config.APIKeyCreateConfig{MaxActivePerUser: 2}}}

	rollback, err := svc.checkAPIKeyCreateLimits(context.Background(), 7)
	require.ErrorIs(t, err, ErrAPIKeyCountExceeded)
	rollback()
}

func TestAPIKeyServiceCreateLimits_CountFailureIsBlocking(t *testing.T) {
	dbErr := errors.New("database unavailable")
	repo := &apiKeyRepoStub{countByUserID: func(context.Context, int64) (int64, error) { return 0, dbErr }}
	svc := &APIKeyService{apiKeyRepo: repo, cfg: &config.Config{APIKeyCreate: config.APIKeyCreateConfig{MaxActivePerUser: 2}}}

	rollback, err := svc.checkAPIKeyCreateLimits(context.Background(), 7)
	require.ErrorIs(t, err, dbErr)
	rollback()
}

func TestAPIKeyServiceCreateLimits_RedisFailureIsFailOpen(t *testing.T) {
	cache := &createCounterCacheStub{incrementErr: errors.New("redis unavailable")}
	svc := &APIKeyService{cache: cache, cfg: &config.Config{APIKeyCreate: config.APIKeyCreateConfig{MaxPerUserPerHour: 1}}}

	rollback, err := svc.checkAPIKeyCreateLimits(context.Background(), 7)
	require.NoError(t, err)
	rollback()
}

func TestAPIKeyServiceCreateLimits_RollsBackFailedReservation(t *testing.T) {
	cache := &createCounterCacheStub{}
	svc := &APIKeyService{cache: cache, cfg: &config.Config{APIKeyCreate: config.APIKeyCreateConfig{MaxPerUserPerHour: 1}}}

	rollback, err := svc.checkAPIKeyCreateLimits(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, int64(1), cache.count)
	rollback()
	require.Equal(t, int64(0), cache.count)
	require.Equal(t, 1, cache.decrements)
}

func TestAPIKeyServiceCreateLimits_RejectsHourlyLimitWithoutConsumingSlot(t *testing.T) {
	cache := &createCounterCacheStub{count: 1}
	svc := &APIKeyService{cache: cache, cfg: &config.Config{APIKeyCreate: config.APIKeyCreateConfig{MaxPerUserPerHour: 1}}}

	rollback, err := svc.checkAPIKeyCreateLimits(context.Background(), 7)
	require.ErrorIs(t, err, ErrAPIKeyCreateLimited)
	require.Equal(t, int64(1), cache.count)
	require.Equal(t, 1, cache.decrements)
	rollback()
}

func TestAPIKeyServiceCreateLimits_ZeroDisablesBoth(t *testing.T) {
	repo := &apiKeyRepoStub{countByUserID: func(context.Context, int64) (int64, error) { return 999, errors.New("must not be called") }}
	cache := &createCounterCacheStub{}
	svc := &APIKeyService{apiKeyRepo: repo, cache: cache, cfg: &config.Config{}}

	rollback, err := svc.checkAPIKeyCreateLimits(context.Background(), 7)
	require.NoError(t, err)
	rollback()
	require.Zero(t, cache.increments)
}
