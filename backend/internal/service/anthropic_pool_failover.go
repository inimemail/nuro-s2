package service

import (
	"context"
	"errors"
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
	if !isAnthropicPoolAccount(account) {
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
	if isAnthropicPoolDownstreamRoutingOrClientConfigError(statusCode, decision.SoftCooldownMessage, upstreamBody) {
		decision.SkipSoftCooldown = true
	}
	if isAnthropicPoolUserRequestError(statusCode, decision.SoftCooldownMessage, upstreamBody) {
		decision.SkipSoftCooldown = true
	}
	if decision.SkipSoftCooldown {
		decision.RetryableOnSame = false
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

func isAnthropicPoolAccount(account *Account) bool {
	return account != nil &&
		account.Platform == PlatformAnthropic &&
		(account.Type == AccountTypeAPIKey || account.Type == AccountTypeBedrock) &&
		account.IsPoolMode()
}

func isAnthropicPoolUserRequestError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusTooManyRequests || statusCode == 529 {
		return false
	}
	combined := openAIPoolCombinedErrorText(upstreamMsg, upstreamBody)
	if combined == "" {
		return false
	}

	if statusCode == http.StatusForbidden {
		if containsAnySubstring(combined,
			"content policy",
			"policy_violation",
			"safety",
			"moderation",
			"prompt was rejected",
			"input was rejected",
			"request was blocked",
			"blocked by",
			"violat",
		) {
			return true
		}
	}

	if isAnthropicPoolAccountLevelError(statusCode, combined) {
		return false
	}

	if statusCode == http.StatusBadRequest ||
		statusCode == http.StatusNotFound ||
		statusCode == http.StatusRequestEntityTooLarge ||
		statusCode == http.StatusUnprocessableEntity ||
		statusCode == http.StatusMethodNotAllowed ||
		statusCode == http.StatusUnsupportedMediaType ||
		statusCode == http.StatusBadGateway ||
		statusCode == http.StatusServiceUnavailable ||
		statusCode == http.StatusGatewayTimeout {
		if containsAnySubstring(combined,
			"api returned 400",
			"status 400",
			"http 400",
			"bad request",
			"context length",
			"context_length",
			"context window",
			"maximum context",
			"too many tokens",
			"token limit",
			"max_tokens",
			"invalid request",
			"invalid_request",
			"invalid_request_error",
			"missing required",
			"unsupported parameter",
			"unknown parameter",
			"invalid parameter",
			"invalid type",
			"invalid value",
			"malformed request",
			"request entity too large",
			"payload too large",
			"tool schema",
			"json schema",
			"messages:",
			"system:",
			"not supported",
			"unsupported",
			"method not allowed",
			"unsupported media type",
		) {
			return true
		}
	}

	if containsAnySubstring(combined,
		"model not found",
		"invalid model",
		"model_not_found",
		"unknown model",
		"unsupported model",
		"model is not available",
		"model not available",
		"model is not supported",
		"model not supported",
		"not found for model",
		"does not exist",
	) {
		return true
	}

	if strings.Contains(combined, "model") {
		if containsAnySubstring(combined,
			"unknown provider",
			"no provider",
			"provider not found",
			"model provider not found",
			"model route not found",
			"no route",
			"no upstream",
		) {
			return true
		}
	}

	return false
}

func isAnthropicPoolAccountLevelError(statusCode int, combined string) bool {
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusTooManyRequests || statusCode == 529 {
		return true
	}
	if containsAnySubstring(combined,
		"rate_limit_error",
		"rate limit",
		"too many requests",
		"overloaded_error",
		"overloaded",
		"temporarily overloaded",
		"authentication_error",
		"authentication",
		"unauthorized",
		"api key",
		"apikey",
		"x-api-key",
		"invalid key",
		"invalid_api_key",
		"expired key",
		"credit balance",
		"insufficient",
		"billing",
		"quota",
		"no credit",
		"balance",
		"organization",
		"workspace",
		"project",
		"ip address",
		"source ip",
		"client ip",
		"origin ip",
		"ip restricted",
		"ip restriction",
		"ip not allowed",
		"ip is not allowed",
		"allowed ip",
		"allowlisted ip",
		"whitelisted ip",
		"geo",
		"country",
		"region",
		"restricted",
	) {
		return true
	}
	return false
}

func isAnthropicPoolDownstreamRoutingOrClientConfigError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != http.StatusBadGateway && statusCode != http.StatusServiceUnavailable && statusCode != http.StatusGatewayTimeout {
		return false
	}
	combined := openAIPoolCombinedErrorText(upstreamMsg, upstreamBody)
	if combined == "" {
		return false
	}
	return containsAnySubstring(combined,
		"no available channel",
		"no available channels",
		"no available account",
		"no available accounts",
		"no available upstream",
		"no available provider",
		"no available route",
		"no channel available",
		"unknown provider for model",
		"unknown provider",
		"no provider for model",
		"provider not found for model",
		"model provider not found",
		"model route not found",
		"no route for model",
		"no upstream for model",
		"request body error",
		"bad request body",
		"invalid request body",
		"malformed request body",
		"request body invalid",
		"body parse error",
		"body parsing error",
		"请求体错误",
		"tun mode",
		"tun 模式",
		"node/tun",
		"节点/tun",
		"/v1 error",
		"/v1 错误",
	)
}

func isAnthropicPoolProbeModelError(statusCode int, msg string) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusNotFound {
		return false
	}
	return isAnthropicPoolUserRequestError(statusCode, msg, nil)
}

func newGatewayUpstreamFailoverError(account *Account, statusCode int, responseBody []byte, requestedModel string) *UpstreamFailoverError {
	failoverErr := &UpstreamFailoverError{
		StatusCode:             statusCode,
		ResponseBody:           responseBody,
		RetryableOnSameAccount: account != nil && account.IsPoolMode() && account.IsPoolModeRetryableStatus(statusCode),
	}
	if !isAnthropicPoolAccount(account) {
		return failoverErr
	}

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(responseBody))
	decision := classifyAnthropicPoolFailover(account, statusCode, upstreamMsg, responseBody, requestedModel)
	failoverErr.Message = upstreamMsg
	failoverErr.ProbeModel = strings.TrimSpace(decision.ProbeModel)
	failoverErr.ProbeKind = firstNonEmptyString(decision.ProbeKind, "messages")
	failoverErr.RetryableOnSameAccount = decision.RetryableOnSame
	failoverErr.SkipPoolSoftCooldown = decision.SkipSoftCooldown
	return failoverErr
}

func shouldAnthropicPoolRequestErrorSoftCooldown(err error, message string) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	combined := strings.ToLower(strings.TrimSpace(message))
	if combined == "" && err != nil {
		combined = strings.ToLower(strings.TrimSpace(err.Error()))
	}
	if combined == "" {
		return true
	}
	return !containsAnySubstring(combined,
		"context canceled",
		"context cancelled",
		"request canceled",
		"request cancelled",
		"operation was canceled",
		"operation was cancelled",
		"client disconnected",
		"client closed",
		"connection closed by client",
	)
}
