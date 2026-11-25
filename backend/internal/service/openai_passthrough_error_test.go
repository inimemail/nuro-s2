package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIHTTPPassthroughReturnsSafeEncryptedContentCode(t *testing.T) {
	setGinTestMode()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := []byte(`{"error":{"type":"relay debug","code":"invalid_encrypted_content","param":"encrypted_content","message":"https://private.example diagnostic"},"secret":"relay-token"}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "Set-Cookie": []string{"private=relay-token"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}}
	s := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID: 12, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{"access_token": "relay-token"},
		Extra:       map[string]any{"openai_passthrough": true},
	}
	_, err := s.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.2","stream":false,"input":"hello"}`))
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.JSONEq(t, `{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","param":"encrypted_content","message":"The encrypted content could not be verified"}}`, rec.Body.String())
	require.Empty(t, rec.Header().Get("Set-Cookie"))
}

func TestOpenAIHTTPPassthroughRuleUsesOnlyLocalSnapshot(t *testing.T) {
	setGinTestMode()
	account := &Account{Platform: PlatformOpenAI, Name: "private-relay", Credentials: map[string]any{"api_key": "custom-credential"}}
	body := []byte(`{"error":{"code":{"debug":"private.example"},"message":"provider internal output"}}`)
	ruleService := &ErrorPassthroughService{}
	generate := func() (*gin.Context, *httptest.ResponseRecorder) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		BindErrorPassthroughService(c, ruleService)
		writeOpenAIHTTPPassthroughError(c, account, http.StatusBadRequest, body, "")
		return c, rec
	}
	_, rec := generate()
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.JSONEq(t, `{"error":{"type":"upstream_error","message":"Upstream request failed"}}`, rec.Body.String())

	status := http.StatusUnprocessableEntity
	message := "Try a shorter request"
	rule := &model.ErrorPassthroughRule{
		Enabled: true, ErrorCodes: []int{http.StatusBadRequest}, MatchMode: model.MatchModeAny,
		ResponseCode: &status, CustomMessage: &message, SkipMonitoring: true,
	}
	ruleService.setLocalCache([]*model.ErrorPassthroughRule{rule})
	c, rec := generate()
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	require.Equal(t, message, gjson.Get(rec.Body.String(), "error.message").String())
	require.Equal(t, true, c.GetBool(OpsSkipPassthroughKey))

	invalidStatus := http.StatusNoContent
	unsafe := "private-relay returned custom-credential"
	ruleCopy := *rule
	ruleCopy.ResponseCode = &invalidStatus
	ruleCopy.CustomMessage = &unsafe
	ruleService.setLocalCache([]*model.ErrorPassthroughRule{&ruleCopy})
	_, rec = generate()
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, safeUpstreamErrorMessage, gjson.Get(rec.Body.String(), "error.message").String())

	account.Credentials[credKeyHeaderOverrides] = map[string]any{"x-relay-secret": "q7x", "x-session-signature": "q8x secondpart"}
	account.Proxy = &Proxy{Username: "proxy7", Password: "relay-pass"}
	for _, unsafe := range []string{"Credential q7x expired", "Token=q8x secondpart", "Check relay-pass", "Contact proxy7", "Unexpected \u202e text"} {
		ruleCopy := *rule
		ruleCopy.CustomMessage = &unsafe
		ruleService.setLocalCache([]*model.ErrorPassthroughRule{&ruleCopy})
		_, rec = generate()
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		require.Equal(t, safeUpstreamErrorMessage, gjson.Get(rec.Body.String(), "error.message").String())
	}
}

func TestOpenAIHTTPPassthroughCompactUsesSingleTerminal(t *testing.T) {
	c, rec := newCompactBridgeTestContext(t, true)
	keepalive := &openAICompactSSEKeepalive{writer: c.Writer, stop: make(chan struct{})}
	c.Set(openAICompactSSEKeepaliveKey, keepalive)
	require.True(t, keepalive.beat())
	body := []byte(`{"response":{"id":"resp_safe","error":{"code":"  ","message":"{\"error\":{\"code\":\"slow_down\"}}"}}}`)
	account := &Account{Platform: PlatformOpenAI}
	writeOpenAIHTTPPassthroughError(c, account, http.StatusServiceUnavailable, body, "")
	writeOpenAIHTTPPassthroughError(c, account, http.StatusServiceUnavailable, body, "")

	raw := strings.TrimPrefix(rec.Body.String(), ": keepalive\n\n")
	events := parseCompactBridgeSSE(t, raw)
	require.Len(t, events, 1)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "response.failed", events[0][0])
	require.Equal(t, "resp_safe", gjson.Get(events[0][1], "response.id").String())
	require.Equal(t, "server_error", gjson.Get(events[0][1], "response.error.code").String())
	require.True(t, strings.HasSuffix(raw, "\n\n"))
	ops, ok := GetOpsStreamError(c)
	require.True(t, ok)
	require.Equal(t, "upstream_error", ops.ErrType)
	require.Equal(t, http.StatusServiceUnavailable, ops.IntendedStatus)
}

