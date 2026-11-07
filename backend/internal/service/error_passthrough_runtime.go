package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const errorPassthroughServiceContextKey = "error_passthrough_service"

// BindErrorPassthroughService 将错误透传服务绑定到请求上下文，供 service 层在非 failover 场景下复用规则。
func BindErrorPassthroughService(c *gin.Context, svc *ErrorPassthroughService) {
	if c == nil || svc == nil {
		return
	}
	c.Set(errorPassthroughServiceContextKey, svc)
	if c.Request != nil {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), errorPassthroughRequestKey{}, svc))
	}
}

func getBoundErrorPassthroughService(c *gin.Context) *ErrorPassthroughService {
	if c == nil {
		return nil
	}
	v, ok := c.Get(errorPassthroughServiceContextKey)
	if !ok {
		return nil
	}
	svc, ok := v.(*ErrorPassthroughService)
	if !ok {
		return nil
	}
	return svc
}

// applyErrorPassthroughRule 按规则改写错误响应；未命中时返回默认响应参数。
func applyErrorPassthroughRule(
	c *gin.Context,
	platform string,
	upstreamStatus int,
	responseBody []byte,
	defaultStatus int,
	defaultErrType string,
	defaultErrMsg string,
) (status int, errType string, errMsg string, matched bool) {
	status = defaultStatus
	errType = defaultErrType
	errMsg = defaultErrMsg

	svc := getBoundErrorPassthroughService(c)
	if svc == nil {
		return status, errType, errMsg, false
	}

	rule := svc.MatchRule(platform, upstreamStatus, responseBody)
	if rule == nil {
		return status, errType, errMsg, false
	}

	status = upstreamStatus
	if !rule.PassthroughCode && rule.ResponseCode != nil {
		status = *rule.ResponseCode
	}

	// Matching still uses the original body internally. Public responses never
	// inherit provider-controlled diagnostics or upstream identity, including
	// identities accidentally included in an administrator-authored message.
	if !rule.PassthroughBody && rule.CustomMessage != nil {
		errMsg = sanitizeUpstreamErrorMessage(*rule.CustomMessage)
	} else {
		errMsg = sanitizeUpstreamErrorMessage(defaultErrMsg)
	}
	if strings.TrimSpace(errMsg) == "" {
		errMsg = firstNonEmptyString(sanitizeUpstreamErrorMessage(defaultErrMsg), safeUpstreamErrorMessage)
	}

	// 命中 skip_monitoring 时在 context 中标记，供 ops_error_logger 跳过记录。
	if rule.SkipMonitoring {
		c.Set(OpsSkipPassthroughKey, true)
	}

	// 与现有 failover 场景保持一致：命中规则时统一返回 upstream_error。
	errType = "upstream_error"
	return status, errType, errMsg, true
}

type errorPassthroughRequestKey struct{}

// Rules can select the final response for a 413 before failover, but never
// change attempt execution certainty or grant permission to replay.
func matchesOpenAI413Rule(ctx context.Context, c *gin.Context, platform string, status int, body []byte) bool {
	if status != http.StatusRequestEntityTooLarge {
		return false
	}
	var svc *ErrorPassthroughService
	if ctx != nil {
		svc, _ = ctx.Value(errorPassthroughRequestKey{}).(*ErrorPassthroughService)
	}
	if svc == nil {
		svc = getBoundErrorPassthroughService(c)
	}
	return svc != nil && svc.MatchRule(platform, status, body) != nil
}
