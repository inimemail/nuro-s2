package handler

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestOpenAIEdgeContinuationMemoryIsOneShot(t *testing.T) {
	h := &OpenAIGatewayHandler{openAIEdgeContinuations: make(map[string]openAIEdgeContinuationMemory)}
	state := edgeRetryContinuation{
		Version:            1,
		StartedAtUnixMS:    time.Now().Add(-100 * time.Millisecond).UnixMilli(),
		DeadlineUnixMS:     time.Now().Add(time.Second).UnixMilli(),
		SameAccountRetries: map[int64]int{11: 3, 12: 1},
		FailedAccountIDs:   []int64{11},
		SwitchCount:        1,
		LastAccountID:      12,
	}
	h.saveOpenAIEdgeContinuation(context.Background(), "one-shot", state)

	got, ok := h.consumeOpenAIEdgeContinuation(context.Background(), "one-shot")
	require.True(t, ok)
	require.Equal(t, state.SwitchCount, got.SwitchCount)
	require.Equal(t, state.SameAccountRetries, got.SameAccountRetries)

	_, ok = h.consumeOpenAIEdgeContinuation(context.Background(), "one-shot")
	require.False(t, ok, "GETDEL/memory consume must prevent replay")
}

func TestOpenAIEdgeContinuationRedisGetDelIsOneShot(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Skipf("local Redis test server unavailable: %v", err)
	}
	t.Cleanup(mini.Close)
	h := &OpenAIGatewayHandler{
		redisClient:             redis.NewClient(&redis.Options{Addr: mini.Addr()}),
		openAIEdgeContinuations: make(map[string]openAIEdgeContinuationMemory),
	}
	state := edgeRetryContinuation{Version: 1, SwitchCount: 2, SameAccountRetries: map[int64]int{31: 4}}
	h.saveOpenAIEdgeContinuation(context.Background(), "redis-one-shot", state)

	got, ok := h.consumeOpenAIEdgeContinuation(context.Background(), "redis-one-shot")
	require.True(t, ok)
	require.Equal(t, state.SwitchCount, got.SwitchCount)
	_, ok = h.consumeOpenAIEdgeContinuation(context.Background(), "redis-one-shot")
	require.False(t, ok)
}

func TestOpenAIEdgeContinuationRedisSaveFailureDoesNotIssueTokenOrUseMemory(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Skipf("local Redis test server unavailable: %v", err)
	}
	addr := mini.Addr()
	mini.Close()
	h := &OpenAIGatewayHandler{
		redisClient: redis.NewClient(&redis.Options{
			Addr:         addr,
			DialTimeout:  20 * time.Millisecond,
			ReadTimeout:  20 * time.Millisecond,
			WriteTimeout: 20 * time.Millisecond,
			MaxRetries:   -1,
		}),
		openAIEdgeLeases: map[string]*openAIEdgeLease{
			"lease-save-failure": {
				edgeRequestID:      "edge-save-failure",
				leaseID:            "lease-save-failure",
				account:            &service.Account{ID: 41},
				sameAccountRetries: map[int64]int{41: 1},
				failedAccountIDs:   map[int64]struct{}{},
			},
		},
		openAIEdgeContinuations: make(map[string]openAIEdgeContinuationMemory),
	}
	t.Cleanup(func() { _ = h.redisClient.Close() })
	decision := service.OpenAIEdgeRetryDecision{Action: service.OpenAIEdgeActionFallbackGo}

	h.attachOpenAIEdgeContinuation(context.Background(), "lease-save-failure", &decision)

	require.Empty(t, decision.ContinuationToken, "an unpersisted continuation token must not be issued")
	require.Empty(t, h.openAIEdgeContinuations, "configured Redis must never fall back to local memory")
}

func TestOpenAIEdgeContinuationRedisConsumeFailureDoesNotUseMemory(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Skipf("local Redis test server unavailable: %v", err)
	}
	addr := mini.Addr()
	mini.Close()
	h := &OpenAIGatewayHandler{
		redisClient: redis.NewClient(&redis.Options{
			Addr:         addr,
			DialTimeout:  20 * time.Millisecond,
			ReadTimeout:  20 * time.Millisecond,
			WriteTimeout: 20 * time.Millisecond,
			MaxRetries:   -1,
		}),
		openAIEdgeContinuations: map[string]openAIEdgeContinuationMemory{
			"ambiguous": {
				expiresAt: time.Now().Add(time.Minute),
				body:      []byte(`{"version":1,"switch_count":1}`),
			},
		},
	}
	t.Cleanup(func() { _ = h.redisClient.Close() })

	_, ok := h.consumeOpenAIEdgeContinuation(context.Background(), "ambiguous")

	require.False(t, ok, "a Redis consume failure must fail closed")
	require.Contains(t, h.openAIEdgeContinuations, "ambiguous", "Redis mode must not consume a local representation")
}

func TestSeedRetryStateFromEdgeContinuationRestoresAllFailoverState(t *testing.T) {
	state := &edgeRetryContinuation{
		Version:            1,
		StartedAtUnixMS:    time.Now().Add(-200 * time.Millisecond).UnixMilli(),
		DeadlineUnixMS:     time.Now().Add(1200 * time.Millisecond).UnixMilli(),
		SameAccountRetries: map[int64]int{21: 3, 22: 2},
		FailedAccountIDs:   []int64{21, 22},
		SwitchCount:        2,
	}
	ctx := context.WithValue(context.Background(), ctxkey.EdgeRetryContinuation, state)
	counts := make(map[int64]int)
	starts := make(map[int64]time.Time)
	failed := make(map[int64]struct{})
	switchCount, restored := seedRetryStateFromEdgeContinuation(ctx, counts, starts, failed)

	require.True(t, restored)
	require.Equal(t, 2, switchCount)
	require.Equal(t, map[int64]int{21: 3, 22: 2}, counts)
	require.Contains(t, failed, int64(21))
	require.Contains(t, failed, int64(22))
	require.Equal(t, time.UnixMilli(state.DeadlineUnixMS), starts[sharedRaceRetryDeadlineKey])
	require.Equal(t, time.UnixMilli(state.StartedAtUnixMS), starts[sharedRaceRetryStartedKey])
}

func TestEdgeContinuationPreservesExplicitRaceBudgetExhaustion(t *testing.T) {
	deadline := time.Now().Add(time.Second)
	lease := &openAIEdgeLease{
		edgeRequestID: "edge-budget-exhausted",
		sameAccountStarted: map[int64]time.Time{
			sharedRaceRetryStartedKey:   time.Now().Add(-time.Second),
			sharedRaceRetryDeadlineKey:  deadline,
			sharedRaceRetryExhaustedKey: time.Now(),
		},
	}
	h := &OpenAIGatewayHandler{}
	state := h.snapshotOpenAIEdgeContinuation(lease)
	if !state.BudgetExhausted || state.DeadlineUnixMS == 0 {
		t.Fatalf("explicit budget exhaustion was not serialized: %+v", state)
	}

	ctx := context.WithValue(context.Background(), ctxkey.EdgeRetryContinuation, &state)
	starts := map[int64]time.Time{}
	seedRetryStateFromEdgeContinuation(ctx, map[int64]int{}, starts, map[int64]struct{}{})
	if starts[sharedRaceRetryExhaustedKey].IsZero() {
		t.Fatal("explicit budget exhaustion was not restored")
	}
	if _, ok := activeSharedRaceDeadline(starts); ok {
		t.Fatal("restored exhausted continuation became an active race window")
	}
}
