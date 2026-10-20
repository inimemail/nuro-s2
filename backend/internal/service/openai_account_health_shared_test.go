package service

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newOpenAISharedHealthTestState(t *testing.T) (*openAIAccountHealthSharedState, *openAIAccountRuntimeStats) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	stats := newOpenAIAccountRuntimeStats()
	shared := newOpenAIAccountHealthSharedState(client, stats, 4)
	t.Cleanup(shared.close)
	return shared, stats
}

func TestOpenAIAccountHealthSharedState_PersistsAndAppliesRouteSamples(t *testing.T) {
	shared, source := newOpenAISharedHealthTestState(t)
	key := openAIAccountRuntimeStatsKey{
		accountID: 42,
		kind:      "text",
		model:     "gpt-5.6-sol",
		transport: string(OpenAIUpstreamTransportHTTPSSE),
	}
	first := 120
	event, err := shared.persist(openAIAccountHealthSharedReport{
		key:         key,
		success:     true,
		ttftMS:      &first,
		updatedNano: 10,
	})
	require.NoError(t, err)

	destination := newOpenAIAccountRuntimeStats()
	destination.applySharedHealthEvent(event)
	errorRate, ttft, hasTTFT, found, samples, ttftSamples, _ := destination.snapshotForKeyWithMeta(key)
	require.True(t, found)
	require.Equal(t, 0.0, errorRate)
	require.True(t, hasTTFT)
	require.Equal(t, 120.0, ttft)
	require.EqualValues(t, 1, samples)
	require.EqualValues(t, 1, ttftSamples)

	// The source process can apply the exact Redis result without changing the
	// local request timing sample or its scoring formula.
	source.applySharedHealthEvent(event)
	_, sourceTTFT, sourceHasTTFT, _, _, _, _ := source.snapshotForKeyWithMeta(key)
	require.True(t, sourceHasTTFT)
	require.Equal(t, 120.0, sourceTTFT)
}

func TestOpenAIAccountHealthSharedState_DoesNotReplaceNewerLocalSample(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	key := openAIAccountRuntimeStatsKey{accountID: 42, kind: "text", model: "gpt-5.6-sol", transport: "http_sse"}
	localTTFT := 90
	stats.reportForKey(key, true, &localTTFT)

	stats.applySharedHealthEvent(openAIAccountHealthSharedEvent{
		AccountID:       key.accountID,
		Kind:            key.kind,
		Model:           key.model,
		Transport:       key.transport,
		ErrorRate:       1,
		TTFT:            9000,
		HasTTFT:         true,
		SampleCount:     100,
		TTFTSampleCount: 100,
		UpdatedNano:     time.Now().Add(-time.Minute).UnixNano(),
		Version:         100,
	})

	errorRate, ttft, hasTTFT, found, samples, _, _ := stats.snapshotForKeyWithMeta(key)
	require.True(t, found)
	require.True(t, hasTTFT)
	require.Equal(t, 0.0, errorRate)
	require.Equal(t, 90.0, ttft)
	require.EqualValues(t, 1, samples)
}

func TestOpenAIAccountHealthSharedState_UsesAtomicEWMA(t *testing.T) {
	shared, _ := newOpenAISharedHealthTestState(t)
	key := openAIAccountRuntimeStatsKey{accountID: 7, kind: "text", model: "gpt-5.6-sol", transport: "http_sse"}
	first := 100
	_, err := shared.persist(openAIAccountHealthSharedReport{key: key, success: true, ttftMS: &first, updatedNano: 1})
	require.NoError(t, err)
	second := 300
	event, err := shared.persist(openAIAccountHealthSharedReport{key: key, success: false, ttftMS: &second, updatedNano: 2})
	require.NoError(t, err)

	require.InDelta(t, 0.2, event.ErrorRate, 0.000001)
	require.InDelta(t, 140.0, event.TTFT, 0.000001)
	require.EqualValues(t, 2, event.SampleCount)
	require.EqualValues(t, 2, event.TTFTSampleCount)

	ttl, err := shared.client.TTL(context.Background(), openAIAccountHealthRedisKey(key)).Result()
	require.NoError(t, err)
	require.Equal(t, openAIAccountHealthSharedTTL, ttl)
}

