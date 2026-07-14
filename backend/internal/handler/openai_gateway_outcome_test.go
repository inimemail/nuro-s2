package handler

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestClassifyOpenAIResponsesForwardResult(t *testing.T) {
	tests := []struct {
		name       string
		result     *service.OpenAIForwardResult
		successful bool
		neutral    bool
	}{
		{name: "non-stream", result: &service.OpenAIForwardResult{}, successful: true},
		{name: "non-stream incomplete", result: &service.OpenAIForwardResult{TerminalEventType: "response.incomplete"}, neutral: true},
		{name: "non-stream failed", result: &service.OpenAIForwardResult{TerminalEventType: "response.failed"}},
		{name: "non-stream error", result: &service.OpenAIForwardResult{TerminalEventType: "error"}},
		{name: "completed", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.completed"}, successful: true},
		{name: "chat done", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "[DONE]"}, successful: true},
		{name: "failed", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.failed"}},
		{name: "missing terminal", result: &service.OpenAIForwardResult{Stream: true}},
		{name: "incomplete", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.incomplete"}, neutral: true},
		{name: "client disconnect after complete", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.completed", ClientDisconnect: true}, neutral: true},
		{name: "cyber failed", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.failed", CyberBlocked: true}, neutral: true},
		{name: "cyber completed", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.completed", CyberBlocked: true}, neutral: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			successful, neutral := classifyOpenAIResponsesForwardResult(tt.result)
			require.Equal(t, tt.successful, successful)
			require.Equal(t, tt.neutral, neutral)
		})
	}
}

func TestClassifyGatewayForwardResult(t *testing.T) {
	tests := []struct {
		name       string
		result     *service.ForwardResult
		err        error
		successful bool
		neutral    bool
	}{
		{name: "success", result: &service.ForwardResult{}, successful: true},
		{name: "forward error", result: &service.ForwardResult{}, err: errors.New("failed")},
		{name: "failed outcome", result: &service.ForwardResult{FailedOutcome: true}},
		{name: "neutral outcome", result: &service.ForwardResult{NeutralOutcome: true}, neutral: true},
		{name: "client disconnect", result: &service.ForwardResult{ClientDisconnect: true}, neutral: true},
		{name: "neutral wins over failed", result: &service.ForwardResult{FailedOutcome: true, NeutralOutcome: true}, neutral: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			successful, neutral := classifyGatewayForwardResult(tt.result, tt.err)
			require.Equal(t, tt.successful, successful)
			require.Equal(t, tt.neutral, neutral)
		})
	}
}

func TestRecordSuccessfulOpenAIOpsTTFT(t *testing.T) {
	gin.SetMode(gin.TestMode)
	firstTokenMs := 42

	tests := []struct {
		name   string
		result *service.OpenAIForwardResult
		err    error
		want   bool
	}{
		{name: "completed", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.completed", FirstTokenMs: &firstTokenMs}, want: true},
		{name: "non-stream success", result: &service.OpenAIForwardResult{FirstTokenMs: &firstTokenMs}, want: true},
		{name: "incomplete", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.incomplete", FirstTokenMs: &firstTokenMs}},
		{name: "failed", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.failed", FirstTokenMs: &firstTokenMs}},
		{name: "completed with forwarding error", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.completed", FirstTokenMs: &firstTokenMs}, err: errors.New("late failure")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			recordSuccessfulOpenAIOpsTTFT(c, tt.result, tt.err)
			value, exists := c.Get(service.OpsTimeToFirstTokenMsKey)
			require.Equal(t, tt.want, exists)
			if tt.want {
				require.Equal(t, int64(firstTokenMs), value)
			}
		})
	}
}

func TestClassifyOpenAIImageForwardResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		result     *service.OpenAIForwardResult
		err        error
		successful bool
		neutral    bool
	}{
		{name: "non-stream success", result: &service.OpenAIForwardResult{}, successful: true},
		{name: "completed stream", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.completed"}, successful: true},
		{name: "partial stream failure", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.completed", ImageCount: 1}, err: errors.New("late stream failure")},
		{name: "client disconnected", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.completed", ClientDisconnect: true, ImageCount: 1}, err: errors.New("client disconnected"), neutral: true},
		{name: "incomplete", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.incomplete"}, err: errors.New("incomplete"), neutral: true},
		{name: "cyber", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.failed", CyberBlocked: true}, err: errors.New("blocked"), neutral: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			successful, neutral := classifyOpenAIImageForwardResult(tt.result, tt.err)
			require.Equal(t, tt.successful, successful)
			require.Equal(t, tt.neutral, neutral)
		})
	}
}

func TestClassifyOpenAIResponsesForwardResultWithError(t *testing.T) {
	firstTokenMs := 120
	result := &service.OpenAIForwardResult{
		Stream:            true,
		TerminalEventType: "response.completed",
		FirstTokenMs:      &firstTokenMs,
	}

	successful, neutral := classifyOpenAIResponsesForwardResultWithError(result, errors.New("late protocol failure"))
	require.False(t, successful)
	require.False(t, neutral)
}

func TestShouldSettleOpenAIForwardResultAfterError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newContext := func() (*gin.Context, int) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		return c, service.OpenAICompactKeepaliveAdjustedWrittenSize(c)
	}

	tests := []struct {
		name   string
		result *service.OpenAIForwardResult
		want   bool
	}{
		{name: "nil", result: nil},
		{name: "empty result", result: &service.OpenAIForwardResult{}},
		{name: "usage", result: &service.OpenAIForwardResult{Usage: service.OpenAIUsage{InputTokens: 1}}, want: true},
		{name: "terminal", result: &service.OpenAIForwardResult{Stream: true, TerminalEventType: "response.failed"}, want: true},
		{name: "client disconnect", result: &service.OpenAIForwardResult{ClientDisconnect: true}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, before := newContext()
			require.Equal(t, tt.want, shouldSettleOpenAIForwardResultAfterError(c, before, tt.result))
		})
	}

	c, before := newContext()
	_, err := c.Writer.WriteString(":\n\n")
	require.NoError(t, err)
	require.True(t, shouldSettleOpenAIForwardResultAfterError(c, before, &service.OpenAIForwardResult{Stream: true}))
}

func TestShouldReportOpenAIWSProxyFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	plain, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.True(t, shouldReportOpenAIWSProxyFailure(plain, errors.New("proxy failed")))
	require.False(t, shouldReportOpenAIWSProxyFailure(plain, nil))

	disconnected, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.False(t, shouldReportOpenAIWSProxyFailure(disconnected, context.Canceled))

	cyber, _ := gin.CreateTestContext(httptest.NewRecorder())
	service.MarkOpsCyberPolicy(cyber, service.CyberPolicyMark{Message: "private upstream policy text"})
	require.False(t, shouldReportOpenAIWSProxyFailure(cyber, errors.New("upstream response failed")))
}
