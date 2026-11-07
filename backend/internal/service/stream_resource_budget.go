package service

import (
	"bytes"
	"io"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ssebudget"
	"github.com/gin-gonic/gin"
)

type streamResourceGuard struct {
	reader   interface{ Exceeded() bool }
	writer   gin.ResponseWriter
	stream   bool
	google   bool
	finished bool
}
type streamBudgetWriter struct {
	gin.ResponseWriter
	reader  interface{ Exceeded() bool }
	stream  bool
	pending []byte
}

func (w *streamBudgetWriter) Write(p []byte) (int, error) {
	if w.reader.Exceeded() {
		w.pending = nil
		return len(p), nil
	}
	if !w.stream {
		return w.ResponseWriter.Write(p)
	}
	// A producer can detect an overflow while an older frame is being
	// transformed. Publish complete frames atomically, never an event prefix.
	w.pending = append(w.pending, p...)
	for {
		end := bytes.Index(w.pending, []byte("\n\n"))
		separator := 2
		if crlf := bytes.Index(w.pending, []byte("\r\n\r\n")); crlf >= 0 && (end < 0 || crlf < end) {
			end = crlf
			separator = 4
		}
		if end < 0 {
			break
		}
		if !w.reader.Exceeded() {
			if _, err := w.ResponseWriter.Write(w.pending[:end+separator]); err != nil {
				return 0, err
			}
		}
		w.pending = w.pending[end+separator:]
	}
	return len(p), nil
}
func (w *streamBudgetWriter) WriteString(p string) (int, error) { return w.Write([]byte(p)) }
func (w *streamBudgetWriter) WriteHeader(status int) {
	if !w.reader.Exceeded() {
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *streamBudgetWriter) WriteHeaderNow() {
	if !w.reader.Exceeded() {
		w.ResponseWriter.WriteHeaderNow()
	}
}
func (w *streamBudgetWriter) Flush() {
	if !w.reader.Exceeded() {
		w.ResponseWriter.Flush()
	}
}

type streamBudgetBody struct {
	io.Reader
	io.Closer
}

func newStreamResourceGuard(resp *http.Response, c *gin.Context, cfg *config.Config, stream, google bool) *streamResourceGuard {
	if cfg == nil || resp == nil || resp.Body == nil {
		return nil
	}
	limits := ssebudget.Limits{LineBytes: cfg.Gateway.SSEMaxLineBytes, EventBytes: cfg.Gateway.SSEMaxEventBytes, EventLines: cfg.Gateway.SSEMaxEventLines}
	if !limits.Enabled() {
		return nil
	}
	reader := ssebudget.NewReader(resp.Body, limits)
	resp.Body = &streamBudgetBody{Reader: reader, Closer: resp.Body}
	g := &streamResourceGuard{reader: reader, stream: stream, google: google}
	if c != nil {
		g.writer = c.Writer
		c.Writer = &streamBudgetWriter{ResponseWriter: c.Writer, reader: reader, stream: stream}
	}
	return g
}
func (g *streamResourceGuard) Err() error {
	if g != nil && g.reader.Exceeded() {
		return ssebudget.ErrExceeded
	}
	return nil
}

// Finish writes a single sanitized failure through the original writer. The
// display gate remains installed so outer handlers cannot append a second one.
// It never closes/cancels the upstream, nor changes the usage already observed.
func (g *streamResourceGuard) Finish() {
	if g == nil || g.Err() == nil || g.writer == nil || g.finished {
		return
	}
	g.finished = true
	body := `{"type":"error","error":{"type":"upstream_error","message":"Upstream response exceeded configured resource limits"}}`
	if g.google {
		body = `{"error":{"code":502,"status":"RESOURCE_EXHAUSTED","message":"Upstream response exceeded configured resource limits"}}`
	}
	if g.stream {
		g.writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = g.writer.WriteString("event: error\ndata: " + body + "\n\n")
		g.writer.Flush()
	} else {
		g.writer.Header().Set("Content-Type", "application/json")
		if !g.writer.Written() {
			g.writer.WriteHeader(http.StatusBadGateway)
		}
		_, _ = g.writer.WriteString(body)
	}
}
func streamResourceQueue(cfg *config.Config) *ssebudget.Queue {
	if cfg == nil {
		return nil
	}
	return ssebudget.NewQueue(cfg.Gateway.SSEQueueMaxBytes)
}
func (s *AntigravityGatewayService) streamResourceConfig() *config.Config {
	if s == nil || s.settingService == nil {
		return nil
	}
	return s.settingService.cfg
}

func newBedrockResourceGuard(decoder *bedrockEventStreamDecoder, c *gin.Context, cfg *config.Config) *streamResourceGuard {
	if cfg == nil || cfg.Gateway.SSEMaxEventBytes <= 0 {
		return nil
	}
	decoder.resourceLimit = cfg.Gateway.SSEMaxEventBytes
	g := &streamResourceGuard{reader: decoder, writer: c.Writer, stream: true}
	c.Writer = &streamBudgetWriter{ResponseWriter: c.Writer, reader: decoder, stream: true}
	return g
}

func (s *GeminiMessagesCompatService) collectGeminiSSEWithResourceLimits(c *gin.Context, resp *http.Response, isOAuth, google bool, observers ...func([]byte)) (result map[string]any, usage *ClaudeUsage, neutral bool, err error) {
	guard := newStreamResourceGuard(resp, c, s.cfg, false, google)
	defer func() {
		if guard.Err() != nil {
			err = guard.Err()
		}
		guard.Finish()
	}()
	return collectGeminiSSE(resp.Body, isOAuth, observers...)
}
