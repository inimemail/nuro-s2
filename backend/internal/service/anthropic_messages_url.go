package service

import (
	"net/url"
	"strings"
)

func anthropicMessagesURL(validatedBase string) string {
	target := strings.TrimRight(validatedBase, "/") + "/v1/messages"
	if isCloudflareRESTMessagesURL(target) {
		return target
	}
	return target + "?beta=true"
}

// Called only after the normal account URL/proxy validation. Encoded paths,
// credentials and queries must never broaden this one endpoint exception.
func isCloudflareRESTMessagesURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "api.cloudflare.com") || (u.Port() != "" && u.Port() != "443") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return false
	}
	parts := strings.Split(u.Path, "/")
	return len(parts) == 8 && parts[0] == "" && parts[1] == "client" && parts[2] == "v4" && parts[3] == "accounts" && parts[4] != "" && parts[5] == "ai" && parts[6] == "v1" && parts[7] == "messages"
}