func TestOpenAICompactCapacityFailureAfterRetries(t *testing.T) {
	c, rec := newCompactBridgeTestContext(t, true)
	keepalive := &openAICompactSSEKeepalive{writer: c.Writer, stop: make(chan struct{})}
	c.Set(openAICompactSSEKeepaliveKey, keepalive)
	require.True(t, keepalive.beat())
	body := []byte(`{"response_id":"resp_private_identity","error":{"code":"server_is_overloaded","message":"private.example"}}`)
	require.True(t, WriteOpenAICompactCapacityFailure(c, http.StatusTooManyRequests, http.StatusTooManyRequests, body, "rate_limit_error", "gpt-5.4"))
	require.Contains(t, rec.Body.String(), `"code":"server_error"`)
	require.Contains(t, rec.Body.String(), `"model":"gpt-5.4"`)
	require.NotContains(t, rec.Body.String(), "private.example")
	require.NotContains(t, rec.Body.String(), "resp_private_identity")
	require.Equal(t, http.StatusOK, rec.Code)
	ops, ok := GetOpsStreamError(c)
	require.True(t, ok)
	require.Equal(t, "rate_limit_error", ops.ErrType)
	require.Equal(t, http.StatusTooManyRequests, ops.IntendedStatus)
	plain, plainRec := newCompactBridgeTestContext(t, false)
	require.False(t, WriteOpenAICompactCapacityFailure(plain, http.StatusTooManyRequests, http.StatusTooManyRequests, body, "rate_limit_error", "gpt-5.4"))
	require.Zero(t, plainRec.Body.Len())
}

func TestOpenAIHTTPPassthroughSerializesWithPlaceholder(t *testing.T) {
	setGinTestMode()
	account := &Account{Platform: PlatformOpenAI}
	for range 20 {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ensureOpenAIPlaceholderCoordinator(c, time.Now())
		var workers sync.WaitGroup
		workers.Go(func() {
			writeOpenAIGatewayPlaceholder(c, openAIResponsesSafeTokenPlaceholderFrame(""), "", 0)
		})
		workers.Go(func() {
			writeOpenAIHTTPPassthroughError(c, account, http.StatusBadRequest,
				[]byte(`{"error":{"code":"invalid_encrypted_content"}}`), "")
		})
		workers.Wait()
		body := rec.Body.String()
		if rec.Code == http.StatusBadRequest {
			require.True(t, gjson.Valid(body))
			require.Equal(t, "invalid_encrypted_content", gjson.Get(body, "error.code").String())
		} else {
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, 1, strings.Count(body, "event: response.failed\n"))
			require.Contains(t, body, `"code":"invalid_encrypted_content"`)
		}
		require.True(t, GatewayErrorTerminalSent(c))
	}
}

func TestOpenAIHTTPPassthroughTerminalFollowsPartialFrame(t *testing.T) {
	for _, pending := range []string{
		`data: {"type":"response.created"`,
		`data: {"type":"response.created","error":null}`,
	} {
		c, rec := newCompactBridgeTestContext(t, false)
		ensureOpenAIPlaceholderCoordinator(c, time.Now())
		c.Header("Content-Type", "text/event-stream")
		_, err := c.Writer.WriteString(pending)
		require.NoError(t, err)
		writeOpenAIHTTPPassthroughError(c, &Account{Platform: PlatformOpenAI}, http.StatusBadRequest, nil, "")
		require.Contains(t, rec.Body.String(), "\n\nevent: response.failed\n")
		require.True(t, OpenAIRequestResponsesTerminalWritten(c))
	}
}

