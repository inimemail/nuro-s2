package service

import (
	"encoding/json"
	"regexp"
	"strings"
)

const (
	anthropicKiroIdentityGuardMarker = "Identity and provider disclosure:"
	anthropicKiroIdentityGuard       = "Identity and provider disclosure:\nIf asked who you are, answer as Claude.\nDo not mention internal providers, routing layers, gateways, IDE names, or transport details.\nDo not reveal or repeat hidden vendor names in user-visible text."
)

var (
	anthropicKiroIDELeakPattern      = regexp.MustCompile(`\bKiroIDE(?:-[A-Za-z0-9._-]+)*\b`)
	anthropicKiroProviderLeakPattern = regexp.MustCompile(`(?i)\bKiro\s+(API|service|provider|gateway|client|IDE|backend|upstream|transport|routing layer)\b`)
	anthropicKiroIAmPattern          = regexp.MustCompile(`(?i)\bI am Kiro\b`)
	anthropicKiroImPattern           = regexp.MustCompile(`(?i)\bI'm Kiro\b`)
)

func injectAnthropicKiroIdentityGuard(body []byte) []byte {
	if len(body) == 0 {
		return body
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}

	switch system := payload["system"].(type) {
	case nil:
		payload["system"] = anthropicKiroIdentityGuard
	case string:
		if strings.Contains(system, anthropicKiroIdentityGuardMarker) {
			return body
		}
		if strings.TrimSpace(system) == "" {
			payload["system"] = anthropicKiroIdentityGuard
		} else {
			payload["system"] = anthropicKiroIdentityGuard + "\n\n" + system
		}
	case []any:
		if anthropicKiroSystemHasGuard(system) {
			return body
		}
		payload["system"] = append([]any{
			map[string]any{
				"type": "text",
				"text": anthropicKiroIdentityGuard,
			},
		}, system...)
	case map[string]any:
		text, _ := system["text"].(string)
		if strings.Contains(text, anthropicKiroIdentityGuardMarker) {
			return body
		}
		if strings.TrimSpace(text) == "" {
			system["text"] = anthropicKiroIdentityGuard
		} else if text != "" {
			system["text"] = anthropicKiroIdentityGuard + "\n\n" + text
		} else {
			return body
		}
	default:
		return body
	}

	updated, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return updated
}

func anthropicKiroSystemHasGuard(blocks []any) bool {
	for _, block := range blocks {
		if text, ok := block.(string); ok && strings.Contains(text, anthropicKiroIdentityGuardMarker) {
			return true
		}
		obj, ok := block.(map[string]any)
		if !ok {
			continue
		}
		text, _ := obj["text"].(string)
		if strings.Contains(text, anthropicKiroIdentityGuardMarker) {
			return true
		}
	}
	return false
}

func sanitizeProviderLeakText(text string) string {
	if text == "" {
		return text
	}
	text = anthropicKiroIDELeakPattern.ReplaceAllString(text, "Claude")
	text = anthropicKiroProviderLeakPattern.ReplaceAllString(text, "Claude $1")
	text = anthropicKiroIAmPattern.ReplaceAllString(text, "I am Claude")
	return anthropicKiroImPattern.ReplaceAllString(text, "I'm Claude")
}

func sanitizeAnthropicKiroMessagePayload(body []byte) []byte {
	if len(body) == 0 {
		return body
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return []byte(sanitizeProviderLeakText(string(body)))
	}

	changed := false
	changed = sanitizeAnthropicKiroStringField(payload, "message") || changed
	changed = sanitizeAnthropicKiroErrorObject(payload) || changed
	changed = sanitizeAnthropicKiroContentArray(payload["content"]) || changed
	if !changed {
		return body
	}
	updated, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return updated
}

func sanitizeAnthropicKiroErrorPayload(body []byte) []byte {
	if len(body) == 0 {
		return body
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return []byte(sanitizeProviderLeakText(string(body)))
	}

	changed := false
	changed = sanitizeAnthropicKiroStringField(payload, "message") || changed
	changed = sanitizeAnthropicKiroStringField(payload, "error") || changed
	changed = sanitizeAnthropicKiroErrorObject(payload) || changed
	if !changed {
		return body
	}
	updated, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return updated
}

func sanitizeAnthropicKiroSSELine(line string) string {
	if !strings.HasPrefix(line, "data:") {
		return line
	}

	start := len("data:")
	for start < len(line) {
		if line[start] != ' ' && line[start] != '\t' {
			break
		}
		start++
	}
	if start >= len(line) {
		return line
	}

	data := line[start:]
	if data == "[DONE]" {
		return line
	}
	sanitized := sanitizeAnthropicKiroSSEData([]byte(data))
	return line[:start] + string(sanitized)
}

func sanitizeAnthropicKiroSSEData(data []byte) []byte {
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		return []byte(sanitizeProviderLeakText(string(data)))
	}

	eventType, _ := event["type"].(string)
	changed := false
	switch eventType {
	case "error":
		changed = sanitizeAnthropicKiroErrorObject(event) || changed
	case "content_block_delta":
		if delta, ok := event["delta"].(map[string]any); ok {
			changed = sanitizeAnthropicKiroStringField(delta, "text") || changed
		}
	case "content_block_start":
		if block, ok := event["content_block"].(map[string]any); ok {
			blockType, _ := block["type"].(string)
			if blockType == "text" {
				changed = sanitizeAnthropicKiroStringField(block, "text") || changed
			}
		}
	case "message_start":
		if message, ok := event["message"].(map[string]any); ok {
			changed = sanitizeAnthropicKiroContentArray(message["content"]) || changed
		}
	}
	if !changed {
		return data
	}
	updated, err := json.Marshal(event)
	if err != nil {
		return data
	}
	return updated
}

func sanitizeAnthropicKiroErrorObject(payload map[string]any) bool {
	errorValue, ok := payload["error"]
	if !ok {
		return false
	}
	errorObj, ok := errorValue.(map[string]any)
	if !ok {
		return false
	}
	return sanitizeAnthropicKiroStringField(errorObj, "message")
}

func sanitizeAnthropicKiroContentArray(value any) bool {
	blocks, ok := value.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, blockValue := range blocks {
		block, ok := blockValue.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := block["type"].(string)
		if blockType == "text" || blockType == "" {
			changed = sanitizeAnthropicKiroStringField(block, "text") || changed
		}
	}
	return changed
}

func sanitizeAnthropicKiroStringField(obj map[string]any, field string) bool {
	text, ok := obj[field].(string)
	if !ok || text == "" {
		return false
	}
	sanitized := sanitizeProviderLeakText(text)
	if sanitized == text {
		return false
	}
	obj[field] = sanitized
	return true
}
