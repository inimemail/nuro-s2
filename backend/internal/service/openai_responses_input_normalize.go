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
