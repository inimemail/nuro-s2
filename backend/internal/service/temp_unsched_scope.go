package service

import (
	"net/http"
	"strings"
)

func modelTempUnschedulableAuthAllowed(account *Account, status int, body []byte) bool {
	if status != http.StatusUnauthorized {
		return true
	}
	return account != nil && account.Type == AccountTypeAPIKey && account.IsOpenAICompatible() && isOpenAICompatibleModelNotFoundBody(body) && extractUpstreamErrorCode(body) != "token_revoked" && extractUpstreamErrorCode(body) != "token_invalidated"
}

// Empty scope retains the legacy account contract. The first matching rule
// wins in both persistence and the runtime scheduling bridge.
func matchedTempUnschedulableScope(account *Account, status int, body []byte) string {
	if account == nil || !account.IsTempUnschedulableEnabled() || account.IsCustomErrorCodesEnabled() || account.IsPoolMode() {
		return ""
	}
	if len(body) > tempUnschedBodyMaxBytes {
		body = body[:tempUnschedBodyMaxBytes]
	}
	lower := strings.ToLower(string(body))
	for _, rule := range account.GetTempUnschedulableRules() {
		if rule.ErrorCode == status && matchTempUnschedKeyword(lower, rule.Keywords) != "" {
			if rule.Scope == "model" && modelTempUnschedulableAuthAllowed(account, status, body) {
				return "model"
			}
			if rule.Scope == "model" {
				return ""
			}
			return "account"
		}
	}
	return ""
}
