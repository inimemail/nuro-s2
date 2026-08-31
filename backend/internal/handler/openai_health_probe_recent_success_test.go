package handler

import (
	"context"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newHealthProbeTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	return ctx, recorder
}

func TestOpenAIHealthProbeRecentSuccessServesCachedResponse(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Skipf("local Redis test server unavailable: %v", err)
	}
	t.Cleanup(mini.Close)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	h := &OpenAIGatewayHandler{redisClient: client}
	apiKey := &service.APIKey{ID: 42}
	const platform = service.PlatformOpenAI
	const model = "gpt-5.6-sol"
	h.recordOpenAIHealthProbeRecentSuccess(apiKey.ID, platform, model)

	ctx, recorder := newHealthProbeTestContext()
	startedAt := time.Now()
	require.True(t, h.tryServeOpenAIHealthProbeRecentSuccess(ctx, apiKey, platform, model, startedAt))
	elapsed := time.Since(startedAt)
	require.GreaterOrEqual(t, elapsed, openAIHealthProbeRecentSuccessMinDelay-20*time.Millisecond)
	require.Equal(t, 200, recorder.Code)
	require.Equal(t, "recent-success", recorder.Header().Get("X-Sub2API-Health-Probe-Source"))
	require.Contains(t, recorder.Body.String(), `"text":"MONITOR_OK"`)
	require.Contains(t, recorder.Body.String(), `"total_tokens":0`)
}

func TestOpenAIHealthProbeRecentSuccessRejectsFutureMarker(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Skipf("local Redis test server unavailable: %v", err)
	}
	t.Cleanup(mini.Close)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	h := &OpenAIGatewayHandler{redisClient: client}
	apiKey := &service.APIKey{ID: 43}
	key := openAIHealthProbeRecentSuccessKey(apiKey.ID, service.PlatformOpenAI, "gpt-5.6-sol")
	mini.Set(key, strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))

	ctx, _ := newHealthProbeTestContext()
	require.False(t, h.tryServeOpenAIHealthProbeRecentSuccess(ctx, apiKey, service.PlatformOpenAI, "gpt-5.6-sol", time.Now()))
}

func TestOpenAIHealthProbeRecentSuccessFallsBackWhenRedisUnavailable(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 20 * time.Millisecond, ReadTimeout: 20 * time.Millisecond, WriteTimeout: 20 * time.Millisecond, MaxRetries: 0})
	t.Cleanup(func() { _ = client.Close() })

	h := &OpenAIGatewayHandler{redisClient: client}
	ctx, _ := newHealthProbeTestContext()
	require.False(t, h.tryServeOpenAIHealthProbeRecentSuccess(ctx, &service.APIKey{ID: 44}, service.PlatformOpenAI, "gpt-5.6-sol", time.Now()))
}

func TestWaitOpenAIHealthProbeRecentSuccessStopsWhenRequestEnds(t *testing.T) {
	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	startedAt := time.Now()

	require.False(t, waitOpenAIHealthProbeRecentSuccess(requestCtx, startedAt, time.Second))
	require.Less(t, time.Since(startedAt), 100*time.Millisecond)
}

func TestWaitOpenAIHealthProbeRecentSuccessHonorsTarget(t *testing.T) {
	startedAt := time.Now()
	require.True(t, waitOpenAIHealthProbeRecentSuccess(context.Background(), startedAt, 20*time.Millisecond))
	require.GreaterOrEqual(t, time.Since(startedAt), 18*time.Millisecond)
}

func TestRandomOpenAIHealthProbeRecentSuccessDelayStaysWithinBounds(t *testing.T) {
	for range 1000 {
		delay := randomOpenAIHealthProbeRecentSuccessDelay()
		require.GreaterOrEqual(t, delay, openAIHealthProbeRecentSuccessMinDelay)
		require.LessOrEqual(t, delay, openAIHealthProbeRecentSuccessMaxDelay)
	}
}
