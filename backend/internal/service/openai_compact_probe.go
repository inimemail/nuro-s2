package service

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	// AccountTestModeDefault drives the standard /responses connection test.
	AccountTestModeDefault = "default"
	// AccountTestModeCompact drives the native remote-compaction-v2 probe.
	AccountTestModeCompact       = "compact"
	AccountTestModeCompactLegacy = "compact_legacy"
)

func normalizeAccountTestMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case AccountTestModeCompact:
		return AccountTestModeCompact
	case AccountTestModeCompactLegacy:
		return AccountTestModeCompactLegacy
	default:
		return AccountTestModeDefault
	}
}

func createOpenAICompactProbePayload(model string, isOAuth bool) map[string]any {
	payload := map[string]any{
		"model":        strings.TrimSpace(model),
		"instructions": "You are a helpful coding assistant.",
		"input": []any{
			map[string]any{
				"type":    "message",
				"role":    "user",
				"content": "Respond with OK.",
			},
			map[string]any{"type": "compaction_trigger"},
		},
		"stream": true,
	}
	if isOAuth {
		payload["store"] = false
	}
	return payload
}

// HTTP success alone is insufficient for SSE: failures and disconnects after
// response headers must not poison an account's persisted compact capability.
func validateOpenAICompactProbeCompletion(body []byte) error {
	completed := false
	var probeErr error
	inspect := func(data []byte) {
		if probeErr != nil {
			return
		}
		if !gjson.ValidBytes(data) {
			probeErr = fmt.Errorf("Compact probe returned invalid JSON; capability was not determined")
			return
		}
		eventType := gjson.GetBytes(data, "type").String()
		response := gjson.ParseBytes(data)
		if nested := response.Get("response"); nested.IsObject() {
			response = nested
		}
		status := response.Get("status").String()
		errorValue := response.Get("error")
		if eventType == "error" || eventType == "response.failed" || status == "failed" ||
			(errorValue.Exists() && errorValue.Type != gjson.Null) {
			message := extractOpenAISSEErrorMessage(data)
			if message == "" {
				message = "upstream response failed"
			}
			probeErr = fmt.Errorf("Compact probe failed: %s", message)
			return
		}
		if eventType == "response.incomplete" || eventType == "response.cancelled" || eventType == "response.canceled" ||
			status == "incomplete" || status == "cancelled" || status == "canceled" {
			probeErr = fmt.Errorf("Compact probe returned an incomplete or cancelled response")
			return
		}
		if eventType == "response.completed" || eventType == "response.done" ||
			(eventType == "" && response.Get("output").IsArray() && (status == "" || status == "completed")) {
			completed = true
		}
	}
	forEachOpenAICompactProbePayload(body, inspect)
	if probeErr != nil {
		return probeErr
	}
	if !completed {
		return fmt.Errorf("Compact probe stream ended before successful completion; capability was not determined")
	}
	return nil
}

// A successful native v2 probe must also contain an actual compaction item.
func openAICompactProbeFoundCompactionItem(body []byte) bool {
	found := false
	validItem := func(item gjson.Result) bool {
		return isResponsesCompactionItemType(item.Get("type").String()) && strings.TrimSpace(item.Get("encrypted_content").String()) != ""
	}
	forEachOpenAICompactProbePayload(body, func(data []byte) {
		value := gjson.ParseBytes(data)
		eventType := value.Get("type").String()
		if (eventType == "response.output_item.done" || eventType == "response.output_item.added") && validItem(value.Get("item")) {
			found = true
		}
		if response := value.Get("response"); response.IsObject() {
			value = response
		}
		for _, item := range value.Get("output").Array() {
			if validItem(item) {
				found = true
			}
		}
	})
	return found
}