func TestOpenAIAccountHealthSharedState_PreservesNanosecondTimestampPrecision(t *testing.T) {
	shared, _ := newOpenAISharedHealthTestState(t)
	key := openAIAccountRuntimeStatsKey{accountID: 8, kind: "text", model: "gpt-5.6-sol", transport: "http_sse"}
	updatedNano := time.Now().UnixNano()

	event, err := shared.persist(openAIAccountHealthSharedReport{key: key, success: true, updatedNano: updatedNano})
	require.NoError(t, err)
	require.Equal(t, updatedNano, event.UpdatedNano)
	require.Equal(t, updatedNano, event.ErrorUpdatedNano)
}

func TestOpenAIAccountHealthSharedState_DelayedReportDoesNotRegressFreshness(t *testing.T) {
	shared, _ := newOpenAISharedHealthTestState(t)
	key := openAIAccountRuntimeStatsKey{accountID: 9, kind: "text", model: "gpt-5.6-sol", transport: "http_sse"}

	_, err := shared.persist(openAIAccountHealthSharedReport{key: key, success: true, updatedNano: 300})
	require.NoError(t, err)
	event, err := shared.persist(openAIAccountHealthSharedReport{key: key, success: false, updatedNano: 250})
	require.NoError(t, err)
	require.Equal(t, int64(300), event.UpdatedNano)
	require.Equal(t, int64(300), event.ErrorUpdatedNano)
}

func TestOpenAIAccountHealthSharedState_AppliesLegacyEventErrorTimestamp(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	key := openAIAccountRuntimeStatsKey{accountID: 10, kind: "text", model: "gpt-5.6-sol", transport: "http_sse"}
	stats.applySharedHealthEvent(openAIAccountHealthSharedEvent{
		AccountID:   key.accountID,
		Kind:        key.kind,
		Model:       key.model,
		Transport:   key.transport,
		ErrorRate:   1,
		SampleCount: 3,
		UpdatedNano: 400,
		Version:     1,
	})

	errorRate, _, _, found, samples, _, _ := stats.snapshotForKeyWithMeta(key)
	require.True(t, found)
	require.Equal(t, 1.0, errorRate)
	require.EqualValues(t, 3, samples)
}

func TestOpenAIAccountHealthRedisKeySeparatesRoutes(t *testing.T) {
	a := openAIAccountRuntimeStatsKey{accountID: 7, kind: "text", model: "gpt-5.6-sol", transport: "http_sse"}
	b := a
	b.transport = "responses_websocket_v2"
	require.NotEqual(t, openAIAccountHealthRedisKey(a), openAIAccountHealthRedisKey(b))
}

func TestOpenAIAccountHealthSharedState_PropagatesAcrossInstances(t *testing.T) {
	mr := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		_ = clientA.Close()
		_ = clientB.Close()
	})

	source := newOpenAIAccountRuntimeStats()
	destination := newOpenAIAccountRuntimeStats()
	source.setSharedRedisClient(clientA)
	destination.setSharedRedisClient(clientB)
	t.Cleanup(source.closeSharedHealth)
	t.Cleanup(destination.closeSharedHealth)

	// Wait for both asynchronous subscribers before publishing the sole event.
	// Otherwise an initial empty snapshot can consume the one-time Redis load
	// before persistence, while pub/sub has no subscriber to receive the update.
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		counts, err := clientA.PubSubNumSub(ctx, openAIAccountHealthSharedChannel).Result()
		return err == nil && counts[openAIAccountHealthSharedChannel] == 2
	}, 2*time.Second, 10*time.Millisecond)

	ttft := 240
	source.reportForRoute(99, true, &ttft, "gpt-5.6-sol", OpenAIUpstreamTransportHTTPSSE)

	require.Eventually(t, func() bool {
		_, got, hasTTFT := destination.snapshotForRoute(99, "gpt-5.6-sol", OpenAIUpstreamTransportHTTPSSE)
		return hasTTFT && got == 240
	}, 2*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	keys, err := clientA.Keys(ctx, openAIAccountHealthSharedKeyPrefix+"*").Result()
	require.NoError(t, err)
	require.Len(t, keys, 1)
}

