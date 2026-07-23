package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const grokCompactSummaryPrompt = `Summarize the conversation so far for a successor assistant. Preserve the user's requests, important technical details, decisions, file paths, and the latest work. Be concise and return the summary inside a single <summary>...</summary> block.`

func buildGrokCompactRequestBody(body []byte) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode compact request: %w", err)
	}
	input, err := normalizeGrokCompactInput(payload["input"])
	if err != nil {
		return nil, err
	}
	input = append(input, map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": grokCompactSummaryPrompt}},
	})
	payload["input"] = input
	payload["include"] = []any{"reasoning.encrypted_content"}
	payload["store"] = false
	payload["stream"] = false
	payload["tool_choice"] = "none"
	return json.Marshal(payload)
}

func normalizeGrokCompactInput(value any) ([]any, error) {
	switch input := value.(type) {
	case nil:
		return []any{}, nil
	case []any:
		return input, nil
	case string:
		return []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": input}}}}, nil
	case map[string]any:
		return []any{input}, nil
	default:
		return nil, fmt.Errorf("compact input must be a string, object, or array")
	}
}

func convertOpenAICompactInputsForGrok(body []byte) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	items, ok := payload["input"].([]any)
	if !ok {
		return body, nil
	}
	converted := make([]any, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || !isOpenAICompactionType(stringValue(item["type"])) {
			converted = append(converted, raw)
			continue
		}
		if encrypted := strings.TrimSpace(stringValue(item["encrypted_content"])); encrypted != "" {
			converted = append(converted, map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": encrypted})
		}
		if summary := compactSummaryText(item["summary"]); summary != "" {
			converted = append(converted, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<conversation_summary>\n" + summary + "\n</conversation_summary>"}}})
		}
	}
	payload["input"] = converted
	return json.Marshal(payload)
}

func convertGrokResponseToOpenAICompact(body []byte) ([]byte, error) {
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	output, ok := response["output"].([]any)
	if !ok {
		return nil, fmt.Errorf("response has no output array")
	}
	var encrypted string
	var summaries []string
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch strings.TrimSpace(stringValue(item["type"])) {
		case "reasoning":
			encrypted = firstNonEmpty(strings.TrimSpace(stringValue(item["encrypted_content"])), encrypted)
		case "message":
			if content, ok := item["content"].([]any); ok {
				for _, partRaw := range content {
					if part, ok := partRaw.(map[string]any); ok {
						if text := strings.TrimSpace(stringValue(part["text"])); text != "" {
							summaries = append(summaries, text)
						}
					}
				}
			}
		}
	}
	if encrypted == "" {
		return nil, fmt.Errorf("response has no reasoning.encrypted_content")
	}
	compact := map[string]any{"id": "cmp_" + strings.ReplaceAll(uuid.NewString(), "-", ""), "type": "compaction", "status": "completed", "encrypted_content": encrypted}
	if summary := strings.TrimSpace(strings.Join(summaries, "\n")); summary != "" {
		compact["summary"] = []any{map[string]any{"type": "summary_text", "text": summary}}
	}
	response["output"] = []any{compact}
	response["status"] = "completed"
	delete(response, "output_text")
	return json.Marshal(response)
}

func compactSummaryText(value any) string {
	parts, ok := value.([]any)
	if !ok {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, raw := range parts {
		if part, ok := raw.(map[string]any); ok {
			if text := strings.TrimSpace(stringValue(part["text"])); text != "" {
				texts = append(texts, text)
			}
		}
	}
	return strings.Join(texts, "\n")
}

func isOpenAICompactionType(value string) bool {
	return value == "compaction" || value == "compaction_summary"
}
