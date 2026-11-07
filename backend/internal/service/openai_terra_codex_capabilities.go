package service

import (
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// Capability projection only: never inject descriptor prompts, sort priorities,
// code_mode_only, cache keys or API defaults into a user's request.
func terraCodexCapabilities() map[string]any {
	return map[string]any{
		"context_window":          500000,
		"max_context_window":      872000,
		"default_reasoning_level": "low",
		"supported_reasoning_levels": []map[string]string{
			{"effort": "low", "description": "Fast responses with lighter reasoning"},
			{"effort": "medium", "description": "Balances speed and reasoning depth"},
			{"effort": "high", "description": "Greater reasoning depth"},
			{"effort": "xhigh", "description": "Extra high reasoning depth"},
			{"effort": "max", "description": "Maximum reasoning depth"},
			{"effort": "ultra", "description": "Maximum reasoning with automatic task delegation"},
		},
		"input_modalities":             []string{"text", "image"},
		"supports_parallel_tool_calls": true,
	}
}

func fillTerraCodexCapabilities(model map[string]json.RawMessage, slug string) bool {
	if !openai.IsGPT61TerraModelSpelling(slug) {
		return false
	}
	changed := false
	for key, value := range terraCodexCapabilities() {
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
