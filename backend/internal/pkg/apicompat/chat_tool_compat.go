package apicompat

import (
	"encoding/json"
	"fmt"
	"strings"
)

func (m ChatMessage) reasoningText() string {
	if m.ReasoningContent != "" {
		return m.ReasoningContent
	}
	return m.Reasoning
}

func (d ChatDelta) reasoningText() *string {
	if d.ReasoningContent != nil && *d.ReasoningContent != "" {
		return d.ReasoningContent
	}
	return d.Reasoning
}

func FunctionToolNames(tools []ResponsesTool) map[string]bool {
	names := make(map[string]bool)
	for _, tool := range tools {
		if tool.Type == "function" {
			names[tool.Name] = true
		}
	}
	return names
}

// Only restore aliases backed by declared tools. Real functions and namespace
// children retain ownership of their names; ambiguous aliases stay untouched.
func customToolCallName(name string, custom, functions map[string]bool, namespaces map[string]NamespacedToolName) (string, bool) {
	if functions[name] {
		return "", false
	}
	if custom[name] {
		return name, true
	}
	if _, exists := namespaces[name]; exists {
		return "", false
	}
	match := ""
	for candidate := range custom {
		for _, ns := range namespaces {
			if flattenNamespaceToolName(ns.Namespace, candidate) == name {
				if match != "" && match != candidate {
					return "", false
				}
				match = candidate
			}
		}
	}
	return match, match != ""
}

func firstFunctionTools(optional []map[string]bool) map[string]bool {
	if len(optional) > 0 {
		return optional[0]
	}
	return nil
}

// ValidateChatToolCalls rejects broken upstream calls before a buffered
// gateway commits HTTP 200. Declared custom/tool-search tools retain the text
// input compatibility already supported by the streaming converter.
func ValidateChatToolCalls(resp *ChatCompletionsResponse, custom map[string]bool, toolSearch bool, namespaces map[string]NamespacedToolName, functions ...map[string]bool) error {
	if resp == nil {
		return nil
	}
	for _, choice := range resp.Choices {
		for _, call := range choice.Message.ToolCalls {
			if toolSearch && call.Function.Name == toolSearchProxyName {
				continue
			}
			if _, ok := customToolCallName(call.Function.Name, custom, firstFunctionTools(functions), namespaces); ok {
				continue
			}
			args := strings.TrimSpace(call.Function.Arguments)
			if args != "" && !json.Valid([]byte(args)) {
				return fmt.Errorf("tool call %q (%s) arguments are invalid JSON", call.ID, call.Function.Name)
			}
		}
	}
	return nil
}