func TestOpenAIAccountHealthSharedState_ResetErrorPropagatesAndPreservesTTFT(t *testing.T) {
	mr := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = clientA.Close(); _ = clientB.Close() })

	source := newOpenAIAccountRuntimeStats()
	destination := newOpenAIAccountRuntimeStats()
	source.setSharedRedisClient(clientA)
	destination.setSharedRedisClient(clientB)
	t.Cleanup(source.closeSharedHealth)
	t.Cleanup(destination.closeSharedHealth)

	ttft := 500
	for range 3 {
		source.reportForRoute(88, false, &ttft, "gpt-5.6-sol", OpenAIUpstreamTransportHTTPSSE)
	}
	require.Eventually(t, func() bool {
		errorRate, _, _, samples, _, _ := destination.snapshotForRouteWithMeta(88, "gpt-5.6-sol", OpenAIUpstreamTransportHTTPSSE)
		return errorRate > 0 && samples == 3
	}, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, source.resetErrorHealth(context.Background(), 88))
	require.Eventually(t, func() bool {
		errorRate, gotTTFT, hasTTFT, samples, ttftSamples, _ := destination.snapshotForRouteWithMeta(88, "gpt-5.6-sol", OpenAIUpstreamTransportHTTPSSE)
		return errorRate == 0 && samples == 0 && hasTTFT && gotTTFT == 500 && ttftSamples == 3
	}, 2*time.Second, 10*time.Millisecond)
}

func TestOpenAIAccountHealthSharedState_ResetFenceRejectsDelayedOldError(t *testing.T) {
	shared, _ := newOpenAISharedHealthTestState(t)
	key := openAIAccountRuntimeStatsKey{accountID: 89, kind: "text", model: "gpt-5.6-sol", transport: "http_sse"}
	ttft := 600
	_, err := shared.persist(openAIAccountHealthSharedReport{key: key, success: false, ttftMS: &ttft, updatedNano: 100})
	require.NoError(t, err)
	require.NoError(t, shared.resetAccountErrorHealth(context.Background(), key.accountID, 200))

	// A report captured before recovery may still be waiting in a publisher
	// queue. It can contribute its TTFT evidence, but must not restore the old
	// failure state after the account has recovered.
	event, err := shared.persist(openAIAccountHealthSharedReport{key: key, success: false, ttftMS: &ttft, updatedNano: 150})
	require.NoError(t, err)
	require.Zero(t, event.ErrorRate)
	require.Zero(t, event.SampleCount)
	require.True(t, event.HasTTFT)
	require.EqualValues(t, 2, event.TTFTSampleCount)
}

func TestOpenAIAccountHealthSharedState_ResetDoesNotEraseNewerFailure(t *testing.T) {
	shared, _ := newOpenAISharedHealthTestState(t)
	key := openAIAccountRuntimeStatsKey{accountID: 90, kind: "text", model: "gpt-5.6-sol", transport: "http_sse"}
	_, err := shared.persist(openAIAccountHealthSharedReport{key: key, success: false, updatedNano: 300})
	require.NoError(t, err)
	require.NoError(t, shared.resetAccountErrorHealth(context.Background(), key.accountID, 200))

	values, err := shared.client.HGetAll(context.Background(), openAIAccountHealthRedisKey(key)).Result()
	require.NoError(t, err)
	event, err := openAIAccountHealthEventFromMap(key, values)
	require.NoError(t, err)
	require.Equal(t, 1.0, event.ErrorRate)
	require.EqualValues(t, 1, event.SampleCount)
}

func TestOpenAIAccountHealthSharedState_NewSampleHonorsResetFenceWithoutScan(t *testing.T) {
	for _, tc := range []struct {
		name            string
		previousSuccess bool
	}{
		{name: "old_success_does_not_dilute_new_failure", previousSuccess: true},
		{name: "old_failure_does_not_taint_recovery", previousSuccess: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shared, _ := newOpenAISharedHealthTestState(t)
			key := openAIAccountRuntimeStatsKey{accountID: 901, kind: "text", model: "gpt-test", transport: "http_sse"}
			ttft := 250
			for updatedNano := int64(100); updatedNano <= 300; updatedNano += 100 {
				_, err := shared.persist(openAIAccountHealthSharedReport{key: key, success: tc.previousSuccess, ttftMS: &ttft, updatedNano: updatedNano})
				require.NoError(t, err)
			}

			// Model keys can outlive a bounded reset scan. The fence alone must
			// stop either kind of old error sample from affecting the new one.
			_, err := openAIAccountHealthResetFenceScript.Run(
				context.Background(), shared.client,
				[]string{openAIAccountHealthResetRedisKey(key.accountID)},
				int64(400), int64(openAIAccountHealthSharedTTL/time.Second),
			).Result()
			require.NoError(t, err)
			event, err := shared.persist(openAIAccountHealthSharedReport{key: key, success: !tc.previousSuccess, updatedNano: 500})
			require.NoError(t, err)
			expectedErrorRate := 0.0
			if tc.previousSuccess {
				expectedErrorRate = 1
			}
			require.Equal(t, expectedErrorRate, event.ErrorRate)
			require.EqualValues(t, 1, event.SampleCount)
			require.True(t, event.HasTTFT)
			require.EqualValues(t, ttft, event.TTFT)
			require.EqualValues(t, 3, event.TTFTSampleCount)
		})
	}
}

