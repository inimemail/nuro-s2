package middleware

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	basemiddleware "github.com/Wei-Shaw/sub2api/internal/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const panelRateLimitWindow = time.Minute

type panelRateLimitAllower interface {
	Allow(context.Context, string, int, time.Duration) (basemiddleware.AllowResult, error)
}

type PanelRateLimiter struct {
	limiter        panelRateLimitAllower
	settingService *service.SettingService
}

func NewPanelRateLimiter(redisClient *redis.Client, settingService *service.SettingService) *PanelRateLimiter {
	return &PanelRateLimiter{limiter: basemiddleware.NewRateLimiter(redisClient), settingService: settingService}
}

func (p *PanelRateLimiter) Global() gin.HandlerFunc {
	return p.userScoped("global", func(settings service.PanelRateLimitSettings) int { return settings.UserRPM })
}

func (p *PanelRateLimiter) Heavy() gin.HandlerFunc {
	return p.userScoped("heavy", func(settings service.PanelRateLimitSettings) int { return settings.HeavyRPM })
}

func (p *PanelRateLimiter) userScoped(scope string, limitOf func(service.PanelRateLimitSettings) int) gin.HandlerFunc {
	return func(c *gin.Context) {
		if p == nil || p.limiter == nil || p.settingService == nil {
			c.Next()
			return
		}
		settings := p.settingService.GetPanelRateLimitSettingsCached(c.Request.Context())
		limit := limitOf(settings)
		if !settings.Enabled || limit <= 0 {
			c.Next()
			return
		}
		subject, ok := GetAuthSubjectFromContext(c)
		if !ok || subject.UserID <= 0 {
			c.Next()
			return
		}
		if settings.ExemptAdmin {
			if role, ok := GetUserRoleFromContext(c); ok && role == service.RoleAdmin {
				c.Next()
				return
			}
		}
		result, err := p.limiter.Allow(c.Request.Context(), "panel:"+scope+":user:"+strconv.FormatInt(subject.UserID, 10), limit, panelRateLimitWindow)
		if err != nil {
			slog.Warn("panel rate limit check failed, allowing request", "scope", scope, "error", err)
			c.Next()
			return
		}
		if !result.Allowed {
			abortPanelRateLimited(c, result.RetryAfter)
			return
		}
		c.Next()
	}
}

func (p *PanelRateLimiter) PublicIP() gin.HandlerFunc {
	return func(c *gin.Context) {
		if p == nil || p.limiter == nil || p.settingService == nil {
			c.Next()
			return
		}
		settings := p.settingService.GetPanelRateLimitSettingsCached(c.Request.Context())
		clientIP := SecurityClientIP(c)
		if !settings.Enabled || settings.PublicIPRPM <= 0 || !isPubliclyRoutableClientIP(clientIP) {
			c.Next()
			return
		}
		result, err := p.limiter.Allow(c.Request.Context(), "panel:public:ip:"+clientIP, settings.PublicIPRPM, panelRateLimitWindow)
		if err != nil {
			slog.Warn("panel public rate limit check failed, allowing request", "error", err)
			c.Next()
			return
		}
		if !result.Allowed {
			abortPanelRateLimited(c, result.RetryAfter)
			return
		}
		c.Next()
	}
}

func isPubliclyRoutableClientIP(value string) bool {
	ip := net.ParseIP(value)
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}

func abortPanelRateLimited(c *gin.Context, retryAfter time.Duration) {
	if retryAfter <= 0 {
		retryAfter = panelRateLimitWindow
	}
	seconds := int64((retryAfter + time.Second - 1) / time.Second)
	c.Header("Retry-After", strconv.FormatInt(seconds, 10))
	AbortWithError(c, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests, please slow down and try again later")
}
