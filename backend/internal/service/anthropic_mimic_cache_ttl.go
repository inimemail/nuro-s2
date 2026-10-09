package service

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Anthropic visits cache breakpoints in tools, system, messages order. Promote
// preceding short breakpoints only when needed to preserve a later explicit 1h.
func normalizeMimicCacheControlTTLOrder(body []byte) []byte {
	_, messages, tools, system := collectCacheControlPaths(body)
	paths := make([]string, 0, len(messages)+len(tools)+len(system)+1)
	paths = append(paths, tools...)
	paths = append(paths, system...)
	paths = append(paths, messages...)
	if gjson.GetBytes(body, "cache_control").Exists() {
		paths = append(paths, "cache_control")
	}
	lastLong := -1
	for i, path := range paths {
		cc := gjson.GetBytes(body, path)
		if cc.Get("type").String() == "ephemeral" && cc.Get("ttl").String() == cacheTTLTarget1h {
			lastLong = i
		}
	}
	out := body
	for _, path := range paths[:lastLong+1] {
		cc := gjson.GetBytes(out, path)
		ttl := cc.Get("ttl").String()
		if cc.Get("type").String() != "ephemeral" || (ttl != "" && ttl != cacheTTLTarget5m) {
			continue
		}
		next, err := sjson.SetBytes(out, path+".ttl", cacheTTLTarget1h)
		if err != nil {
			return body
		}
		out = next
	}
	return out
}
