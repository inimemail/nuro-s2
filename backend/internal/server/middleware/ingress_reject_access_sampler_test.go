package middleware

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIngressRejectAccessSamplerConcurrentGlobalLimit(t *testing.T) {
	sampler := newIngressRejectAccessSampler(10, time.Hour, time.Minute)
	now := time.Now()
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := sampler.allow(now); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int64(10), allowed.Load())
}

func TestIngressRejectAccessSamplerDroppedSummaryIsLowFrequency(t *testing.T) {
	sampler := newIngressRejectAccessSampler(1, time.Hour, time.Second)
	now := time.Now()
	allowed, summary := sampler.allow(now)
	require.True(t, allowed)
	require.Zero(t, summary)

	allowed, summary = sampler.allow(now.Add(100 * time.Millisecond))
	require.False(t, allowed)
	require.Equal(t, uint64(1), summary)
	allowed, summary = sampler.allow(now.Add(200 * time.Millisecond))
	require.False(t, allowed)
	require.Zero(t, summary)
	allowed, summary = sampler.allow(now.Add(2 * time.Second))
	require.False(t, allowed)
	require.Equal(t, uint64(2), summary)
}
