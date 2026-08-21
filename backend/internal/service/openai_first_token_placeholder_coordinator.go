package service

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIPlaceholderCoordinatorKey = "openai_first_token_placeholder_coordinator"

// openAIPlaceholderCoordinator owns the downstream commitment state for one
// client request. It deliberately survives same-account retries and account
// switches, so a local timing placeholder is emitted at most once.
type openAIPlaceholderCoordinator struct {
	mu       sync.Mutex
	writeMu  sync.Mutex
	stopOnce sync.Once
	stop     chan struct{}

	startedAt  time.Time
	configured bool
	active     bool
	deadline   time.Time
	armed      bool

	placeholderWritten       bool
	safePlaceholderWritten   bool
	gatewayWriteObserved     bool
	upstreamCommitted        bool
	terminalWritten          bool
	responsesTerminalWritten bool
	stallFailoverClaimed     bool
	stallActionPending       bool
	stallProgressKey         openAIStreamProgressKey
	chatID                   string
	chatCreated              int64
}

type openAIPlaceholderWriter struct {
	gin.ResponseWriter
	coordinator *openAIPlaceholderCoordinator
}

func (w *openAIPlaceholderWriter) Write(data []byte) (int, error) {
	if w == nil || w.ResponseWriter == nil {
		return 0, http.ErrNotSupported
	}
	w.coordinator.writeMu.Lock()
	defer w.coordinator.writeMu.Unlock()
	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		w.coordinator.mu.Lock()
		w.coordinator.markWriteLocked(data[:min(n, len(data))])
		w.coordinator.mu.Unlock()
	}
	return n, err
}

func (w *openAIPlaceholderWriter) WriteString(data string) (int, error) {
	if w == nil || w.ResponseWriter == nil {
		return 0, http.ErrNotSupported
	}
	w.coordinator.writeMu.Lock()
	defer w.coordinator.writeMu.Unlock()
	n, err := w.ResponseWriter.WriteString(data)
	if n > 0 {
		w.coordinator.mu.Lock()
		w.coordinator.markWriteLocked([]byte(data[:min(n, len(data))]))
		w.coordinator.mu.Unlock()
	}
	return n, err
}

func (w *openAIPlaceholderWriter) WriteHeader(code int) {
	if w == nil || w.ResponseWriter == nil {
		return
	}
	w.coordinator.writeMu.Lock()
	defer w.coordinator.writeMu.Unlock()
	w.ResponseWriter.WriteHeader(code)
}

func (w *openAIPlaceholderWriter) WriteHeaderNow() {
	if w == nil || w.ResponseWriter == nil {
		return
	}
	w.coordinator.writeMu.Lock()
	defer w.coordinator.writeMu.Unlock()
	w.ResponseWriter.WriteHeaderNow()
}

func (w *openAIPlaceholderWriter) Flush() {
	if w == nil || w.ResponseWriter == nil {
		return
	}
	w.coordinator.writeMu.Lock()
	defer w.coordinator.writeMu.Unlock()
	w.ResponseWriter.Flush()
}

func (w *openAIPlaceholderWriter) Status() int {
	if w == nil || w.ResponseWriter == nil {
		return 0
	}
	w.coordinator.writeMu.Lock()
	defer w.coordinator.writeMu.Unlock()
	return w.ResponseWriter.Status()
}

func (w *openAIPlaceholderWriter) Size() int {
	if w == nil || w.ResponseWriter == nil {
		return -1
	}
	w.coordinator.writeMu.Lock()
	defer w.coordinator.writeMu.Unlock()
	return w.ResponseWriter.Size()
}

func (w *openAIPlaceholderWriter) Written() bool {
	if w == nil || w.ResponseWriter == nil {
		return false
	}
	w.coordinator.writeMu.Lock()
	defer w.coordinator.writeMu.Unlock()
	return w.ResponseWriter.Written()
}

