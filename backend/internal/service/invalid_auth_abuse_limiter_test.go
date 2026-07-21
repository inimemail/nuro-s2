package service

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func newInvalidAuthServiceForTest(threshold, capacity int) (*APIKeyService, *invalidAuthAbuseLimiter) {
	cfg := &config.Config{APIKeyAuth: config.APIKeyAuthCacheConfig{InvalidAbuse: config.InvalidAuthAbuseConfig{
		Enabled: true, Threshold: threshold, WindowSeconds: 60, BlockSeconds: 10, Capacity: capacity,
	}}}
	limiter := newInvalidAuthAbuseLimiter(cfg)
	return &APIKeyService{invalidAuthAbuse: limiter}, limiter
}

func TestInvalidAuthAbuseLimiterCapacityNeverGloballyBlocksUntrackedSources(t *testing.T) {
	service, limiter := newInvalidAuthServiceForTest(2, 2)
	now := time.Now()
	limiter.now = func() time.Time { return now }
	service.RecordInvalidAuthFailure("198.51.100.1")
	service.RecordInvalidAuthFailure("198.51.100.2")
	service.RecordInvalidAuthFailure("198.51.100.3")
	service.RecordInvalidAuthFailure("198.51.100.4")

	_, blocked := service.CheckInvalidAuthAbuse("198.51.100.5")
	require.False(t, blocked)
	_, trackedBlocked := service.CheckInvalidAuthAbuse("198.51.100.1")
	require.False(t, trackedBlocked)
	health := service.InvalidAuthAbuseHealth()
	require.Equal(t, int64(2), health.Tracked)
	require.EqualValues(t, 2, health.Overflowed)
	require.Zero(t, health.GlobalBlocked)
}

func TestInvalidAuthAbuseLimiterBlocksRepeatedFailuresForTrackedSource(t *testing.T) {
	service, limiter := newInvalidAuthServiceForTest(2, 2)
	now := time.Now()
	limiter.now = func() time.Time { return now }
	service.RecordInvalidAuthFailure("198.51.100.1")
	service.RecordInvalidAuthFailure("198.51.100.1")

	retry, blocked := service.CheckInvalidAuthAbuse("198.51.100.1")
	require.True(t, blocked)
	require.Equal(t, 10*time.Second, retry)
}

func TestInvalidAuthAbuseLimiterConcurrentCapacityIsBounded(t *testing.T) {
	const capacity = 64
	service, _ := newInvalidAuthServiceForTest(1000, capacity)
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			service.RecordInvalidAuthFailure(fmt.Sprintf("198.51.100.%d", i))
		}(i)
	}
	wg.Wait()
	health := service.InvalidAuthAbuseHealth()
	require.LessOrEqual(t, health.Tracked, int64(capacity))
	require.EqualValues(t, 1000, health.Recorded)
	require.EqualValues(t, 1000-capacity, health.Overflowed)
}

func TestInvalidAuthAbuseLimiterReclaimsExpiredCapacity(t *testing.T) {
	const capacity = 16
	service, limiter := newInvalidAuthServiceForTest(100, capacity)
	now := time.Now()
	limiter.now = func() time.Time { return now }
	for i := 0; i < capacity; i++ {
		service.RecordInvalidAuthFailure(fmt.Sprintf("source-%d", i))
	}
	now = now.Add(61 * time.Second)
	for i := 0; i < invalidAuthAbuseShardCount; i++ {
		service.CheckInvalidAuthAbuse(fmt.Sprintf("new-source-%d", i))
		now = now.Add(101 * time.Millisecond)
	}
	require.Less(t, service.InvalidAuthAbuseHealth().Tracked, int64(capacity))
	service.RecordInvalidAuthFailure("fresh-source")
	require.LessOrEqual(t, service.InvalidAuthAbuseHealth().Tracked, int64(capacity))
}
