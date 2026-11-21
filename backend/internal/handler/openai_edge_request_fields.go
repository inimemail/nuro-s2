package handler

import (
	"bytes"
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestfields"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ValidateOpenAIEdgePrepareFields checks the client body before prepare or
// claim can select an account or acquire a lease.
func (h *OpenAIGatewayHandler) ValidateOpenAIEdgePrepareFields(c *gin.Context) {
	if !h.requireOpenAIEdgeSecret(c) {
		c.Abort()
		return
	}

	req := c.Request
	// WS prepare can carry both the parsed JSON and the original, base64 body.
	maxBody := h.cfg.Gateway.MaxBodySize
	var envelopeLimit int64
	if maxBody > 0 && maxBody <= (math.MaxInt64-(1<<20))/3 {
		envelopeLimit = 3*maxBody + (1 << 20)
	}
	original := req.Body
	body, err := httputil.ReadBoundedRequestBody(req, envelopeLimit)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "prepare request is too large"})
		return
	}
	if err != nil || !gjson.ValidBytes(body) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid prepare request"})
		return
	}
	if original != nil {
		_ = original.Close()
	}
	if requestfields.HasConflict(body, "") {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid prepare request"})
		return
	}

	var clientBody []byte
	if encoded := strings.TrimSpace(gjson.GetBytes(body, "body_raw_base64").String()); encoded != "" {
		clientBody, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid prepare raw body"})
			return
		}
	} else {
		clientBody = []byte(gjson.GetBytes(body, "body").Raw)
	}
	if maxBody > 0 && int64(len(clientBody)) > maxBody {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body is too large"})
		return
	}
	if requestfields.HasConflict(bytes.TrimSpace(clientBody), "") {
		middleware.MarkIngressRejected(c, "conflicting_request_fields")
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Request contains duplicate or ambiguous fields"})
		return
	}
	httputil.RestorePreReadRequestBody(req, body)
	c.Next()
}
