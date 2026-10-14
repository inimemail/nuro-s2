package claude

import "strings"

// normalize55Model accepts provider prefixes and the documented date/thinking
// spellings while keeping model matching exact after normalization.
func normalize55Model(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	id = strings.TrimPrefix(id, "models/")
	id = strings.TrimPrefix(id, "anthropic/")
	for _, prefix := range []string{"us.", "eu.", "apac.", "global."} {
		id = strings.TrimPrefix(id, prefix)
	}
	id = strings.TrimPrefix(id, "anthropic.")
	id = strings.TrimSuffix(id, "-v1:0")
	id = strings.ReplaceAll(id, "@", "-")
	id = strings.TrimSuffix(id, "-thinking")
	if len(id) >= 9 {
		suffix := id[len(id)-9:]
		digits := suffix[0] == '-'
		for _, r := range suffix[1:] {
			if r < '0' || r > '9' {
				digits = false
			}
		}
		if digits {
			id = id[:len(id)-9]
		}
	}
	return id
}

// IsOpus55 accepts only the fixed model and documented provider/date/thinking
// spellings, never another model whose name happens to share a prefix.
func IsOpus55(model string) bool {
	return normalize55Model(model) == "claude-opus-5-5"
}

func IsSonnet55(model string) bool {
	return normalize55Model(model) == "claude-sonnet-5-5"
}

func IsHaiku55(model string) bool {
	return normalize55Model(model) == "claude-haiku-5-5"
}

// IsAdaptiveThinkingModel identifies Claude 5.5 models that require the
// adaptive-thinking request and Responses conversion rules.
func IsAdaptiveThinkingModel(model string) bool {
	return IsOpus55(model) || IsSonnet55(model) || IsHaiku55(model)
}
