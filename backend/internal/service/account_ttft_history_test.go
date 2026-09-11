package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type accountTTFTHistoryRepoStub struct {
	UsageLogRepository
	summaries  map[int64]AccountTTFTHistory
	err        error
	calls      int
	startTimes []time.Time
}

func (s *accountTTFTHistoryRepoStub) GetAccountTTFTHistoryBatch(_ context.Context, accountIDs []int64, startTime time.Time, _ int) (map[int64]AccountTTFTHistory, error) {
	s.calls++
	s.startTimes = append(s.startTimes, startTime)
	if s.err != nil {
		return nil, s.err
	}
	result := make(map[int64]AccountTTFTHistory)
	for _, id := range accountIDs {
		if summary, ok := s.summaries[id]; ok {
			result[id] = summary
		}
	}
	return result, nil
}

func TestAccountTTFTHistoryCacheSeparatesFreshnessWindows(t *testing.T) {
	now := time.Now()
	repo := &accountTTFTHistoryRepoStub{summaries: map[int64]AccountTTFTHistory{
		1: {AccountID: 1, SampleCount: 3, P50Ms: 200, P90Ms: 300, LatestAt: now.Add(-10 * time.Minute)},
	}}
	cache := newAccountTTFTHistoryCache(repo)

	require.NotEmpty(t, cache.loadWithin(context.Background(), []*Account{{ID: 1}}, now, 15*time.Minute))
	require.Empty(t, cache.loadWithin(context.Background(), []*Account{{ID: 1}}, now, 5*time.Minute))
	require.NotEmpty(t, cache.loadWithin(context.Background(), []*Account{{ID: 1}}, now, 15*time.Minute))
	require.Equal(t, 2, repo.calls)
	require.WithinDuration(t, now.Add(-15*time.Minute), repo.startTimes[0], time.Millisecond)
	require.WithinDuration(t, now.Add(-5*time.Minute), repo.startTimes[1], time.Millisecond)
}

func TestAccountTTFTHistoryCacheBatchesAndCachesAccountWideResults(t *testing.T) {
	repo := &accountTTFTHistoryRepoStub{summaries: map[int64]AccountTTFTHistory{
		1: {AccountID: 1, SampleCount: 8, P50Ms: 250, P90Ms: 400},
	}}
	cache := newAccountTTFTHistoryCache(repo)
	now := time.Now()
	accounts := []*Account{{ID: 1}, {ID: 2}, {ID: 1}}

	first := cache.load(context.Background(), accounts, now)
	second := cache.load(context.Background(), accounts, now.Add(time.Minute))
	require.Equal(t, int64(8), first[1].SampleCount)
	require.Equal(t, first, second)
	require.Equal(t, 1, repo.calls)
}

func TestAccountTTFTHistoryCacheFailsOpenAndNegativeCachesError(t *testing.T) {
	repo := &accountTTFTHistoryRepoStub{err: errors.New("database unavailable")}
	cache := newAccountTTFTHistoryCache(repo)
	now := time.Now()

	require.Empty(t, cache.load(context.Background(), []*Account{{ID: 1}}, now))
	require.Empty(t, cache.load(context.Background(), []*Account{{ID: 1}}, now.Add(time.Second)))
	require.Equal(t, 1, repo.calls)
}

func TestAccountTTFTHistoryCacheFailureBackoffSurvivesExpiredEntryCleanup(t *testing.T) {
	repo := &accountTTFTHistoryRepoStub{err: errors.New("database unavailable")}
	cache := newAccountTTFTHistoryCache(repo)
	now := time.Now()
	accounts := []*Account{{ID: 1}}

	require.Empty(t, cache.load(context.Background(), accounts, now))
	require.Empty(t, cache.load(context.Background(), accounts, now.Add(31*time.Second)))
	require.Empty(t, cache.load(context.Background(), accounts, now.Add(62*time.Second)))
	require.Equal(t, 2, repo.calls, "the second failure must retain its one-minute retry backoff")
}

func TestAccountTTFTHistoryCacheKeepsLastSuccessWhenRefreshFails(t *testing.T) {
	repo := &accountTTFTHistoryRepoStub{summaries: map[int64]AccountTTFTHistory{
		1: {AccountID: 1, SampleCount: 8, P50Ms: 250, P90Ms: 400},
	}}
	cache := newAccountTTFTHistoryCache(repo)
	now := time.Now()
	accounts := []*Account{{ID: 1}}

	first := cache.load(context.Background(), accounts, now)
	require.Equal(t, 250.0, first[1].P50Ms)
	require.Equal(t, 1, repo.calls)

	repo.err = errors.New("database unavailable")
	stale := cache.load(context.Background(), accounts, now.Add(accountTTFTHistoryTTL+time.Second))
	require.Equal(t, first, stale)
	require.Equal(t, 2, repo.calls)

	stillCached := cache.load(context.Background(), accounts, now.Add(accountTTFTHistoryTTL+2*time.Second))
	require.Equal(t, first, stillCached)
	require.Equal(t, 2, repo.calls)
}

func TestAccountTTFTHistoryRetryDelayBacksOffAndCaps(t *testing.T) {
	require.Equal(t, 30*time.Second, accountTTFTHistoryRetryDelay(1))
	require.Equal(t, time.Minute, accountTTFTHistoryRetryDelay(2))
	require.Equal(t, 2*time.Minute, accountTTFTHistoryRetryDelay(3))
	require.Equal(t, 4*time.Minute, accountTTFTHistoryRetryDelay(4))
	require.Equal(t, 5*time.Minute, accountTTFTHistoryRetryDelay(5))
	require.Equal(t, 5*time.Minute, accountTTFTHistoryRetryDelay(20))
}
