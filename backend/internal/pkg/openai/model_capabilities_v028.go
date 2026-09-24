package openai

import "strings"

// CanonicalGPT6SolLunaModel deliberately excludes unknown family suffixes.
func CanonicalGPT6SolLunaModel(model string) string {
	id := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(model)), "_", "-")
	id = strings.TrimPrefix(id, "openai/")
	id = strings.TrimPrefix(id, "models/")
	for _, base := range []string{"gpt-6-sol", "gpt-6-luna"} {
		if id == base {
			return base
		}
		if suffix, ok := strings.CutPrefix(id, base+"-"); ok {
			switch suffix {
			case "none", "low", "medium", "high", "xhigh", "max", "openai-compact":
				return base
			}
		}
	}
	return ""
}
func IsGPT6SolOrLunaModelSpelling(model string) bool { return CanonicalGPT6SolLunaModel(model) != "" }
