package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type failingOpenAIPlaceholderWriter struct {
	gin.ResponseWriter
}

func (w *failingOpenAIPlaceholderWriter) Write(data []byte) (int, error) {
	return 0, errors.New("write failed")
}

func (w *failingOpenAIPlaceholderWriter) WriteString(data string) (int, error) {
	return 0, errors.New("write failed")
}

func TestOpenAIPlaceholderCoordinatorWritesTimeoutOnlyOnceAcrossAttempts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	StartOpenAIPlaceholderCoordination(c, time.Now().Add(-time.Second))

	first := writeOpenAIRequestFirstTokenTimeoutPlaceholder(c, time.Now(), "gpt-5.4", openAIRequestFirstTokenPlaceholderDialectResponses)
	second := writeOpenAIRequestFirstTokenTimeoutPlaceholder(c, time.Now(), "gpt-5.4", openAIRequestFirstTokenPlaceholderDialectResponses)

	require.True(t, first.Sent)
	require.True(t, second.Sent)
	require.False(t, first.UpstreamCommitted)
	require.Equal(t, 1, strings.Count(recorder.Body.String(), `"type":"response.transport_progress.delta"`))
	require.True(t, OpenAIRequestAllowsFailover(c, -1))
}

func TestOpenAIPlaceholderCoordinatorBlocksFailoverAfterUpstreamCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	StartOpenAIPlaceholderCoordination(c, time.Now())
	require.True(t, writeOpenAIRequestFirstTokenTimeoutPlaceholder(c, time.Now(), "gpt-5.4", openAIRequestFirstTokenPlaceholderDialectResponses).Sent)

	_, err := c.Writer.WriteString("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
	require.NoError(t, err)

	require.True(t, OpenAIRequestUpstreamCommitted(c))
	require.False(t, OpenAIRequestAllowsFailover(c, -1))
}

func TestOpenAIPlaceholderCoordinatorMixedBatchCommitsUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	StartOpenAIPlaceholderCoordination(c, time.Now())

	mixed := strings.Join([]string{
		": keepalive",
		"",
		`data: {"type":"response.transport_progress.delta","delta":"in_progress"}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"ok"}`,
		"",
	}, "\n")
	_, err := c.Writer.WriteString(mixed)
	require.NoError(t, err)
	require.True(t, OpenAIRequestUpstreamCommitted(c))
	require.False(t, OpenAIRequestAllowsFailover(c, -1))
}

func TestOpenAIPlaceholderCoordinatorRealTextContainingPlaceholderNameCommitsUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	StartOpenAIPlaceholderCoordination(c, time.Now())

	_, err := c.Writer.WriteString(
		`data: {"type":"response.output_text.delta","delta":"response.transport_progress.delta"}` + "\n\n",
	)
	require.NoError(t, err)
	require.True(t, OpenAIRequestUpstreamCommitted(c))
	require.False(t, OpenAIRequestAllowsFailover(c, -1))
}

func TestOpenAIPlaceholderCoordinatorFailedWriteDoesNotCommitUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Writer = &failingOpenAIPlaceholderWriter{ResponseWriter: c.Writer}
	StartOpenAIPlaceholderCoordination(c, time.Now())

	_, err := c.Writer.WriteString(`data: {"type":"response.output_text.delta","delta":"ok"}` + "\n\n")
	require.Error(t, err)
	require.False(t, OpenAIRequestUpstreamCommitted(c))
	require.True(t, OpenAIRequestAllowsFailover(c, -1))
}

func TestOpenAIPlaceholderCoordinatorWritesSafePlaceholderOnlyOnceAcrossAttempts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	StartOpenAIPlaceholderCoordination(c, time.Now())

	frame := openAIResponsesSafeTokenPlaceholderFrame("")
	require.True(t, writeOpenAISafePlaceholder(c, frame, "", 0))
	require.True(t, writeOpenAISafePlaceholder(c, frame, "", 0))
	require.Equal(t, 1, strings.Count(recorder.Body.String(), `"type":"response.transport_progress.delta"`))
	require.True(t, OpenAIRequestAllowsFailover(c, -1))
}

func TestOpenAIPlaceholderCoordinatorDoesNotChangeCommentOnlyFailoverSemantics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	StartOpenAIPlaceholderCoordination(c, time.Now())
	before := c.Writer.Size()

	_, err := c.Writer.WriteString(":\n\n")
	require.NoError(t, err)
	require.False(t, OpenAIRequestUpstreamCommitted(c))
	require.False(t, OpenAIRequestAllowsFailover(c, before))
	require.False(t, OpenAIRequestTokenPlaceholderWritten(c))
	require.False(t, openAIWSPlaceholderCoordinationPending(c))
}

