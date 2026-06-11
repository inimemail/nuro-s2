package service

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var openAIUpstreamStrongIsolationHeaders = []string{
	"conversation_id",
	"originator",
	"session_id",
	"x-codex-turn-metadata",
	"x-codex-turn-state",
}

var openAIUpstreamStrongIsolationBodyFields = []string{
	"client_metadata",
	"conversation_id",
	"session_id",
	"previous_response_id",
}

var openAIUpstreamStrongIsolationClientMetadataFields = []string{
	"client_metadata." + openAIWSTurnMetadataHeader,
	"client_metadata." + openAIWSTurnStateHeader,
}

func applyOpenAIUpstreamStrongIsolationMap(reqBody map[string]any, forceStoreFalse bool) bool {
	if reqBody == nil {
		return false
	}
	changed := false
	for _, field := range openAIUpstreamStrongIsolationBodyFields {
		if _, ok := reqBody[field]; ok {
			delete(reqBody, field)
			changed = true
		}
	}
	if forceStoreFalse {
		if v, ok := reqBody["store"]; !ok {
			reqBody["store"] = false
			changed = true
		} else if enabled, ok := v.(bool); !ok || enabled {
			reqBody["store"] = false
			changed = true
		}
	} else if _, ok := reqBody["store"]; ok {
		if v, ok := reqBody["store"].(bool); !ok || v {
			reqBody["store"] = false
			changed = true
		}
	}
	return changed
}

func applyOpenAIUpstreamStrongIsolationWSMap(reqBody map[string]any, forceStoreFalse bool) bool {
	changed := applyOpenAIUpstreamStrongIsolationMap(reqBody, forceStoreFalse)
	if reqBody == nil {
		return changed
	}
	if metadata, ok := reqBody["client_metadata"].(map[string]any); ok {
		for _, field := range []string{openAIWSTurnMetadataHeader, openAIWSTurnStateHeader} {
			if _, exists := metadata[field]; exists {
				delete(metadata, field)
				changed = true
			}
		}
	}
	return changed
}

func applyOpenAIUpstreamStrongIsolationBody(body []byte, forceStoreFalse bool) ([]byte, bool, error) {
	if len(bytes.TrimSpace(body)) == 0 || !gjson.ValidBytes(body) {
		return body, false, nil
	}
	changed := false
	for _, field := range openAIUpstreamStrongIsolationBodyFields {
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
	if forceStoreFalse || gjson.GetBytes(body, "store").Exists() {
		next, err := sjson.SetBytes(body, "store", false)
		if err != nil {
			return body, changed, err
		}
		if !bytes.Equal(next, body) {
			body = next
			changed = true
		}
	}
	return body, changed, nil
}

func applyOpenAIUpstreamStrongIsolationWSBody(body []byte, forceStoreFalse bool) ([]byte, bool, error) {
	body, changed, err := applyOpenAIUpstreamStrongIsolationBody(body, forceStoreFalse)
	if err != nil {
		return body, changed, err
	}
	for _, field := range openAIUpstreamStrongIsolationClientMetadataFields {
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

func applyOpenAIUpstreamStrongIsolationHeaders(req *http.Request) {
	if req == nil {
		return
	}
	applyOpenAIUpstreamStrongIsolationHeaderMap(req.Header)
}

func applyOpenAIUpstreamStrongIsolationHeaderMap(headers http.Header) {
	if headers == nil {
		return
	}
	for _, header := range openAIUpstreamStrongIsolationHeaders {
		headers.Del(header)
	}
	for key := range headers {
		lower := strings.ToLower(strings.TrimSpace(key))
		for _, header := range openAIUpstreamStrongIsolationHeaders {
			if lower == header {
				headers.Del(key)
				break
			}
		}
	}
}
