package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	maxReasoningEffortMappings = 64
	maxReasoningEffortValueLen = 64
)

var openAIReasoningEffortValues = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

func NormalizeMaxReasoningEffort(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	value = strings.NewReplacer("-", "", "_", "", " ", "").Replace(value)
	switch value {
	case "minimal", "low", "medium", "high", "xhigh", "extrahigh", "max":
		if value == "extrahigh" {
			return "xhigh"
		}
		return value
	default:
		return ""
	}
}

func normalizeMaxReasoningEffortForPlatform(platform, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	if platform != PlatformOpenAI {
		return "", fmt.Errorf("reasoning effort policy is only supported for platform %q", PlatformOpenAI)
	}
	value := NormalizeMaxReasoningEffort(raw)
	for _, allowed := range openAIReasoningEffortValues {
		if value == allowed {
			return value, nil
		}
	}
	return "", fmt.Errorf("reasoning effort %q is not supported for platform %q", raw, platform)
}

func reasoningEffortRank(raw string) (int, bool) {
	switch NormalizeMaxReasoningEffort(raw) {
	case "minimal":
		return 1, true
	case "low":
		return 2, true
	case "medium":
		return 3, true
	case "high":
		return 4, true
	case "xhigh":
		return 5, true
	case "max":
		return 6, true
	default:
		return 0, false
	}
}

func NormalizeReasoningEffortMappings(platform string, raw []ReasoningEffortMapping) ([]ReasoningEffortMapping, error) {
	if len(raw) > maxReasoningEffortMappings {
		return nil, fmt.Errorf("reasoning effort mappings cannot exceed %d entries", maxReasoningEffortMappings)
	}
	normalized := make([]ReasoningEffortMapping, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for i, mapping := range raw {
		from := NormalizeMaxReasoningEffort(mapping.From)
		to := NormalizeMaxReasoningEffort(mapping.To)
		if from == "" || to == "" || len(from) > maxReasoningEffortValueLen || len(to) > maxReasoningEffortValueLen {
			return nil, fmt.Errorf("reasoning effort mapping %d contains an invalid value", i+1)
		}
		if _, err := normalizeMaxReasoningEffortForPlatform(platform, from); err != nil {
			return nil, err
		}
		if _, err := normalizeMaxReasoningEffortForPlatform(platform, to); err != nil {
			return nil, err
		}
		if _, exists := seen[from]; exists {
			return nil, fmt.Errorf("duplicate reasoning effort mapping source %q", from)
		}
		seen[from] = struct{}{}
		normalized = append(normalized, ReasoningEffortMapping{From: from, To: to})
	}
	return normalized, nil
}

func ApplyOpenAIReasoningEffortPolicy(body []byte, maxEffort string, mappings []ReasoningEffortMapping) ([]byte, bool) {
	maxRank, hasMax := reasoningEffortRank(maxEffort)
	if len(body) == 0 || (!hasMax && len(mappings) == 0) {
		return body, false
	}
	result := body
	changed := false
	for _, path := range []string{"reasoning.effort", "reasoning_effort"} {
		field := gjson.GetBytes(result, path)
		if !field.Exists() || field.Type != gjson.String {
			continue
		}
		original := strings.TrimSpace(field.String())
		effective := original
		canonical := NormalizeMaxReasoningEffort(original)
		for _, mapping := range mappings {
			if canonical != "" && canonical == mapping.From {
				effective = mapping.To
				break
			}
		}
		if rank, recognized := reasoningEffortRank(effective); recognized && hasMax && rank > maxRank {
			effective = NormalizeMaxReasoningEffort(maxEffort)
		}
		if effective == original {
			continue
		}
		updated, err := sjson.SetBytes(result, path, effective)
		if err != nil {
			continue
		}
		result = updated
		changed = true
	}
	return result, changed
}

func sanitizeGroupReasoningEffortPolicy(group *Group) {
	if group == nil {
		return
	}
	maxEffort, maxErr := normalizeMaxReasoningEffortForPlatform(group.Platform, group.MaxReasoningEffort)
	mappings, mappingsErr := NormalizeReasoningEffortMappings(group.Platform, group.ReasoningEffortMappings)
	if maxErr != nil {
		maxEffort = ""
	}
	if mappingsErr != nil {
		mappings = nil
	}
	group.MaxReasoningEffort = maxEffort
	group.ReasoningEffortMappings = mappings
}
