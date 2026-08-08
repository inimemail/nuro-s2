package service

import (
	"bytes"
	"sort"

	"github.com/tidwall/gjson"
)

const (
	openAIResponsesToolSchemaMaxDepth     = 4
	openAIResponsesToolSchemaFallbackType = `"object"`
	openAIResponsesToolSchemaNullLiteral  = "null"
)

type openAIResponsesToolSchemaNullType struct{ offset, length int }

// sanitizeOpenAIResponsesToolParameterTypes performs one bounded copy of the
// request body and only replaces explicit tools[].parameters.type = null.
func sanitizeOpenAIResponsesToolParameterTypes(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	// Avoid a full JSON walk for the overwhelmingly common valid-schema path.
	// Whitespace and nesting variations are handled by gjson once a candidate
	// type/null pair is present somewhere in the body.
	if !bytes.Contains(body, []byte(`"type"`)) || !bytes.Contains(body, []byte("null")) {
		return body, false, nil
	}
	hits := make([]openAIResponsesToolSchemaNullType, 0, 2)
	collectOpenAIResponsesToolSchemaNullTypes(body, gjson.GetBytes(body, "tools"), 0, &hits)
	if input := gjson.GetBytes(body, "input"); input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			if item.IsObject() {
				collectOpenAIResponsesToolSchemaNullTypes(body, item.Get("tools"), 0, &hits)
			}
			return true
		})
	}
	if len(hits) == 0 {
		return body, false, nil
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].offset < hits[j].offset })
	sanitized := make([]byte, 0, len(body)+len(hits)*len(openAIResponsesToolSchemaFallbackType))
	cursor := 0
	for _, hit := range hits {
		if hit.offset < cursor || hit.offset+hit.length > len(body) {
			continue
		}
		sanitized = append(sanitized, body[cursor:hit.offset]...)
		sanitized = append(sanitized, openAIResponsesToolSchemaFallbackType...)
		cursor = hit.offset + hit.length
	}
	sanitized = append(sanitized, body[cursor:]...)
	return sanitized, true, nil
}

func collectOpenAIResponsesToolSchemaNullTypes(body []byte, tools gjson.Result, depth int, hits *[]openAIResponsesToolSchemaNullType) {
	if depth > openAIResponsesToolSchemaMaxDepth || !tools.IsArray() {
		return
	}
	tools.ForEach(func(_, tool gjson.Result) bool {
		if !tool.IsObject() {
			return true
		}
		for _, suffix := range []string{"parameters", "function.parameters"} {
			params := tool.Get(suffix)
			if !params.IsObject() {
				continue
			}
			typ := params.Get("type")
			if typ.Type == gjson.Null && typ.Raw == openAIResponsesToolSchemaNullLiteral {
				end := typ.Index + len(typ.Raw)
				if typ.Index > 0 && end <= len(body) && bytes.Equal(body[typ.Index:end], []byte(typ.Raw)) {
					*hits = append(*hits, openAIResponsesToolSchemaNullType{offset: typ.Index, length: len(typ.Raw)})
				}
			}
		}
		collectOpenAIResponsesToolSchemaNullTypes(body, tool.Get("tools"), depth+1, hits)
		return true
	})
}
