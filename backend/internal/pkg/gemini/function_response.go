package gemini

import (
	"strconv"
	"strings"
)

// Gemini 3 and later accept media inside functionResponse.parts. Gemini 2
// keeps the previous sibling-part representation.
func SupportsMultimodalFunctionResponses(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	if !strings.HasPrefix(model, "gemini-") {
		return false
	}
	rest := strings.TrimPrefix(model, "gemini-")
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	major, err := strconv.Atoi(rest[:end])
	return err == nil && major >= 3
}
