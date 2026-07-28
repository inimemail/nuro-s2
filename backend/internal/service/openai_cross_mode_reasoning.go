package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// SanitizeOpenAICrossModeFailoverReasoning derives a retry-only body from the
// canonical request. Provider-specific encrypted reasoning items are removed
// as a whole when a passthrough attempt fails over to a non-passthrough
// account. The input slice is never mutated.
func SanitizeOpenAICrossModeFailoverReasoning(body []byte) ([]byte, bool, error) {
	if len(body) == 0 || !gjson.GetBytes(body, "input").Exists() {
		return body, false, nil
	}
	var decoded map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return body, false, fmt.Errorf("decode cross-mode failover body: %w", err)
	}
	if !dropOpenAIEncryptedReasoningInputItems(decoded) {
		return body, false, nil
	}
	out, err := json.Marshal(decoded)
	if err != nil {
		return body, false, fmt.Errorf("serialize cross-mode failover body: %w", err)
	}
	return out, true, nil
}

func dropOpenAIEncryptedReasoningInputItems(reqBody map[string]any) bool {
	input, ok := reqBody["input"]
	if !ok {
		return false
	}
	switch items := input.(type) {
	case []any:
		filtered := make([]any, 0, len(items))
		changed := false
		for _, item := range items {
			if isOpenAIEncryptedReasoningInputItem(item) {
				changed = true
				continue
			}
			filtered = append(filtered, item)
		}
		if !changed {
			return false
		}
		if len(filtered) == 0 {
			delete(reqBody, "input")
		} else {
			reqBody["input"] = filtered
		}
		return true
	case map[string]any:
		if !isOpenAIEncryptedReasoningInputItem(items) {
			return false
		}
		delete(reqBody, "input")
		return true
	default:
		return false
	}
}

func isOpenAIEncryptedReasoningInputItem(item any) bool {
	value, ok := item.(map[string]any)
	if !ok || !strings.EqualFold(strings.TrimSpace(crossModeStringValue(value["type"])), "reasoning") {
		return false
	}
	_, present := value["encrypted_content"]
	return present
}

func crossModeStringValue(value any) string {
	text, _ := value.(string)
	return text
}
