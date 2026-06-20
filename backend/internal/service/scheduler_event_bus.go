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
)

type SchedulerEvent struct {
	Type      SchedulerEventType
	Bucket    SchedulerBucket
	AccountID int64
	At        time.Time
	Reason    string
	Source    string
}

type SchedulerEventBus interface {
	Publish(ctx context.Context, event SchedulerEvent) error
	Subscribe(buffer int) (<-chan SchedulerEvent, func())
}

type localSchedulerEventBus struct {
	mu          sync.RWMutex
	subscribers map[chan SchedulerEvent]struct{}
}

func NewLocalSchedulerEventBus() SchedulerEventBus {
	return &localSchedulerEventBus{
		subscribers: make(map[chan SchedulerEvent]struct{}),
	}
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
