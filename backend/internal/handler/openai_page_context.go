package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var codexSelectedPageName = regexp.MustCompile(`^codex_apps_selected_page_part_([1-9][0-9]*)_of_([1-9][0-9]*)$`)

// Desktop injects page snapshots as standalone outputs, without a model tool call.
// Keep them as untrusted user text, including on history replay and mixed bootstrap turns.
func normalizeCodexPageContext(body []byte) ([]byte, bool) {
	if (!bytes.Contains(body, []byte("codex_apps_open_page")) && !bytes.Contains(body, []byte("codex_apps_selected_page_part_"))) || !hasUniqueJSONMembers(body) {
		return body, false
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return body, false
	}
	var input []json.RawMessage
	if json.Unmarshal(request["input"], &input) != nil {
		return body, false
	}
	changed := false
	var multipartValidated map[int]bool
	for i, raw := range input {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		readString := func(key string) string {
			var value string
			_ = json.Unmarshal(item[key], &value)
			return value
		}
		if readString("type") != "function_call_output" || readString("namespace") != "agent" {
			continue
		}
		if callID, exists := item["call_id"]; exists {
			var value string
			if bytes.Equal(bytes.TrimSpace(callID), []byte("null")) || json.Unmarshal(callID, &value) != nil || strings.TrimSpace(value) != "" {
				continue
			}
		}
		name, output := readString("name"), readString("output")
		if !validCodexPageContext(name, output) {
			continue
		}
		if parts := codexSelectedPageName.FindStringSubmatch(name); len(parts) == 3 && parts[2] != "1" && !multipartValidated[i] {
			if parts[1] != "1" || !completeCodexPageSnapshot(input, i, parts) {
				continue
			}
			if multipartValidated == nil {
				multipartValidated = make(map[int]bool)
			}
			total, _ := strconv.Atoi(parts[2])
			for n := 0; n < total; n++ {
				multipartValidated[i+n] = true
			}
		}
		message := map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]string{{
				"type": "input_text",
				"text": "<external_" + name + ">\n" + output + "\n</external_" + name + ">",
			}},
		}
		encoded, err := json.Marshal(message)
		if err != nil {
			return body, false
		}
		input[i] = encoded
		changed = true
	}
	if !changed {
		return body, false
	}
	encodedInput, err := json.Marshal(input)
	if err != nil {
		return body, false
	}
	request["input"] = encodedInput
	encoded, err := json.Marshal(request)
	if err != nil {
		return body, false
	}
	return encoded, true
}

// Validate the entire contiguous multipart snapshot before converting any part.
// A missing/reordered part must remain a normal invalid tool output.
func completeCodexPageSnapshot(input []json.RawMessage, index int, parts []string) bool {
	part, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	total, err := strconv.Atoi(parts[2])
	if err != nil || total > len(input) || part > index+1 {
		return false
	}
	start := index - part + 1
	if start < 0 || total > len(input)-start {
		return false
	}
	var snapshot strings.Builder
	for n := 1; n <= total; n++ {
		var item map[string]json.RawMessage
		if json.Unmarshal(input[start+n-1], &item) != nil {
			return false
		}
		var kind, namespace, name, output string
		if json.Unmarshal(item["type"], &kind) != nil || kind != "function_call_output" ||
			json.Unmarshal(item["namespace"], &namespace) != nil || namespace != "agent" ||
			json.Unmarshal(item["name"], &name) != nil || name != fmt.Sprintf("codex_apps_selected_page_part_%d_of_%d", n, total) ||
			json.Unmarshal(item["output"], &output) != nil || !validCodexPageContext(name, output) {
			return false
		}
		if raw, exists := item["call_id"]; exists {
			var callID string
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &callID) != nil || strings.TrimSpace(callID) != "" {
				return false
			}
		}
		_, chunk, _ := strings.Cut(output, "\n")
		_, _ = snapshot.WriteString(chunk)
	}
	return validCodexPageSnapshot(snapshot.String())
}

func validCodexPageContext(name, output string) bool {
	if name == "codex_apps_open_page" {
		return validCodexPageSnapshot(output)
	}
	parts := codexSelectedPageName.FindStringSubmatch(name)
	if len(parts) != 3 {
		return false
	}
	part, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	total, err := strconv.Atoi(parts[2])
	if err != nil || part > total {
		return false
	}
	header, snapshot, ok := strings.Cut(output, "\n")
	if !ok || strings.TrimSuffix(header, "\r") != fmt.Sprintf("Companion Page snapshot part %d of %d:", part, total) || strings.TrimSpace(snapshot) == "" {
		return false
	}
	// A multi-part snapshot may split inside a JSON string; preserve each chunk verbatim.
	return total > 1 || validCodexPageSnapshot(snapshot)
}

func validCodexPageSnapshot(text string) bool {
	if !hasUniqueJSONMembers([]byte(text)) {
		return false
	}
	var snapshot map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &snapshot) != nil {
		return false
	}
	id, exists := snapshot["page_id"]
	if !exists {
		return false
	}
	if bytes.Equal(bytes.TrimSpace(id), []byte("null")) {
		return true
	}
	var value string
	return json.Unmarshal(id, &value) == nil && strings.TrimSpace(value) != ""
}
