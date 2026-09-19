package apicompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// Convert only root unions whose object semantics can be retained exactly.
// Incompatible branches are rejected rather than silently widening a tool's
// arguments (notably correlated required fields and closed allOf objects).
func normalizeAnthropicRootSchema(raw json.RawMessage) (json.RawMessage, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || root == nil {
		return raw, nil
	}
	union := false
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if _, ok := root[key]; ok {
			union = true
		}
	}
	if !union {
		return raw, nil
	}
	if len(raw) > 256*1024 {
		return nil, fmt.Errorf("tool root union schema exceeds size limit")
	}
	flat, err := flattenSafeRootSchema(root, 0)
	if err != nil {
		return nil, err
	}
	return json.Marshal(flat)
}

func schemaEqual(a, b json.RawMessage) bool {
	// UseNumber avoids loss of integer constraints above 2^53.
	var x, y any
	decode := func(raw []byte, out *any) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		return d.Decode(out)
	}
	e1 := decode(a, &x)
	e2 := decode(b, &y)
	return e1 == nil && e2 == nil && reflect.DeepEqual(x, y)
}

func flattenSafeRootSchema(root map[string]json.RawMessage, depth int) (map[string]json.RawMessage, error) {
	fail := func() (map[string]json.RawMessage, error) {
		return nil, fmt.Errorf("tool root union cannot be represented safely as an Anthropic object schema")
	}
	if depth > 16 {
		return fail()
	}
	var keyword string
	for _, k := range []string{"allOf", "anyOf", "oneOf"} {
		if _, ok := root[k]; ok {
			if keyword != "" {
				return fail()
			}
			keyword = k
		}
	}
	if keyword == "" {
		return root, nil
	}
	// Root siblings other than annotations/type need intersection semantics.
	for k, v := range root {
		if k == keyword || k == "title" || k == "description" {
			continue
		}
		if k != "type" || string(v) != `"object"` {
			return fail()
		}
	}
	var branches []map[string]json.RawMessage
	if json.Unmarshal(root[keyword], &branches) != nil || len(branches) == 0 || len(branches) > 64 {
		return fail()
	}
	props := make([]map[string]json.RawMessage, len(branches))
	required := make([][]string, len(branches))
	for i, branch := range branches {
		var err error
		branches[i], err = flattenSafeRootSchema(branch, depth+1)
		if err != nil {
			return nil, err
		}
		branch = branches[i]
		if branch == nil || (string(branch["type"]) != `"object"` && !(len(branch["type"]) == 0 && string(root["type"]) == `"object"`)) {
			return fail()
		}
		for k := range branch {
			if k != "type" && k != "properties" && k != "required" && k != "additionalProperties" && k != "description" && k != "title" {
				return fail()
			}
		}
		props[i] = map[string]json.RawMessage{}
		if v, ok := branch["properties"]; ok && json.Unmarshal(v, &props[i]) != nil {
			return fail()
		}
		if props[i] == nil {
			return fail()
		}
		if v, ok := branch["required"]; ok && json.Unmarshal(v, &required[i]) != nil {
			return fail()
		}
		sort.Strings(required[i])
	}
	merged := map[string]json.RawMessage{}
	out := map[string]json.RawMessage{"type": json.RawMessage(`"object"`)}
	for _, k := range []string{"title", "description"} {
		if v, ok := root[k]; ok {
			out[k] = v
		}
	}
	if keyword == "allOf" {
		req := map[string]bool{}
		for i, branch := range branches {
			if v, ok := branch["additionalProperties"]; ok && string(v) != "true" {
				return fail()
			}
			for k, v := range props[i] {
				if previous, ok := merged[k]; ok && !schemaEqual(previous, v) {
					merged[k], _ = json.Marshal(map[string][]json.RawMessage{"allOf": {previous, v}})
				} else {
					merged[k] = v
				}
			}
			for _, k := range required[i] {
				req[k] = true
			}
		}
		keys := make([]string, 0, len(req))
		for k := range req {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			out["required"], _ = json.Marshal(keys)
		}
	} else {
		// Equal object constraints and at most one varying property preserve
		// anyOf correlations. For oneOf the varying property must be required,
		// so nested oneOf has exactly the same branch cardinality.
		varying := ""
		for k, v := range props[0] {
			merged[k] = v
		}
		for i := 1; i < len(branches); i++ {
			if len(props[i]) != len(props[0]) || !reflect.DeepEqual(required[0], required[i]) {
				return fail()
			}
			a, aok := branches[0]["additionalProperties"]
			b, bok := branches[i]["additionalProperties"]
			if aok != bok || (aok && !schemaEqual(a, b)) {
				return fail()
			}
			for k, v := range props[0] {
				other, ok := props[i][k]
				if !ok {
					return fail()
				}
				if !schemaEqual(v, other) {
					if varying != "" && varying != k {
						return fail()
					}
					varying = k
				}
			}
		}
		if varying == "" && keyword == "oneOf" && len(branches) > 1 {
			return fail()
		}
		if varying != "" {
			if keyword == "oneOf" {
				found := false
				for _, k := range required[0] {
					if k == varying {
						found = true
					}
				}
				if !found {
					return fail()
				}
			}
			values := make([]json.RawMessage, len(branches))
			for i := range branches {
				values[i] = props[i][varying]
			}
			merged[varying], _ = json.Marshal(map[string][]json.RawMessage{keyword: values})
		}
		if len(required[0]) > 0 {
			out["required"], _ = json.Marshal(required[0])
		}
		if v, ok := branches[0]["additionalProperties"]; ok {
			out["additionalProperties"] = v
		}
	}
	out["properties"], _ = json.Marshal(merged)
	return out, nil
}
