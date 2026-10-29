package service

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/tidwall/gjson"
)

// validateGPT61SolRequestBody applies the GPT-6.1 Sol restriction to both
// explicit request fields and model aliases. Every model candidate is checked
// because routing may retain the client model, apply an account mapping, and
// then normalize an OAuth model before the request is sent upstream.
func validateGPT61SolRequestBody(body []byte, modelCandidates ...string) error {
	effort := strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String())
	if effort == "" {
		effort = strings.TrimSpace(gjson.GetBytes(body, "reasoning_effort").String())
	}
	if effort == "" {
		effort = strings.TrimSpace(gjson.GetBytes(body, "output_config.effort").String())
	}
	// Anthropic's converter treats disabled thinking as reasoning effort none,
	// even when output_config.effort is absent (or contains another value).
	// Resolve that effective value here so a protocol conversion cannot bypass
	// GPT-6.1 Sol's unsupported none/minimal restriction.
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()), "disabled") {
		effort = "none"
	}
	for _, model := range modelCandidates {
		if strings.TrimSpace(model) == "" {
			continue
		}
		if err := openai.ValidateGPT61SolModelRequest(model, effort); err != nil {
			return err
		}
	}
	return nil
}

func NormalizeOpenAICompatRequestedModel(model string) string {
	if openai.IsGPT61TerraModelSpelling(model) {
		return "gpt-6.1-terra"
	}
	if openai.IsGPT61SolModelSpelling(model) {
		return "gpt-6.1-sol"
	}
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return ""
	}

	normalized, _, ok := splitOpenAICompatReasoningModel(trimmed)
	if !ok || normalized == "" {
		return trimmed
	}
	return normalized
}

func applyOpenAICompatModelNormalization(req *apicompat.AnthropicRequest) {
	if req == nil {
		return
	}

	originalModel := strings.TrimSpace(req.Model)
	if originalModel == "" {
		return
	}

	if openai.IsGPT61TerraModelSpelling(originalModel) {
		req.Model = NormalizeOpenAICompatRequestedModel(originalModel)
		if req.OutputConfig == nil || strings.TrimSpace(req.OutputConfig.Effort) == "" {
			_, effort, ok := splitOpenAICompatReasoningModel(originalModel)
			if ok {
				if req.OutputConfig == nil {
					req.OutputConfig = &apicompat.AnthropicOutputConfig{}
				}
				req.OutputConfig.Effort = effort
			}
		}
		return
	}
	normalizedModel, derivedEffort, hasReasoningSuffix := splitOpenAICompatReasoningModel(originalModel)
	if hasReasoningSuffix && normalizedModel != "" {
		req.Model = normalizedModel
	}

	if req.OutputConfig != nil && strings.TrimSpace(req.OutputConfig.Effort) != "" {
		return
	}

	claudeEffort := openAIReasoningEffortToClaudeOutputEffort(derivedEffort)
	if claudeEffort == "" {
		return
	}

	if req.OutputConfig == nil {
		req.OutputConfig = &apicompat.AnthropicOutputConfig{}
	}
	req.OutputConfig.Effort = claudeEffort
}

func splitOpenAICompatReasoningModel(model string) (normalizedModel string, reasoningEffort string, ok bool) {
	if openai.IsGPT61TerraModelSpelling(model) {
		id := canonicalizeOpenAIModelAliasSpelling(model)
		effort, suffix := strings.CutPrefix(id, "gpt-6.1-terra-")
		if suffix && effort != "openai-compact" {
			return "gpt-6.1-terra", effort, true
		}
		return "gpt-6.1-terra", "", false
	}
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return "", "", false
	}

	modelID := trimmed
	if strings.Contains(modelID, "/") {
		parts := strings.Split(modelID, "/")
		modelID = parts[len(parts)-1]
	}
	modelID = strings.TrimSpace(modelID)
	if !strings.HasPrefix(strings.ToLower(modelID), "gpt-") {
		return trimmed, "", false
	}

	parts := strings.FieldsFunc(strings.ToLower(modelID), func(r rune) bool {
		switch r {
		case '-', '_', ' ':
			return true
		default:
			return false
		}
	})
	if len(parts) == 0 {
		return trimmed, "", false
	}

	last := strings.NewReplacer("-", "", "_", "", " ", "").Replace(parts[len(parts)-1])
	switch last {
	case "none", "minimal":
	case "low", "medium", "high":
		reasoningEffort = last
	case "xhigh", "extrahigh":
		reasoningEffort = "xhigh"
	default:
		return trimmed, "", false
	}

	return normalizeCodexModel(modelID), reasoningEffort, true
}

func openAIReasoningEffortToClaudeOutputEffort(effort string) string {
	switch strings.TrimSpace(effort) {
	case "low", "medium", "high":
		return effort
	case "xhigh":
		return "max"
	default:
		return ""
	}
}

// openAICompatAnthropicReasoningEffort is applied only after the final
// upstream model is known. GPT-5.6 accepts Anthropic's original max effort;
// other models retain the existing converted value.
func openAICompatAnthropicReasoningEffort(req *apicompat.AnthropicRequest, upstreamModel, convertedEffort string) string {
	if req == nil || req.OutputConfig == nil || !strings.EqualFold(strings.TrimSpace(req.OutputConfig.Effort), "max") {
		return convertedEffort
	}
	if normalized := normalizeOpenAIReasoningEffortForModel(req.OutputConfig.Effort, upstreamModel); normalized != "" {
		return normalized
	}
	return convertedEffort
}
