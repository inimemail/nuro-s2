package service

import (
	"net/http"
	"strings"
)

type anthropicPoolFailoverDecision struct {
	Failover            bool
	RetryableOnSame     bool
	SkipSoftCooldown    bool
	ProbeModel          string
	ProbeKind           string
	SoftCooldownMessage string
}

type anthropicPoolSoftCooldownContext struct {
	ProbeModel      string
	ProbeKind       string
	CooldownSource  string
	StatusCode      int
	Reason          string
	LastProbeStatus int
	LastProbeReason string
}

func classifyAnthropicPoolFailover(account *Account, statusCode int, upstreamMsg string, upstreamBody []byte, requestedModel string) anthropicPoolFailoverDecision {
	if !isAnthropicAPIKeyPoolAccount(account) {
		return anthropicPoolFailoverDecision{}
	}
	decision := anthropicPoolFailoverDecision{
		Failover:            shouldAnthropicPoolFailoverStatus(statusCode),
		RetryableOnSame:     account.IsPoolModeRetryableStatus(statusCode),
		ProbeModel:          strings.TrimSpace(requestedModel),
		ProbeKind:           "messages",
		SoftCooldownMessage: strings.TrimSpace(upstreamMsg),
	}
	if decision.SoftCooldownMessage == "" {
		decision.SoftCooldownMessage = strings.TrimSpace(extractUpstreamErrorMessage(upstreamBody))
	}
	if isAnthropicPoolUserRequestError(statusCode, decision.SoftCooldownMessage, upstreamBody) {
		decision.SkipSoftCooldown = true
	}
	return decision
}

func shouldAnthropicPoolFailoverStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, 529:
		return true
	default:
		return statusCode >= 500
	}
}

func isAnthropicAPIKeyPoolAccount(account *Account) bool {
	return account != nil &&
		account.Platform == PlatformAnthropic &&
		account.Type == AccountTypeAPIKey &&
		account.IsPoolMode()
}

func isAnthropicPoolUserRequestError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	combined := strings.ToLower(strings.TrimSpace(upstreamMsg + " " + string(upstreamBody)))
	if combined == "" {
		return false
	}
	if statusCode == http.StatusBadRequest {
		for _, marker := range []string{
			"context length",
			"context_length",
			"max_tokens",
			"invalid request",
			"invalid_request_error",
			"tool schema",
			"json schema",
			"messages:",
			"system:",
		} {
			if strings.Contains(combined, marker) {
				return true
			}
		}
	}
	for _, marker := range []string{
		"model not found",
		"invalid model",
		"model_not_found",
		"not found for model",
		"does not exist",
	} {
		if strings.Contains(combined, marker) {
			return true
		}
	}
	return false
}

func isAnthropicPoolProbeModelError(statusCode int, msg string) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusNotFound {
		return false
	}
	return isAnthropicPoolUserRequestError(statusCode, msg, nil)
}
