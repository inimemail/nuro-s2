package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func filterGrokPingTestInput(t *testing.T, input string) string {
	t.Helper()
	body := newGrokResponsesBillingPingFilterBody(
		io.NopCloser(strings.NewReader(input)),
		&Account{Platform: PlatformGrok},
		defaultMaxLineSize,
	)
	output, err := io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	return string(output)
}

func TestGrokResponsesBillingPingFilterPreservesFramesAndTerminalUsage(t *testing.T) {
	input := strings.Join([]string{
		": upstream keepalive", "",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"hello"}`, "",
		"event: ping",
		`data: {"type":"ping","x-opencode-type":"inference-cost","cost":2.75}`, "",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":5}}}`, "",
	}, "\n")

	result := filterGrokPingTestInput(t, input)
	require.Contains(t, result, ": upstream keepalive\n\n")
	require.Contains(t, result, "response.output_text.delta")
	require.Contains(t, result, "response.completed")
	require.Contains(t, result, `"usage":{"input_tokens":3,"output_tokens":5}`)
	require.Contains(t, result, ": ping\n\n")
	require.NotContains(t, result, "event: ping")
	require.NotContains(t, result, "inference-cost")

	gin.SetMode(gin.TestMode)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body: newGrokResponsesBillingPingFilterBody(
			io.NopCloser(strings.NewReader(input)), &Account{Platform: PlatformGrok}, defaultMaxLineSize,
		),
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	svc := &OpenAIGatewayService{
		cfg:           &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		toolCorrector: NewCodexToolCorrector(),
	}
	streamResult, err := svc.handleStreamingResponse(
		context.Background(), resp, c, &Account{ID: 1, Platform: PlatformGrok}, time.Now(), "grok-4.5", "grok-4.5",
	)
	require.NoError(t, err)
	require.Equal(t, 3, streamResult.usage.InputTokens)
	require.Equal(t, 5, streamResult.usage.OutputTokens)
	require.Equal(t, "resp_1", streamResult.responseID)
}

func TestGrokResponsesBillingPingFilterPreservesNonPingAndOversizedCandidates(t *testing.T) {
	for _, input := range []string{
		"event: ping\nid: 7\ndata: {\"type\":\"ping\"}\n\n",
		"event: ping\ndata: {\"type\":\"response.completed\"}\n\n",
		"event: custom\ndata: {\"type\":\"ping\"}\n\n",
	} {
		require.Equal(t, input, filterGrokPingTestInput(t, input))
	}

	lines := []string{"event: ping"}
	for i := 0; i < grokResponsesPingFrameMaxLines; i++ {
		lines = append(lines, ": filler")
	}
	lines = append(lines, `data: {"type":"ping"}`, "")
	oversizedFrame := strings.Join(lines, "\n")
	require.Equal(t, oversizedFrame, filterGrokPingTestInput(t, oversizedFrame))

	body := newGrokResponsesBillingPingFilterBody(
		io.NopCloser(strings.NewReader("data: 123456789\n\n")),
		&Account{Platform: PlatformGrok},
		8,
	)
	_, err := io.ReadAll(body)
	require.ErrorContains(t, err, "filter Grok Responses billing ping")
	require.NoError(t, body.Close())
}

type grokPingFilterTestReadCloser struct {
	reader     io.ReadCloser
	closeCount atomic.Int32
}

func (r *grokPingFilterTestReadCloser) Read(p []byte) (int, error) { return r.reader.Read(p) }
func (r *grokPingFilterTestReadCloser) Close() error {
	r.closeCount.Add(1)
	return r.reader.Close()
}

func TestGrokResponsesBillingPingFilterCloseCancelsSourceOnce(t *testing.T) {
	upstreamReader, upstreamWriter := io.Pipe()
	source := &grokPingFilterTestReadCloser{reader: upstreamReader}
	body := newGrokResponsesBillingPingFilterBody(source, &Account{Platform: PlatformGrok}, defaultMaxLineSize)

	require.NoError(t, body.Close())
	require.Eventually(t, func() bool { return source.closeCount.Load() == 1 }, time.Second, time.Millisecond)
	_, err := upstreamWriter.Write([]byte("blocked"))
	require.Error(t, err)
	require.NoError(t, upstreamWriter.Close())
}

func TestGrokResponsesBillingPingFilterFlushesCompletedFrames(t *testing.T) {
	upstreamReader, upstreamWriter := io.Pipe()
	body := newGrokResponsesBillingPingFilterBody(upstreamReader, &Account{Platform: PlatformGrok}, defaultMaxLineSize)
	t.Cleanup(func() { require.NoError(t, body.Close()) })

	go func() {
		_, _ = io.WriteString(upstreamWriter, "event: future.event\ndata: {\"type\":\"future.event\"}\n\n")
	}()
	result := make(chan error, 1)
	go func() {
		buffer := make([]byte, 128)
		n, err := body.Read(buffer)
		if err == nil && !strings.Contains(string(buffer[:n]), "future.event") {
			err = errors.New("completed frame was not forwarded")
		}
		result <- err
	}()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("completed frame was buffered until upstream EOF")
	}
}
