package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type ingressRejectRepoStub struct {
	mu      sync.Mutex
	items   []*OpsIngressRejectAggregate
	flushes int
	err     error
}

func (r *ingressRejectRepoStub) BatchUpsertIngressRejects(_ context.Context, items []*OpsIngressRejectAggregate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushes++
	if r.err != nil {
		return r.err
	}
	for _, item := range items {
		if item == nil {
			continue
		}
		copy := *item
		r.items = append(r.items, &copy)
	}
	return nil
}

func (r *ingressRejectRepoStub) ListIngressRejects(context.Context, *OpsIngressRejectFilter) (*OpsIngressRejectList, error) {
	return &OpsIngressRejectList{}, nil
}

func TestOpsIngressRejectAggregatorConcurrentCountAndStopFlush(t *testing.T) {
	repo := &ingressRejectRepoStub{}
	aggregator := NewOpsIngressRejectAggregator(repo)
	aggregator.Start()

	var wg sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				aggregator.RecordIngressReject("invalid_auth", "responses", "openai", "203.0.113.1", 0, 0)
			}
		}()
	}
	wg.Wait()
	aggregator.Stop()

	require.Len(t, repo.items, 1)
	require.EqualValues(t, 3200, repo.items[0].RequestCount)
	require.EqualValues(t, 3200, aggregator.Health().Flushed)
	require.False(t, aggregator.Health().Accepting)
}

func TestOpsIngressRejectAggregatorCapacityIsBounded(t *testing.T) {
	aggregator := NewOpsIngressRejectAggregator(&ingressRejectRepoStub{})
	aggregator.accepting.Store(true)
	for i := 0; i < ingressRejectMaxEntries+1; i++ {
		aggregator.RecordIngressReject("invalid_auth", "responses", "openai", fmt.Sprintf("10.0.%d.%d", i/256, i%256), 0, 0)
	}
	health := aggregator.Health()
	require.Equal(t, ingressRejectMaxEntries, int(health.Cardinality))
	require.EqualValues(t, 2, health.Overflowed)
}

func TestOpsIngressRejectAggregatorFlushFailureKeepsBoundedPendingBatch(t *testing.T) {
	repo := &ingressRejectRepoStub{err: errors.New("postgres internal details")}
	aggregator := NewOpsIngressRejectAggregator(repo)
	aggregator.Start()
	aggregator.RecordIngressReject("invalid_auth", "responses", "openai", "203.0.113.2", 0, 0)
	aggregator.Stop()

	health := aggregator.Health()
	require.Equal(t, 1, health.PendingBatches)
	require.EqualValues(t, 1, health.FlushFailures)
	require.Equal(t, "flush failed", health.LastError)
	require.NotContains(t, health.LastError, "postgres")
}
