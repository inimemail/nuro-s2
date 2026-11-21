package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
)

func projectCodexManifestForAccount(body []byte, account *Account) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	var models []json.RawMessage
	if bytes.Equal(bytes.TrimSpace(envelope["models"]), []byte("null")) {
		return nil, fmt.Errorf("invalid models array")
	}
	if err := json.Unmarshal(envelope["models"], &models); err != nil {
		return nil, err
	}
	mapping := account.GetModelMapping()
	passthrough := account.IsOpenAIPassthroughEnabled()
	if passthrough {
		mapping = nil
	}
	byID := make(map[string]json.RawMessage, len(models))
	ids := make([]string, 0, len(models)+len(mapping))
	for _, raw := range models {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(raw, &item); err != nil || item == nil {
			return nil, fmt.Errorf("invalid model entry")
		}
		var id string
		if err := json.Unmarshal(item["slug"], &id); err != nil || strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("invalid model slug")
		}
		id = strings.TrimSpace(id)
		if _, exists := byID[id]; !exists {
			byID[id] = raw
			ids = append(ids, id)
		}
	}
	if len(mapping) > 0 {
		aliases := make([]string, 0, len(mapping))
		for alias := range mapping {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		ids = append(ids, aliases...)
	}
	projected := make([]json.RawMessage, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] || strings.Contains(id, "*") {
			continue
		}
		if account.IsOpenAIPassthroughModelFilterEnabled() && !account.IsModelSupported(id) {
			continue
		}
		if passthrough && account.IsOpenAIOAuthLike() && !account.IsShadow() &&
			!account.IsImagePoolMode() && isClearlyForeignOpenAIOAuthTarget(id) {
			continue
		}
		target := id
		if len(mapping) > 0 {
			var matched bool
			target, matched = account.ResolveMappedModel(id)
			if !matched {
				continue
			}
		}
		raw, available := byID[strings.TrimSpace(target)]
		if !available {
			continue
		}
		seen[id] = true
		var item map[string]json.RawMessage
		_ = json.Unmarshal(raw, &item)
		changed := false
		for _, field := range []string{"supports_search_tool", "use_responses_lite"} {
			if value, exists := item[field]; exists && !bytes.Equal(bytes.TrimSpace(value), []byte("true")) && !bytes.Equal(bytes.TrimSpace(value), []byte("false")) {
				item[field] = json.RawMessage(`false`)
				changed = true
			}
		}
		if account.IsOpenAIApiKey() && (!account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses) ||
			!openai_compat.ShouldUseResponsesAPI(account.Extra)) {
			modalities, removed := removeCodexAudioModality(item["input_modalities"])
			if removed {
				item["input_modalities"], _ = json.Marshal(modalities)
				changed = true
			}
		}
		if id != target {
			item["slug"], _ = json.Marshal(id)
			item["display_name"], _ = json.Marshal(id)
			changed = true
		}
		if value, exists := item["service_tiers"]; exists {
			var tiers []struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			valid := !bytes.Equal(bytes.TrimSpace(value), []byte("null")) && json.Unmarshal(value, &tiers) == nil
			for _, tier := range tiers {
				valid = valid && strings.TrimSpace(tier.ID) != ""
			}
			if !valid {
				item["service_tiers"] = json.RawMessage(`[]`)
				changed = true
			}
		}
		if changed {
			var err error
			raw, err = json.Marshal(item)
			if err != nil {
				return nil, err
			}
		}
		projected = append(projected, raw)
	}
	if slices.EqualFunc(models, projected, func(a, b json.RawMessage) bool { return bytes.Equal(a, b) }) {
		return body, nil
	}
	envelope["models"], _ = json.Marshal(projected)
	return json.Marshal(envelope)
}

func filterCodexManifestForGroup(body []byte, group *Group) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	var models []json.RawMessage
	if err := json.Unmarshal(envelope["models"], &models); err != nil {
		return nil, err
	}
	filtered := make([]json.RawMessage, 0, len(models))
	for _, raw := range models {
		var item struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		if groupModelAllowed(group.ModelsListConfig.Models, item.Slug) {
			filtered = append(filtered, raw)
		}
	}
	if len(filtered) == len(models) {
		return body, nil
	}
	envelope["models"], _ = json.Marshal(filtered)
	return json.Marshal(envelope)
}

func intersectCodexManifestCapabilities(first, next json.RawMessage) (json.RawMessage, error) {
	var a, b map[string]json.RawMessage
	if err := json.Unmarshal(first, &a); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(next, &b); err != nil {
		return nil, err
	}
	changed := false
	for _, field := range []string{"supports_search_tool", "use_responses_lite", "service_tiers"} {
		av, bv := bytes.TrimSpace(a[field]), bytes.TrimSpace(b[field])
		if len(av) == 0 && len(bv) == 0 {
			continue
		}
		if bytes.Equal(av, bv) {
			continue
		}
		fallback := json.RawMessage(`false`)
		if field == "service_tiers" {
			fallback = json.RawMessage(`[]`)
		}
		if !bytes.Equal(av, fallback) {
			a[field] = fallback
			changed = true
		}
	}
	var nextModalities []string
	if json.Unmarshal(b["input_modalities"], &nextModalities) != nil || !slices.Contains(nextModalities, "audio") {
		if modalities, removed := removeCodexAudioModality(a["input_modalities"]); removed {
			a["input_modalities"], _ = json.Marshal(modalities)
			changed = true
		}
	}
	if !changed {
		return first, nil
	}
	return json.Marshal(a)
}

func removeCodexAudioModality(raw json.RawMessage) ([]string, bool) {
	var modalities []string
	if json.Unmarshal(raw, &modalities) != nil || !slices.Contains(modalities, "audio") {
		return nil, false
	}
	return slices.DeleteFunc(modalities, func(modality string) bool { return modality == "audio" }), true
}
