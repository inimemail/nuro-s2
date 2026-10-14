package service

import (
	"encoding/json"
	"errors"
)

// Management requests accept only content for which countTokens can establish
// a price. Reject files and media instead of inventing their token count.
func validateGeminiCachePayload(fields map[string]json.RawMessage) error {
	var hasText bool
	content := func(raw json.RawMessage, system bool) error {
		var object struct {
			Role  string                       `json:"role"`
			Parts []map[string]json.RawMessage `json:"parts"`
		}
		if json.Unmarshal(raw, &object) != nil || len(object.Parts) == 0 || len(object.Parts) > 4096 || (object.Role != "" && object.Role != "user" && object.Role != "model" && !(system && object.Role == "system")) {
			return errors.New("invalid cached content parts or role")
		}
		for _, part := range object.Parts {
			var text string
			if len(part) != 1 || json.Unmarshal(part["text"], &text) != nil || text == "" {
				return errors.New("cached content quotation currently requires text parts")
			}
			hasText = true
		}
		return nil
	}
	if raw, ok := fields["contents"]; ok {
		var contents []json.RawMessage
		if json.Unmarshal(raw, &contents) != nil || len(contents) == 0 || len(contents) > 4096 {
			return errors.New("invalid cache contents")
		}
		for _, item := range contents {
			if err := content(item, false); err != nil {
				return err
			}
		}
	}
	if raw, ok := fields["systemInstruction"]; ok {
		if err := content(raw, true); err != nil {
			return err
		}
	}
	if !hasText {
		return errors.New("cached content is empty")
	}
	if raw, ok := fields["tools"]; ok {
		var tools []map[string]json.RawMessage
		if json.Unmarshal(raw, &tools) != nil || len(tools) > 128 || tools == nil {
			return errors.New("invalid cache tools")
		}
		for _, tool := range tools {
			var declarations []map[string]json.RawMessage
			if len(tool) != 1 || json.Unmarshal(tool["functionDeclarations"], &declarations) != nil || len(declarations) == 0 || len(declarations) > 128 {
				return errors.New("only function declarations can be cached")
			}
			for _, d := range declarations {
				var name string
				if json.Unmarshal(d["name"], &name) != nil || name == "" || len(name) > 64 {
					return errors.New("invalid cached function name")
				}
			}
		}
	}
	if raw, ok := fields["toolConfig"]; ok {
		var config map[string]json.RawMessage
		if json.Unmarshal(raw, &config) != nil || config == nil {
			return errors.New("invalid cache tool configuration")
		}
	}
	return nil
}
