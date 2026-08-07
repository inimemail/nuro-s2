//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type messagesTempUnschedulableRepo struct {
	stubOpenAIAccountRepo
	accountID int64
	until     time.Time
}

func (r *messagesTempUnschedulableRepo) SetTempUnschedulable(_ context.Context, accountID int64, until time.Time, _ string) error {
	r.accountID = accountID
	r.until = until
	return nil
}

func buildResponsesFailedSSEStream(errType, errorMessage string) string {
	failed := fmt.Sprintf(`{"type":"response.failed","response":{"id":"resp_err","object":"response","status":"failed","error":{"type":"%s","message":"%s"},"output":[],"usage":{"input_tokens":10,"output_tokens":0,"total_tokens":10}}}`, errType, errorMessage)
	return fmt.Sprintf("data: %s\n\n", failed)
}

func TestForwardAsAnthropic_BufferedResponseFailed_ReturnsError(t *testing.T) {
	setGinTestMode()

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(buildResponsesFailedSSEStream("invalid_request_error", "Content policy violation"))),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, rawChatCompletionsTestAccount(), body, "", "")

	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream response failed")
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestForwardAsAnthropic_StreamingResponseFailed_ReturnsError(t *testing.T) {
	setGinTestMode()

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(buildResponsesFailedSSEStream("invalid_request_error", "private-provider.example failed"))),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, rawChatCompletionsTestAccount(), body, "", "")

	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream response failed")
	require.Contains(t, rec.Body.String(), safeUpstreamErrorMessage)
	require.NotContains(t, rec.Body.String(), "private-provider.example")
}

func TestForwardAsAnthropicStreamingBareErrorAfterOutputIsSanitized(t *testing.T) {
	setGinTestMode()

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_bare_error","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		"",
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial"}`,
		"",
		`event: error`,
		`data: {"type":"error","error":{"type":"server_error","message":"<!DOCTYPE html><title>private-provider.example | 502</title>"}}`,
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, rawChatCompletionsTestAccount(), body, "", "")

	require.Error(t, err)
	require.Contains(t, rec.Body.String(), `"text":"partial"`)
	require.Contains(t, rec.Body.String(), safeUpstreamErrorMessage)
	require.NotContains(t, rec.Body.String(), "private-provider.example")
	require.NotContains(t, rec.Body.String(), "DOCTYPE")
	require.NotContains(t, rec.Body.String(), "event: message_stop")
}

func TestForwardAsAnthropic_BufferedResponseFailed_Failover(t *testing.T) {
	setGinTestMode()

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(buildResponsesFailedSSEStream("rate_limit_error", "Rate limit reached"))),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, rawChatCompletionsTestAccount(), body, "", "")

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
}

func TestForwardAsAnthropic_TempUnschedulableHTTPErrorFailsOverBeforeCommit(t *testing.T) {
	setGinTestMode()
	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := []byte(`{"error":{"message":"Our servers are currently overloaded. Please try again later."}}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(upstreamBody)),
	}}
	repo := &messagesTempUnschedulableRepo{}
	account := rawChatCompletionsTestAccount()
	account.Credentials["temp_unschedulable_enabled"] = true
	account.Credentials["temp_unschedulable_rules"] = []any{map[string]any{
		"error_code": float64(http.StatusBadRequest), "keywords": []any{"currently overloaded"}, "duration_minutes": float64(1),
	}}
	svc := &OpenAIGatewayService{
		cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream,
		rateLimitService: NewRateLimitService(repo, nil, nil, nil, nil),
	}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.Equal(t, account.ID, repo.accountID)
	require.True(t, repo.until.After(time.Now()))
	require.False(t, IsResponseCommitted(c))
	require.Empty(t, rec.Body.String())
}
