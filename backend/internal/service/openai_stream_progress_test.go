package service

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClassifyOpenAIStreamProgressIgnoresStructuralAndPlaceholderEvents(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.created"}`,
		`{"type":"response.output_item.added","item":{"type":"message"}}`,
		`{"type":"response.content_part.added"}`,
		`{"type":"response.transport_progress.delta","delta":"in_progress"}`,
	} {
		observation := classifyOpenAIStreamProgress([]byte(payload), streamProgressTypeForTest(payload))
		require.False(t, observation.semantic, payload)
		require.False(t, observation.visible, payload)
	}
}

func TestClassifyOpenAIStreamProgressDistinguishesReasoningAndVisibleOutput(t *testing.T) {
	reasoning := classifyOpenAIStreamProgress(
		[]byte(`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`),
		"response.reasoning_summary_text.delta",
	)
	require.True(t, reasoning.semantic)
	require.True(t, reasoning.reasoning)
	require.False(t, reasoning.visible)

	visible := classifyOpenAIStreamProgress(
		[]byte(`{"type":"response.output_text.delta","delta":"answer"}`),
		"response.output_text.delta",
	)
	require.True(t, visible.semantic)
	require.False(t, visible.reasoning)
	require.True(t, visible.visible)
}

func TestOpenAIStreamProgressTrackerFreezesSampleAtFirstVisibleOutput(t *testing.T) {
	startedAt := time.Unix(100, 0)
	tracker := newOpenAIStreamProgressTracker(startedAt)
	tracker.observe(startedAt.Add(2*time.Second), openAIStreamProgressObservation{semantic: true, reasoning: true})
	tracker.observe(startedAt.Add(5*time.Second), openAIStreamProgressObservation{semantic: true, visible: true})
	tracker.observe(startedAt.Add(25*time.Second), openAIStreamProgressObservation{semantic: true, visible: true})
	tracker.observe(startedAt.Add(30*time.Second), openAIStreamProgressObservation{semantic: true, terminal: true})

	require.Equal(t, 3*time.Second, tracker.successfulSample())
}

func TestOpenAIStreamProgressTrackerKeepsTerminalOnlySample(t *testing.T) {
	startedAt := time.Unix(100, 0)
	tracker := newOpenAIStreamProgressTracker(startedAt)
	tracker.observe(startedAt.Add(7*time.Second), openAIStreamProgressObservation{semantic: true, terminal: true})

	require.Equal(t, 7*time.Second, tracker.successfulSample())
}

func TestOpenAIStreamProgressLearnerLearnsBoundedP95AfterMinimumSamples(t *testing.T) {
	learner := &openAIStreamProgressLearner{}
	key := openAIStreamProgressKey{accountID: 1, model: "gpt-5.4", transport: OpenAIUpstreamTransportHTTPSSE}
	for i := 0; i < openAIStreamProgressMinSamples-1; i++ {
		learner.observeSuccess(key, time.Duration(5+i)*time.Second)
	}
	require.Zero(t, learner.deadline(key, 0), "learning state must observe only")
	learner.observeSuccess(key, 12*time.Second)
	deadline := learner.deadline(key, 0)
	require.GreaterOrEqual(t, deadline, 12*time.Second)
	require.LessOrEqual(t, deadline, 30*time.Second)
}

func TestOpenAIStreamProgressLearnerDoesNotAllocateForUnobservedRoute(t *testing.T) {
	learner := &openAIStreamProgressLearner{}
	key := openAIStreamProgressKey{accountID: 1, model: "gpt-5.4", transport: OpenAIUpstreamTransportHTTPSSE}

	require.Zero(t, learner.deadline(key, 0))
	require.Zero(t, learner.count.Load())
	_, exists := learner.stats.Load(key)
	require.False(t, exists)
}

func TestOpenAIStreamProgressLearnerFallbackDoesNotAllocateRoute(t *testing.T) {
	learner := &openAIStreamProgressLearner{}
	key := openAIStreamProgressKey{accountID: 1, model: "gpt-5.4", transport: OpenAIUpstreamTransportHTTPSSE}

	deadline := learner.deadline(key, 10*time.Second)
	require.Equal(t, 17*time.Second, deadline)
	require.Zero(t, learner.count.Load())
	_, exists := learner.stats.Load(key)
	require.False(t, exists)
}

func TestOpenAIStreamProgressLearnerSuspendsAfterCorrelatedFailures(t *testing.T) {
	learner := &openAIStreamProgressLearner{}
	key := openAIStreamProgressKey{accountID: 1, model: "gpt-5.4", transport: OpenAIUpstreamTransportHTTPSSE}
	for i := 0; i < openAIStreamProgressMinSamples; i++ {
		learner.observeSuccess(key, 10*time.Second)
	}
	for i := 0; i < 5; i++ {
		learner.recordActionResult(key, i >= 3)
	}
	require.Zero(t, learner.deadline(key, 0))
}

func TestOpenAIStreamProgressLearnerStrictlyBoundsRouteCount(t *testing.T) {
	learner := &openAIStreamProgressLearner{}
	for accountID := int64(1); accountID <= openAIStreamProgressMaxRoutes+32; accountID++ {
		learner.observeSuccess(openAIStreamProgressKey{
			accountID: accountID,
			model:     "gpt-5.4",
			transport: OpenAIUpstreamTransportHTTPSSE,
		}, time.Second)
	}
	require.EqualValues(t, openAIStreamProgressMaxRoutes, learner.count.Load())
	entries := 0
	learner.stats.Range(func(_, _ any) bool {
		entries++
		return true
	})
	require.Equal(t, openAIStreamProgressMaxRoutes, entries)
}

func TestOpenAIStreamStallActionResultFeedsSuspensionGuard(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 7}
	key := normalizeOpenAIStreamProgressKey(account, "gpt-5.4", OpenAIUpstreamTransportHTTPSSE, "")
	for i := 0; i < openAIStreamProgressMinSamples; i++ {
		svc.openaiStreamProgressLearner.observeSuccess(key, 10*time.Second)
	}
	for i := 0; i < 5; i++ {
		svc.RecordOpenAIStreamStallActionResult(account, "gpt-5.4", OpenAIUpstreamTransportHTTPSSE, "", i >= 3)
	}
	require.Zero(t, svc.openaiStreamProgressLearner.deadline(key, 0))
}

func TestOpenAIStreamSemanticStallFailoverIsRequestLocalAndNeutral(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Name: "route"}
	err := svc.newOpenAIStreamSemanticStallFailoverError(nil, account)
	require.False(t, err.RetryableOnSameAccount)
	require.True(t, err.SkipPoolSoftCooldown)
	require.True(t, err.SkipPromptCacheAvoidance)
	require.True(t, err.SkipStickySessionEviction)
	require.True(t, err.SkipSchedulePenalty)
	require.Equal(t, GatewayFailureScopeRequest, err.Scope)
	require.Equal(t, NextAccountRetry, err.NextAccountAction)
}

func streamProgressTypeForTest(payload string) string {
	for _, eventType := range []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added",
		"response.transport_progress.delta",
	} {
		if strings.Contains(payload, `"type":"`+eventType+`"`) {
			return eventType
		}
	}
	return ""
}
