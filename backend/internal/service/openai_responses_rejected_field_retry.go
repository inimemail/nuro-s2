package service

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const maxOpenAIResponsesRejectedFieldRetries = 6

var (
	openAIResponsesRejectedNamespaceParamPattern = regexp.MustCompile(`(?i)^input\[(\d+)\]\.namespace$`)
	openAIResponsesRejectedMessageParamPattern   = regexp.MustCompile(`(?i)(?:unknown|unsupported)[ _-]+parameter\s*(?::|=|is)?\s*["']?(max_output_tokens|input\[\d+\]\.namespace)(?:["']|\b)`)
)

type openAIResponsesRejectedFieldRetryState struct {
	attempts       int
	seenBodyHashes map[[sha256.Size]byte]struct{}
}

// OpenAIResponsesRejectedFieldRetryState bounds compatibility retries for an
// upstream that explicitly rejects a supported Responses request field. The
// zero value is ready to use and allocates only after the first valid rewrite.
type OpenAIResponsesRejectedFieldRetryState struct {
	state openAIResponsesRejectedFieldRetryState
}

// Rewrite removes exactly one explicitly rejected field and rejects duplicate
// or excessive body variants. Callers must only retry before writing downstream.
func (s *OpenAIResponsesRejectedFieldRetryState) Rewrite(statusCode int, currentBody, responseBody []byte) ([]byte, string, bool, error) {
	if s == nil {
		return nil, "", false, nil
	}
	nextBody, field, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(statusCode, currentBody, responseBody)
	if err != nil || !changed {
		return nextBody, field, false, err
	}
	if !s.state.Allow(currentBody, nextBody) {
		return nil, "", false, nil
	}
	return nextBody, field, true, nil
}

func (s *openAIResponsesRejectedFieldRetryState) Allow(currentBody, nextBody []byte) bool {
	if s == nil || len(nextBody) == 0 || s.attempts >= maxOpenAIResponsesRejectedFieldRetries {
		return false
	}
	if s.seenBodyHashes == nil {
		s.seenBodyHashes = make(map[[sha256.Size]byte]struct{}, maxOpenAIResponsesRejectedFieldRetries+1)
		s.seenBodyHashes[sha256.Sum256(currentBody)] = struct{}{}
	}
	bodyHash := sha256.Sum256(nextBody)
	if _, seen := s.seenBodyHashes[bodyHash]; seen {
		return false
	}
	s.seenBodyHashes[bodyHash] = struct{}{}
	s.attempts++
	return true
}

func normalizeOpenAIResponsesRejectedFieldRetryBody(statusCode int, body, responseBody []byte) ([]byte, string, bool, error) {
	if statusCode != http.StatusBadRequest || len(body) == 0 || len(responseBody) == 0 {
		return nil, "", false, nil
	}
	code := strings.ToLower(strings.TrimSpace(openAIResponsesRejectedErrorField(responseBody, "code")))
	message := strings.ToLower(strings.TrimSpace(openAIResponsesRejectedErrorField(responseBody, "message")))
	if !isExplicitOpenAIResponsesFieldRejection(code, message) {
		return nil, "", false, nil
	}
	param := strings.ToLower(strings.TrimSpace(openAIResponsesRejectedErrorField(responseBody, "param")))
	if param == "" {
		param = openAIResponsesRejectedParamFromMessage(message)
	}
	if index, ok := openAIResponsesRejectedNamespaceIndex(param); ok {
		return removeOpenAIResponsesRejectedNamespaceAtIndex(body, index)
	}
	if param == "max_output_tokens" && gjson.GetBytes(body, "max_output_tokens").Exists() {
		retryBody, changed, err := RemoveOpenAIResponsesRejectedField(body, param)
		return retryBody, param, changed, err
	}
	return nil, "", false, nil
}

// RemoveOpenAIResponsesRejectedField removes a previously confirmed
// compatibility field from a Responses body without consuming retry budget.
// max_tokens is the local compatibility source for max_output_tokens, so both
// aliases must be removed before a plan is rebuilt for another account.
func RemoveOpenAIResponsesRejectedField(body []byte, field string) ([]byte, bool, error) {
	field = strings.ToLower(strings.TrimSpace(field))
	if field == "max_output_tokens" {
		updated := body
		changed := false
		for _, alias := range []string{"max_output_tokens", "max_tokens"} {
			if !gjson.GetBytes(updated, alias).Exists() {
				continue
			}
			next, err := sjson.DeleteBytes(updated, alias)
			if err != nil {
				return nil, false, fmt.Errorf("delete rejected %s: %w", alias, err)
			}
			updated = next
			changed = true
		}
		return updated, changed, nil
	}
	if index, ok := openAIResponsesRejectedNamespaceIndex(field); ok {
		updated, _, changed, err := removeOpenAIResponsesRejectedNamespaceAtIndex(body, index)
		return updated, changed, err
	}
	return body, false, nil
}

func openAIResponsesRejectedErrorField(responseBody []byte, field string) string {
	if len(responseBody) == 0 || strings.TrimSpace(field) == "" {
		return ""
	}
	if value := strings.TrimSpace(gjson.GetBytes(responseBody, "error."+field).String()); value != "" {
		return value
	}
	return strings.TrimSpace(gjson.GetBytes(responseBody, "response.error."+field).String())
}

func isExplicitOpenAIResponsesFieldRejection(code, message string) bool {
	switch strings.TrimSpace(code) {
	case "unknown_parameter", "unsupported_parameter":
		return true
	}
	return strings.Contains(message, "unknown parameter") || strings.Contains(message, "unsupported parameter")
}

func openAIResponsesRejectedParamFromMessage(message string) string {
	match := openAIResponsesRejectedMessageParamPattern.FindStringSubmatch(strings.TrimSpace(message))
	if len(match) != 2 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(match[1]))
}

func openAIResponsesRejectedNamespaceIndex(param string) (int, bool) {
	match := openAIResponsesRejectedNamespaceParamPattern.FindStringSubmatch(strings.TrimSpace(param))
	if len(match) != 2 {
		return 0, false
	}
	index, err := strconv.Atoi(match[1])
	return index, err == nil && index >= 0
}

func removeOpenAIResponsesRejectedNamespaceAtIndex(body []byte, index int) ([]byte, string, bool, error) {
	itemPath := fmt.Sprintf("input.%d", index)
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, itemPath+".type").String())) {
	case "function_call", "tool_call", "custom_tool_call", "mcp_tool_call":
	default:
		return nil, "", false, nil
	}
	namespacePath := itemPath + ".namespace"
	if !gjson.GetBytes(body, namespacePath).Exists() {
		return nil, "", false, nil
	}
	retryBody, err := sjson.DeleteBytes(body, namespacePath)
	if err != nil {
		return nil, "", false, fmt.Errorf("delete rejected namespace at input[%d]: %w", index, err)
	}
	return retryBody, fmt.Sprintf("input[%d].namespace", index), true, nil
}
