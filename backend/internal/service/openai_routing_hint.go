package service

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	openAICodexRoutingHintHeader = "x-codex-routing-hint"
)

func setOpenAICodexRoutingHint(headers http.Header, account *Account, model, tier string, enabled bool) {
	if headers == nil {
		return
	}
	for key := range headers {
		if strings.EqualFold(key, openAICodexRoutingHintHeader) {
			delete(headers, key)
		}
	}
	if !enabled || account == nil || !account.IsOpenAIOAuth() {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" || strings.ContainsAny(model, ";=") || account.IsOpenAIUpstreamStrongIsolationEnabled() {
		return
	}
	hint := "model=" + model
	switch normalizedOpenAIServiceTierValue(tier) {
	case OpenAIFastTierPriority, OpenAIFastTierUltrafast, OpenAIFastTierFlex:
		hint += ";tier=" + normalizedOpenAIServiceTierValue(tier)
	}
	headers.Set(openAICodexRoutingHintHeader, hint)
}

func openAICodexRoutingHintEligible(account *Account, enabled bool) bool {
	return enabled && account != nil && account.IsOpenAIOAuth() && !account.IsOpenAIUpstreamStrongIsolationEnabled()
}

func setOpenAICodexRoutingHintFromBody(headers http.Header, account *Account, body []byte, enabled bool) {
	if !openAICodexRoutingHintEligible(account, enabled) {
		setOpenAICodexRoutingHint(headers, account, "", "", false)
		return
	}
	fields := gjson.GetManyBytes(body, "model", "service_tier")
	setOpenAICodexRoutingHint(headers, account, fields[0].String(), fields[1].String(), enabled)
}
