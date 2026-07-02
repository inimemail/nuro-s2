package service

import (
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
	for _, field := range []string{"max_output_tokens", "max_completion_tokens"} {
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
