package service

import (
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// Project only established API-key capabilities; OAuth descriptors pass through
// unchanged. No media, routing, cache keys or Ultra billing are enabled here.
func gpt62CodexCapabilities(model string) map[string]any {
	id := openai.GPT62ModelID(model)
	if id == "" {
		return nil
	}
	context := 500000
	if id == "gpt-6.2-astra" {
		context = 272000
	}
	return map[string]any{
		"context_window": context, "max_context_window": 872000,
		"default_reasoning_level": "low",
		"supported_reasoning_levels": []map[string]string{
			{"effort": "low", "description": "Fast responses with lighter reasoning"},
			{"effort": "medium", "description": "Balances speed and reasoning depth"},
			{"effort": "high", "description": "Greater reasoning depth"},
			{"effort": "xhigh", "description": "Extra high reasoning depth"},
			{"effort": "max", "description": "Maximum reasoning depth"},
		},
		"input_modalities":             []string{"text", "image"},
		"supports_parallel_tool_calls": true,
	}
}

func fillGPT62CodexCapabilities(model map[string]json.RawMessage, slug string) bool {
	changed := false
	for key, value := range gpt62CodexCapabilities(slug) {
		if _, exists := model[key]; exists {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			continue
		}
		model[key] = encoded
		changed = true
	}
	return changed
}
