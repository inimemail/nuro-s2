package service

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// GPT-5.5 retired from Codex with ChatGPT sign-in on 2026-10-14 at
// 19:00 UTC. API-key upstreams remain supported. Do not infer upstream
// eligibility from the API key a client uses to authenticate to this gateway.
// https://learn.chatgpt.com/docs/models#gpt-55-retirement
func isRetiredOpenAISubscriptionModel(account *Account, upstreamModel string) bool {
	if account == nil || !account.IsOpenAI() || (!account.IsOpenAIOAuthLike() && !account.IsShadow()) {
		return false
	}
	model := canonicalizeOpenAIModelAliasSpelling(upstreamModel)
	return model == "gpt-5.5" || strings.HasPrefix(model, "gpt-5.5-")
}

// Only remove entries blocked by retirement. Unknown models and other platforms
// retain their existing listing policy. An explicitly mapped replacement is
// allowed, but a public alias targeting retired GPT-5.5 is not.
func openAIModelRetiredForAccounts(accounts []Account, modelID string) bool {
	retired := false
	for i := range accounts {
		account := &accounts[i]
		if !account.IsOpenAI() {
			continue
		}
		target := resolveOpenAIAccountUpstreamModelForRequest(account, modelID, false, "")
		if isRetiredOpenAISubscriptionModel(account, target) {
			retired = true
		} else if account.IsModelSupported(modelID) {
			return false
		}
	}
	return retired
}

func filterRetiredOpenAIModelIDs(accounts []Account, models []string) []string {
	filtered := make([]string, 0, len(models))
	for _, model := range models {
		if !openAIModelRetiredForAccounts(accounts, model) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// Entries must already use client-visible names, not raw upstream IDs. Resolve
// each public name exactly as routing does, after projection and before the
// final client ETag is calculated. Preserve unknown fields.
func filterRetiredOpenAIPublicModelsBody(body []byte, accounts []Account, codex bool) ([]byte, bool, error) {
	field, idField := "data", "id"
	if codex {
		field, idField = "models", "slug"
	}
	var envelope map[string]json.RawMessage
	err := json.Unmarshal(body, &envelope)
	var entries []json.RawMessage
	if err == nil {
		err = json.Unmarshal(envelope[field], &entries)
	}
	if err != nil {
		return nil, false, err
	}
	filtered := make([]json.RawMessage, 0, len(entries))
	for _, raw := range entries {
		var entry map[string]json.RawMessage
		var id string
		if json.Unmarshal(raw, &entry) == nil {
			_ = json.Unmarshal(entry[idField], &id)
		}
		if !openAIModelRetiredForAccounts(accounts, id) {
			filtered = append(filtered, raw)
		}
	}
	if len(filtered) == len(entries) {
		return body, false, nil
	}
	envelope[field], err = json.Marshal(filtered)
	if err != nil {
		return nil, false, err
	}
	encoded, err := json.Marshal(envelope)
	return encoded, true, err
}

// Compact-only administrator mappings are evaluated against their actual
// target, without treating an old client-visible name as a retired target.
func supportsOpenAIRequestModel(account *Account, requestedModel string, requireCompact bool) bool {
	if isRetiredOpenAISubscriptionModel(account, resolveOpenAIAccountUpstreamModelForRequest(account, requestedModel, requireCompact, "")) {
		return false
	}
	return account.IsModelSupported(requestedModel)
}

// FilterRetiredOpenAIAccountModels keeps the admin test list aligned with routing.
func FilterRetiredOpenAIAccountModels(account *Account, models []openai.Model) []openai.Model {
	if account == nil {
		return models
	}
	out := make([]openai.Model, 0, len(models))
	for _, model := range models {
		if !openAIModelRetiredForAccounts([]Account{*account}, model.ID) {
			out = append(out, model)
		}
	}
	return out
}

// Keep legacy selection and fresh-account rechecks aligned with the advanced
// scheduler when a server-wide compact fallback supplies the upstream model.
func (s *OpenAIGatewayService) isOpenAIAccountEligibleForRequest(ctx context.Context, account *Account, model string, compact bool, capability OpenAIEndpointCapability, images OpenAIImagesCapability, platform ...string) bool {
	if !isOpenAIAccountEligibleForRequest(ctx, account, model, compact, capability, images, platform...) {
		return false
	}
	return !isRetiredOpenAISubscriptionModel(account, resolveOpenAIAccountUpstreamModelForRequest(account, model, compact, s.openAICompactModelFallback()))
}
