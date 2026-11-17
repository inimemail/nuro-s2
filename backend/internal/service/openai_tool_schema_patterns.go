package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Python supports lookahead and fixed-width lookbehind. Only remove syntax
// belonging to other regex dialects; never rewrite instance-valued keywords.
func unsupportedPythonSchemaPattern(pattern string) bool {
	inClass := false
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			if i+1 >= len(pattern) {
				return true
			}
			i++
			if strings.ContainsRune("pPRKQEG", rune(pattern[i])) {
				return true
			}
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '(':
			if !inClass && i+2 < len(pattern) && pattern[i+1] == '?' {
				if pattern[i+2] == '\'' {
					return true
				}
				if pattern[i+2] == '<' && i+3 < len(pattern) && pattern[i+3] != '=' && pattern[i+3] != '!' {
					return true
				}
			}
		}
	}
	return false
}

func sanitizeOpenAIToolSchemaPatterns(body []byte) ([]byte, bool, error) {
	if !bytes.Contains(body, []byte("pattern")) {
		return body, false, nil
	}
	if len(body) > 256<<20 {
		return body, false, errors.New("tool schema body limit exceeded")
	}
	if !gjson.ValidBytes(body) {
		return body, false, errors.New("invalid tool schema JSON")
	}
	out := body
	changed := false
	nodes := 0
	var failure error
	var schemaWalk func(map[string]any, int) bool
	schemaWalk = func(schema map[string]any, depth int) bool {
		nodes++
		if depth > 64 || nodes > 8192 {
			failure = errors.New("tool schema complexity limit exceeded")
			return false
		}
		edited := false
		if pattern, ok := schema["pattern"].(string); ok && unsupportedPythonSchemaPattern(pattern) {
			delete(schema, "pattern")
			edited = true
		}
		if patterns, ok := schema["patternProperties"].(map[string]any); ok {
			dropped := false
			for pattern := range patterns {
				if unsupportedPythonSchemaPattern(pattern) {
					delete(patterns, pattern)
					dropped = true
					edited = true
				}
			}
			// A schema-valued additionalProperties is meaningful and must survive.
			if closed, ok := schema["additionalProperties"].(bool); dropped && ok && !closed {
				schema["additionalProperties"] = true
			}
		}
		for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
			if m, ok := schema[key].(map[string]any); ok {
				for _, v := range m {
					if child, ok := v.(map[string]any); ok {
						edited = schemaWalk(child, depth+1) || edited
					}
				}
			}
		}
		for _, key := range []string{"items", "additionalItems", "additionalProperties", "unevaluatedProperties", "unevaluatedItems", "contains", "propertyNames", "not", "if", "then", "else", "allOf", "anyOf", "oneOf", "prefixItems"} {
			switch value := schema[key].(type) {
			case map[string]any:
				edited = schemaWalk(value, depth+1) || edited
			case []any:
				for _, v := range value {
					if child, ok := v.(map[string]any); ok {
						edited = schemaWalk(child, depth+1) || edited
					}
				}
			}
		}
		return edited
	}
	var toolsWalk func(gjson.Result, string, int)
	toolsWalk = func(tools gjson.Result, path string, depth int) {
		if failure != nil || !tools.IsArray() {
			return
		}
		if depth > 4 {
			failure = errors.New("nested tool limit exceeded")
			return
		}
		tools.ForEach(func(index, tool gjson.Result) bool {
			prefix := path + "." + strconv.FormatInt(index.Int(), 10)
			for _, suffix := range []string{"parameters", "function.parameters", "input_schema", "contentSchema"} {
				raw := tool.Get(suffix)
				if !raw.IsObject() {
					continue
				}
				var schema map[string]any
				// Schema enum/default/constraint values must retain exact JSON
				// numbers when an unrelated regex requires re-encoding the schema.
				decoder := json.NewDecoder(strings.NewReader(raw.Raw))
				decoder.UseNumber()
				if err := decoder.Decode(&schema); err != nil {
					failure = err
					return false
				}
				if schemaWalk(schema, 0) {
					encoded, err := json.Marshal(schema)
					if err != nil {
						failure = err
						return false
					}
					out, err = sjson.SetRawBytes(out, prefix+"."+suffix, encoded)
					if err != nil {
						failure = err
						return false
					}
					changed = true
				}
				if failure != nil {
					return false
				}
			}
			toolsWalk(tool.Get("tools"), prefix+".tools", depth+1)
			return failure == nil
		})
	}
	toolsWalk(gjson.GetBytes(body, "tools"), "tools", 0)
	gjson.GetBytes(body, "input").ForEach(func(index, item gjson.Result) bool {
		toolsWalk(item.Get("tools"), "input."+strconv.FormatInt(index.Int(), 10)+".tools", 0)
		return failure == nil
	})
	if failure != nil {
		return body, false, failure
	}
	return out, changed, nil
}
