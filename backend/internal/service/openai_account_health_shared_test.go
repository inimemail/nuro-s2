package service

import (
	"context"
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