func TestOpenAIAccountHealthSharedState_RecreatedHashRestoresSynchronization(t *testing.T) {
	shared, stats := newOpenAISharedHealthTestState(t)
	key := openAIAccountRuntimeStatsKey{accountID: 902, kind: "text", model: "gpt-test", transport: "http_sse"}
	var oldEvent openAIAccountHealthSharedEvent
	for updatedNano := int64(100); updatedNano <= 300; updatedNano += 100 {
		var err error
		oldEvent, err = shared.persist(openAIAccountHealthSharedReport{key: key, success: true, updatedNano: updatedNano})
		require.NoError(t, err)
		stats.applySharedHealthEvent(oldEvent)
	}
	require.NoError(t, shared.client.Del(t.Context(), openAIAccountHealthRedisKey(key)).Err())
	newEvent, err := shared.persist(openAIAccountHealthSharedReport{key: key, success: false, updatedNano: 400})
	require.NoError(t, err)
	require.EqualValues(t, 1, newEvent.Version)
	stats.applySharedHealthEvent(newEvent)
	stats.applySharedHealthEvent(oldEvent)
	errorRate, _, _, _, samples, _, updated := stats.snapshotForKeyWithMeta(key)
	require.Equal(t, 1.0, errorRate)
	require.EqualValues(t, 1, samples)
	require.EqualValues(t, 400, updated.UnixNano())
}

func TestOpenAIAccountHealthSharedState_DelayedEventAfterResetKeepsOnlyTTFT(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	key := openAIAccountRuntimeStatsKey{accountID: 91, kind: "text", model: "gpt-5.6-sol", transport: "http_sse"}
	stats.resetLocalErrorHealth(key.accountID, 200)

	stats.applySharedHealthEvent(openAIAccountHealthSharedEvent{
		AccountID:        key.accountID,
		Kind:             key.kind,
		Model:            key.model,
		Transport:        key.transport,
		ErrorRate:        1,
		TTFT:             700,
		HasTTFT:          true,
		SampleCount:      3,
		TTFTSampleCount:  3,
		UpdatedNano:      150,
		ErrorUpdatedNano: 150,
		TTFTUpdatedNano:  150,
		Version:          1,
	})

	errorRate, ttft, hasTTFT, found, samples, ttftSamples, _ := stats.snapshotForKeyWithMeta(key)
	require.True(t, found)
	require.Zero(t, errorRate)
	require.Zero(t, samples)
	require.True(t, hasTTFT)
	require.Equal(t, 700.0, ttft)
	require.EqualValues(t, 3, ttftSamples)
}

func TestOpenAIAccountHealthSharedState_AppliesFailureNewerThanReset(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	key := openAIAccountRuntimeStatsKey{accountID: 92, kind: "text", model: "gpt-5.6-sol", transport: "http_sse"}
	stats.resetLocalErrorHealth(key.accountID, 200)

	stats.applySharedHealthEvent(openAIAccountHealthSharedEvent{
		AccountID:        key.accountID,
		Kind:             key.kind,
		Model:            key.model,
		Transport:        key.transport,
		ErrorRate:        1,
		SampleCount:      1,
		UpdatedNano:      250,
		ErrorUpdatedNano: 250,
		Version:          1,
	})

	errorRate, _, _, found, samples, _, _ := stats.snapshotForKeyWithMeta(key)
	require.True(t, found)
	require.Equal(t, 1.0, errorRate)
	require.EqualValues(t, 1, samples)
}

func TestOpenAIAccountHealthSharedState_CloseStopsWorkers(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	stats := newOpenAIAccountRuntimeStats()
	shared := newOpenAIAccountHealthSharedState(client, stats, 4)
	shared.start()
	require.Eventually(t, func() bool {
		counts, err := client.PubSubNumSub(context.Background(), openAIAccountHealthSharedChannel).Result()
		return err == nil && counts[openAIAccountHealthSharedChannel] == 1
	}, time.Second, 10*time.Millisecond)

	done := make(chan struct{})
	go func() {
		shared.close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shared health workers did not stop")
	}
}

