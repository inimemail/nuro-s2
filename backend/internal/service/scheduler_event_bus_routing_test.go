package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRuntimeRoutedSchedulerEventBusFiltersOrdinaryEventsFromRuntimeBus(t *testing.T) {
	snapshotBus := NewLocalSchedulerEventBus()
	runtimeBus := NewLocalSchedulerEventBus()
	routed := NewRuntimeRoutedSchedulerEventBus(snapshotBus, runtimeBus)
	events, unsubscribe := routed.Subscribe(4)
	defer unsubscribe()

	require.NoError(t, runtimeBus.Publish(context.Background(), SchedulerEvent{Type: SchedulerEventAccountUpdated, AccountID: 11}))
	select {
	case event := <-events:
		t.Fatalf("ordinary runtime-bus event leaked into routed bus: %+v", event)
	case <-time.After(20 * time.Millisecond):
	}

	require.NoError(t, runtimeBus.Publish(context.Background(), SchedulerEvent{Type: SchedulerEventAccountRuntimeCleared, AccountID: 12, Generation: 1}))
	select {
	case event := <-events:
		require.Equal(t, SchedulerEventAccountRuntimeCleared, event.Type)
		require.Equal(t, int64(12), event.AccountID)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runtime-clear event")
	}
}
