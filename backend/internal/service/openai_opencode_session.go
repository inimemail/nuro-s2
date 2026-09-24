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

func isOfficialOpenCodeOrCommandCodeTarget(targetURL string) bool {
	u, err := url.Parse(targetURL)
	return err == nil && u.Scheme == "https" && u.User == nil && (u.Port() == "" || u.Port() == "443") && (strings.EqualFold(u.Hostname(), "opencode.ai") || strings.EqualFold(u.Hostname(), "api.commandcode.ai"))
}

func applyOpenCodeUpstreamUserAgent(account *Account, targetURL string, headers http.Header) {
	if headers == nil || !isOfficialOpenCodeOrCommandCodeTarget(targetURL) {
		return
	}
	if account != nil {
		for key := range account.GetHeaderOverrides() {
			if strings.EqualFold(key, "User-Agent") {
				return
			}
		}
	}
	u, _ := url.Parse(targetURL)
	ua := "opencode/1.0.0"
	if strings.EqualFold(u.Hostname(), "api.commandcode.ai") {
		ua = safeCanonicalCodexUserAgent(currentCodexIdentityRuntime())
	}
	for key := range headers {
		if strings.EqualFold(key, "User-Agent") {
			delete(headers, key)
		}
	}
	headers.Set("User-Agent", ua)
}

func isOfficialProviderCloudflare1010(account *Account, body []byte) bool {
	if account == nil || account.Type != AccountTypeAPIKey || !isOfficialOpenCodeOrCommandCodeTarget(account.GetBaseURL()) {
		return false
	}
	text := strings.ToLower(string(body))
	return strings.Contains(text, "error code: 1010") && (strings.Contains(text, "cloudflare") || strings.Contains(text, "access denied"))
}
