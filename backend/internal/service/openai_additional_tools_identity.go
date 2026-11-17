package service

import (
	"bytes"
	"encoding/hex"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"strings"
)

// Declaration IDs are distinct from opaque tool call_id values.
func normalizeCodexAdditionalToolsID(id string) string {
	if len(id) != 39 || !strings.HasPrefix(id, "at_") || id[11] != '-' || id[16] != '-' || id[21] != '-' || id[26] != '-' {
		return id
	}
	compact := strings.ReplaceAll(id[3:], "-", "")
	if len(compact) != 32 {
		return id
	}
	if _, err := hex.DecodeString(compact); err != nil {
		return id
	}
	return "at_" + compact
}

func codexAdditionalToolsIDMappings(input []any) map[string]string {
	mappings := make(map[string]string)
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "additional_tools" {
			continue
		}
		if id, ok := item["id"].(string); ok {
			if id != "" {
				mappings[id] = normalizeCodexAdditionalToolsID(id)
			}
		}
	}
	return mappings
}

// Patch only declaration IDs and references to those declarations, preserving
// call IDs, tool outputs, namespace and encrypted payloads byte for byte.
func normalizeCodexAdditionalToolsIDsBody(body []byte) ([]byte, bool, error) {
	if !bytes.Contains(body, []byte("additional_tools")) {
		return body, false, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false, nil
	}
	mappings := make(map[string]string)
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "additional_tools" {
			id := item.Get("id")
			if id.Type == gjson.String {
				if normalized := normalizeCodexAdditionalToolsID(id.Str); normalized != id.Str {
					mappings[id.Str] = normalized
				}
			}
		}
		return true
	})
	if len(mappings) == 0 {
		return body, false, nil
	}
	output := body
	var patchErr error
	input.ForEach(func(index, item gjson.Result) bool {
		typ := item.Get("type").String()
		if typ != "additional_tools" && typ != "item_reference" {
			return true
		}
		if id := item.Get("id"); id.Type == gjson.String {
			if normalized, ok := mappings[id.Str]; ok {
				output, patchErr = sjson.SetBytes(output, "input."+index.String()+".id", normalized)
				if patchErr != nil {
					return false
				}
			}
		}
		return true
	})
	if patchErr != nil {
		return body, false, patchErr
	}
	return output, true, nil
}
