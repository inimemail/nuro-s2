//go:build unit

package service

import (
	"context"
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

func TestCNToolsSilentChatStreamRespectsIdleTimeout(t *testing.T) {
	setGinTestMode()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway.StreamDataIntervalTimeout = 1
	svc := &OpenAIGatewayService{cfg: cfg}
	done := make(chan error, 1)
	go func() {
		_, err := svc.streamChatCompletionsAsResponses(context.Background(), c, &http.Response{StatusCode: 200, Header: make(http.Header), Body: reader}, &Account{ID: 1, Platform: PlatformDeepSeek, Type: AccountTypeAPIKey}, "deepseek", "deepseek", "deepseek", nil, nil, nil, false, nil, time.Now())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("silent stream returned success")
		}
	case <-time.After(1500 * time.Millisecond):
		t.Error("silent stream still blocked past configured 1-second idle timeout")
		writer.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("stream did not exit after EOF")
		}
	}
}

func TestCNBridgeNativeMultilineAndTerminalClosesBody(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	// Omit data.type on one event to exercise the SSE event-name fallback.
	body := cnBroadStart + ": keepalive\n\nevent: content_block_start\ndata: {\"index\":0,\ndata: \"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	go func() { _, _ = io.Copy(writer, strings.NewReader(body)) }()
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	done := make(chan error, 1)
	go func() {
		result, _, err := svc.readNativeAnthropicResponse(&http.Response{Body: reader})
		if err == nil && (len(result.Content) != 1 || result.Content[0].Text != "hello") {
			err = fmt.Errorf("text lost: %+v", result.Content)
		}
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("native terminal waited for TCP EOF")
	}
	_, err := writer.Write([]byte("unused"))
	require.ErrorIs(t, err, io.ErrClosedPipe)
}

func TestCNBridgeRawChatTerminalDoesNotWaitForEOF(t *testing.T) {
	setGinTestMode()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	go func() {
		_, _ = io.WriteString(writer, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	}()
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	done := make(chan error, 1)
	go func() {
		result, err := svc.streamRawChatCompletions(context.Background(), c, &http.Response{StatusCode: 200, Header: make(http.Header), Body: reader}, rawChatCompletionsTestAccount(), "test", "test", "test", nil, nil, time.Now(), 0, openAIRequestFirstTokenPlaceholderState{})
		if err == nil && (result.Usage.InputTokens != 2 || result.Usage.OutputTokens != 3) {
			err = fmt.Errorf("usage lost: %+v", result.Usage)
		}
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("chat terminal waited for TCP EOF")
	}
	require.True(t, strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n"))
}
