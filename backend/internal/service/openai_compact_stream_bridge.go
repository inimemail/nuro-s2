package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAICompactClientStreamKey = "openai_compact_client_stream"

// MarkOpenAICompactClientStream records that a body-signal compact request was
// originally sent by the client as stream:true before compact normalization
// removed the stream field for the upstream unary compact call.
func MarkOpenAICompactClientStream(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(openAICompactClientStreamKey, true)
}

func OpenAICompactClientStreamKeyForTest() string {
	return openAICompactClientStreamKey
}

func openAICompactClientWantsStream(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, ok := c.Get(openAICompactClientStreamKey)
	if !ok {
		return false
	}
	wants, _ := value.(bool)
	return wants
}

func writeOpenAICompactSSEBridge(c *gin.Context, statusCode int, finalResponse []byte) bool {
	if c == nil || !openAICompactClientWantsStream(c) {
		return false
	}
	committed := StopOpenAICompactSSEKeepaliveCommitted(c)
	if statusCode < 200 || statusCode >= 300 {
		if committed {
			writeOpenAICompactSSEFailure(c, statusCode, finalResponse)
			return true
		}
		return false
	}
	payload, ok := buildOpenAICompactSSEPayload(finalResponse)
	if !ok {
		if committed {
			writeOpenAICompactSSEFailure(c, http.StatusBadGateway, finalResponse)
			return true
		}
		return false
	}
	if !committed {
		header := c.Writer.Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("Connection", "keep-alive")
		header.Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(statusCode)
	}
	_, _ = c.Writer.Write(payload)
	c.Writer.Flush()
	return true
}

func writeOpenAICompactSSEFailure(c *gin.Context, statusCode int, errorBody []byte) {
	code, _, _ := openAIHTTPErrorMetadata(statusCode, errorBody)
	writeOpenAIResponsesFailureSSE(c, statusCode, "upstream_error", firstNonEmptyString(code, "upstream_error"), safeUpstreamErrorMessage, "", "")
}

func writeOpenAICompactSSEFailureMessage(c *gin.Context, statusCode int, errType, message string) {
	writeOpenAIResponsesFailureSSE(c, statusCode, errType, errType, message, "", "")
}

func writeOpenAIResponsesFailureSSE(c *gin.Context, statusCode int, errType, code, message, responseID, model string) {
	withOpenAIPlaceholderWriterLock(c, func(writer gin.ResponseWriter) {
		writeOpenAIResponsesFailureSSELocked(c, writer, statusCode, errType, code, message, responseID, model)
	})
}

// The request's writer lock covers format selection, framing and the terminal
// write so the placeholder timer cannot commit a different response in between.
func writeOpenAIResponsesFailureSSELocked(c *gin.Context, writer gin.ResponseWriter, statusCode int, errType, code, message, responseID, model string) {
	if OpenAIRequestResponsesTerminalWritten(c) || !claimGatewayErrorTerminal(c) {
		return
	}
	coordinator := openAIPlaceholderCoordinatorFromContext(c)
	frame := ""
	needsFailure := true
	if coordinator != nil && !coordinator.sseAtEventBoundary {
		frame = "\n\n"
		// Finishing a pending event may already deliver an upstream terminal.
		needsFailure = !pendingOpenAIResponsesEventIsTerminal(coordinator.ssePending)
	}
	if needsFailure {
		MarkOpsStreamError(c, statusCode, errType, message)
		frame += buildOpenAIResponsesFailureSSEFrame(code, message, responseID, model)
	}
	n, _ := writer.WriteString(frame)
	if coordinator != nil {
		coordinator.mu.Lock()
		if n > 0 {
			coordinator.markWriteLocked([]byte(frame[:min(n, len(frame))]))
		}
		// A short write still owns the terminal. Stop the placeholder timer
		// without recording a complete Responses terminal that did not arrive.
		coordinator.placeholderOutputStarted = true
		coordinator.pendingTimeoutFrame = nil
		coordinator.stopOnce.Do(func() { close(coordinator.stop) })
		coordinator.mu.Unlock()
	}
	MarkResponseCommitted(c)
	writer.Flush()
}

