package service

import (
	"github.com/tidwall/gjson"
	"net/http"
	"strings"
)

var upstreamModelNotFoundKeywords = []string{"model not found", "unknown model", "not found"}

// Codex OAuth accounts return a deterministic 400 when the ChatGPT plan does
// not allow the requested model. Treat this as a model/account capability
// mismatch, rather than a transient upstream failure.
const openAICodexPlanGatedModelPhrase = "model is not supported when using codex"

func isOpenAICodexPlanGatedModelError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	normalized := normalizeModelNotFoundBody(body)
	return normalized != "" && strings.Contains(normalized, openAICodexPlanGatedModelPhrase)
}

func isUpstreamModelNotFoundError(statusCode int, body []byte) bool {
	if statusCode != http.StatusNotFound {
		return false
	}
	normalized := normalizeModelNotFoundBody(body)
	if normalized == "" || !strings.Contains(normalized, "model") {
		return false
	}
	return containsModelNotFoundKeyword(normalized)
}

// A 401 carrying a model capability error must not ban an otherwise valid API key.
func isOpenAICompatibleModelNotFoundBody(body []byte) bool {
	if !gjson.ValidBytes(body) {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
	if code == "model_not_found" || code == "unknown_model" {
		return true
	}
	message := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.message").String()))
	return strings.HasPrefix(message, "model not found") || strings.HasPrefix(message, "unknown model") ||
		(strings.HasPrefix(message, "the model ") && (strings.Contains(message, "does not exist") || strings.Contains(message, "not found")))
}

func isModelNotFoundError(statusCode int, body []byte) bool {
	return isUpstreamModelNotFoundError(statusCode, body) || statusCode == http.StatusNotFound
}

func containsModelNotFoundKeyword(normalizedBody string) bool {
	if normalizedBody == "" {
		return false
	}
	for _, keyword := range upstreamModelNotFoundKeywords {
		if strings.Contains(normalizedBody, keyword) {
			return true
		}
	}
	return false
}

func normalizeModelNotFoundBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	normalized := strings.ToLower(string(body))
	normalized = strings.NewReplacer("_", " ", "-", " ", "\n", " ", "\r", " ", "\t", " ").Replace(normalized)
	return strings.Join(strings.Fields(normalized), " ")
}
