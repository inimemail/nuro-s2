package service

import "net/http"

const anthropicMessagesForeignOAuthReason = GatewayFailureReason("anthropic_messages_foreign_oauth")

func anthropicMessagesCredentialAllowed(account *Account) bool {
	if account == nil {
		return false
	}
	return !account.IsOAuth() || account.Platform == PlatformAnthropic
}
func checkAnthropicMessagesCredential(account *Account) error {
	if anthropicMessagesCredentialAllowed(account) {
		return nil
	}
	return &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Reason: anthropicMessagesForeignOAuthReason, SkipPoolSoftCooldown: true, SkipSchedulePenalty: true, SkipStickySessionEviction: true}
}
func getAnthropicGatewayBaseURL(account *Account) string {
	if account == nil {
		return ""
	}
	if base := account.GetAnthropicProtocolBaseURL(); base != "" {
		return base
	}
	return account.GetBaseURL()
}
