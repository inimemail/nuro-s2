package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type failingRuntimeClearEventBus struct {
	mu         sync.Mutex
	generation int64
	attempts   int
}

type callbackRuntimeClearEventBus struct {
	SchedulerEventBus
	SchedulerRuntimeClearGenerationStore
	onPublish func(SchedulerEvent)
}

type steppingRuntimeClearEventBus struct {
	mu        sync.Mutex
	loadCalls int
}

type ambiguousAdvanceRuntimeClearEventBus struct {
	mu             sync.Mutex
	generation     int64
	commitOnError  bool
	reconcileError error
}

func (b *steppingRuntimeClearEventBus) Publish(context.Context, SchedulerEvent) error { return nil }
func (b *steppingRuntimeClearEventBus) Subscribe(int) (<-chan SchedulerEvent, func()) {
	events := make(chan SchedulerEvent)
	return events, func() { close(events) }
}
func (b *steppingRuntimeClearEventBus) AdvanceAccountRuntimeClearGeneration(context.Context, int64) (int64, error) {
	return 1, nil
}
func (b *steppingRuntimeClearEventBus) LoadAccountRuntimeClearGeneration(context.Context, int64) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.loadCalls++
	if b.loadCalls == 1 {
		return 0, nil
	}
	return 1, nil
}

func (b *ambiguousAdvanceRuntimeClearEventBus) Publish(context.Context, SchedulerEvent) error {
	return nil
}

func (b *ambiguousAdvanceRuntimeClearEventBus) Subscribe(int) (<-chan SchedulerEvent, func()) {
	events := make(chan SchedulerEvent)
	return events, func() { close(events) }
}

func (b *ambiguousAdvanceRuntimeClearEventBus) AdvanceAccountRuntimeClearGeneration(context.Context, int64) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.commitOnError {
		b.generation++
	}
	return 0, errors.New("ambiguous redis timeout")
}

func (b *ambiguousAdvanceRuntimeClearEventBus) LoadAccountRuntimeClearGeneration(context.Context, int64) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reconcileError != nil && b.generation > 0 {
		return 0, b.reconcileError
	}
	return b.generation, nil
}

func (b *callbackRuntimeClearEventBus) Publish(ctx context.Context, event SchedulerEvent) error {
	if b.onPublish != nil {
		b.onPublish(event)
	}
	return b.SchedulerEventBus.Publish(ctx, event)
}

func (b *failingRuntimeClearEventBus) Publish(context.Context, SchedulerEvent) error {
	b.mu.Lock()
	b.attempts++
	b.mu.Unlock()
	return errors.New("redis unavailable")
}

func (b *failingRuntimeClearEventBus) Subscribe(int) (<-chan SchedulerEvent, func()) {
	events := make(chan SchedulerEvent)
	return events, func() { close(events) }
}

func (b *failingRuntimeClearEventBus) AdvanceAccountRuntimeClearGeneration(context.Context, int64) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.generation++
	return b.generation, nil
}

func (b *failingRuntimeClearEventBus) LoadAccountRuntimeClearGeneration(context.Context, int64) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.generation, nil
}

