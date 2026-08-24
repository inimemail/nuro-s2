package service

import "strings"

const (
	cnBillingModeExtraKey = "cn_billing_mode"
	cnAPIProtocolExtraKey = "cn_api_mode"
	cnAPIBaseURLsExtraKey = "cn_api_base_urls"
)

func (a *Account) IsKimi() bool {
	return a != nil && a.Platform == PlatformKimi
}

func (a *Account) IsZhipu() bool {
	return a != nil && a.Platform == PlatformZhipu
}

func (a *Account) IsDeepSeek() bool {
	return a != nil && a.Platform == PlatformDeepSeek
}

func (a *Account) IsCNProvider() bool {
	return a != nil && IsCNProvider(a.Platform)
}

// GetCNBillingMode reads the local Extra representation first. Credentials
// values are accepted only for compatibility with imported upstream accounts.
func (a *Account) GetCNBillingMode() string {
	if a == nil || !a.IsCNProvider() {
		return ""
	}
	if mode := strings.TrimSpace(a.getExtraString(cnBillingModeExtraKey)); mode != "" {
		switch mode {
		case CNBillingModePayG, CNBillingModeCodingPlan:
			return mode
		}
	}
	switch strings.TrimSpace(a.GetCredential("account_mode")) {
	case AccountModeCoding, CNBillingModeCodingPlan:
		return CNBillingModeCodingPlan
	default:
		return CNBillingModePayG
	}
}

// GetAccountMode exposes the upstream-compatible spelling to quota services.
func (a *Account) GetAccountMode() string {
	switch a.GetCNBillingMode() {
	case CNBillingModeCodingPlan:
		return AccountModeCoding
	case CNBillingModePayG:
		return AccountModePayG
	default:
		return ""
	}
}

func (a *Account) IsCodingPlan() bool {
	return a.GetCNBillingMode() == CNBillingModeCodingPlan
}

func (a *Account) GetCodingPlanProvider() string {
	if a == nil || !a.IsCodingPlan() {
		return ""
	}
	switch a.Platform {
	case PlatformKimi, PlatformZhipu:
		return a.Platform
	default:
		return ""
	}
}

// GetAPIProtocol defaults to the exact legacy Chat Completions path. Native
// Responses is deliberately restricted to DeepSeek.
func (a *Account) GetAPIProtocol() string {
	if a == nil || !a.IsCNProvider() {
		return APIProtocolChatCompletions
	}
	protocol := strings.TrimSpace(a.getExtraString(cnAPIProtocolExtraKey))
	if protocol == "" {
		protocol = strings.TrimSpace(a.GetCredential("api_protocol"))
	}
	switch protocol {
	case APIProtocolAdaptive, APIProtocolAnthropic, APIProtocolChatCompletions:
		return protocol
	case APIProtocolResponses:
		if a.IsDeepSeek() {
			return protocol
		}
	}
	return APIProtocolChatCompletions
}

func (a *Account) IsAdaptiveAPIProtocol() bool {
	return a.GetAPIProtocol() == APIProtocolAdaptive
}

func (a *Account) IsAnthropicProtocol() bool {
	return a.GetAPIProtocol() == APIProtocolAnthropic
}

func (a *Account) GetCNAPIKey() string {
	if a == nil || !a.IsCNProvider() || a.Type != AccountTypeAPIKey {
		return ""
	}
	return a.GetCredential("api_key")
}

// GetOpenAIProtocolAPIKey returns the credential used by OpenAI-format
// transports without broadening IsOpenAIApiKey scheduler semantics.
func (a *Account) GetOpenAIProtocolAPIKey() string {
	if a == nil {
		return ""
	}
	if a.IsCNProvider() {
		return a.GetCNAPIKey()
	}
	return a.GetOpenAIApiKey()
}

func (a *Account) getCNConfiguredProtocolBaseURL(protocol string) string {
	if a == nil {
		return ""
	}
	for _, raw := range []any{a.Extra[cnAPIBaseURLsExtraKey], a.Credentials["api_base_urls"]} {
		switch values := raw.(type) {
		case map[string]any:
			if value, ok := values[protocol].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		case map[string]string:
			if value := strings.TrimSpace(values[protocol]); value != "" {
				return value
			}
		}
	}
	return ""
}

func (a *Account) defaultCNProtocolBaseURL(protocol string) string {
	switch protocol {
	case APIProtocolAnthropic:
		switch a.Platform {
		case PlatformKimi:
			if a.IsCodingPlan() {
				return DefaultKimiCodingAnthropicBaseURL
			}
			return DefaultKimiPayGAnthropicBaseURL
		case PlatformZhipu:
			return DefaultZhipuAnthropicBaseURL
		case PlatformDeepSeek:
			return DefaultDeepSeekAnthropicBaseURL
		}
	case APIProtocolResponses:
		if a.IsDeepSeek() {
			return DefaultDeepSeekResponsesBaseURL
		}
	case APIProtocolChatCompletions:
		switch a.Platform {
		case PlatformKimi:
			if a.IsCodingPlan() {
				return DefaultKimiCodingBaseURL
			}
			return DefaultKimiPayGBaseURL
		case PlatformZhipu:
			if a.IsCodingPlan() {
				return DefaultZhipuCodingBaseURL
			}
			return DefaultZhipuPayGBaseURL
		case PlatformDeepSeek:
			return DefaultDeepSeekChatBaseURL
		}
	}
	return ""
}

func (a *Account) GetCNProtocolBaseURL(protocol string) string {
	if a == nil || !a.IsCNProvider() {
		return ""
	}
	if a.IsAdaptiveAPIProtocol() {
		if configured := a.getCNConfiguredProtocolBaseURL(protocol); configured != "" {
			return configured
		}
	}
	if protocol == a.GetAPIProtocol() || (!a.IsAdaptiveAPIProtocol() && protocol == APIProtocolChatCompletions) {
		if configured := strings.TrimSpace(a.GetCredential("base_url")); configured != "" {
			return configured
		}
	}
	return a.defaultCNProtocolBaseURL(protocol)
}

func (a *Account) GetAnthropicProtocolBaseURL() string {
	if a == nil || (!a.IsAnthropicProtocol() && !a.IsAdaptiveAPIProtocol()) {
		return ""
	}
	return a.GetCNProtocolBaseURL(APIProtocolAnthropic)
}

func (a *Account) GetOpenAIFormatBaseURL() string {
	if a == nil || !a.IsCNProvider() {
		return a.GetOpenAIBaseURL()
	}
	return a.GetCNProtocolBaseURL(APIProtocolChatCompletions)
}
