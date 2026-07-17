package securityaudit

import (
	"encoding/json"
	"sort"
	"strings"
)

type auditTextSegment struct {
	role string
	text string
}

var auditMessageRoles = map[string]struct{}{
	"user": {}, "assistant": {}, "tool": {}, "model": {}, "system": {},
	"developer": {}, "function": {},
}

func extractStructuredAuditPrompt(protocol string, body []byte) (string, int) {
	if len(body) == 0 {
		return "", 0
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return "", 0
	}
	segments := make([]auditTextSegment, 0, 8)
	switch protocol {
	case "openai_chat_completions":
		collectAuditMessages(root["messages"], &segments)
	case "openai_responses":
		collectAuditResponses(root["input"], &segments)
	case "anthropic_messages":
		appendAuditSegment(&segments, "system", collectAuditContent(root["system"]))
		collectAuditMessages(root["messages"], &segments)
	case "gemini":
		collectAuditGemini(root["contents"], &segments)
	default:
		collectAuditResponses(root["input"], &segments)
		collectAuditMessages(root["messages"], &segments)
		collectAuditGemini(root["contents"], &segments)
	}
	return assembleAuditSegments(segments, MaxPromptRunes)
}

func collectAuditMessages(value any, segments *[]auditTextSegment) {
	items, ok := value.([]any)
	if !ok {
		return
	}
	for _, item := range items {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(stringField(message, "role")))
		if _, allowed := auditMessageRoles[role]; !allowed {
			continue
		}
		parts := collectAuditContent(message["content"])
		parts = append(parts, collectAuditContent(message["function_call"])...)
		parts = append(parts, collectAuditContent(message["tool_calls"])...)
		appendAuditSegment(segments, role, parts)
	}
}

func collectAuditResponses(value any, segments *[]auditTextSegment) {
	switch input := value.(type) {
	case string:
		appendAuditSegment(segments, "user", []string{input})
	case []any:
		for _, item := range input {
			collectAuditResponseItem(item, segments)
		}
	case map[string]any:
		collectAuditResponseItem(input, segments)
	}
}

func collectAuditResponseItem(value any, segments *[]auditTextSegment) {
	item, ok := value.(map[string]any)
	if !ok {
		return
	}
	role := strings.ToLower(strings.TrimSpace(stringField(item, "role")))
	if role == "" {
		switch strings.ToLower(strings.TrimSpace(stringField(item, "type"))) {
		case "function_call", "output_text", "assistant", "message":
			role = "assistant"
		case "function_call_output", "tool_result":
			role = "tool"
		default:
			role = "user"
		}
	}
	if _, allowed := auditMessageRoles[role]; !allowed {
		return
	}
	parts := collectAuditContent(item["content"])
	for _, key := range []string{"text", "arguments", "output", "result"} {
		parts = append(parts, collectAuditContent(item[key])...)
	}
	appendAuditSegment(segments, role, parts)
}

func collectAuditGemini(value any, segments *[]auditTextSegment) {
	items, ok := value.([]any)
	if !ok {
		return
	}
	for _, item := range items {
		content, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(stringField(content, "role")))
		if role == "" {
			role = "user"
		}
		if _, allowed := auditMessageRoles[role]; !allowed {
			continue
		}
		appendAuditSegment(segments, role, collectAuditContent(content["parts"]))
	}
}

func collectAuditContent(value any) []string {
	parts := make([]string, 0, 4)
	collectAuditContentInto(value, &parts, false)
	return parts
}

func collectAuditContentInto(value any, parts *[]string, argumentValue bool) {
	switch item := value.(type) {
	case string:
		item = strings.TrimSpace(item)
		if item != "" && !strings.Contains(strings.ToLower(item), ";base64,") {
			*parts = append(*parts, item)
		}
	case []any:
		for _, child := range item {
			collectAuditContentInto(child, parts, argumentValue)
		}
	case map[string]any:
		typ := strings.ToLower(strings.TrimSpace(stringField(item, "type")))
		if isAuditMediaType(typ) {
			return
		}
		if argumentValue {
			keys := make([]string, 0, len(item))
			for key := range item {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if !isAuditMediaKey(key) {
					collectAuditContentInto(item[key], parts, true)
				}
			}
			return
		}
		for _, key := range []string{
			"text", "content", "input_text", "output_text", "output", "result", "response",
			"function", "function_call", "functionCall", "function_response", "functionResponse", "tool_result",
		} {
			collectAuditContentInto(item[key], parts, false)
		}
		for _, key := range []string{"arguments", "input", "args"} {
			collectAuditContentInto(item[key], parts, true)
		}
	}
}

func isAuditMediaType(value string) bool {
	return strings.Contains(value, "image") || strings.Contains(value, "audio") ||
		strings.Contains(value, "video") || strings.Contains(value, "file")
}

func isAuditMediaKey(value string) bool {
	value = strings.ToLower(strings.ReplaceAll(value, "_", ""))
	return value == "data" || value == "base64" || value == "inlinedata" ||
		strings.Contains(value, "image") || strings.Contains(value, "audio") ||
		strings.Contains(value, "video") || strings.Contains(value, "file") || value == "mime" || value == "mimetype"
}

func appendAuditSegment(segments *[]auditTextSegment, role string, parts []string) {
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	if text != "" {
		*segments = append(*segments, auditTextSegment{role: role, text: text})
	}
}

func assembleAuditSegments(segments []auditTextSegment, limit int) (string, int) {
	if len(segments) == 0 || limit <= 0 {
		return "", 0
	}
	latestUser := -1
	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i].role == "user" {
			latestUser = i
			break
		}
	}
	ordered := make([]auditTextSegment, 0, len(segments))
	if latestUser >= 0 {
		ordered = append(ordered, segments[latestUser])
	}
	for i := len(segments) - 1; i >= 0; i-- {
		if i != latestUser {
			ordered = append(ordered, segments[i])
		}
	}

	remaining := limit
	parts := make([]string, 0, len(ordered))
	for _, segment := range ordered {
		runes := []rune(segment.text)
		if len(parts) > 0 {
			remaining--
		}
		if remaining <= 0 {
			break
		}
		if len(runes) > remaining {
			runes = runes[:remaining]
		}
		parts = append(parts, string(runes))
		remaining -= len(runes)
	}
	return strings.Join(parts, "\n"), len(segments)
}

func stringField(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return text
}
