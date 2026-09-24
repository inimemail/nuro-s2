package antigravity

import (
	"fmt"
	"reflect"
	"strings"
)

// NormalizeCompatibleSchema rejects constraints Gemini cannot represent rather
// than selecting one tuple member and silently widening the tool contract.
func NormalizeCompatibleSchema(schema map[string]any) error {
	budget := 10000
	if err := validateCompatibleSchemaRefs(schema, schema, nil, 0, &budget); err != nil {
		return err
	}
	budget = 10000
	return normalizeCompatibleSchema(schema, 0, &budget)
}
func normalizeCompatibleSchema(schema map[string]any, depth int, budget *int) error {
	if schema == nil {
		return nil
	}
	*budget--
	if depth > 64 || *budget < 0 {
		return fmt.Errorf("tool schema exceeds compatibility limits")
	}
	if constant, exists := schema["const"]; exists {
		switch constant.(type) {
		case string, bool, float64, int:
		default:
			return fmt.Errorf("tool schema const requires a scalar value supported by Gemini")
		}
		if values, ok := schema["enum"].([]any); ok {
			found := false
			for _, value := range values {
				if reflect.DeepEqual(value, constant) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("tool schema const contradicts enum")
			}
		}
		schema["enum"] = []any{constant}
		delete(schema, "const")
	}
	tuple, prefix := schema["prefixItems"].([]any)
	if raw, present := schema["prefixItems"]; present && raw != nil && !prefix {
		return fmt.Errorf("prefixItems must be an array")
	}
	if legacy, ok := schema["items"].([]any); ok {
		if prefix {
			return fmt.Errorf("cannot combine prefixItems and tuple items")
		}
		tuple = legacy
	}
	if tuple != nil {
		if len(tuple) == 0 {
			return fmt.Errorf("empty tuple schemas are not supported by Gemini")
		}
		first, ok := tuple[0].(map[string]any)
		if !ok {
			return fmt.Errorf("tuple items must be schemas")
		}
		for _, item := range tuple[1:] {
			if !reflect.DeepEqual(first, item) {
				return fmt.Errorf("heterogeneous tuple schemas are not supported by Gemini")
			}
		}
		tailKey := "additionalItems"
		if prefix {
			tailKey = "items"
		}
		tail, specified := schema[tailKey]
		if !specified {
			return fmt.Errorf("tuple with an unconstrained tail cannot be represented by Gemini")
		}
		if tail == false {
			if max, ok := schema["maxItems"].(float64); !ok || max > float64(len(tuple)) {
				schema["maxItems"] = len(tuple)
			}
		} else if !reflect.DeepEqual(tail, first) {
			return fmt.Errorf("tuple tail differs from its item schema")
		}
		schema["items"] = first
		delete(schema, "prefixItems")
		delete(schema, "additionalItems")
	}
	if raw, exists := schema["items"]; exists {
		child, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("array items must be a schema object")
		}
		if err := normalizeCompatibleSchema(child, depth+1, budget); err != nil {
			return err
		}
	} else if schema["type"] == "array" {
		return fmt.Errorf("array schema requires items")
	}
	// Visit only schema positions, never examples/defaults or user field names.
	for _, key := range []string{"properties", "$defs", "definitions", "patternProperties"} {
		if children, ok := schema[key].(map[string]any); ok {
			for _, child := range children {
				if m, ok := child.(map[string]any); ok {
					if err := normalizeCompatibleSchema(m, depth+1, budget); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if children, ok := schema[key].([]any); ok {
			for _, child := range children {
				if m, ok := child.(map[string]any); ok {
					if err := normalizeCompatibleSchema(m, depth+1, budget); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, key := range []string{"additionalProperties", "not"} {
		if child, ok := schema[key].(map[string]any); ok {
			if err := normalizeCompatibleSchema(child, depth+1, budget); err != nil {
				return err
			}
		}
	}
	return nil
}

// Bound the actual expansion cost before the legacy cleaner copies references.
func validateCompatibleSchemaRefs(node any, root map[string]any, path map[string]bool, depth int, budget *int) error {
	*budget--
	if depth > 64 || *budget < 0 {
		return fmt.Errorf("tool schema reference expansion exceeds compatibility limits")
	}
	switch value := node.(type) {
	case map[string]any:
		if ref, ok := value["$ref"].(string); ok {
			if path[ref] {
				return fmt.Errorf("recursive tool schema references are not supported by Gemini")
			}
			var target any
			for _, prefix := range []string{"#/$defs/", "#/definitions/"} {
				if strings.HasPrefix(ref, prefix) {
					defs, _ := root[strings.TrimSuffix(strings.TrimPrefix(prefix, "#/"), "/")].(map[string]any)
					target = defs[strings.TrimPrefix(ref, prefix)]
				}
			}
			if target == nil {
				return fmt.Errorf("unsupported or unresolved tool schema reference %q", ref)
			}
			next := make(map[string]bool, len(path)+1)
			for k, v := range path {
				next[k] = v
			}
			next[ref] = true
			if err := validateCompatibleSchemaRefs(target, root, next, depth+1, budget); err != nil {
				return err
			}
		}
		for _, child := range compatibleSchemaChildren(value) {
			if err := validateCompatibleSchemaRefs(child, root, path, depth+1, budget); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := validateCompatibleSchemaRefs(child, root, path, depth+1, budget); err != nil {
				return err
			}
		}
	}
	return nil
}

func compatibleSchemaChildren(schema map[string]any) []any {
	var children []any
	for _, key := range []string{"properties", "$defs", "definitions", "patternProperties", "dependentSchemas"} {
		if values, ok := schema[key].(map[string]any); ok {
			for _, child := range values {
				children = append(children, child)
			}
		}
	}
	for _, key := range []string{"items", "prefixItems", "additionalItems", "additionalProperties", "anyOf", "oneOf", "allOf", "not", "if", "then", "else", "contains"} {
		if child, ok := schema[key]; ok {
			children = append(children, child)
		}
	}
	return children
}