func (w *openAIPlaceholderWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w == nil || w.ResponseWriter == nil {
		return nil, nil, http.ErrNotSupported
	}
	w.coordinator.writeMu.Lock()
	defer w.coordinator.writeMu.Unlock()
	return w.ResponseWriter.Hijack()
}

func (c *openAIPlaceholderCoordinator) markWriteLocked(data []byte) {
	if openAIWriteContainsOnlyGatewayFrames(data) {
		c.gatewayWriteObserved = true
		if openAIWriteContainsTokenPlaceholder(data) {
			c.active = true
		}
		return
	}
	c.upstreamCommitted = true
	if openAIWriteContainsTerminalFrame(data) {
		c.terminalWritten = true
	}
	if openAIWriteContainsResponsesTerminalFrame(data) {
		c.responsesTerminalWritten = true
	}
	c.stopOnce.Do(func() { close(c.stop) })
}

func openAIWriteContainsTerminalFrame(data []byte) bool {
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.Equal(line, []byte("data: [DONE]")) {
			return true
		}
	}
	return openAIWriteContainsResponsesTerminalFrame(data)
}

func openAIWriteContainsResponsesTerminalFrame(data []byte) bool {
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if !gjson.ValidBytes(payload) {
			continue
		}
		eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
		if openAIResponsesTerminalEventType(eventType) != "" || eventType == "error" || gjson.GetBytes(payload, "error").Exists() {
			return true
		}
	}
	return false
}

func openAIWriteContainsOnlyGatewayFrames(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return true
	}
	for _, block := range bytes.Split(trimmed, []byte("\n\n")) {
		block = bytes.TrimSpace(block)
		if len(block) == 0 {
			continue
		}
		if bytes.HasPrefix(block, []byte(":")) && !bytes.Contains(block, []byte("\ndata:")) {
			continue
		}
		lines := bytes.Split(block, []byte("\n"))
		hasPlaceholderData := false
		for _, line := range lines {
			line = bytes.TrimSpace(line)
			if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) {
				continue
			}
			if !bytes.HasPrefix(line, []byte("data:")) {
				return false
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if !gjson.ValidBytes(payload) ||
				gjson.GetBytes(payload, "type").String() != "response.transport_progress.delta" {
				return false
			}
			hasPlaceholderData = true
		}
		if !hasPlaceholderData {
			return false
		}
	}
	return true
}

func openAIWriteContainsTokenPlaceholder(data []byte) bool {
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if gjson.ValidBytes(payload) &&
			gjson.GetBytes(payload, "type").String() == "response.transport_progress.delta" {
			return true
		}
	}
	return false
}

func (c *openAIPlaceholderCoordinator) arm(
	requestDone <-chan struct{},
	timeout time.Duration,
	emit func(),
) {
	if c == nil || timeout <= 0 || emit == nil {
		return
	}
	c.configure(timeout)
	c.mu.Lock()
	if c.armed || c.placeholderWritten || c.upstreamCommitted || c.deadline.IsZero() {
		c.mu.Unlock()
		return
	}
	c.armed = true
	remaining := time.Until(c.deadline)
	c.mu.Unlock()
	go func() {
		if remaining > 0 {
			timer := time.NewTimer(remaining)
			defer timer.Stop()
			select {
			case <-requestDone:
				return
			case <-c.stop:
				return
			case <-timer.C:
			}
		}
		emit()
	}()
}

func (c *openAIPlaceholderCoordinator) configure(timeout time.Duration) {
	if c == nil || timeout <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.configured {
		return
	}
	c.configured = true
	c.active = true
	c.deadline = c.startedAt.Add(timeout)
}

func (c *openAIPlaceholderCoordinator) activate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.active = true
	c.mu.Unlock()
}

func (c *openAIPlaceholderCoordinator) isActive() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

func (c *openAIPlaceholderCoordinator) ensureChatIdentity(model string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.chatID != "" {
		return
	}
	state := apicompat.NewResponsesEventToChatState()
	state.Model = model
	c.chatID = state.ID
	c.chatCreated = state.Created
}

