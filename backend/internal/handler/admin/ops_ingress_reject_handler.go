package admin

import (
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

var ingressRejectAllowed = map[string]map[string]struct{}{
	"reason":       {"query_api_key_deprecated": {}, "api_key_required": {}, "invalid_api_key": {}, "invalid_auth_rate_limited": {}, "api_key_auth_overloaded": {}, "api_key_disabled": {}, "ip_restricted": {}, "user_inactive": {}, "group_deleted": {}, "group_disabled": {}, "group_not_allowed": {}, "group_unassigned": {}, "other": {}},
	"route_family": {"antigravity": {}, "gemini": {}, "codex": {}, "messages": {}, "responses": {}, "chat_completions": {}, "images": {}, "videos": {}, "embeddings": {}, "models": {}, "other": {}},
	"protocol":     {"google": {}, "anthropic": {}, "openai": {}, "gateway": {}, "other": {}},
}

func (h *OpsHandler) ListIngressRejects(c *gin.Context) {
	if h == nil || h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	if err := h.opsService.RequireMonitoringEnabled(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	page, size := response.ParsePagination(c)
	if size > 200 {
		size = 200
	}
	f := &service.OpsIngressRejectFilter{Page: page, PageSize: size}
	start, end, err := parseOpsTimeRange(c, "1h")
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if !start.IsZero() {
		f.StartTime = &start
	}
	if !end.IsZero() {
		f.EndTime = &end
	}
	for _, name := range []string{"reason", "route_family", "protocol"} {
		v := strings.TrimSpace(c.Query(name))
		if v != "" {
			if _, ok := ingressRejectAllowed[name][v]; !ok {
				response.BadRequest(c, "Invalid "+name)
				return
			}
			switch name {
			case "reason":
				f.RejectReason = v
			case "route_family":
				f.RouteFamily = v
			case "protocol":
				f.Protocol = v
			}
		}
	}
	if v := strings.TrimSpace(c.Query("client_ip")); v != "" {
		a, err := netip.ParseAddr(v)
		if err != nil {
			response.BadRequest(c, "Invalid client_ip")
			return
		}
		a = a.Unmap()
		if a.Is6() {
			a = netip.PrefixFrom(a, 64).Masked().Addr()
		}
		f.ClientIP = a.String()
	}
	if v := strings.TrimSpace(c.Query("user_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid user_id")
			return
		}
		f.UserID = &id
	}
	if v := strings.TrimSpace(c.Query("api_key_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid api_key_id")
			return
		}
		f.APIKeyID = &id
	}
	result, err := h.opsService.ListIngressRejects(c.Request.Context(), f)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *OpsHandler) GetIngressRejectHealth(c *gin.Context) {
	if h == nil || h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	if err := h.opsService.RequireMonitoringEnabled(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, h.opsService.GetIngressRejectHealth())
}
