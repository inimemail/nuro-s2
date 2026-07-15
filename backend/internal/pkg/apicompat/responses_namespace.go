package apicompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ResponsesNamespaceName identifies a function child in a Responses namespace.
type ResponsesNamespaceName = NamespacedToolName

// FlattenResponsesNamespacesExcept converts namespace function tools to flat
// function tools for HTTP upstreams that do not understand namespace. The
// preserved set is reserved for gateway-owned tools such as image_gen.
func FlattenResponsesNamespacesExcept(req map[string]any, preserved map[string]bool) (map[string]ResponsesNamespaceName, bool, error) {
	if req == nil {
		return nil, false, nil
	}
	toolLists := responsesNamespaceToolLists(req)
	if len(toolLists) == 0 {
		return nil, false, nil
	}

	topLevel := make(map[string]bool)
	for _, list := range toolLists {
		for _, raw := range list.tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, name := strings.TrimSpace(stringValue(tool["type"])), strings.TrimSpace(stringValue(tool["name"]))
			if (typ == "function" || typ == "custom") && name != "" {
				topLevel[name] = true
			}
		}
	}

	names := make(map[string]ResponsesNamespaceName)
	for _, list := range toolLists {
		for _, raw := range list.tools {
			tool, ok := raw.(map[string]any)
			if !ok || strings.TrimSpace(stringValue(tool["type"])) != "namespace" {
				continue
			}
			namespace := strings.TrimSpace(stringValue(tool["name"]))
			if namespace == "" || preserved[namespace] {
				continue
			}
			for _, rawChild := range namespaceChildren(tool) {
				child, ok := rawChild.(map[string]any)
				if !ok || strings.TrimSpace(stringValue(child["type"])) != "function" {
					continue
				}
				name := strings.TrimSpace(stringValue(child["name"]))
				if name == "" {
					continue
				}
				flat := flattenNamespaceToolName(namespace, name)
				entry := ResponsesNamespaceName{Namespace: namespace, Name: name}
				if topLevel[flat] {
					return nil, false, fmt.Errorf("namespace tool %q/%q conflicts with top-level tool %q", namespace, name, flat)
				}
				if prev, exists := names[flat]; exists && prev != entry {
					return nil, false, fmt.Errorf("namespace tools %q/%q and %q/%q conflict at %q", prev.Namespace, prev.Name, namespace, name, flat)
				}
				names[flat] = entry
			}
		}
	}
	if len(names) == 0 {
		return nil, false, nil
	}

	for _, list := range toolLists {
		flattened := make([]any, 0, len(list.tools)+len(names))
		seen := make(map[string]bool)
		for _, raw := range list.tools {
			tool, ok := raw.(map[string]any)
			if !ok || strings.TrimSpace(stringValue(tool["type"])) != "namespace" {
				flattened = append(flattened, raw)
				continue
			}
			namespace := strings.TrimSpace(stringValue(tool["name"]))
			if preserved[namespace] {
				flattened = append(flattened, raw)
				continue
			}
			for _, rawChild := range namespaceChildren(tool) {
				child, ok := rawChild.(map[string]any)
				if !ok {
					continue
				}
				name := strings.TrimSpace(stringValue(child["name"]))
				flat := flattenNamespaceToolName(namespace, name)
				if strings.TrimSpace(stringValue(child["type"])) != "function" || name == "" || seen[flat] {
					continue
				}
				seen[flat] = true
				flatChild := make(map[string]any, len(child)+1)
				for key, value := range child {
					flatChild[key] = value
				}
				flatChild["name"] = flat
				flattened = append(flattened, flatChild)
			}
		}
		list.container[list.key] = flattened
	}
	rewriteNamespaceQualifiedCalls(req["input"], names)
	if choice, ok := req["tool_choice"].(map[string]any); ok {
		if strings.TrimSpace(stringValue(choice["type"])) == "namespace" && !preserved[strings.TrimSpace(stringValue(choice["name"]))] {
			req["tool_choice"] = "auto"
		} else {
			rewriteNamespaceQualifiedCall(choice, names)
		}
	}
	return names, true, nil
}

type responsesNamespaceToolList struct {
	container map[string]any
	key       string
	tools     []any
}

func responsesNamespaceToolLists(req map[string]any) []responsesNamespaceToolList {
	lists := make([]responsesNamespaceToolList, 0, 2)
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		lists = append(lists, responsesNamespaceToolList{container: req, key: "tools", tools: tools})
	}
	input, _ := req["input"].([]any)
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(stringValue(item["type"])) != "additional_tools" {
			continue
		}
		if tools, ok := item["tools"].([]any); ok && len(tools) > 0 {
			lists = append(lists, responsesNamespaceToolList{container: item, key: "tools", tools: tools})
		}
	}
	return lists
}

func FlattenResponsesNamespaces(req map[string]any) (map[string]ResponsesNamespaceName, bool, error) {
	return FlattenResponsesNamespacesExcept(req, nil)
}

func RestoreResponsesNamespaceCalls(payload []byte, names map[string]ResponsesNamespaceName) ([]byte, bool, error) {
	if len(payload) == 0 || len(names) == 0 {
		return payload, false, nil
	}
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return payload, false, err
	}
	changed := restoreResponsesNamespaceValue(value, names)
	if !changed {
		return payload, false, nil
	}
	var rebuilt bytes.Buffer
	encoder := json.NewEncoder(&rebuilt)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return payload, false, err
	}
	return bytes.TrimSuffix(rebuilt.Bytes(), []byte("\n")), true, nil
}

func namespaceChildren(tool map[string]any) []any {
	if children, ok := tool["tools"].([]any); ok && len(children) > 0 {
		return children
	}
	children, _ := tool["children"].([]any)
	return children
}

func rewriteNamespaceQualifiedCalls(value any, names map[string]ResponsesNamespaceName) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			rewriteNamespaceQualifiedCalls(item, names)
		}
	case map[string]any:
		if strings.TrimSpace(stringValue(typed["type"])) == "function_call" {
			rewriteNamespaceQualifiedCall(typed, names)
		}
		for _, child := range typed {
			rewriteNamespaceQualifiedCalls(child, names)
		}
	}
}

func rewriteNamespaceQualifiedCall(item map[string]any, names map[string]ResponsesNamespaceName) bool {
	namespace := strings.TrimSpace(stringValue(item["namespace"]))
	name := strings.TrimSpace(stringValue(item["name"]))
	entry, ok := names[flattenNamespaceToolName(namespace, name)]
	if namespace == "" || name == "" || !ok || entry.Namespace != namespace || entry.Name != name {
		return false
	}
	item["name"] = flattenNamespaceToolName(namespace, name)
	delete(item, "namespace")
	return true
}

func restoreResponsesNamespaceValue(value any, names map[string]ResponsesNamespaceName) bool {
	changed := false
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			changed = restoreResponsesNamespaceValue(item, names) || changed
		}
	case map[string]any:
		if strings.TrimSpace(stringValue(typed["type"])) == "function_call" {
			if entry, ok := names[strings.TrimSpace(stringValue(typed["name"]))]; ok {
				typed["name"], typed["namespace"] = entry.Name, entry.Namespace
				changed = true
			}
		}
		for _, child := range typed {
			changed = restoreResponsesNamespaceValue(child, names) || changed
		}
	}
	return changed
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
