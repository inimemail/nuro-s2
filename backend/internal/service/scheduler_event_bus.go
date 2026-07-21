package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

type SchedulerEventType string

const (
	SchedulerEventSnapshotUpdated SchedulerEventType = "snapshot_updated"
	SchedulerEventSnapshotDeleted SchedulerEventType = "snapshot_deleted"
	SchedulerEventAccountUpdated  SchedulerEventType = "account_updated"
	SchedulerEventAccountDeleted  SchedulerEventType = "account_deleted"
	// SchedulerEventAccountRuntimeCleared clears process-local cooldown and
	// recovery-probe state on every application replica.
	SchedulerEventAccountRuntimeCleared SchedulerEventType = "account_runtime_cleared"
)

type SchedulerEvent struct {
	Type       SchedulerEventType
	Bucket     SchedulerBucket
	AccountID  int64
	Generation int64
	At         time.Time
	Reason     string
	Source     string
}

type SchedulerEventBus interface {
	Publish(ctx context.Context, event SchedulerEvent) error
	Subscribe(buffer int) (<-chan SchedulerEvent, func())
}

type SchedulerRuntimeClearGenerationStore interface {
	AdvanceAccountRuntimeClearGeneration(ctx context.Context, accountID int64) (int64, error)
	LoadAccountRuntimeClearGeneration(ctx context.Context, accountID int64) (int64, error)
}

type routedSchedulerEventBus struct {
	snapshotBus SchedulerEventBus
	runtimeBus  SchedulerEventBus
}

// NewRuntimeRoutedSchedulerEventBus keeps optional snapshot invalidation on its
// configured bus while routing correctness-critical runtime clears to Redis.
func NewRuntimeRoutedSchedulerEventBus(snapshotBus, runtimeBus SchedulerEventBus) SchedulerEventBus {
	if runtimeBus == nil {
		return snapshotBus
	}
	return &routedSchedulerEventBus{snapshotBus: snapshotBus, runtimeBus: runtimeBus}
}

func (b *routedSchedulerEventBus) Publish(ctx context.Context, event SchedulerEvent) error {
	if event.Type == SchedulerEventAccountRuntimeCleared {
		return b.runtimeBus.Publish(ctx, event)
	}
	if b.snapshotBus == nil {
		return nil
	}
	return b.snapshotBus.Publish(ctx, event)
}

func (b *routedSchedulerEventBus) AdvanceAccountRuntimeClearGeneration(ctx context.Context, accountID int64) (int64, error) {
	store, ok := b.runtimeBus.(SchedulerRuntimeClearGenerationStore)
	if !ok {
		return 0, nil
	}
	return store.AdvanceAccountRuntimeClearGeneration(ctx, accountID)
}

func (b *routedSchedulerEventBus) LoadAccountRuntimeClearGeneration(ctx context.Context, accountID int64) (int64, error) {
	store, ok := b.runtimeBus.(SchedulerRuntimeClearGenerationStore)
	if !ok {
		return 0, nil
	}
	return store.LoadAccountRuntimeClearGeneration(ctx, accountID)
}

func (b *routedSchedulerEventBus) Subscribe(buffer int) (<-chan SchedulerEvent, func()) {
	if buffer <= 0 {
		buffer = 64
	}
	runtimeEvents, unsubscribeRuntime := b.runtimeBus.Subscribe(buffer)
	out := make(chan SchedulerEvent, buffer)
	done := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	unsubscribeSnapshot := func() {}
	forward := func(events <-chan SchedulerEvent, runtimeOnly bool) {
		defer wg.Done()
		for {
			select {
			case event, ok := <-events:
				if !ok {
					return
				}
				if runtimeOnly && !isSchedulerRuntimeClearEvent(event) {
					continue
				}
				select {
				case out <- event:
				case <-done:
					return
				}
			case <-done:
				return
			}
		}
	}
	wg.Add(1)
	if b.snapshotBus != nil {
		snapshotEvents, unsubscribe := b.snapshotBus.Subscribe(buffer)
		unsubscribeSnapshot = unsubscribe
		wg.Add(1)
		go forward(snapshotEvents, false)
	}
	go forward(runtimeEvents, true)
	go func() {
		wg.Wait()
		close(out)
	}()

	return out, func() {
		stopOnce.Do(func() {
			close(done)
			unsubscribeSnapshot()
			unsubscribeRuntime()
		})
	}
}

type localSchedulerEventBus struct {
	mu                      sync.RWMutex
	subscribers             map[chan SchedulerEvent]struct{}
	runtimeClearGenerations map[int64]int64
}

func isSchedulerRuntimeClearEvent(event SchedulerEvent) bool {
	return event.Type == SchedulerEventAccountRuntimeCleared
}

func NewLocalSchedulerEventBus() SchedulerEventBus {
	return &localSchedulerEventBus{
		subscribers:             make(map[chan SchedulerEvent]struct{}),
		runtimeClearGenerations: make(map[int64]int64),
	}
}

func (b *localSchedulerEventBus) AdvanceAccountRuntimeClearGeneration(_ context.Context, accountID int64) (int64, error) {
	if b == nil || accountID <= 0 {
		return 0, nil
	}
	b.mu.Lock()
	b.runtimeClearGenerations[accountID]++
	generation := b.runtimeClearGenerations[accountID]
	b.mu.Unlock()
	return generation, nil
}

func (b *localSchedulerEventBus) LoadAccountRuntimeClearGeneration(_ context.Context, accountID int64) (int64, error) {
	if b == nil || accountID <= 0 {
		return 0, nil
	}
	b.mu.RLock()
	generation := b.runtimeClearGenerations[accountID]
	b.mu.RUnlock()
	return generation, nil
}

func NewSchedulerEventBus(cfg *config.Config) SchedulerEventBus {
	if cfg == nil || !cfg.Gateway.Scheduling.EventBusEnabled {
		return nil
	}
	// Repository wiring injects the Redis Stream implementation when configured.
	// This service-level constructor is the in-process fallback used by tests
	// and by deployments that do not provide a repository-backed event bus.
	return NewLocalSchedulerEventBus()
}

func (b *localSchedulerEventBus) Publish(ctx context.Context, event SchedulerEvent) error {
	if b == nil {
		return nil
	}
	if event.At.IsZero() {
		event.At = time.Now()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subscribers {
		if isSchedulerRuntimeClearEvent(event) {
			select {
			case ch <- event:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		select {
		case ch <- event:
		default:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (b *localSchedulerEventBus) Subscribe(buffer int) (<-chan SchedulerEvent, func()) {
	if b == nil {
		ch := make(chan SchedulerEvent)
		close(ch)
		return ch, func() {}
	}
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan SchedulerEvent, buffer)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()

	unsubscribe := func() {
		b.mu.Lock()
		if _, ok := b.subscribers[ch]; ok {
			delete(b.subscribers, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
	return ch, unsubscribe
}
