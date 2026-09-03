package service

import (
	"maps"
	"net/http"
	"strings"
)

// ApplyCNProviderHeaders adds provider-specific headers required by team plans.
func (a *Account) ApplyCNProviderHeaders(h http.Header) {
	if a == nil || h == nil || a.Platform != PlatformZhipu || !a.IsCodingPlan() {
		return
	}
	// Team identity comes only from account credentials. Clear any value that
	// arrived through client passthrough or a static header override first.
	h.Del("bigmodel-organization")
	h.Del("bigmodel-project")
	if org := strings.TrimSpace(a.GetCredential("zhipu_organization")); org != "" {
		h.Set("bigmodel-organization", org)
		if project := strings.TrimSpace(a.GetCredential("zhipu_project")); project != "" {
			h.Set("bigmodel-project", project)
		}
	}
}

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

// normalizeCNProviderStoredConfig canonicalizes both the current Extra
// representation and the legacy credential representation. Domestic protocol
// selection is deliberately lossless so adaptive routing and its per-protocol
// endpoints survive unrelated account edits.
func normalizeCNProviderStoredConfig(platform string, extra, credentials map[string]any) (map[string]any, map[string]any) {
	if !IsCNProvider(platform) {
		return extra, credentials
	}
	normalizedExtra := maps.Clone(extra)
	if normalizedExtra == nil {
		normalizedExtra = make(map[string]any)
	}
	protocol := APIProtocolChatCompletions
	requestedProtocol := strings.TrimSpace(valueAsString(normalizedExtra[cnAPIProtocolExtraKey]))
	if requestedProtocol == "" {
		requestedProtocol = strings.TrimSpace(valueAsString(credentials["api_protocol"]))
	}
	switch requestedProtocol {
	case APIProtocolAdaptive, APIProtocolAnthropic, APIProtocolChatCompletions:
		protocol = requestedProtocol
	case APIProtocolResponses:
		if platform == PlatformDeepSeek || platform == PlatformKimi {
			protocol = requestedProtocol
		}
	}
	normalizedExtra[cnAPIProtocolExtraKey] = protocol
	if _, ok := normalizedExtra[cnAPIBaseURLsExtraKey]; !ok {
		if legacy, ok := credentials["api_base_urls"]; ok {
			normalizedExtra[cnAPIBaseURLsExtraKey] = legacy
		}
	}

	normalizedCredentials := maps.Clone(credentials)
	if normalizedCredentials != nil {
		delete(normalizedCredentials, "api_protocol")
		delete(normalizedCredentials, "api_base_urls")
	}
	return normalizedExtra, normalizedCredentials
}

// normalizeCNProviderStoredConfigForAccount applies an incremental Extra edit
// without treating an omitted protocol as a request to downgrade an existing
// DeepSeek/Kimi Responses account. This is important for UpdateAccountExtra, where
// callers commonly edit an unrelated key or only remove legacy base URLs.
func normalizeCNProviderStoredConfigForAccount(account *Account, extra, credentials map[string]any) (map[string]any, map[string]any) {
	if account == nil || !account.IsCNProvider() {
		return extra, credentials
	}
	mergedExtra := maps.Clone(account.Extra)
	if mergedExtra == nil {
		mergedExtra = make(map[string]any)
	}
	for key, value := range extra {
		mergedExtra[key] = value
	}
	mergedCredentials := maps.Clone(account.Credentials)
	if mergedCredentials == nil {
		mergedCredentials = make(map[string]any)
	}
	for key, value := range credentials {
		mergedCredentials[key] = value
	}
	// An explicitly submitted legacy credential protocol is still an update
	// request. Let it override the stored Extra value when this incremental edit
	// did not also submit the canonical Extra field.
	if _, extraProtocolSubmitted := extra[cnAPIProtocolExtraKey]; !extraProtocolSubmitted {
		if requestedProtocol, credentialProtocolSubmitted := credentials["api_protocol"]; credentialProtocolSubmitted {
			mergedExtra[cnAPIProtocolExtraKey] = requestedProtocol
		}
	}
	if _, extraBaseURLsSubmitted := extra[cnAPIBaseURLsExtraKey]; !extraBaseURLsSubmitted {
		if requestedBaseURLs, credentialBaseURLsSubmitted := credentials["api_base_urls"]; credentialBaseURLsSubmitted {
			mergedExtra[cnAPIBaseURLsExtraKey] = requestedBaseURLs
		}
	}
	normalizedExtra, _ := normalizeCNProviderStoredConfig(account.Platform, mergedExtra, mergedCredentials)

	// Keep this function incremental: return only the caller's keys, plus the
	// canonical protocol when a domestic protocol edit was requested.
	resultExtra := maps.Clone(extra)
	if resultExtra == nil {
		resultExtra = make(map[string]any)
	}
	_, requestedProtocol := extra[cnAPIProtocolExtraKey]
	_, requestedCredentialProtocol := credentials["api_protocol"]
	_, requestedCredentialBaseURLs := credentials["api_base_urls"]
	_, requestedBaseURLs := extra[cnAPIBaseURLsExtraKey]
	if requestedProtocol || requestedCredentialProtocol || requestedCredentialBaseURLs || requestedBaseURLs {
		resultExtra[cnAPIProtocolExtraKey] = normalizedExtra[cnAPIProtocolExtraKey]
	}
	if requestedBaseURLs || requestedCredentialBaseURLs {
		resultExtra[cnAPIBaseURLsExtraKey] = normalizedExtra[cnAPIBaseURLsExtraKey]
	}
	resultCredentials := maps.Clone(credentials)
	if resultCredentials != nil {
		delete(resultCredentials, "api_protocol")
		delete(resultCredentials, "api_base_urls")
	}
	return resultExtra, resultCredentials
}

