package service

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applyGeminiChatResponseFormat 把 Chat Completions 的 response_format 写入 Gemini generationConfig：
// json_object 写 responseMimeType=application/json；json_schema 另把 json_schema.schema 原样写入
// responseJsonSchema（JSON Schema 形态，不转换为 OpenAPI 子集的 responseSchema）。
// text、缺省或无法解析的 response_format 不改请求。
// Gemini 2.x 的 tools + JSON 组合由入口拒绝；Gemini 3/4 保留 tools。
// 以下情况不写：
// upstreamModel 为图片输出模型（输出含图片，与 JSON responseMimeType 冲突）。
func applyGeminiChatResponseFormat(geminiReq []byte, responseFormat json.RawMessage, upstreamModel string) []byte {
	if len(responseFormat) == 0 || !gjson.ValidBytes(responseFormat) || isGeminiImageOutputModel(upstreamModel) {
		return geminiReq
	}
	format := gjson.ParseBytes(responseFormat)
	if !format.IsObject() {
		return geminiReq
	}

	var schema gjson.Result
	switch format.Get("type").String() {
	case "json_object":
	case "json_schema":
		schema = format.Get("json_schema.schema")
	default:
		return geminiReq
	}

	if tools := gjson.GetBytes(geminiReq, "tools"); tools.IsArray() && len(tools.Array()) > 0 && !geminiStructuredOutputToolsSupported(upstreamModel) {
		return geminiReq
	}

	out, err := sjson.SetBytes(geminiReq, "generationConfig.responseMimeType", "application/json")
	if err != nil {
		return geminiReq
	}
	if schema.IsObject() {
		withSchema, err := sjson.SetRawBytes(out, "generationConfig.responseJsonSchema", []byte(schema.Raw))
		if err != nil {
			return geminiReq
		}
		out = withSchema
	}
	return out
}

func validateGeminiChatResponseFormat(format json.RawMessage, model string) error {
	if len(format) == 0 {
		return nil
	}
	if !gjson.ValidBytes(format) || !gjson.ParseBytes(format).IsObject() {
		return errors.New("invalid response_format")
	}
	switch gjson.GetBytes(format, "type").String() {
	case "text":
		return nil
	case "json_object":
	case "json_schema":
		if !gjson.GetBytes(format, "json_schema.schema").IsObject() {
			return errors.New("response_format.json_schema.schema must be an object")
		}
	default:
		return errors.New("unsupported response_format type")
	}
	if isGeminiImageOutputModel(model) {
		return errors.New("structured output is not supported for image models")
	}
	return nil
}

func geminiStructuredOutputToolsSupported(model string) bool {
	name := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "models/")
	return strings.HasPrefix(name, "gemini-3") || strings.HasPrefix(name, "gemini-4")
}