func TestCompositeAccountRuntimeBlocker_AdminClearClearsLocallyAndPublishesOnce(t *testing.T) {
	cfg := &config.Config{}
	bus := NewLocalSchedulerEventBus()
	events, unsubscribe := bus.Subscribe(2)
	defer unsubscribe()
	snapshot := NewSchedulerSnapshotService(nil, nil, nil, nil, cfg, bus)
	openAI := &OpenAIGatewayService{schedulerSnapshot: snapshot}
	anthropic := &GatewayService{schedulerSnapshot: snapshot}
	blocker := NewCompositeAccountRuntimeBlocker(openAI, anthropic, nil, nil)
	accountID := int64(701)
	openAI.openaiPoolSoftCooldownUntil.Store(accountID, time.Now().Add(time.Minute))
	anthropic.anthropicPoolSoftCooldownUntil.Store(accountID, time.Now().Add(time.Minute))

	require.NoError(t, blocker.ClearAccountSchedulingBlockAcrossReplicas(context.Background(), accountID))

	_, openAICooling := openAI.openAIPoolAccountSoftCooldownUntilByID(accountID)
	_, anthropicCooling := anthropic.anthropicPoolAccountSoftCooldownUntilByID(accountID)
	require.False(t, openAICooling)
	require.False(t, anthropicCooling)
	select {
	case event := <-events:
		require.Equal(t, SchedulerEventAccountRuntimeCleared, event.Type)
		require.Equal(t, accountID, event.AccountID)
		require.Equal(t, int64(1), event.Generation)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runtime-clear event")
	}
	select {
	case event := <-events:
		t.Fatalf("unexpected duplicate runtime-clear event: %+v", event)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestRuntimeClearGenerationClearsOldStateAndPreservesNewState(t *testing.T) {
	cfg := &config.Config{}
	bus := NewLocalSchedulerEventBus()
	snapshot := NewSchedulerSnapshotService(nil, nil, nil, nil, cfg, bus)
	openAI := &OpenAIGatewayService{schedulerSnapshot: snapshot}
	anthropic := &GatewayService{schedulerSnapshot: snapshot}
	snapshot.RegisterAccountRuntimeClearHandler(openAI.clearLocalAccountSchedulingBlockBefore)
	snapshot.RegisterAccountRuntimeClearHandler(anthropic.clearAnthropicPoolSoftCooldownBefore)
	oldID := int64(702)
	newID := int64(703)
	openAI.openaiPoolSoftCooldownUntil.Store(oldID, accountRuntimeDeadline{Until: time.Now().Add(time.Minute), ClearGeneration: 0})
	anthropic.anthropicPoolSoftCooldownUntil.Store(oldID, accountRuntimeDeadline{Until: time.Now().Add(time.Minute), ClearGeneration: 0})
	openAI.openaiPoolSoftCooldownUntil.Store(newID, accountRuntimeDeadline{Until: time.Now().Add(time.Minute), ClearGeneration: 1})
	anthropic.anthropicPoolSoftCooldownUntil.Store(newID, accountRuntimeDeadline{Until: time.Now().Add(time.Minute), ClearGeneration: 1})

	for _, accountID := range []int64{oldID, newID} {
		snapshot.handleSchedulerEvent(context.Background(), SchedulerEvent{
			Type:       SchedulerEventAccountRuntimeCleared,
			AccountID:  accountID,
			Generation: 1,
		})
	}

	_, openAIOld := openAI.openaiPoolSoftCooldownUntil.Load(oldID)
	_, anthropicOld := anthropic.anthropicPoolSoftCooldownUntil.Load(oldID)
	_, openAINew := openAI.openaiPoolSoftCooldownUntil.Load(newID)
	_, anthropicNew := anthropic.anthropicPoolSoftCooldownUntil.Load(newID)
	require.False(t, openAIOld)
	require.False(t, anthropicOld)
	require.True(t, openAINew)
	require.True(t, anthropicNew)
}

func TestCompositeAccountRuntimeBlocker_PublishFailureRetriesAndStillClearsLocalState(t *testing.T) {
	bus := &failingRuntimeClearEventBus{}
	snapshot := NewSchedulerSnapshotService(nil, nil, nil, nil, &config.Config{}, bus)
	openAI := &OpenAIGatewayService{schedulerSnapshot: snapshot}
	remoteOpenAI := &OpenAIGatewayService{schedulerSnapshot: snapshot}
	blocker := NewCompositeAccountRuntimeBlocker(openAI, nil, nil, nil)
	accountID := int64(704)
	openAI.openaiPoolSoftCooldownUntil.Store(accountID, time.Now().Add(time.Minute))
	remoteOpenAI.openaiPoolSoftCooldownUntil.Store(accountID, time.Now().Add(time.Minute))

	err := blocker.ClearAccountSchedulingBlockAcrossReplicas(context.Background(), accountID)

	require.ErrorContains(t, err, "redis unavailable")
	_, cooling := openAI.openaiPoolSoftCooldownUntil.Load(accountID)
	require.False(t, cooling)
	require.False(t, remoteOpenAI.isOpenAIPoolAccountSoftCooling(&Account{
		ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}))
	bus.mu.Lock()
	require.Equal(t, 3, bus.attempts)
	bus.mu.Unlock()
}

func TestCompositeAccountRuntimeBlocker_PreservesSourceStateCreatedAfterGenerationAdvance(t *testing.T) {
	localBus := NewLocalSchedulerEventBus()
	bus := &callbackRuntimeClearEventBus{
		SchedulerEventBus:                    localBus,
		SchedulerRuntimeClearGenerationStore: localBus.(SchedulerRuntimeClearGenerationStore),
	}
	snapshot := NewSchedulerSnapshotService(nil, nil, nil, nil, &config.Config{}, bus)
	openAI := &OpenAIGatewayService{schedulerSnapshot: snapshot}
	blocker := NewCompositeAccountRuntimeBlocker(openAI, nil, nil, nil)
	accountID := int64(705)
	openAI.openaiPoolSoftCooldownUntil.Store(accountID, accountRuntimeDeadline{Until: time.Now().Add(time.Minute), ClearGeneration: 0})
	bus.onPublish = func(event SchedulerEvent) {
		openAI.openaiPoolSoftCooldownUntil.Store(accountID, accountRuntimeDeadline{
			Until:           time.Now().Add(2 * time.Minute),
			ClearGeneration: event.Generation,
		})
	}

	require.NoError(t, blocker.ClearAccountSchedulingBlockAcrossReplicas(context.Background(), accountID))

	value, ok := openAI.openaiPoolSoftCooldownUntil.Load(accountID)
	require.True(t, ok)
	_, generation, valid := parseAccountRuntimeDeadline(value)
	require.True(t, valid)
	require.Equal(t, int64(1), generation)
}

func TestCooldownStoreDoesNotResurrectStateAfterConcurrentGenerationAdvance(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		store func(*SchedulerSnapshotService, int64) bool
	}{
		{
			name: "openai",
			store: func(snapshot *SchedulerSnapshotService, accountID int64) bool {
				svc := &OpenAIGatewayService{schedulerSnapshot: snapshot}
				svc.storeOpenAIPoolSoftCooldownUntil(accountID, time.Now().Add(time.Minute))
				_, exists := svc.openaiPoolSoftCooldownUntil.Load(accountID)
				return exists
			},
		},
		{
			name: "anthropic",
			store: func(snapshot *SchedulerSnapshotService, accountID int64) bool {
				svc := &GatewayService{schedulerSnapshot: snapshot}
				svc.storeAnthropicPoolSoftCooldownUntil(accountID, time.Now().Add(time.Minute))
				_, exists := svc.anthropicPoolSoftCooldownUntil.Load(accountID)
				return exists
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			bus := &steppingRuntimeClearEventBus{}
			snapshot := NewSchedulerSnapshotService(nil, nil, nil, nil, &config.Config{}, bus)
			require.False(t, testCase.store(snapshot, 706))
		})
	}
}

func TestHealthyRuntimeChecksDoNotLoadSharedClearGeneration(t *testing.T) {
	bus := &steppingRuntimeClearEventBus{}
	snapshot := NewSchedulerSnapshotService(nil, nil, nil, nil, &config.Config{}, bus)
	openAI := &OpenAIGatewayService{schedulerSnapshot: snapshot}
	anthropic := &GatewayService{schedulerSnapshot: snapshot}

	require.False(t, openAI.isOpenAIAccountRuntimeBlocked(&Account{
		ID: 801, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
	}))
	require.False(t, openAI.isOpenAIPoolAccountSoftCooling(&Account{
		ID: 802, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}))
	_, cooling := anthropic.anthropicPoolAccountSoftCooldownUntilByID(803)
	require.False(t, cooling)

	bus.mu.Lock()
	require.Zero(t, bus.loadCalls)
	bus.mu.Unlock()
}

func TestCompositeAccountRuntimeBlocker_ReconcilesCommittedAmbiguousAdvance(t *testing.T) {
	bus := &ambiguousAdvanceRuntimeClearEventBus{commitOnError: true}
	snapshot := NewSchedulerSnapshotService(nil, nil, nil, nil, &config.Config{}, bus)
	openAI := &OpenAIGatewayService{schedulerSnapshot: snapshot}
	blocker := NewCompositeAccountRuntimeBlocker(openAI, nil, nil, nil)
	accountID := int64(804)
	openAI.openaiPoolSoftCooldownUntil.Store(accountID, accountRuntimeDeadline{
		Until: time.Now().Add(time.Minute), ClearGeneration: 0,
	})

	require.NoError(t, blocker.ClearAccountSchedulingBlockAcrossReplicas(context.Background(), accountID))
	_, exists := openAI.openaiPoolSoftCooldownUntil.Load(accountID)
	require.False(t, exists)
}

func TestCompositeAccountRuntimeBlocker_DoesNotClearWhenAdvanceOutcomeCannotBeConfirmed(t *testing.T) {
	bus := &ambiguousAdvanceRuntimeClearEventBus{
		commitOnError:  true,
		reconcileError: errors.New("redis unavailable"),
	}
	snapshot := NewSchedulerSnapshotService(nil, nil, nil, nil, &config.Config{}, bus)
	openAI := &OpenAIGatewayService{schedulerSnapshot: snapshot}
	blocker := NewCompositeAccountRuntimeBlocker(openAI, nil, nil, nil)
	accountID := int64(805)
	deadline := accountRuntimeDeadline{Until: time.Now().Add(time.Minute), ClearGeneration: 0}
	openAI.openaiPoolSoftCooldownUntil.Store(accountID, deadline)

	err := blocker.ClearAccountSchedulingBlockAcrossReplicas(context.Background(), accountID)
	require.ErrorContains(t, err, "reconcile account runtime-clear generation")
	value, exists := openAI.openaiPoolSoftCooldownUntil.Load(accountID)
	require.True(t, exists)
	require.Equal(t, deadline, value)
}
