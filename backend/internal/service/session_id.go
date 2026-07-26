package service

import (
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const maxPersistedSessionIDLength = 255

var clientSessionIDHeaders = []string{
	"session_id",
	"conversation_id",
	"X-Session-Affinity",
	"X-Session-Id",
	"X-OpenCode-Session",
	"X-Conversation-ID",
	claudeCodeSessionHeader,
}

// ExtractClientSessionID returns an explicit client correlation ID for audit
// persistence only. It never consumes prompt_cache_key or derived sticky state.
func ExtractClientSessionID(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	for _, header := range clientSessionIDHeaders {
		if value := sanitizeSessionID(c.GetHeader(header)); value != "" {
			return value
		}
	}
	if isGrokRequestContext(c) {
		return sanitizeSessionID(c.GetHeader(grokConversationIDHeader))
	}
	return ""
}

func sanitizeSessionID(raw string) string {
	if !utf8.ValidString(raw) {
		return ""
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	count := 0
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
		count++
		if count > maxPersistedSessionIDLength {
			return ""
		}
	}
	return value
}
