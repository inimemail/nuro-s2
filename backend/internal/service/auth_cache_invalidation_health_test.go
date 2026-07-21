package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dgraph-io/ristretto"
	"github.com/stretchr/testify/require"
)

type healthOutboxRepo struct {
	stats AuthCacheInvalidationOutboxStats
}

func (r *healthOutboxRepo) Claim(context.Context, string, int, time.Duration) ([]AuthCacheInvalidationEvent, error) {
	return nil, nil
}
func (r *healthOutboxRepo) DeleteClaimed(context.Context, int64, string) error { return nil }
func (r *healthOutboxRepo) ScheduleSecondPass(context.Context, int64, string, time.Time) error {
	return nil
}
func (r *healthOutboxRepo) RetryClaimed(context.Context, int64, string, time.Time, string) error {
	return nil
}
func (r *healthOutboxRepo) Stats(context.Context) (AuthCacheInvalidationOutboxStats, error) {
	return r.stats, nil
}

type healthAuthCache struct {
	mu      sync.Mutex
	closed  chan struct{}
	started chan struct{}
}

func (c *healthAuthCache) GetCreateAttemptCount(context.Context, int64) (int, error) { return 0, nil }
func (c *healthAuthCache) IncrementCreateAttemptCount(context.Context, int64) error  { return nil }
func (c *healthAuthCache) DeleteCreateAttemptCount(context.Context, int64) error     { return nil }
func (c *healthAuthCache) IncrementDailyUsage(context.Context, string) error         { return nil }
func (c *healthAuthCache) SetDailyUsageExpiry(context.Context, string, time.Duration) error {
	return nil
}
func (c *healthAuthCache) GetAuthCache(context.Context, string) (*APIKeyAuthCacheEntry, error) {
	return nil, errors.New("miss")
}
func (c *healthAuthCache) SetAuthCache(context.Context, string, *APIKeyAuthCacheEntry, time.Duration) error {
	return nil
}
func (c *healthAuthCache) DeleteAuthCache(context.Context, string) error              { return nil }
func (c *healthAuthCache) PublishAuthCacheInvalidation(context.Context, string) error { return nil }
func (c *healthAuthCache) SubscribeAuthCacheInvalidation(ctx context.Context, _ func(string)) error {
	NotifyAuthCacheSubscriptionReady(ctx)
	select {
	case <-c.started:
	default:
		close(c.started)
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestAuthCacheInvalidationWorkerHealthIncludesLagAndStatsErrorBoundary(t *testing.T) {
	oldest := time.Now().Add(-time.Minute)
	worker := NewAuthCacheInvalidationWorker(&healthOutboxRepo{stats: AuthCacheInvalidationOutboxStats{
		Pending: 4, OldestCreatedAt: &oldest, MaxAttempts: 3, LastError: "redis unavailable",
	}}, nil, nil)
	health := worker.Health(context.Background())
	require.Equal(t, int64(4), health.Pending)
	require.Equal(t, 3, health.MaxAttempts)
	require.Equal(t, "redis unavailable", health.LastError)
	require.GreaterOrEqual(t, health.OldestLag, time.Minute)
	require.Equal(t, 35*time.Second, health.HealthySLA)
	require.Equal(t, 6*time.Minute, health.RecoverySLA)
}

func TestBoundedAuthInvalidationErrorPreservesUTF8(t *testing.T) {
	got := boundedAuthInvalidationError(errors.New(""))
	require.Empty(t, got)

	got = boundedAuthInvalidationError(errors.New("failure: " + strings.Repeat("界", 400)))
	require.LessOrEqual(t, len(got), 1024)
	require.True(t, utf8.ValidString(got))
}

func TestAuthInvalidationRetriesAreBoundedByAttemptsAndAge(t *testing.T) {
	now := time.Now().UTC()
	require.False(t, shouldDropAuthInvalidation(AuthCacheInvalidationEvent{
		Attempts: authInvalidationMaxAttempts - 2, CreatedAt: now.Add(-time.Minute),
	}, now))
	require.True(t, shouldDropAuthInvalidation(AuthCacheInvalidationEvent{
		Attempts: authInvalidationMaxAttempts - 1, CreatedAt: now.Add(-time.Minute),
	}, now))
	require.True(t, shouldDropAuthInvalidation(AuthCacheInvalidationEvent{
		Attempts: 1, CreatedAt: now.Add(-authInvalidationMaxAge),
	}, now))
}

func TestAuthCacheInvalidationSubscriberStopsWithService(t *testing.T) {
	cache := &healthAuthCache{started: make(chan struct{}), closed: make(chan struct{})}
	local, err := ristretto.NewCache(&ristretto.Config{NumCounters: 10, MaxCost: 1, BufferItems: 64})
	require.NoError(t, err)
	defer local.Close()
	svc := &APIKeyService{cache: cache, authCacheL1: local}
	svc.StartAuthCacheInvalidationSubscriber(context.Background())
	select {
	case <-cache.started:
	case <-time.After(time.Second):
		t.Fatal("subscriber did not start")
	}
	require.Eventually(t, func() bool {
		return svc.AuthCacheInvalidationSubscriberHealth().Connected
	}, time.Second, 5*time.Millisecond)
	svc.StopAuthCacheInvalidationSubscriber()
	require.Eventually(t, func() bool {
		return !svc.AuthCacheInvalidationSubscriberHealth().Connected
	}, time.Second, 5*time.Millisecond)
}
