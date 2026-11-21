package claude

import "strings"

// RejectsTemperature reports whether to omit temperature on a model probe.
func RejectsTemperature(model string) bool {
	if IsOpus55(model) || IsFable52(model) || IsMythos52(model) {
		return true
	}
	id := normalize52Model(model)
	for _, family := range []string{"claude-opus-4-7", "claude-opus-4-8"} {
		if id == family || strings.HasPrefix(id, family+"-") {
			return true
		}
	}
	return false
}