func TestOpenAIAccountHealthSharedState_LoadQueueIsBounded(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	stats := newOpenAIAccountRuntimeStats()
	shared := newOpenAIAccountHealthSharedState(client, stats, 1)
	shared.loads = make(chan openAIAccountRuntimeStatsKey, 1)
	t.Cleanup(shared.close)

	first := openAIAccountRuntimeStatsKey{accountID: 1, kind: "text", model: "gpt-5.5", transport: "http_sse"}
	second := openAIAccountRuntimeStatsKey{accountID: 2, kind: "text", model: "gpt-5.5", transport: "http_sse"}
	shared.load(first)
	shared.load(second)

	require.Len(t, shared.loads, 1)
	_, firstQueued := shared.loadOnce.Load(openAIAccountHealthRedisKey(first))
	_, secondQueued := shared.loadOnce.Load(openAIAccountHealthRedisKey(second))
	require.True(t, firstQueued)
	require.False(t, secondQueued, "a dropped load must remain retryable")
}

func TestOpenAIAccountHealthSharedReportRetryIsIdempotent(t *testing.T) {
	shared, _ := newOpenAISharedHealthTestState(t)
	key := openAIAccountRuntimeStatsKey{accountID: 804, kind: "text", model: "gpt-test", transport: "http_sse"}
	ttft := 160
	report := openAIAccountHealthSharedReport{reportID: "lost-ack-report", key: key, success: false, ttftMS: &ttft, updatedNano: time.Now().UnixNano()}
	first, err := shared.persist(report)
	require.NoError(t, err)
	retried, err := shared.persist(report)
	require.NoError(t, err)
	require.Equal(t, first, retried)
	require.EqualValues(t, 1, retried.SampleCount)
	require.EqualValues(t, 1, retried.TTFTSampleCount)
	report.reportID = "next-real-report"
	report.updatedNano++
	next, err := shared.persist(report)
	require.NoError(t, err)
	require.EqualValues(t, 2, next.SampleCount)
}

func TestOpenAIAccountHealthRefreshRepairsMissedPublicationAndEmptyLoad(t *testing.T) {
	shared, stats := newOpenAISharedHealthTestState(t)
	stats.shared = shared
	key := openAIAccountRuntimeStatsKey{accountID: 805, kind: "text", model: "gpt-test", transport: "http_sse"}
	// A route was first seen while no shared sample existed.
	stat := stats.loadOrCreateForKey(key)
	shared.loadFromRedis(<-shared.loads)
	_, pending := shared.loadOnce.Load(openAIAccountHealthRedisKey(key))
	require.False(t, pending, "an empty first load must remain refreshable")
	_, err := shared.persist(openAIAccountHealthSharedReport{key: key, success: false, updatedNano: time.Now().UnixNano()})
	require.NoError(t, err) // Deliberately do not publish this update.
	shared.refreshActiveKeys()
	shared.loadFromRedis(<-shared.loads)
	errorRate, _, _, found, samples, _, _ := stats.snapshotForKeyWithMeta(key)
	require.True(t, found)
	require.Equal(t, 1.0, errorRate)
	require.EqualValues(t, 1, samples)
	// Periodic refresh is bounded and coalesced, not a Redis read per candidate.
	for range 100 {
		shared.refreshKey(key, stat)
	}
	require.Empty(t, shared.loads)
}

func TestOpenAIAccountEmptyRouteHydrationKeepsGenericFallback(t *testing.T) {
	shared, stats := newOpenAISharedHealthTestState(t)
	stats.shared = shared
	stats.report(806, false, nil)
	for range 2 {
		errorRate, _, _, samples, _, _ := stats.snapshotForRouteWithMeta(806, "new-model", OpenAIUpstreamTransportHTTPSSE)
		require.Equal(t, .2, errorRate)
		require.EqualValues(t, 1, samples)
	}
}

