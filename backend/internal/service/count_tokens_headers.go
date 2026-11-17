package service

import (
	"net/http"
	"sort"
	"strings"
)

// Beta is a list-valued header. Collapse all wire values before body sanitizing
// and forwarding, while single-valued headers keep their existing first value.
func countTokensBetaHeader(headers http.Header) string {
	keys := make([]string, 0, 1)
	for key := range headers {
		if strings.EqualFold(key, "anthropic-beta") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var values []string
	for _, key := range keys {
		values = append(values, headers[key]...)
	}
	return mergeAnthropicBeta(nil, strings.Join(values, ","))
}
