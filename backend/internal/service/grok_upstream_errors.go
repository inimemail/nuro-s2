package service

import (
	"encoding/json"
	"net/http"
	"strings"
)

func isGrokContentPolicyRejection(statusCode int, body []byte) bool {
	if statusCode != http.StatusForbidden || len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, marker := range []string{"content_policy", "content policy", "content_moderation", "content moderation", "prohibited content", "prompt violates policy", "request blocked by policy", "image is sensitive", "text is sensitive"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	var payload any
	if json.Unmarshal(body, &payload) == nil {
		return grokStructuredContentPolicyMarker(payload)
	}
	return false
}

func grokStructuredContentPolicyMarker(value any) bool {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			if (key == "code" || key == "type" || key == "category" || key == "reason") && isGrokContentPolicyCode(stringValue(child)) {
				return true
			}
			if grokStructuredContentPolicyMarker(child) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if grokStructuredContentPolicyMarker(child) {
				return true
			}
		}
	}
	return false
}

func isGrokContentPolicyCode(value string) bool {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "-", "_")) {
	case "content_filter", "content_policy", "content_policy_violation", "content_moderation", "new_sensitive":
		return true
	default:
		return false
	}
}

func (s *OpenAIGatewayService) shouldFailoverGrokUpstreamError(statusCode int, body []byte) bool {
	return !isGrokContentPolicyRejection(statusCode, body) && s.shouldFailoverUpstreamError(statusCode)
}
