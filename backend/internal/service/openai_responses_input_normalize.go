package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func normalizeOpenAIResponsesStringInputMap(reqBody map[string]any) bool {
	if reqBody == nil {
		return false
	}
	input, ok := reqBody["input"].(string)
	if !ok {
		return false
	}
	reqBody["input"] = openAIResponsesInputListFromString(input)
	return true
}

func normalizeOpenAIResponsesStringInputBody(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || input.Type != gjson.String {
		return body, false, nil
	}
	normalized, err := sjson.SetBytes(body, "input", openAIResponsesInputListFromString(input.String()))
	if err != nil {
		return body, false, err
	}
	return normalized, true, nil
}

func normalizeOpenAIAPIKeyResponsesUnsupportedParamsBody(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	normalized := body
	changed := false
	maxTokens := gjson.GetBytes(normalized, "max_tokens")
	if maxTokens.Exists() {
		if !gjson.GetBytes(normalized, "max_output_tokens").Exists() {
			next, err := sjson.SetBytes(normalized, "max_output_tokens", maxTokens.Value())
			if err != nil {
				return body, false, err
			}
			normalized = next
		}
		next, err := sjson.DeleteBytes(normalized, "max_tokens")
		if err != nil {
			return body, false, err
		}
		normalized = next
		changed = true
	}
	for _, field := range []string{"max_completion_tokens"} {
		if !gjson.GetBytes(normalized, field).Exists() {
			continue
		}
		next, err := sjson.DeleteBytes(normalized, field)
		if err != nil {
			return body, false, err
		}
		normalized = next
		changed = true
	}
	return normalized, changed, nil
}

func normalizeOpenAIResponsesInputArgumentsBody(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false, nil
	}

	normalized := body
	changed := false
	for i, item := range input.Array() {
		if !openAIResponsesInputItemNeedsObjectArguments(item.Get("type").String()) {
			continue
		}
		args := item.Get("arguments")
		if args.Type != gjson.String {
			continue
		}

		argObject, ok := openAIResponsesArgumentsObjectFromString(args.String())
		if !ok {
			continue
		}

		next, err := sjson.SetBytes(normalized, fmt.Sprintf("input.%d.arguments", i), argObject)
		if err != nil {
			return body, false, err
		}
		normalized = next
		changed = true
	}
	return normalized, changed, nil
}

func normalizeOpenAIResponsesInputArgumentsMap(reqBody map[string]any) bool {
	if reqBody == nil {
		return false
	}
	input, ok := reqBody["input"].([]any)
	if !ok {
		return false
	}

	changed := false
	for _, item := range input {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := itemMap["type"].(string)
		if !openAIResponsesInputItemNeedsObjectArguments(itemType) {
			continue
		}
		args, ok := itemMap["arguments"].(string)
		if !ok {
			continue
		}
		argObject, ok := openAIResponsesArgumentsObjectFromString(args)
		if !ok {
			continue
		}
		itemMap["arguments"] = argObject
		changed = true
	}
	return changed
}

func openAIResponsesInputItemNeedsObjectArguments(itemType string) bool {
	return strings.TrimSpace(itemType) == "tool_search_call"
}

func openAIResponsesArgumentsObjectFromString(arguments string) (map[string]any, bool) {
	trimmed := strings.TrimSpace(arguments)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil, false
	}
	if parsed == nil {
		return nil, false
	}
	return parsed, true
}

func openAIResponsesInputListFromString(input string) []any {
	return []any{
		map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "input_text",
					"text": input,
				},
			},
		},
	}
}
