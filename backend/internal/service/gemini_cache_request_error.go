package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

func isGeminiCachedContentNotFound(statusCode int, body []byte) bool {
	if statusCode != 400 && statusCode != 403 && statusCode != 404 {
		return false
	}
	if len(body) > 2<<20 || !json.Valid(body) || !uniqueJSONValue(json.NewDecoder(bytes.NewReader(body)), 0) {
		return false
	}
	var envelope struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Error.Code != statusCode {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(envelope.Error.Message))
	switch statusCode {
	case http.StatusForbidden:
		return envelope.Error.Status == "PERMISSION_DENIED" && strings.HasPrefix(message, "cachedcontent not found")
	case http.StatusBadRequest:
		return envelope.Error.Status == "INVALID_ARGUMENT" && strings.HasPrefix(message, "invalid resource state for cache content ")
	case http.StatusNotFound:
		return envelope.Error.Status == "NOT_FOUND" && strings.HasPrefix(message, "cached content ") && strings.HasSuffix(message, "not found.")
	}
	return false
}
