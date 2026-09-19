package antigravity

import "strings"

// Only Claude Code's leading transport attribution is removed. The same text
// inside user instructions is meaningful content and must remain untouched.
func stripLeadingClaudeAttribution(text string) string {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if !strings.HasPrefix(trimmed, "x-anthropic-billing-header:") {
		return text
	}
	if index := strings.IndexByte(trimmed, '\n'); index >= 0 {
		return trimmed[index+1:]
	}
	return ""
}
