package service

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var anthropicUpstreamStrongIsolationHeaders = []string{
	"conversation_id",
	"session_id",
	"x-claude-code-session-id",
	"x-codex-turn-metadata",
	"x-codex-turn-state",
}

var anthropicUpstreamStrongIsolationBodyFields = []string{
	"client_metadata",
	"conversation_id",
	"session_id",
}

func applyAnthropicUpstreamStrongIsolationBody(body []byte) ([]byte, bool, error) {
	if len(bytes.TrimSpace(body)) == 0 || !gjson.ValidBytes(body) {
		return body, false, nil
	}
	changed := false
	for _, field := range anthropicUpstreamStrongIsolationBodyFields {
		if !gjson.GetBytes(body, field).Exists() {
			continue
		}
		next, err := sjson.DeleteBytes(body, field)
		if err != nil {
			return body, changed, err
		}
		body = next
		changed = true
	}
	return body, changed, nil
}

func applyAnthropicUpstreamStrongIsolationHeaders(req *http.Request) {
	if req == nil {
		return
	}
	applyAnthropicUpstreamStrongIsolationHeaderMap(req.Header)
}

func applyAnthropicUpstreamStrongIsolationHeaderMap(headers http.Header) {
	if headers == nil {
		return
	}
	for _, header := range anthropicUpstreamStrongIsolationHeaders {
		headers.Del(header)
	}
	for key := range headers {
		lower := strings.ToLower(strings.TrimSpace(key))
		for _, header := range anthropicUpstreamStrongIsolationHeaders {
			if lower == header {
				headers.Del(key)
				break
			}
		}
	}
}
