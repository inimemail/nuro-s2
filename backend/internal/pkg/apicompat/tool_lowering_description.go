package apicompat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// A function-only upstream cannot enforce a custom tool grammar. Retain it as
// input guidance instead of silently discarding the caller's format contract.
func loweredCustomToolDescription(description string, format json.RawMessage) string {
	const guidance = `This is a free-form tool exposed through a function adapter. Invoke it using structured tool calling with a JSON object whose "input" field contains the complete raw tool input as a string. Do not print a tool invocation in assistant text.`
	parts := []string{description}
	// Inherited declarations can already carry the lowered description.
	if !strings.Contains(description, guidance) {
		parts = append(parts, guidance)
	}
	var grammar struct {
		Type       string `json:"type"`
		Syntax     string `json:"syntax"`
		Definition string `json:"definition"`
	}
	if json.Unmarshal(format, &grammar) == nil && grammar.Type == "grammar" && grammar.Definition != "" {
		instruction := fmt.Sprintf("The raw input must follow this %s grammar:\n%s", grammar.Syntax, grammar.Definition)
		if !strings.HasSuffix(description, instruction) {
			parts = append(parts, instruction)
		}
	}
	return joinToolDescriptions(parts...)
}

func loweredNamespaceToolDescription(namespace, name, namespaceDescription, description string) string {
	prefix := fmt.Sprintf("Tool %s.%s, exposed as %s for structured tool calling.", namespace, name, flattenNamespaceToolName(namespace, name))
	if namespaceDescription == "" && strings.HasPrefix(description, prefix) {
		return description
	}
	return joinToolDescriptions(
		prefix,
		namespaceDescription,
		description,
	)
}

func joinToolDescriptions(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "\n\n")
}