func TestOpenAIHealthRefreshRepairsMissedResetFence(t *testing.T) {
	shared, stats := newOpenAISharedHealthTestState(t)
	key := openAIAccountRuntimeStatsKey{accountID: 807, kind: "text", model: "gpt-test", transport: "http_sse"}
	ttft := 700
	event, err := shared.persist(openAIAccountHealthSharedReport{key: key, success: false, ttftMS: &ttft, updatedNano: 100})
	require.NoError(t, err)
	stats.applySharedHealthEvent(event)
	// Persist the reset but deliberately miss its PubSub event on this replica.
	_, err = openAIAccountHealthResetFenceScript.Run(t.Context(), shared.client,
		[]string{openAIAccountHealthResetRedisKey(key.accountID)}, 200, 600).Result()
	require.NoError(t, err)
	_, err = openAIAccountHealthResetErrorScript.Run(t.Context(), shared.client,
		[]string{openAIAccountHealthRedisKey(key)}, 200, 600).Result()
	require.NoError(t, err)
	shared.loadFromRedis(key)
	errorRate, value, hasTTFT, _, samples, _, _ := stats.snapshotForKeyWithMeta(key)
	require.Zero(t, errorRate)
	require.Zero(t, samples)
	require.True(t, hasTTFT)
	require.EqualValues(t, ttft, value)
	// A newer real failure must survive the same reset fence on future refresh.
	_, err = shared.persist(openAIAccountHealthSharedReport{key: key, success: false, updatedNano: 300})
	require.NoError(t, err)
	shared.loadFromRedis(key)
	errorRate, _, _, _, samples, _, _ = stats.snapshotForKeyWithMeta(key)
	require.Equal(t, 1.0, errorRate)
	require.EqualValues(t, 1, samples)
}

func TestOpenAIHealthColdRouteHydrationIsBounded(t *testing.T) {
	shared, stats := newOpenAISharedHealthTestState(t)
	stats.shared = shared
	for model := 0; model < openAIAccountHealthMissingKeyLimit+100; model++ {
		key := openAIAccountRuntimeStatsKey{accountID: int64(model + 1), kind: "text", model: "unknown-model", transport: "http_sse"}
		stats.snapshotForKeyWithMeta(key)
	}
	require.LessOrEqual(t, len(shared.missingUntil), openAIAccountHealthMissingKeyLimit)
	require.LessOrEqual(t, len(shared.loads), openAIAccountHealthLoadQueueSize)
	require.Zero(t, stats.size(), "cold reads must not allocate runtime stats for every candidate/model pair")
}

func TestOpenAIHealthRedisStallHonorsHealthSocketTimeout(t *testing.T) {
	for _, operation := range []string{"persist", "refresh", "reset", "subscribe"} {
		t.Run(operation, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })
			go func() { _, _ = io.Copy(io.Discard, serverConn) }()
			client := redis.NewClient(&redis.Options{
				Dialer:      func(context.Context, string, string) (net.Conn, error) { return clientConn, nil },
				ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second,
				MaxRetries: -1, PoolSize: 1, DisableIdentity: true,
			})
			t.Cleanup(func() { _ = client.Close() })
			shared := newOpenAIAccountHealthSharedState(client, newOpenAIAccountRuntimeStats(), 4)
			t.Cleanup(shared.close)
			key := openAIAccountRuntimeStatsKey{accountID: 808, kind: "text"}
			started := time.Now()
			if operation == "persist" {
				_, err := shared.persist(openAIAccountHealthSharedReport{key: key, reportID: "stalled", updatedNano: started.UnixNano()})
				require.Error(t, err)
			} else if operation == "reset" {
				err := shared.resetAccountErrorHealth(context.Background(), key.accountID, started.UnixNano())
				require.Error(t, err)
			} else if operation == "subscribe" {
				done := make(chan struct{})
				go func() { shared.runSubscriber(); close(done) }()
				cancelTimer := time.AfterFunc(50*time.Millisecond, shared.cancel)
				defer cancelTimer.Stop()
				select {
				case <-done:
				case <-time.After(2 * openAIAccountHealthIOTimeout):
					t.Fatal("stalled subscriber ignored cancellation and its health I/O window")
				}
			} else {
				shared.loadOnce.Store(openAIAccountHealthRedisKey(key), struct{}{})
				shared.loadFromRedis(key)
				require.EqualValues(t, 1, shared.loadErrors.Load())
				_, pending := shared.loadOnce.Load(openAIAccountHealthRedisKey(key))
				require.False(t, pending, "a stalled load must not prevent a later refresh")
			}
			require.Less(t, time.Since(started), 2*openAIAccountHealthIOTimeout,
				"health I/O must not wait for the global Redis socket timeout")
			require.Equal(t, 10*time.Second, client.Options().ReadTimeout, "billing/scheduling Redis options remain unchanged")
		})
	}
}
