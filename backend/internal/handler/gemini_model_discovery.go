package handler

import (
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/gemini"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func allowedGeminiDiscoveryModels(models []gemini.Model, group *service.Group) []gemini.Model {
	filtered := make([]gemini.Model, 0, len(models))
	for _, model := range models {
		if geminiDiscoveryModelAllowed(model.Name, group) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func geminiDiscoveryModelAllowed(name string, group *service.Group) bool {
	if group == nil || !group.CustomModelsListEnabled() {
		return true
	}
	name = strings.TrimPrefix(name, "models/")
	for _, pattern := range group.ModelsListConfig.Models {
		pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "models/")
		if pattern == name || (strings.HasSuffix(pattern, "*") && strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))) {
			return true
		}
	}
	return false
}

func appendUpstreamGeminiModels(body []byte, extra []gemini.Model, group *service.Group) ([]byte, bool) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil || envelope == nil {
		return body, false
	}
	var models []json.RawMessage
	if raw, ok := envelope["models"]; !ok || json.Unmarshal(raw, &models) != nil {
		return body, false
	}
	seen := make(map[string]bool)
	kept := make([]json.RawMessage, 0, len(models)+len(extra))
	changed := false
	for _, raw := range models {
		var model gemini.Model
		if json.Unmarshal(raw, &model) != nil {
			return body, false
		}
		if len(allowedGeminiDiscoveryModels([]gemini.Model{model}, group)) == 0 {
			changed = true
			continue
		}
		seen[model.Name] = true
		kept = append(kept, raw)
	}
	for _, model := range allowedGeminiDiscoveryModels(extra, group) {
		if seen[model.Name] {
			continue
		}
		encoded, err := json.Marshal(model)
		if err != nil {
			return body, false
		}
		kept = append(kept, encoded)
		seen[model.Name] = true
		changed = true
	}
	if !changed {
		return body, true
	}
	envelope["models"], _ = json.Marshal(kept)
	encoded, err := json.Marshal(envelope)
	return encoded, err == nil
}