func pendingOpenAIResponsesEventIsTerminal(pending []byte) bool {
	var parser openAICompatSSEFrameParser
	for line := range strings.SplitSeq(strings.TrimSuffix(string(pending), "\n"), "\n") {
		parser.AddLine(strings.TrimSuffix(line, "\r"))
	}
	frame, ok := parser.Finish()
	if !ok || !gjson.Valid(frame.Data) {
		return false
	}
	payload := gjson.Parse(frame.Data)
	eventType := firstNonEmptyString(payload.Get("type").Str, frame.EventType)
	return payload.IsObject() && (openAIResponsesTerminalEventType(eventType) != "" || eventType == "error" || payload.Get("error").IsObject())
}

func buildOpenAIResponsesFailureSSEFrame(code, message, responseID, model string) string {
	if responseID == "" {
		responseID = "resp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	response := map[string]any{
		"id":     responseID,
		"object": "response",
		"status": "failed",
		"output": []any{},
		"error":  map[string]any{"code": code, "message": message},
	}
	if model != "" {
		response["model"] = model
		response["created_at"] = time.Now().Unix()
	}
	payload, _ := json.Marshal(map[string]any{
		"type":            "response.failed",
		"sequence_number": 0,
		"response":        response,
	})
	return "event: response.failed\ndata: " + string(payload) + "\n\n"
}

func buildOpenAICompactSSEPayload(finalResponse []byte) ([]byte, bool) {
	if len(finalResponse) == 0 || !gjson.ValidBytes(finalResponse) {
		return nil, false
	}
	if !gjson.ParseBytes(finalResponse).IsObject() {
		return nil, false
	}

	var compacted bytes.Buffer
	if err := json.Compact(&compacted, finalResponse); err != nil {
		return nil, false
	}
	response := compacted.Bytes()
	root := gjson.ParseBytes(response)
	if strings.TrimSpace(root.Get("id").String()) == "" {
		next, err := sjson.SetBytes(response, "id", "resp_"+strings.ReplaceAll(uuid.NewString(), "-", ""))
		if err != nil {
			return nil, false
		}
		response = next
	}
	if usage := gjson.GetBytes(response, "usage"); usage.Exists() && !openAICompactUsageParsableByCodex(usage) {
		next, err := sjson.DeleteBytes(response, "usage")
		if err != nil {
			return nil, false
		}
		response = next
	}

	var buf bytes.Buffer
	outputIndex := 0
	sequenceNumber := 0
	appendEvent := func(eventType string, data []byte) bool {
		numbered, err := sjson.SetBytes(data, "sequence_number", sequenceNumber)
		if err != nil {
			return false
		}
		sequenceNumber++
		_, _ = buf.WriteString("event: ")
		_, _ = buf.WriteString(eventType)
		_, _ = buf.WriteString("\ndata: ")
		_, _ = buf.Write(numbered)
		_, _ = buf.WriteString("\n\n")
		return true
	}
	for _, item := range gjson.GetBytes(response, "output").Array() {
		if !item.IsObject() {
			continue
		}
		event, err := sjson.SetBytes([]byte(`{"type":"response.output_item.done"}`), "output_index", outputIndex)
		if err != nil {
			return nil, false
		}
		event, err = sjson.SetRawBytes(event, "item", []byte(item.Raw))
		if err != nil {
			return nil, false
		}
		if !appendEvent("response.output_item.done", event) {
			return nil, false
		}
		outputIndex++
	}

	completed, err := sjson.SetRawBytes([]byte(`{"type":"response.completed"}`), "response", response)
	if err != nil {
		return nil, false
	}
	if !appendEvent("response.completed", completed) {
		return nil, false
	}
	return buf.Bytes(), true
}

func openAICompactUsageParsableByCodex(usage gjson.Result) bool {
	if !usage.IsObject() {
		return false
	}
	for _, field := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if usage.Get(field).Type != gjson.Number {
			return false
		}
	}
	return true
}
