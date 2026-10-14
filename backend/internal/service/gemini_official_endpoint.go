package service

import (
	"net/url"
	"strings"
)

// A custom proxy is never assigned AI Studio's simulated quotas just because
// its path or host contains Google's name.
func isOfficialGeminiAPIBaseURL(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return false
	}
	if !strings.EqualFold(u.Hostname(), "generativelanguage.googleapis.com") || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	return u.RawPath == "" && (u.Path == "" || u.Path == "/")
}

func (a *Account) IsThirdPartyGeminiAPIKey() bool {
	return a != nil && a.Platform == PlatformGemini && a.Type == AccountTypeAPIKey && !isOfficialGeminiAPIBaseURL(a.GetCredential("base_url"))
}
