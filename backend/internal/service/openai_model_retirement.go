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
	views := make([]openAIModelRetirementAccountView, 0, len(accounts))
	for i := range accounts {
		if accounts[i].IsOpenAI() {
			views = append(views, openAIModelRetirementAccountView{account: &accounts[i], mapping: accounts[i].GetModelMapping()})
		}
	}
	return openAIModelRetiredForAccountViews(views, modelID)
}

type openAIModelRetirementAccountView struct {
	account *Account
	mapping map[string]string
}

func openAIModelRetirementAccountSupportsModel(view openAIModelRetirementAccountView, modelID string) bool {
	if view.account == nil {
		return false
	}
	if len(view.mapping) == 0 {
		if view.account.IsOpenAIOAuthLike() && !view.account.IsOpenAIPassthroughEnabled() {
			return isOpenAIOAuthServableModel(modelID)
		}
		return true
	}
	if mappingSupportsRequestedModel(view.mapping, modelID) {
		return true
	}
	normalized := normalizeRequestedModelForLookup(view.account.Platform, modelID)
	return normalized != modelID && mappingSupportsRequestedModel(view.mapping, normalized)
}

func openAIModelRetiredForAccountViews(views []openAIModelRetirementAccountView, modelID string) bool {
	retired := false
	for _, view := range views {
		if view.account == nil {
			continue
		}
		target, matched := resolveRequestedModelInMapping(view.mapping, modelID)
		if !matched {
			target = modelID
			normalized := normalizeRequestedModelForLookup(view.account.Platform, modelID)
			if normalized != modelID {
				target, matched = resolveRequestedModelInMapping(view.mapping, normalized)
				if !matched {
					target = modelID
				}
			}
		}
		if isRetiredOpenAISubscriptionModel(view.account, target) {
			retired = true
		} else if openAIModelRetirementAccountSupportsModel(view, modelID) {
			return false
		}
	}
	return retired
}

func filterRetiredOpenAIModelIDs(accounts []Account, models []string) []string {
	views := make([]openAIModelRetirementAccountView, 0, len(accounts))
	for i := range accounts {
		if accounts[i].IsOpenAI() {
			views = append(views, openAIModelRetirementAccountView{account: &accounts[i], mapping: accounts[i].GetModelMapping()})
		}
	}
	filtered := make([]string, 0, len(models))
	for _, model := range models {
		if !openAIModelRetiredForAccountViews(views, model) {
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
	views := make([]openAIModelRetirementAccountView, 0, len(accounts))
	for i := range accounts {
		if accounts[i].IsOpenAI() {
			views = append(views, openAIModelRetirementAccountView{account: &accounts[i], mapping: accounts[i].GetModelMapping()})
		}
	}
	filtered := make([]json.RawMessage, 0, len(entries))
	for _, raw := range entries {
		var entry map[string]json.RawMessage
		var id string
		if json.Unmarshal(raw, &entry) == nil {
			_ = json.Unmarshal(entry[idField], &id)
		}
		if !openAIModelRetiredForAccountViews(views, id) {
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

// Availability diagnostics follow the request model check, including ordinary
// passthrough ignoring account mappings that are not sent upstream.
func supportsOpenAIRequestModel(account *Account, requestedModel string, requireCompact bool) bool {
	if account == nil {
		return false
	}
	return openAITextRequestModelSupported(account, OpenAIAccountScheduleRequest{
		RequestedModel:     requestedModel,
		RequireCompact:     requireCompact,
		RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
		RequestPlatform:    account.Platform,
	})
}

// FilterRetiredOpenAIAccountModels keeps the admin test list aligned with routing.
func FilterRetiredOpenAIAccountModels(account *Account, models []openai.Model) []openai.Model {
	if account == nil {
		return models
	}
	accountView := *account
	views := []openAIModelRetirementAccountView{{account: &accountView, mapping: accountView.GetModelMapping()}}
	out := make([]openai.Model, 0, len(models))
	for _, model := range models {
		if !openAIModelRetiredForAccountViews(views, model.ID) {
			out = append(out, model)
		}
	}
	return out
}

// Keep legacy selection and fresh-account rechecks aligned with the advanced
// scheduler when a server-wide compact fallback supplies the upstream model.
func (s *OpenAIGatewayService) isOpenAIAccountEligibleForRequest(ctx context.Context, account *Account, model string, compact bool, capability OpenAIEndpointCapability, images OpenAIImagesCapability, platform ...string) bool {
	return isOpenAIAccountEligibleForRequestWithCompactFallback(ctx, account, model, compact, capability, images, s.openAICompactModelFallback(), platform...)
}
