package middleware

import (
	"math"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

type IngressRejectReason string

const (
	IngressRejectQueryAPIKeyDeprecated  IngressRejectReason = "query_api_key_deprecated"
	IngressRejectAPIKeyRequired         IngressRejectReason = "api_key_required"
	IngressRejectInvalidAPIKey          IngressRejectReason = "invalid_api_key"
	IngressRejectAPIKeyDisabled         IngressRejectReason = "api_key_disabled"
	IngressRejectIPRestricted           IngressRejectReason = "ip_restricted"
	IngressRejectUserInactive           IngressRejectReason = "user_inactive"
	IngressRejectGroupDeleted           IngressRejectReason = "group_deleted"
	IngressRejectGroupDisabled          IngressRejectReason = "group_disabled"
	IngressRejectGroupNotAllowed        IngressRejectReason = "group_not_allowed"
	IngressRejectGroupUnassigned        IngressRejectReason = "group_unassigned"
	IngressRejectInvalidAuthRateLimited IngressRejectReason = "invalid_auth_rate_limited"
	IngressRejectAPIKeyAuthOverloaded   IngressRejectReason = "api_key_auth_overloaded"
)

const ingressRejectReasonContextKey = "ingress_reject_reason"

type IngressRejectRecorder interface {
	RecordIngressReject(reason, routeFamily, protocol, clientIP string, userID, apiKeyID int64)
}

func invalidAuthClientKey(c *gin.Context) string {
	return normalizeIngressRejectIP(SecurityClientIP(c))
}

func rejectInvalidAuthAbuse(c *gin.Context, limiter interface {
	CheckInvalidAuthAbuse(string) (time.Duration, bool)
}) bool {
	if c == nil || limiter == nil {
		return false
	}
	retry, blocked := limiter.CheckInvalidAuthAbuse(invalidAuthClientKey(c))
	if !blocked {
		return false
	}
	seconds := int(math.Ceil(retry.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	c.Header("Retry-After", strconv.Itoa(seconds))
	MarkIngressRejected(c, IngressRejectInvalidAuthRateLimited)
	return true
}

func recordInvalidAuthFailure(c *gin.Context, limiter interface{ RecordInvalidAuthFailure(string) }) {
	if c != nil && limiter != nil {
		limiter.RecordInvalidAuthFailure(invalidAuthClientKey(c))
	}
}

type ingressRejectRecorderHolder struct{ recorder IngressRejectRecorder }

var activeIngressRejectRecorder atomic.Pointer[ingressRejectRecorderHolder]

func SetIngressRejectRecorder(recorder IngressRejectRecorder) {
	if recorder == nil {
		activeIngressRejectRecorder.Store(nil)
		return
	}
	activeIngressRejectRecorder.Store(&ingressRejectRecorderHolder{recorder: recorder})
}

func MarkIngressRejected(c *gin.Context, reason IngressRejectReason) {
	if c != nil && reason != "" {
		c.Set(ingressRejectReasonContextKey, reason)
	}
}

func GetIngressRejectReason(c *gin.Context) (IngressRejectReason, bool) {
	if c == nil {
		return "", false
	}
	v, ok := c.Get(ingressRejectReasonContextKey)
	r, typed := v.(IngressRejectReason)
	return r, ok && typed && r != ""
}

func recordIngressReject(c *gin.Context, reason IngressRejectReason) {
	h := activeIngressRejectRecorder.Load()
	if h == nil || h.recorder == nil || c == nil || c.Request == nil {
		return
	}
	route, protocol := ingressRejectRoute(c.Request.URL.Path)
	clientIP := normalizeIngressRejectIP(SecurityClientIP(c))
	var userID, keyID int64
	if key, ok := GetAPIKeyFromContext(c); ok && key != nil {
		keyID = key.ID
		if key.User != nil {
			userID = key.User.ID
		}
	} else if key, ok := GetOpsFallbackAPIKey(c); ok && key != nil {
		keyID = key.ID
		if key.User != nil {
			userID = key.User.ID
		}
	}
	h.recorder.RecordIngressReject(string(reason), route, protocol, clientIP, userID, keyID)
}

func normalizeIngressRejectIP(raw string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return "0.0.0.0"
	}
	addr = addr.Unmap()
	if addr.Is6() {
		return netip.PrefixFrom(addr, 64).Masked().Addr().String()
	}
	return addr.String()
}

func ingressRejectRoute(path string) (string, string) {
	path = strings.ToLower(strings.TrimSpace(path))
	switch {
	case strings.HasPrefix(path, "/antigravity/v1beta"):
		return "antigravity", "google"
	case strings.HasPrefix(path, "/v1beta"):
		return "gemini", "google"
	case strings.HasPrefix(path, "/backend-api/codex"):
		return "codex", "openai"
	case strings.Contains(path, "/messages"):
		return "messages", "anthropic"
	case strings.Contains(path, "/responses"):
		return "responses", "openai"
	case strings.Contains(path, "/chat/completions"):
		return "chat_completions", "openai"
	case strings.Contains(path, "/images"):
		return "images", "openai"
	case strings.Contains(path, "/videos"):
		return "videos", "openai"
	case strings.Contains(path, "/embeddings"):
		return "embeddings", "openai"
	case strings.Contains(path, "/models"):
		return "models", "openai"
	default:
		return "other", "gateway"
	}
}