func hasCNProviderStoredConfigUpdate(extra, credentials map[string]any, extraRemoveKeys []string) bool {
	if _, ok := extra[cnAPIProtocolExtraKey]; ok {
		return true
	}
	if _, ok := extra[cnAPIBaseURLsExtraKey]; ok {
		return true
	}
	if _, ok := credentials["api_protocol"]; ok {
		return true
	}
	if _, ok := credentials["api_base_urls"]; ok {
		return true
	}
	for _, key := range extraRemoveKeys {
		key = strings.TrimSpace(key)
		if key == cnAPIProtocolExtraKey || key == cnAPIBaseURLsExtraKey {
			return true
		}
	}
	return false
}

func normalizeBulkUpdateForAccount(account *Account, updates AccountBulkUpdate) AccountBulkUpdate {
	updates.Extra = maps.Clone(updates.Extra)
	updates.Credentials = maps.Clone(updates.Credentials)
	updates.ExtraRemoveKeys = append([]string(nil), updates.ExtraRemoveKeys...)
	_, submittedBaseURLs := updates.Extra[cnAPIBaseURLsExtraKey]
	_, submittedLegacyBaseURLs := updates.Credentials["api_base_urls"]
	removeBaseURLs := bulkUpdateContainsKey(updates.ExtraRemoveKeys, cnAPIBaseURLsExtraKey)
	clearBaseURLs := (submittedBaseURLs && isEmptyCNBaseURLs(updates.Extra[cnAPIBaseURLsExtraKey])) ||
		(submittedLegacyBaseURLs && isEmptyCNBaseURLs(updates.Credentials["api_base_urls"]))

	if account != nil && account.IsCNProvider() {
		mergedExtra := maps.Clone(account.Extra)
		if mergedExtra == nil {
			mergedExtra = make(map[string]any)
		}
		for _, key := range updates.ExtraRemoveKeys {
			delete(mergedExtra, strings.TrimSpace(key))
		}
		for key, value := range updates.Extra {
			mergedExtra[key] = value
		}
		mergedCredentials := maps.Clone(account.Credentials)
		if mergedCredentials == nil {
			mergedCredentials = make(map[string]any)
		}
		for key, value := range updates.Credentials {
			mergedCredentials[key] = value
		}
		if _, extraProtocolSubmitted := updates.Extra[cnAPIProtocolExtraKey]; !extraProtocolSubmitted {
			if requestedProtocol, credentialProtocolSubmitted := updates.Credentials["api_protocol"]; credentialProtocolSubmitted {
				mergedExtra[cnAPIProtocolExtraKey] = requestedProtocol
			}
		}
		if _, extraBaseURLsSubmitted := updates.Extra[cnAPIBaseURLsExtraKey]; !extraBaseURLsSubmitted {
			if requestedBaseURLs, credentialBaseURLsSubmitted := updates.Credentials["api_base_urls"]; credentialBaseURLsSubmitted {
				mergedExtra[cnAPIBaseURLsExtraKey] = requestedBaseURLs
			}
		}
		normalizedExtra, _ := normalizeCNProviderStoredConfig(account.Platform, mergedExtra, mergedCredentials)
		if updates.Extra == nil {
			updates.Extra = make(map[string]any)
		}
		updates.Extra[cnAPIProtocolExtraKey] = normalizedExtra[cnAPIProtocolExtraKey]
		if clearBaseURLs {
			updates.Extra[cnAPIBaseURLsExtraKey] = nil
		} else if submittedBaseURLs || submittedLegacyBaseURLs {
			updates.Extra[cnAPIBaseURLsExtraKey] = normalizedExtra[cnAPIBaseURLsExtraKey]
		} else {
			delete(updates.Extra, cnAPIBaseURLsExtraKey)
		}
		updates.ExtraRemoveKeys = removeBulkUpdateKeys(updates.ExtraRemoveKeys, cnAPIProtocolExtraKey)
		if submittedBaseURLs || submittedLegacyBaseURLs || !removeBaseURLs {
			updates.ExtraRemoveKeys = removeBulkUpdateKeys(updates.ExtraRemoveKeys, cnAPIBaseURLsExtraKey)
		}
		// Never persist legacy protocol fields supplied by a client. BulkUpdate
		// merges JSONB credentials and has no credential removal list, so only
		// neutralize fields that already exist on the account.
		delete(updates.Credentials, "api_protocol")
		delete(updates.Credentials, "api_base_urls")
		// BulkUpdate merges JSONB credentials and has no credential removal list.
		// Only neutralize fields that already exist on the account; incoming
		// legacy fields on a clean account are simply ignored.
		if _, exists := account.Credentials["api_protocol"]; exists {
			if updates.Credentials == nil {
				updates.Credentials = make(map[string]any)
			}
			updates.Credentials["api_protocol"] = nil
		}
		if _, exists := account.Credentials["api_base_urls"]; exists {
			if updates.Credentials == nil {
				updates.Credentials = make(map[string]any)
			}
			updates.Credentials["api_base_urls"] = nil
		}
		return updates
	}

	delete(updates.Extra, cnAPIProtocolExtraKey)
	delete(updates.Extra, cnAPIBaseURLsExtraKey)
	delete(updates.Credentials, "api_protocol")
	delete(updates.Credentials, "api_base_urls")
	updates.ExtraRemoveKeys = removeBulkUpdateKeys(updates.ExtraRemoveKeys, cnAPIProtocolExtraKey, cnAPIBaseURLsExtraKey)
	return updates
}

