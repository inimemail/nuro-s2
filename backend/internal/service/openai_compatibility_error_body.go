package service

import (
	"bytes"
	"encoding/json"
	"io"
)

// Error-body compatibility retries need a complete, unambiguous error object.
// Status-based authentication/rate-limit handling remains independent of this.
func readOpenAICompatibilityErrorBody(r io.Reader) ([]byte, bool) {
	const limit = 2 << 20
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if len(body) > limit {
		return body[:limit], false
	}
	if err != nil || !json.Valid(body) {
		return body, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if !uniqueJSONValue(dec, 0) {
		return body, false
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return body, false
	}
	var upstreamError map[string]json.RawMessage
	if json.Unmarshal(envelope["error"], &upstreamError) != nil || len(upstreamError) == 0 {
		return body, false
	}
	for _, key := range []string{"message", "type", "code", "param"} {
		if raw, exists := upstreamError[key]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			var value string
			if json.Unmarshal(raw, &value) != nil {
				return body, false
			}
		}
	}
	return body, true
}

func uniqueJSONValue(dec *json.Decoder, depth int) bool {
	if depth > 64 {
		return false
	}
	token, err := dec.Token()
	if err != nil {
		return false
	}
	delim, container := token.(json.Delim)
	if !container {
		return true
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return false
			}
			name, ok := key.(string)
			if !ok {
				return false
			}
			if _, duplicate := seen[name]; duplicate {
				return false
			}
			seen[name] = struct{}{}
			if !uniqueJSONValue(dec, depth+1) {
				return false
			}
		}
	case '[':
		for dec.More() {
			if !uniqueJSONValue(dec, depth+1) {
				return false
			}
		}
	default:
		return false
	}
	_, err = dec.Token()
	return err == nil
}
