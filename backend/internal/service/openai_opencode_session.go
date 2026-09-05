package service

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

const openCodeSessionHeader = "X-OpenCode-Session"

// Forward the caller session only to the official OpenCode origin. This keeps
// account-wide header overrides from leaking a conversation identifier to
// unrelated OpenAI-compatible providers.
func applyOpenCodeSessionHeader(c *gin.Context, account *Account, targetURL string, headers http.Header) {
	if c == nil || c.Request == nil || account == nil || account.Type != AccountTypeAPIKey || headers == nil {
		return
	}
	u, err := url.Parse(targetURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "opencode.ai") {
		return
	}
	value := strings.TrimSpace(c.GetHeader(openCodeSessionHeader))
	if value == "" {
		return
	}
	for key := range headers {
		if strings.EqualFold(key, openCodeSessionHeader) {
			delete(headers, key)
		}
	}
	headers.Set(openCodeSessionHeader, value)
}
