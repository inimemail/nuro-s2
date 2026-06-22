package service

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	anthropicCacheBoostNormalMinBodyBytes     = 16 * 1024
	anthropicCacheBoostAggressiveMinBodyBytes = 4 * 1024
)

func (s *GatewayService) applyAnthropicCacheBoostBody(ctx context.Context, account *Account, body []byte) []byte {
	if account == nil || !account.IsAnthropicCacheBoostEnabled() || len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	minBytes := anthropicCacheBoostNormalMinBodyBytes
	if account.IsAnthropicCacheBoostAggressive() {
		minBytes = anthropicCacheBoostAggressiveMinBodyBytes
	}
	if len(body) < minBytes {
		return body
	}

	out := stripMessageCacheControl(body)
	if account.IsAnthropicCacheBoostAggressive() {
		out = addSystemLastCacheBreakpoint(out)
		out = applyToolsLastCacheBreakpoint(out)
	}
	out = addMessageCacheBreakpoints(out)
	out = enforceCacheControlLimit(out)
	return out
}

func addSystemLastCacheBreakpoint(body []byte) []byte {
	system := gjson.GetBytes(body, "system")
	if !system.Exists() {
		return body
	}
	switch {
	case system.Type == gjson.String:
		text := system.String()
		raw := fmt.Sprintf(
			`[{"type":"text","text":%s,"cache_control":{"type":"ephemeral","ttl":%q}}]`,
			mustJSONString(text), claude.DefaultCacheControlTTL,
		)
		if next, err := sjson.SetRawBytes(body, "system", []byte(raw)); err == nil {
			return next
		}
		return body
	case system.IsArray():
		arr := system.Array()
		if len(arr) == 0 {
			return body
		}
		lastIdx := len(arr) - 1
		last := arr[lastIdx]
		if cc := last.Get("cache_control"); cc.Exists() && cc.Get("ttl").String() != "" {
			return body
		}
		pathPrefix := fmt.Sprintf("system.%d.cache_control", lastIdx)
		if last.Get("cache_control").Exists() {
			if next, err := sjson.SetBytes(body, pathPrefix+".ttl", claude.DefaultCacheControlTTL); err == nil {
				return next
			}
			return body
		}
		raw := fmt.Sprintf(`{"type":"ephemeral","ttl":%q}`, claude.DefaultCacheControlTTL)
		if next, err := sjson.SetRawBytes(body, pathPrefix, []byte(raw)); err == nil {
			return next
		}
	}
	return body
}
