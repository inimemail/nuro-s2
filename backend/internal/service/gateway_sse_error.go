package service

import (
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// WriteGatewayCommittedError keeps errors in the protocol already committed by
// queue pings. Cancellation never emits a synthetic terminal.
func WriteGatewayCommittedError(c *gin.Context, dialect, code, message string) bool {
	if c == nil || c.Writer == nil || !c.Writer.Written() {
		return false
	}
	queueSSE, _ := c.Get("gateway_queue_sse_committed")
	if queueSSE != true && !strings.HasPrefix(strings.ToLower(c.Writer.Header().Get("Content-Type")), "text/event-stream") {
		return false
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		return true
	}
	if sent, _ := c.Get("gateway_error_terminal_sent"); sent == true {
		return true
	}
	c.Set("gateway_error_terminal_sent", true)
	errorBody := gin.H{"type": code, "message": message}
	switch dialect {
	case "responses":
		payload, _ := json.Marshal(gin.H{"type": "response.failed", "sequence_number": 0, "response": gin.H{"id": "resp_" + uuid.NewString(), "object": "response", "status": "failed", "output": []any{}, "error": gin.H{"code": code, "type": code, "message": message}}})
		_, _ = c.Writer.Write([]byte("event: response.failed\ndata: " + string(payload) + "\n\n"))
	case "chat":
		payload, _ := json.Marshal(gin.H{"error": errorBody})
		_, _ = c.Writer.Write([]byte("data: " + string(payload) + "\n\ndata: [DONE]\n\n"))
	default:
		_, _ = c.Writer.Write([]byte(buildAnthropicStreamErrorSSE(code, message)))
	}
	c.Writer.Flush()
	return true
}

func writeAnthropicErrorJSON(c *gin.Context, status int, body gin.H) {
	if nested, ok := body["error"].(gin.H); ok {
		code, _ := nested["type"].(string)
		message, _ := nested["message"].(string)
		if WriteGatewayCommittedError(c, "anthropic", code, message) {
			return
		}
	}
	c.JSON(status, body)
}

// Used only by JSON error exits outside the streaming relay/coordinator. Preserve
// the existing uncommitted body, and respect the protocol of a sent queue ping.
func writeOpenAIErrorJSON(c *gin.Context, status int, body gin.H) {
	if nested, ok := body["error"].(gin.H); ok {
		code, _ := nested["type"].(string)
		message, _ := nested["message"].(string)
		dialect := "responses"
		if c.Request != nil {
			path := c.Request.URL.Path
			if strings.HasSuffix(path, "/chat/completions") {
				dialect = "chat"
			} else if strings.HasSuffix(path, "/messages") {
				dialect = "anthropic"
			}
		}
		if WriteGatewayCommittedError(c, dialect, code, message) {
			return
		}
	}
	c.JSON(status, body)
}
