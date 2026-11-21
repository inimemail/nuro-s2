package middleware

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestfields"
	"github.com/gin-gonic/gin"
)

// RejectConflictingGatewayFields checks authenticated input before gateway
// routing and forwards the already bounded body to the selected handler.
func RejectConflictingGatewayFields(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		req := c.Request
		switch req.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
		default:
			c.Next()
			return
		}
		if req.Body == nil || req.Body == http.NoBody {
			c.Next()
			return
		}
		originalBody := req.Body
		body, err := httputil.ReadBoundedRequestBody(req, maxBytes)
		if err != nil {
			writeGatewayFieldReadError(c, err)
			return
		}
		_ = originalBody.Close()
		if requestfields.HasConflict(body, req.Header.Get("Content-Type")) {
			MarkIngressRejected(c, "conflicting_request_fields")
			writeGatewayFieldError(c, http.StatusBadRequest, "Request contains duplicate or ambiguous fields")
			return
		}
		trimmed := bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf})
		doc := bytes.TrimSpace(trimmed)
		if len(doc) > 0 && (doc[0] == '{' || doc[0] == '[') {
			body = trimmed
		}
		httputil.RestorePreReadRequestBody(req, body)
		c.Next()
	}
}

func writeGatewayFieldReadError(c *gin.Context, err error) {
	var tooLarge *http.MaxBytesError
	var timeout net.Error
	switch {
	case errors.As(err, &tooLarge):
		writeGatewayFieldError(c, http.StatusRequestEntityTooLarge, "Request body is too large")
	case errors.Is(err, httputil.ErrBodyBudgetExceeded):
		writeGatewayFieldError(c, http.StatusServiceUnavailable, "Request body capacity is temporarily exhausted")
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		writeGatewayFieldError(c, http.StatusRequestTimeout, "Request body read timed out")
	case errors.Is(err, context.Canceled), c.Request.Context().Err() != nil:
		writeGatewayFieldError(c, 499, "Request cancelled")
	default:
		writeGatewayFieldError(c, http.StatusBadRequest, "Failed to read request body")
	}
}

func writeGatewayFieldError(c *gin.Context, status int, message string) {
	path := c.Request.URL.Path
	if strings.HasPrefix(path, "/v1beta/") || strings.HasPrefix(path, "/antigravity/v1beta/") {
		GoogleErrorWriter(c, status, message)
		c.Abort()
		return
	}
	payload := gin.H{"error": gin.H{"type": "invalid_request_error", "message": message}}
	if strings.Contains(path, "/messages") {
		payload["type"] = "error"
	}
	c.AbortWithStatusJSON(status, payload)
}