func TestOpenAIHTTPPassthroughCompletesPendingTerminalWithoutDuplicate(t *testing.T) {
	for _, eventType := range []string{"response.completed", "response.failed"} {
		t.Run(eventType, func(t *testing.T) {
			c, rec := newCompactBridgeTestContext(t, false)
			ensureOpenAIPlaceholderCoordinator(c, time.Now())
			c.Header("Content-Type", "text/event-stream")
			frame := "event: " + eventType + "\ndata: {\"type\":\"" + eventType + "\",\"response\":{\"id\":\"resp_upstream\"}}"
			_, err := c.Writer.WriteString(frame)
			require.NoError(t, err)
			require.False(t, OpenAIRequestResponsesTerminalWritten(c))

			writeOpenAIHTTPPassthroughError(c, &Account{Platform: PlatformOpenAI}, http.StatusBadGateway, nil, "")

			require.Equal(t, frame+"\n\n", rec.Body.String())
			require.True(t, OpenAIRequestResponsesTerminalWritten(c))
			require.True(t, GatewayErrorTerminalSent(c))
			_, hasSyntheticError := GetOpsStreamError(c)
			require.False(t, hasSyntheticError)
		})
	}
}

type shortOpenAIErrorWriter struct {
	gin.ResponseWriter
	writes int
}

func (w *shortOpenAIErrorWriter) WriteString(frame string) (int, error) {
	w.writes++
	n, _ := w.ResponseWriter.WriteString(frame[:len(frame)/2])
	return n, io.ErrShortWrite
}

func TestOpenAIHTTPPassthroughShortTerminalStopsPlaceholder(t *testing.T) {
	c, rec := newCompactBridgeTestContext(t, false)
	shortWriter := &shortOpenAIErrorWriter{ResponseWriter: c.Writer}
	c.Writer = shortWriter
	coordinator := ensureOpenAIPlaceholderCoordinator(c, time.Now())
	c.Header("Content-Type", "text/event-stream")
	_, err := c.Writer.Write([]byte(`data: {"type":"response.created"`))
	require.NoError(t, err)
	placeholder := openAIResponsesSafeTokenPlaceholderFrame("")
	require.True(t, writeOpenAIGatewayPlaceholder(c, placeholder, "", 0))
	require.NotNil(t, coordinator.pendingTimeoutFrame)

	account := &Account{Platform: PlatformOpenAI}
	writeOpenAIHTTPPassthroughError(c, account, http.StatusBadRequest, nil, "")
	require.True(t, GatewayErrorTerminalSent(c))
	require.True(t, OpenAIRequestUpstreamCommitted(c))
	require.False(t, OpenAIRequestResponsesTerminalWritten(c))
	require.Nil(t, coordinator.pendingTimeoutFrame)
	select {
	case <-coordinator.stop:
	default:
		t.Fatal("placeholder timer is still running after the terminal write")
	}
	require.False(t, writeOpenAIGatewayPlaceholder(c, placeholder, "", 0))
	writeOpenAIHTTPPassthroughError(c, account, http.StatusBadRequest, nil, "")
	require.Equal(t, 1, shortWriter.writes)
	require.NotContains(t, rec.Body.String(), "response.transport_progress.delta")
}

func TestOpenAIHTTPPassthroughMetadataRequiresAValidEnvelope(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":false,"message":"{\"error\":{\"code\":\"invalid_encrypted_content\"}}"}}`,
		`{"error":{"code":{"message":"context_length_exceeded"},"param":"private.example"}}`,
		`{"error":{"code":"invalid_encrypted_content"}} <html>`,
		`{"response":{"id":"resp_private"}}`,
		`{"response":{"id":"resp_private"},"error":{"code":null}}`,
		`{"response_id":123,"error":{"code":null}}`,
		`{"response_id":"resp_private ","error":{"code":null}}`,
		`{"error":{"code":"invalid_encrypted_content","message":"` + "\xff" + `"}}`,
		strings.Repeat(" ", maxOpenAIHTTPErrorMetadataBytes) + `{"error":{"code":"invalid_encrypted_content"}}`,
	} {
		code, param, responseID := openAIHTTPErrorMetadata(http.StatusBadRequest, []byte(body))
		require.Empty(t, code)
		require.Empty(t, param)
		require.Empty(t, responseID)
	}
	code, param, _ := openAIHTTPErrorMetadata(http.StatusBadRequest, []byte(`{"error":{"code":"thinking_signature_invalid","message":"Encrypted content could not be verified","param":"private.example"}}`))
	require.Equal(t, "invalid_encrypted_content", code)
	require.Empty(t, param)
	code, _, _ = openAIHTTPErrorMetadata(http.StatusBadRequest, []byte(`{"error":{"message":"maximum context length exceeded"}}`))
	require.Equal(t, "context_length_exceeded", code)
}
