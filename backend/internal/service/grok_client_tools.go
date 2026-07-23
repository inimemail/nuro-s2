package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
)

const grokClientToolMappingKey = "grok_client_tool_mapping"
const grokToolSearchProxyName = "__sub2api_tool_search"

type grokClientToolMapping struct {
	custom map[string]bool
	search bool
}

func clearGrokClientToolMapping(c *gin.Context) {
	if c != nil {
		c.Set(grokClientToolMappingKey, grokClientToolMapping{})
	}
}

func adaptGrokClientTools(c *gin.Context, body []byte) ([]byte, grokClientToolMapping, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, grokClientToolMapping{}, fmt.Errorf("decode Grok client tools: %w", err)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) == 0 {
		return body, grokClientToolMapping{}, nil
	}
	mapping := grokClientToolMapping{custom: map[string]bool{}}
	seenCustom := map[string]bool{}
	seenFunction := map[string]bool{}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := strings.TrimSpace(stringValue(tool["type"]))
		name := strings.TrimSpace(stringValue(tool["name"]))
		if typ == "function" && name != "" {
			seenFunction[name] = true
		}
	}
	lowered := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			lowered = append(lowered, raw)
			continue
		}
		typ, name := strings.TrimSpace(stringValue(tool["type"])), strings.TrimSpace(stringValue(tool["name"]))
		switch typ {
		case "custom":
			if name == "" || seenCustom[name] || seenFunction[name] {
				return body, grokClientToolMapping{}, fmt.Errorf("invalid or duplicate custom tool")
			}
			if name == grokToolSearchProxyName {
				return body, grokClientToolMapping{}, fmt.Errorf("custom tool name is reserved")
			}
			seenCustom[name] = true
			mapping.custom[name] = true
			copy := map[string]any{}
			for k, v := range tool {
				copy[k] = v
			}
			copy["type"] = "function"
			copy["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}}
			delete(copy, "format")
			lowered = append(lowered, copy)
		case "tool_search":
			if mapping.search {
				continue
			}
			mapping.search = true
			lowered = append(lowered, map[string]any{"type": "function", "name": grokToolSearchProxyName, "description": "Search available tools", "parameters": map[string]any{"type": "object"}})
		default:
			lowered = append(lowered, raw)
		}
	}
	payload["tools"] = lowered
	rewriteGrokClientToolHistory(payload["input"], mapping)
	if choice, ok := payload["tool_choice"].(map[string]any); ok {
		switch strings.TrimSpace(stringValue(choice["type"])) {
		case "custom":
			if mapping.custom[strings.TrimSpace(stringValue(choice["name"]))] {
				choice["type"] = "function"
			}
		case "tool_search":
			if mapping.search {
				payload["tool_choice"] = map[string]any{"type": "function", "name": grokToolSearchProxyName}
			}
		}
	}
	if c != nil && (len(mapping.custom) > 0 || mapping.search) {
		c.Set(grokClientToolMappingKey, mapping)
	}
	encoded, err := json.Marshal(payload)
	return encoded, mapping, err
}

func rewriteGrokClientToolHistory(value any, mapping grokClientToolMapping) {
	switch node := value.(type) {
	case []any:
		for _, child := range node {
			rewriteGrokClientToolHistory(child, mapping)
		}
	case map[string]any:
		typ := strings.TrimSpace(stringValue(node["type"]))
		if typ == "custom_tool_call" && mapping.custom[strings.TrimSpace(stringValue(node["name"]))] {
			node["type"] = "function_call"
			node["arguments"] = `{"input":` + mustGrokJSONString(stringValue(node["input"])) + `}`
			delete(node, "input")
		} else if typ == "custom_tool_call_output" || typ == "tool_search_output" {
			node["type"] = "function_call_output"
			normalizeGrokToolOutput(node)
		} else if typ == "tool_search_call" && mapping.search {
			node["type"] = "function_call"
			node["name"] = grokToolSearchProxyName
			if _, ok := node["arguments"]; !ok {
				node["arguments"] = "{}"
			}
		}
		for _, child := range node {
			rewriteGrokClientToolHistory(child, mapping)
		}
	}
}

func normalizeGrokToolOutput(node map[string]any) {
	if value, ok := node["output"]; ok {
		if text, ok := value.(string); ok {
			_ = text
		} else if encoded, err := json.Marshal(value); err == nil {
			node["output"] = string(encoded)
		} else {
			node["output"] = ""
		}
	}
}

func restoreGrokClientToolPayload(c *gin.Context, body []byte) ([]byte, error) {
	if c == nil {
		return body, nil
	}
	value, ok := c.Get(grokClientToolMappingKey)
	if !ok {
		return body, nil
	}
	mapping, ok := value.(grokClientToolMapping)
	if !ok || (len(mapping.custom) == 0 && !mapping.search) {
		return body, nil
	}
	// Most SSE frames are response metadata, text deltas, usage, or terminal
	// events and cannot contain a lowered client-tool call. Avoid a JSON
	// round-trip on those frames, especially before the first real token.
	if !bytes.Contains(body, []byte(`"function_call"`)) &&
		!bytes.Contains(body, []byte(`"function_call_output"`)) &&
		!bytes.Contains(body, []byte(`"custom_tool_call"`)) &&
		!bytes.Contains(body, []byte(`"tool_search"`)) {
		return body, nil
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}
	restoreGrokClientToolValue(payload, mapping)
	return json.Marshal(payload)
}

func restoreGrokClientToolValue(value any, mapping grokClientToolMapping) {
	switch node := value.(type) {
	case []any:
		for _, child := range node {
			restoreGrokClientToolValue(child, mapping)
		}
	case map[string]any:
		if strings.TrimSpace(stringValue(node["type"])) == "function_call" {
			name := strings.TrimSpace(stringValue(node["name"]))
			if mapping.custom[name] {
				node["type"] = "custom_tool_call"
				node["input"] = extractGrokCustomInput(stringValue(node["arguments"]))
				delete(node, "arguments")
			} else if mapping.search && name == grokToolSearchProxyName {
				node["type"] = "tool_search_call"
				node["execution"] = "client"
				delete(node, "name")
			}
		}
		for _, child := range node {
			restoreGrokClientToolValue(child, mapping)
		}
	}
}

func extractGrokCustomInput(arguments string) string {
	var value map[string]any
	if json.Unmarshal([]byte(arguments), &value) == nil {
		if input, ok := value["input"].(string); ok {
			return input
		}
	}
	return arguments
}

func mustGrokJSONString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
