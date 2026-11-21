package websearch

import "net/http"

const (
	maxResponseSize   = 1 << 20 // 1 MB
	errorBodyTruncLen = 200
)

// Search requests may carry a provider credential in a header or a body.
func searchProviderClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	configured := *client
	configured.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &configured
}

// truncateBody returns a truncated string of body for error messages.
func truncateBody(body []byte) string {
	if len(body) <= errorBodyTruncLen {
		return string(body)
	}
	return string(body[:errorBodyTruncLen]) + "...(truncated)"
}