func bulkUpdateContainsKey(values []string, key string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == key {
			return true
		}
	}
	return false
}

func isEmptyCNBaseURLs(value any) bool {
	switch values := value.(type) {
	case map[string]any:
		return len(values) == 0
	case map[string]string:
		return len(values) == 0
	default:
		return false
	}
}

func removeBulkUpdateKeys(values []string, targets ...string) []string {
	targetSet := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		targetSet[target] = struct{}{}
	}
	filtered := values[:0]
	for _, value := range values {
		if _, drop := targetSet[strings.TrimSpace(value)]; !drop {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func valueAsString(value any) string {
	text, _ := value.(string)
	return text
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

// GetAPIProtocol returns the selected domestic provider wire protocol.
// Responses is supported by DeepSeek and Kimi; all domestic providers support
// adaptive, Chat Completions, and native Anthropic routing.
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
		if a.IsDeepSeek() || a.IsKimi() {
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
		switch a.Platform {
		case PlatformDeepSeek:
			return DefaultDeepSeekResponsesBaseURL
		case PlatformKimi:
			if a.IsCodingPlan() {
				return DefaultKimiCodingResponsesBaseURL
			}
			return DefaultKimiPayGResponsesBaseURL
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
	if protocol == APIProtocolAnthropic && !a.IsAnthropicProtocol() && !a.IsAdaptiveAPIProtocol() {
		return ""
	}
	if a.IsAdaptiveAPIProtocol() {
		if configured := a.getCNConfiguredProtocolBaseURL(protocol); configured != "" {
			return configured
		}
	}
	if protocol == a.GetAPIProtocol() {
		if configured := a.getCNConfiguredProtocolBaseURL(protocol); configured != "" {
			return configured
		}
	}
	// Chat Completions remains the legacy base_url fallback for every domestic
	// mode, including adaptive accounts created before the endpoint map existed.
	if protocol == a.GetAPIProtocol() || (a.IsAdaptiveAPIProtocol() && protocol == APIProtocolChatCompletions) {
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
