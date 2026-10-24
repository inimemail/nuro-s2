package service

import (
	"strings"

	"github.com/tidwall/gjson"
)

// isCloudflare1010JSON requires explicit CDN evidence, not a business error
// whose numeric code happens to be 1010. Work is bounded on untrusted bodies.
func isCloudflare1010JSON(body []byte) bool {
	if len(body) == 0 || len(body) > 1<<20 || !gjson.ValidBytes(body) {
		return false
	}
	remaining := 1024
	var visit func(gjson.Result, int, bool) bool
	visit = func(node gjson.Result, depth int, cloudflare bool) bool {
		remaining--
		if remaining < 0 || depth > 16 {
			return false
		}
		if node.IsObject() {
			code1010, name := false, false
			node.ForEach(func(key, value gjson.Result) bool {
				switch strings.ToLower(key.Str) {
				case "cloudflare_error":
					cloudflare = cloudflare || value.Type == gjson.True
				case "error_name":
					name = strings.EqualFold(strings.TrimSpace(value.String()), "browser_signature_banned")
				case "code", "error_code":
					code1010 = code1010 || value.Type == gjson.Number && value.Num == 1010 || value.Type == gjson.String && strings.TrimSpace(value.Str) == "1010"
				}
				return true
			})
			if name || cloudflare && code1010 {
				return true
			}
		}

		found := false
		if node.IsObject() || node.IsArray() {
			node.ForEach(func(_, child gjson.Result) bool {
				if child.IsObject() || child.IsArray() {
					found = visit(child, depth+1, cloudflare)
				}
				return !found && remaining > 0
			})
		}
		return found
	}
	return visit(gjson.ParseBytes(body), 0, false)
}
