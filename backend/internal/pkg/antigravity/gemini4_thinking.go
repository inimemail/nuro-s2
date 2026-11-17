package antigravity

import "strings"

func isGemini4LevelModel(model string) bool {
	name := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "models/")
	for _, base := range []string{"gemini-4-carbon", "gemini-4-hydrogen"} {
		switch name {
		case base, base + "-low", base + "-medium", base + "-high", base + "-tiered":
			return true
		}
	}
	return false
}
func gemini4ThinkingConfig(req *ClaudeRequest) *GeminiThinkingConfig {
	cfg := &GeminiThinkingConfig{}
	if req.Thinking != nil {
		cfg.IncludeThoughts = req.Thinking.Type == "enabled" || req.Thinking.Type == "adaptive"
	}
	name := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(req.Model)), "models/")
	if name == "gemini-4-carbon" || name == "gemini-4-hydrogen" {
		if req.OutputConfig != nil {
			switch strings.ToLower(strings.TrimSpace(req.OutputConfig.Effort)) {
			case "none", "minimal", "low":
				cfg.ThinkingLevel = "low"
			case "medium":
				cfg.ThinkingLevel = "medium"
			case "high", "xhigh", "max":
				cfg.ThinkingLevel = "high"
			}
		}
		if req.Thinking != nil {
			if req.Thinking.Type == "disabled" {
				cfg.ThinkingLevel = "low"
			} else if cfg.ThinkingLevel == "" && req.Thinking.Type == "enabled" && req.Thinking.BudgetTokens > 0 {
				switch {
				case req.Thinking.BudgetTokens <= 1024:
					cfg.ThinkingLevel = "low"
				case req.Thinking.BudgetTokens <= 8192:
					cfg.ThinkingLevel = "medium"
				default:
					cfg.ThinkingLevel = "high"
				}
			}
		}
	}
	if !cfg.IncludeThoughts && cfg.ThinkingLevel == "" {
		return nil
	}
	return cfg
}
