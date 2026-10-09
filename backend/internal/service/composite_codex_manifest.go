package service

import "encoding/json"

// Composite aliases have no trusted upstream capability snapshot. Emit the
// complete Codex wire contract without inferring capacity from a public alias.
func BuildCompositeCodexManifest(models []string) ([]byte, error) {
	items := make([]map[string]any, 0, len(models))
	for _, model := range models {
		items = append(items, map[string]any{
			"slug": model, "display_name": model, "description": "", "base_instructions": "",
			"supported_reasoning_levels": []any{}, "shell_type": "unified_exec",
			"visibility": "list", "supported_in_api": true, "priority": 0,
			"additional_speed_tiers": []string{}, "service_tiers": []any{},
			"default_service_tier": nil, "availability_nux": nil, "upgrade": nil,
			"model_messages": map[string]any{
				"instructions_template": "", "instructions_variables": nil,
				"approvals": nil, "collaboration_modes": nil, "auto_review": nil,
				"permissions": nil, "multi_agent": nil, "token_budget": nil, "guardian_v2": nil,
			},
			"include_skills_usage_instructions": false, "include_plugin_usage_instructions": false,
			"include_apps_usage_instructions": false, "supports_reasoning_summary_parameter": false,
			"default_reasoning_summary": "none", "support_verbosity": false, "default_verbosity": nil,
			"apply_patch_tool_type": nil, "web_search_tool_type": "text",
			"truncation_policy":              map[string]any{"mode": "bytes", "limit": 10000},
			"supports_image_detail_original": false, "supports_parallel_tool_calls": false,
			"context_window": nil, "max_context_window": nil, "auto_compact_token_limit": nil,
			"comp_hash": nil, "effective_context_window_percent": 95,
			"experimental_supported_tools": []string{}, "input_modalities": []string{"text"},
			"supports_search_tool": false, "use_responses_lite": false,
			"node_repl_auto_review_required": false, "node_repl_disabled": true,
			"auto_review_model_override": nil, "model_specialty": nil, "tool_mode": nil, "multi_agent_version": nil,
		})
	}
	return json.Marshal(map[string]any{"models": items})
}