func (c *openAIPlaceholderCoordinator) snapshot() openAIRequestFirstTokenPlaceholderState {
	if c == nil {
		return openAIRequestFirstTokenPlaceholderState{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return openAIRequestFirstTokenPlaceholderState{
		Sent:              c.placeholderWritten,
		SafeSent:          c.safePlaceholderWritten,
		UpstreamCommitted: c.upstreamCommitted,
		ChatID:            c.chatID,
		ChatCreated:       c.chatCreated,
	}
}

func (c *openAIPlaceholderCoordinator) tryClaimStallFailover(key openAIStreamProgressKey) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.upstreamCommitted || c.stallFailoverClaimed {
		return false
	}
	c.stallFailoverClaimed = true
	c.stallActionPending = true
	c.stallProgressKey = key
	return true
}

func (c *openAIPlaceholderCoordinator) completeStallAction(success bool) (openAIStreamProgressKey, bool) {
	if c == nil {
		return openAIStreamProgressKey{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stallActionPending {
		return openAIStreamProgressKey{}, false
	}
	c.stallActionPending = false
	return c.stallProgressKey, true
}

func OpenAIRequestTerminalWritten(c *gin.Context) bool {
	coordinator := openAIPlaceholderCoordinatorFromContext(c)
	if coordinator == nil {
		return false
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return coordinator.terminalWritten
}

func OpenAIRequestResponsesTerminalWritten(c *gin.Context) bool {
	coordinator := openAIPlaceholderCoordinatorFromContext(c)
	if coordinator == nil {
		return false
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return coordinator.responsesTerminalWritten
}

func ensureOpenAIPlaceholderCoordinator(c *gin.Context, startedAt time.Time) *openAIPlaceholderCoordinator {
	if c == nil || c.Writer == nil {
		return nil
	}
	if value, ok := c.Get(openAIPlaceholderCoordinatorKey); ok {
		if coordinator, ok := value.(*openAIPlaceholderCoordinator); ok && coordinator != nil {
			return coordinator
		}
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	coordinator := &openAIPlaceholderCoordinator{startedAt: startedAt, stop: make(chan struct{})}
	c.Writer = &openAIPlaceholderWriter{ResponseWriter: c.Writer, coordinator: coordinator}
	c.Set(openAIPlaceholderCoordinatorKey, coordinator)
	return coordinator
}

// StartOpenAIPlaceholderCoordination starts request-level timing after local
// validation and billing checks, before the first account attempt.
func StartOpenAIPlaceholderCoordination(c *gin.Context, startedAt time.Time) {
	ensureOpenAIPlaceholderCoordinator(c, startedAt)
}

func armOpenAIFirstTokenPlaceholder(
	c *gin.Context,
	account *Account,
	requestedModel string,
	startedAt time.Time,
	dialect openAIRequestFirstTokenPlaceholderDialect,
	service *OpenAIGatewayService,
) {
	if c == nil || service == nil {
		return
	}
	timeout := service.openAIStreamFirstTokenTimeoutPlaceholder(account, requestedModel)
	if timeout <= 0 {
		return
	}
	coordinator := ensureOpenAIPlaceholderCoordinator(c, startedAt)
	if coordinator == nil {
		return
	}
	if dialect == openAIRequestFirstTokenPlaceholderDialectChatCompletions {
		coordinator.ensureChatIdentity(requestedModel)
	}
	var done <-chan struct{}
	if c.Request != nil {
		done = c.Request.Context().Done()
	}
	coordinator.arm(done, timeout, func() {
		writeOpenAIRequestFirstTokenTimeoutPlaceholder(c, coordinator.startedAt, requestedModel, dialect)
	})
}

func (s *OpenAIGatewayService) ArmOpenAIResponsesFirstTokenPlaceholder(c *gin.Context, account *Account, requestedModel string) {
	armOpenAIFirstTokenPlaceholder(c, account, requestedModel, time.Now(), openAIRequestFirstTokenPlaceholderDialectResponses, s)
}

func (s *OpenAIGatewayService) ArmOpenAIChatFirstTokenPlaceholder(c *gin.Context, account *Account, requestedModel string) {
	armOpenAIFirstTokenPlaceholder(c, account, requestedModel, time.Now(), openAIRequestFirstTokenPlaceholderDialectChatCompletions, s)
}

func openAIPlaceholderCoordinatorFromContext(c *gin.Context) *openAIPlaceholderCoordinator {
	if c == nil {
		return nil
	}
	value, ok := c.Get(openAIPlaceholderCoordinatorKey)
	if !ok {
		return nil
	}
	coordinator, _ := value.(*openAIPlaceholderCoordinator)
	return coordinator
}

// OpenAIRequestUpstreamCommitted reports whether non-gateway content has been
// made visible. A local placeholder alone intentionally returns false.
func OpenAIRequestUpstreamCommitted(c *gin.Context) bool {
	coordinator := openAIPlaceholderCoordinatorFromContext(c)
	if coordinator == nil {
		return c != nil && IsResponseCommitted(c)
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return coordinator.upstreamCommitted
}

func OpenAIRequestPlaceholderWritten(c *gin.Context) bool {
	coordinator := openAIPlaceholderCoordinatorFromContext(c)
	if coordinator == nil {
		return false
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return coordinator.placeholderWritten || coordinator.gatewayWriteObserved
}

// OpenAIRequestTokenPlaceholderWritten excludes comment-only preflushes. It is
// used to stop transport-local reconnect loops once the downstream panel has
// already observed a compatibility first-token frame.
func OpenAIRequestTokenPlaceholderWritten(c *gin.Context) bool {
	coordinator := openAIPlaceholderCoordinatorFromContext(c)
	if coordinator == nil {
		return false
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return coordinator.placeholderWritten || coordinator.safePlaceholderWritten
}

func openAIRequestPlaceholderCoordinationActive(c *gin.Context) bool {
	coordinator := openAIPlaceholderCoordinatorFromContext(c)
	return coordinator != nil && coordinator.isActive()
}

func OpenAIRequestAllowsFailover(c *gin.Context, writerSizeBefore int) bool {
	if coordinator := openAIPlaceholderCoordinatorFromContext(c); coordinator != nil && coordinator.isActive() {
		return !OpenAIRequestUpstreamCommitted(c)
	}
	return OpenAICompactKeepaliveAdjustedWrittenSize(c) == writerSizeBefore
}

func OpenAIRequestHasPlaceholderCoordinator(c *gin.Context) bool {
	return openAIPlaceholderCoordinatorFromContext(c) != nil
}

func openAIGatewayFrameWriteFailed(c *gin.Context, written bool) bool {
	return !written && !OpenAIRequestUpstreamCommitted(c)
}

func openAIRequestPlaceholderEffectiveStartTime(c *gin.Context, fallback time.Time, timeout time.Duration) time.Time {
	coordinator := openAIPlaceholderCoordinatorFromContext(c)
	if coordinator == nil {
		return fallback
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.configured && timeout > 0 && !coordinator.deadline.IsZero() {
		return coordinator.deadline.Add(-timeout)
	}
	return coordinator.startedAt
}

func withOpenAIPlaceholderWriterLock(c *gin.Context, fn func(gin.ResponseWriter)) {
	if c == nil || c.Writer == nil || fn == nil {
		return
	}
	coordinator := openAIPlaceholderCoordinatorFromContext(c)
	if coordinator == nil {
		fn(c.Writer)
		return
	}
	coordinator.writeMu.Lock()
	defer coordinator.writeMu.Unlock()
	writer := c.Writer
	if wrapped, ok := writer.(*openAIPlaceholderWriter); ok && wrapped.ResponseWriter != nil {
		writer = wrapped.ResponseWriter
	}
	fn(writer)
}

func writeOpenAIGatewayPlaceholder(c *gin.Context, frame string, chatID string, chatCreated int64) bool {
	if c == nil || c.Writer == nil || frame == "" {
		return false
	}
	coordinator := ensureOpenAIPlaceholderCoordinator(c, time.Now())
	if coordinator == nil {
		return false
	}
	coordinator.writeMu.Lock()
	defer coordinator.writeMu.Unlock()
	coordinator.mu.Lock()
	if coordinator.placeholderWritten || coordinator.upstreamCommitted {
		written := coordinator.placeholderWritten
		coordinator.mu.Unlock()
		return written
	}
	coordinator.mu.Unlock()
	writer := c.Writer
	if wrapped, ok := writer.(*openAIPlaceholderWriter); ok && wrapped.ResponseWriter != nil {
		writer = wrapped.ResponseWriter
	}
	if !writer.Written() {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		writer.Header().Set("X-Accel-Buffering", "no")
		writer.WriteHeader(http.StatusOK)
	}
	if _, err := writer.WriteString(frame); err != nil {
		return false
	}
	writer.Flush()
	coordinator.mu.Lock()
	coordinator.placeholderWritten = true
	coordinator.gatewayWriteObserved = true
	coordinator.active = true
	if chatID != "" {
		coordinator.chatID = chatID
		coordinator.chatCreated = chatCreated
	}
	coordinator.mu.Unlock()
	return true
}

// writeOpenAIGatewayOnlyFrame emits a gateway-owned frame without consuming
// the timeout-placeholder budget. It is used by the independent safe-frame and
// comment controls.
func writeOpenAIGatewayOnlyFrame(c *gin.Context, frame string) bool {
	if c == nil || c.Writer == nil || strings.TrimSpace(frame) == "" {
		return false
	}
	coordinator := ensureOpenAIPlaceholderCoordinator(c, time.Now())
	if coordinator == nil {
		return false
	}
	coordinator.writeMu.Lock()
	defer coordinator.writeMu.Unlock()
	coordinator.mu.Lock()
	if coordinator.upstreamCommitted {
		coordinator.mu.Unlock()
		return false
	}
	coordinator.mu.Unlock()
	writer := c.Writer
	if wrapped, ok := writer.(*openAIPlaceholderWriter); ok && wrapped.ResponseWriter != nil {
		writer = wrapped.ResponseWriter
	}
	if !writer.Written() {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		writer.Header().Set("X-Accel-Buffering", "no")
		writer.WriteHeader(http.StatusOK)
	}
	if _, err := writer.WriteString(frame); err != nil {
		return false
	}
	writer.Flush()
	coordinator.mu.Lock()
	coordinator.gatewayWriteObserved = true
	coordinator.mu.Unlock()
	return true
}

func writeOpenAISafePlaceholder(c *gin.Context, frame string, chatID string, chatCreated int64) bool {
	if c == nil || c.Writer == nil || strings.TrimSpace(frame) == "" {
		return false
	}
	coordinator := ensureOpenAIPlaceholderCoordinator(c, time.Now())
	if coordinator == nil {
		return false
	}
	coordinator.writeMu.Lock()
	defer coordinator.writeMu.Unlock()
	coordinator.mu.Lock()
	if coordinator.safePlaceholderWritten {
		coordinator.mu.Unlock()
		return true
	}
	if coordinator.upstreamCommitted {
		coordinator.mu.Unlock()
		return false
	}
	coordinator.mu.Unlock()
	writer := c.Writer
	if wrapped, ok := writer.(*openAIPlaceholderWriter); ok && wrapped.ResponseWriter != nil {
		writer = wrapped.ResponseWriter
	}
	if !writer.Written() {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		writer.Header().Set("X-Accel-Buffering", "no")
		writer.WriteHeader(http.StatusOK)
	}
	if _, err := writer.WriteString(frame); err != nil {
		return false
	}
	writer.Flush()
	coordinator.mu.Lock()
	coordinator.safePlaceholderWritten = true
	coordinator.gatewayWriteObserved = true
	coordinator.active = true
	if chatID != "" && coordinator.chatID == "" {
		coordinator.chatID = chatID
		coordinator.chatCreated = chatCreated
	}
	coordinator.mu.Unlock()
	return true
}
