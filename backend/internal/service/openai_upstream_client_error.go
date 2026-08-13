package service

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	openAIUpstreamClientErrorFallbackType    = "invalid_request_error"
	openAIUpstreamClientErrorFallbackMessage = "Upstream rejected the request"
)

// Only 400 is deterministic for the same request. Authentication, endpoint,
// billing and rate-limit failures remain gateway/account concerns.
func isOpenAIDeterministicClientError(statusCode int) bool {
	return statusCode == http.StatusBadRequest
}

// upstreamMsg has already passed the gateway's identity/error sanitizers.
func writeOpenAIUpstreamClientError(c *gin.Context, statusCode int, body []byte, upstreamMsg string) {
	payload := gin.H{"type": openAIUpstreamClientErrorFallbackType}
	if value := strings.TrimSpace(gjson.GetBytes(body, "error.type").String()); value != "" {
		payload["type"] = value
	}
	if value := strings.TrimSpace(extractUpstreamErrorCode(body)); value != "" {
		payload["code"] = value
	}
	if value := strings.TrimSpace(gjson.GetBytes(body, "error.param").String()); value != "" {
		payload["param"] = value
	}
	if upstreamMsg == "" {
		upstreamMsg = openAIUpstreamClientErrorFallbackMessage
	}
	payload["message"] = upstreamMsg
	c.JSON(statusCode, gin.H{"error": payload})
}
