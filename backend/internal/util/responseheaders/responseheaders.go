package responseheaders

import (
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// defaultAllowed 定义允许透传的响应头白名单
// 注意：以下头部由 Go HTTP 包自动处理，不应手动设置：
//   - content-length: 由 ResponseWriter 根据实际写入数据自动设置
//   - transfer-encoding: 由 HTTP 库根据需要自动添加/移除
//   - connection: 由 HTTP 库管理连接复用
var defaultAllowed = map[string]struct{}{
	"content-type":                   {},
	"content-encoding":               {},
	"content-language":               {},
	"cache-control":                  {},
	"etag":                           {},
	"last-modified":                  {},
	"expires":                        {},
	"vary":                           {},
	"date":                           {},
	"x-request-id":                   {},
	"x-ratelimit-limit-requests":     {},
	"x-ratelimit-limit-tokens":       {},
	"x-ratelimit-remaining-requests": {},
	"x-ratelimit-remaining-tokens":   {},
	"x-ratelimit-reset-requests":     {},
	"x-ratelimit-reset-tokens":       {},
	"retry-after":                    {},
}

// hopByHopHeaders 是跳过的 hop-by-hop 头部，这些头部由 HTTP 库自动处理
var hopByHopHeaders = map[string]struct{}{
	"content-length":    {},
	"transfer-encoding": {},
	"connection":        {},
}

// These headers can embed the upstream host or authentication realm. They are
// never public, even when an administrator adds them to AdditionalAllowed.
var sensitiveUpstreamHeaders = map[string]struct{}{
	"location":                      {},
	"www-authenticate":              {},
	"server":                        {},
	"via":                           {},
	"x-powered-by":                  {},
	"x-served-by":                   {},
	"x-cache":                       {},
	"x-cache-hits":                  {},
	"cf-ray":                        {},
	"cf-cache-status":               {},
	"x-amz-cf-id":                   {},
	"x-amz-cf-pop":                  {},
	"x-envoy-upstream-service-time": {},
}

var (
	sensitiveUpstreamHeaderPrefixes = []string{
		"openai-", "x-openai-", "anthropic-", "x-anthropic-", "x-claude-",
		"x-grok-", "x-xai-", "x-groq-", "x-openrouter-", "x-google-", "x-goog-",
		"x-amz-", "x-amzn-", "x-aws-", "x-ms-", "cf-", "x-envoy-",
	}
	sensitiveUpstreamHeaderNames = map[string]struct{}{
		"server-timing": {},
		"x-backend":     {},
		"x-origin":      {},
		"x-provider":    {},
		"x-upstream":    {},
	}
	upstreamHeaderEndpointPattern = regexp.MustCompile(`(?i)(?:https?|wss?)://|(?:[a-z0-9-]+\.)+[a-z]{2,24}(?::\d+)?`)
	upstreamHeaderIdentityPattern = regexp.MustCompile(`(?i)(^|[^a-z0-9])(openai|chatgpt|anthropic|claude|gemini|vertex|grok|xai|x\.ai|groq|openrouter|cloudflare|cloudfront|fastly|akamai|envoy|nginx|bedrock|aws|amazon|azure)([^a-z0-9]|$)`)
)

func isSensitiveUpstreamHeader(name string) bool {
	if _, sensitive := sensitiveUpstreamHeaders[name]; sensitive {
		return true
	}
	if _, sensitive := sensitiveUpstreamHeaderNames[name]; sensitive {
		return true
	}
	for _, prefix := range sensitiveUpstreamHeaderPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func upstreamHeaderValueIsSensitive(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && (upstreamHeaderEndpointPattern.MatchString(value) || upstreamHeaderIdentityPattern.MatchString(value))
}

// SafeContentType returns a protocol-safe Content-Type without forwarding
// provider-controlled extension parameters. Charset and multipart boundary are
// the only parameters required to preserve response decoding semantics.
func SafeContentType(value, fallback string) string {
	value = strings.TrimSpace(value)
	fallback = strings.TrimSpace(fallback)
	if value == "" || upstreamHeaderValueIsSensitive(value) {
		return fallback
	}

	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil || strings.TrimSpace(mediaType) == "" || upstreamHeaderValueIsSensitive(mediaType) {
		return fallback
	}
	safeParams := make(map[string]string, 2)
	for _, name := range []string{"charset", "boundary"} {
		param := strings.TrimSpace(params[name])
		if param == "" {
			continue
		}
		if upstreamHeaderValueIsSensitive(param) {
			return fallback
		}
		safeParams[name] = param
	}
	formatted := mime.FormatMediaType(mediaType, safeParams)
	if formatted == "" {
		return fallback
	}
	return formatted
}

// PublicRequestID keeps downstream correlation possible without exposing a
// provider-controlled request ID prefix, hostname, or account identifier.
func PublicRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return "req_" + hex.EncodeToString(digest[:12])
}

type CompiledHeaderFilter struct {
	allowed     map[string]struct{}
	forceRemove map[string]struct{}
}

var defaultCompiledHeaderFilter = CompileHeaderFilter(config.ResponseHeaderConfig{})

func CompileHeaderFilter(cfg config.ResponseHeaderConfig) *CompiledHeaderFilter {
	allowed := make(map[string]struct{}, len(defaultAllowed)+len(cfg.AdditionalAllowed))
	for key := range defaultAllowed {
		allowed[key] = struct{}{}
	}
	// 关闭时只使用默认白名单，additional/force_remove 不生效
	if cfg.Enabled {
		for _, key := range cfg.AdditionalAllowed {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if normalized == "" {
				continue
			}
			allowed[normalized] = struct{}{}
		}
	}

	forceRemove := map[string]struct{}{}
	if cfg.Enabled {
		forceRemove = make(map[string]struct{}, len(cfg.ForceRemove))
		for _, key := range cfg.ForceRemove {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if normalized == "" {
				continue
			}
			forceRemove[normalized] = struct{}{}
		}
	}

	return &CompiledHeaderFilter{
		allowed:     allowed,
		forceRemove: forceRemove,
	}
}

func FilterHeaders(src http.Header, filter *CompiledHeaderFilter) http.Header {
	if filter == nil {
		filter = defaultCompiledHeaderFilter
	}

	filtered := make(http.Header, len(src))
	for key, values := range src {
		lower := strings.ToLower(key)
		if isSensitiveUpstreamHeader(lower) {
			continue
		}
		if _, blocked := filter.forceRemove[lower]; blocked {
			continue
		}
		if _, ok := filter.allowed[lower]; !ok {
			continue
		}
		// 跳过 hop-by-hop 头部，这些由 HTTP 库自动处理
		if _, isHopByHop := hopByHopHeaders[lower]; isHopByHop {
			continue
		}
		for _, value := range values {
			if lower == "x-request-id" || lower == "request-id" || lower == "x-correlation-id" || lower == "correlation-id" {
				if publicID := PublicRequestID(value); publicID != "" {
					filtered.Add(key, publicID)
				}
				continue
			}
			if lower == "content-type" {
				if safe := SafeContentType(value, ""); safe != "" {
					filtered.Add(key, safe)
				}
				continue
			}
			if upstreamHeaderValueIsSensitive(value) {
				continue
			}
			filtered.Add(key, value)
		}
	}
	return filtered
}

func WriteFilteredHeaders(dst http.Header, src http.Header, filter *CompiledHeaderFilter) {
	filtered := FilterHeaders(src, filter)
	for key, values := range filtered {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
