package service

import (
	"bytes"
	"github.com/tidwall/gjson"
	"sort"
)

// Normalize only schema-keyword required:null, not properties named "required" or tool data.
// [] is the standard equivalent of an omitted required list. A single bounded copy
// preserves all unrelated request bytes, including signed thinking and prompt prefixes.
func sanitizeToolSchemaRequired(body []byte) ([]byte, bool) {
	if !bytes.Contains(body, []byte(`"required"`)) || !bytes.Contains(body, []byte("null")) {
		return body, false
	}
	var offsets []int
	var schema func(gjson.Result, int)
	schema = func(node gjson.Result, depth int) {
		if depth > 64 || !node.IsObject() {
			return
		}
		required := node.Get("required")
		if required.Type == gjson.Null && required.Raw == "null" && required.Index > 0 {
			offsets = append(offsets, required.Index)
		}
		for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
			node.Get(key).ForEach(func(_, child gjson.Result) bool { schema(child, depth+1); return true })
		}
		for _, key := range []string{"items", "additionalProperties", "additionalItems", "contains", "not", "if", "then", "else", "propertyNames", "unevaluatedProperties", "unevaluatedItems", "contentSchema"} {
			child := node.Get(key)
			if child.IsArray() {
				child.ForEach(func(_, item gjson.Result) bool { schema(item, depth+1); return true })
			} else {
				schema(child, depth+1)
			}
		}
		for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
			node.Get(key).ForEach(func(_, child gjson.Result) bool { schema(child, depth+1); return true })
		}
	}
	var tools func(gjson.Result, int)
	tools = func(list gjson.Result, depth int) {
		if depth > 4 || !list.IsArray() {
			return
		}
		list.ForEach(func(_, tool gjson.Result) bool {
			for _, key := range []string{"parameters", "function.parameters", "input_schema", "contentSchema"} {
				schema(tool.Get(key), 0)
			}
			tools(tool.Get("tools"), depth+1)
			return true
		})
	}
	root := gjson.ParseBytes(body)
	tools(root.Get("tools"), 0)
	root.Get("input").ForEach(func(_, item gjson.Result) bool { tools(item.Get("tools"), 0); return true })
	if len(offsets) == 0 {
		return body, false
	}
	sort.Ints(offsets)
	out := make([]byte, 0, len(body))
	cursor := 0
	for _, offset := range offsets {
		if offset < cursor || offset+4 > len(body) {
			continue
		}
		out = append(out, body[cursor:offset]...)
		out = append(out, '[', ']')
		cursor = offset + 4
	}
	return append(out, body[cursor:]...), true
}