func forEachOpenAICompactProbePayload(body []byte, inspect func([]byte)) {
	if gjson.ValidBytes(body) {
		inspect(body)
		return
	}
	var parser openAICompatSSEFrameParser
	inspectFrame := func(frame openAICompatSSEFrame) {
		emitOpenAISSEDataPayloads(strings.Split(frame.Data, "\n"), func(data []byte) {
			inspect([]byte(openAICompatPayloadWithEventType(string(data), frame.EventType)))
		})
	}
	for _, line := range strings.Split(string(body), "\n") {
		if frame, ok := parser.AddLine(strings.TrimSuffix(line, "\r")); ok {
			inspectFrame(frame)
		}
	}
	if frame, ok := parser.Finish(); ok {
		inspectFrame(frame)
	}
}

func shouldMarkOpenAICompactUnsupported(status int, body []byte) bool {
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	case http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity:
		lower := strings.ToLower(strings.TrimSpace(extractUpstreamErrorMessage(body) + " " + string(body)))
		if strings.Contains(lower, "compact") {
			for _, keyword := range []string{
				"unsupported",
				"not support",
				"does not support",
				"not available",
				"disabled",
			} {
				if strings.Contains(lower, keyword) {
					return true
				}
			}
		}
	}
	return false
}

func buildOpenAICompactProbeExtraUpdates(resp *http.Response, body []byte, probeErr error, compactionFound bool, now time.Time) map[string]any {
	updates := map[string]any{
		"openai_compact_checked_at":  now.Format(time.RFC3339),
		"openai_compact_last_status": nil,
	}

	if resp != nil {
		updates["openai_compact_last_status"] = resp.StatusCode
	}

	switch {
	case probeErr != nil:
		updates["openai_compact_last_error"] = truncateString(sanitizeUpstreamErrorMessage(probeErr.Error()), 2048)
	case resp == nil:
		updates["openai_compact_last_error"] = "compact probe failed"
	default:
		errMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
		if errMsg == "" && len(body) > 0 {
			errMsg = strings.TrimSpace(string(body))
		}
		if errMsg == "" && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
			errMsg = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
		errMsg = truncateString(sanitizeUpstreamErrorMessage(errMsg), 2048)
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300 && compactionFound:
			updates["openai_compact_supported"] = true
			updates["openai_compact_last_error"] = ""
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			updates["openai_compact_supported"] = false
			updates["openai_compact_last_error"] = "upstream returned 2xx without a compaction output item (native remote compaction v2 unsupported)"
		default:
			if shouldMarkOpenAICompactUnsupported(resp.StatusCode, body) {
				updates["openai_compact_supported"] = false
			}
			updates["openai_compact_last_error"] = errMsg
		}
	}

	return updates
}

func buildOpenAICompactProbeUpdatesForMode(resp *http.Response, body []byte, probeErr error, compactionFound bool, native bool, account *Account) map[string]any {
	updates := buildOpenAICompactProbeExtraUpdates(resp, body, probeErr, compactionFound, time.Now())
	if !native {
		if account != nil {
			previousError, _ := account.Extra["openai_compact_last_error"].(string)
			if _, determined := updates["openai_compact_supported"]; !determined && strings.Contains(previousError, "native remote compaction v2 unsupported") {
				// A transient reprobe must not revive the old native false negative
				// by replacing the diagnostic used to identify it.
				updates["openai_compact_supported"] = nil
			}
		}
		if message, ok := updates["openai_compact_last_error"].(string); ok {
			updates["openai_compact_last_error"] = strings.ReplaceAll(message, "native remote compaction v2 unsupported", "standalone compact unsupported")
		}
		return updates
	}
	namespaced := make(map[string]any, len(updates))
	for key, value := range updates {
		namespaced[strings.Replace(key, "openai_compact_", "openai_native_compact_", 1)] = value
	}
	return namespaced
}

func mergeExtraUpdates(base map[string]any, more map[string]any) map[string]any {
	if len(base) == 0 && len(more) == 0 {
		return nil
	}
	out := make(map[string]any, len(base)+len(more))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range more {
		out[key] = value
	}
	return out
}

func compactProbeSessionID(accountID int64) string {
	if accountID <= 0 {
		return deriveStableUUIDv4("sub2api:codex-compact-probe:v1:anonymous")
	}
	return deriveStableUUIDv4("sub2api:codex-compact-probe:v1:" + strconv.FormatInt(accountID, 10))
}
