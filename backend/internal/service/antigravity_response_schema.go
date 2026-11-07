package service

import "github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"

func normalizeAntigravityResponseSchema(payload map[string]any) bool {
	changed := false
	for _, key := range []string{"generationConfig", "generation_config"} {
		cfg, ok := payload[key].(map[string]any)
		if !ok {
			continue
		}
		for _, alias := range []string{"responseJsonSchema", "response_json_schema"} {
			raw, exists := cfg[alias]
			if !exists {
				continue
			}
			delete(cfg, alias)
			changed = true
			_, camel := cfg["responseSchema"]
			_, snake := cfg["response_schema"]
			if camel || snake {
				continue
			}
			if schema, ok := raw.(map[string]any); ok {
				antigravity.DeepCleanUndefined(schema)
				cfg["responseSchema"] = antigravity.CleanJSONSchema(schema)
			}
		}
	}
	return changed
}