func TestOpenAIWSPlaceholderStopsSameAccountReconnectAndSwitchesAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	StartOpenAIPlaceholderCoordination(c, time.Now())
	require.False(t, openAIWSPlaceholderCoordinationPending(c))
	openAIPlaceholderCoordinatorFromContext(c).activate()
	require.True(t, openAIWSPlaceholderCoordinationPending(c))
	require.True(t, openAIWSShouldStopReconnectForPlaceholderCoordination(c, wrapOpenAIWSFallback("read_event", errors.New("connection closed"))), "configured placeholder requests stop retryable WS reconnects before the deadline too")
	require.False(t, openAIWSShouldStopReconnectForPlaceholderCoordination(c, wrapOpenAIWSFallback("auth_failed", errors.New("unauthorized"))), "non-retryable WS errors retain their existing response semantics")
	require.False(t, openAIWSShouldStopReconnectForPlaceholderCoordination(c, wrapOpenAIWSFallback("previous_response_not_found", errors.New("missing"))), "protocol recovery errors are handled before reconnect suppression")

	state := writeOpenAIRequestFirstTokenTimeoutPlaceholder(c, time.Now(), "gpt-5.4", openAIRequestFirstTokenPlaceholderDialectResponses)
	require.True(t, state.Sent)
	require.True(t, OpenAIRequestTokenPlaceholderWritten(c))
	require.True(t, openAIWSShouldStopReconnectForPlaceholderCoordination(c, wrapOpenAIWSFallback("read_event", errors.New("connection closed"))))

	failoverErr := newOpenAIWSPlaceholderFailoverError(wrapOpenAIWSFallback("read_event", errors.New("connection closed")))
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.True(t, failoverErr.SkipPoolSoftCooldown)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.True(t, openAIWSShouldFailoverFailedEventForPlaceholderCoordination(c, []byte(
		`{"type":"response.failed","response":{"error":{"code":"server_error","message":"upstream failed"}}}`,
	)))

	_, err := c.Writer.WriteString(`data: {"type":"response.output_text.delta","delta":"ok"}` + "\n\n")
	require.NoError(t, err)
	require.True(t, OpenAIRequestUpstreamCommitted(c))
	require.False(t, openAIWSPlaceholderCoordinationPending(c))
	require.False(t, openAIWSShouldFailoverFailedEventForPlaceholderCoordination(c, []byte(
		`{"type":"response.failed","response":{"error":{"code":"server_error","message":"upstream failed"}}}`,
	)))
}

func TestOpenAIPlaceholderCoordinatorArmsAgainstRequestStart(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	StartOpenAIPlaceholderCoordination(c, time.Now().Add(-time.Second))
	account := &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra: map[string]any{
			openAIAPIKeyFirstTokenTimeoutPlaceholderEnabledExtraKey:      true,
			openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey:           800,
			openAIAPIKeyFirstTokenTimeoutPlaceholderGuardEnabledExtraKey: false,
		},
	}

	(&OpenAIGatewayService{}).ArmOpenAIResponsesFirstTokenPlaceholder(c, account, "gpt-5.4")

	require.Eventually(t, func() bool {
		return OpenAIRequestPlaceholderWritten(c)
	}, time.Second, 10*time.Millisecond)
	require.Contains(t, recorder.Body.String(), `"type":"response.transport_progress.delta"`)
	require.True(t, OpenAIRequestAllowsFailover(c, -1))
}

func TestOpenAIPlaceholderCoordinatorPreparesStableChatIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	StartOpenAIPlaceholderCoordination(c, time.Now())
	account := &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra: map[string]any{
			openAIAPIKeyFirstTokenTimeoutPlaceholderEnabledExtraKey:      true,
			openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey:           800,
			openAIAPIKeyFirstTokenTimeoutPlaceholderGuardEnabledExtraKey: false,
		},
	}

	(&OpenAIGatewayService{}).ArmOpenAIChatFirstTokenPlaceholder(c, account, "gpt-5.4")
	before := openAIPlaceholderCoordinatorFromContext(c).snapshot()
	require.NotEmpty(t, before.ChatID)
	require.NotZero(t, before.ChatCreated)
	require.False(t, before.Sent)

	written := writeOpenAIRequestFirstTokenTimeoutPlaceholder(c, time.Now(), "gpt-5.4", openAIRequestFirstTokenPlaceholderDialectChatCompletions)
	require.True(t, written.Sent)
	require.Equal(t, before.ChatID, written.ChatID)
	require.Equal(t, before.ChatCreated, written.ChatCreated)
	require.Contains(t, recorder.Body.String(), before.ChatID)
}

func TestOpenAIPlaceholderCoordinatorComposesWithCompactKeepaliveWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	MarkOpenAICompactClientStream(c)
	stop := StartOpenAICompactSSEKeepalive(c, time.Hour)
	defer stop()
	StartOpenAIPlaceholderCoordination(c, time.Now())

	done := make(chan openAIRequestFirstTokenPlaceholderState, 1)
	go func() {
		done <- writeOpenAIRequestFirstTokenTimeoutPlaceholder(c, time.Now(), "gpt-5.4", openAIRequestFirstTokenPlaceholderDialectResponses)
	}()

	select {
	case state := <-done:
		require.True(t, state.Sent)
	case <-time.After(time.Second):
		t.Fatal("placeholder writer deadlocked with compact keepalive wrapper")
	}
	require.Contains(t, recorder.Body.String(), `"type":"response.transport_progress.delta"`)
}
