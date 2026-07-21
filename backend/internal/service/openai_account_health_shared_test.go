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
