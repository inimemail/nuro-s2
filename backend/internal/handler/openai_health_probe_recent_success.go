package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	openAIHealthProbeRecentSuccessPrefix        = "sub2api:openai-health-probe:recent-success:"
	openAIHealthProbeRecentSuccessLookupTimeout = 100 * time.Millisecond
	openAIHealthProbeRecentSuccessWriteTimeout  = 200 * time.Millisecond
)

func openAIHealthProbeRuntime(cfg *config.Config) config.OpenAIHealthProbeRuntime {
	if cfg == nil {
		return config.OpenAIHealthProbeRuntime{
			RecentSuccessEnabled: true, RecentSuccessTTLSeconds: 60,
			MaxAccountSwitches: 4, TotalTimeoutSeconds: 40,
		}
	}
	return cfg.OpenAIHealthProbe()
}

func openAIHealthProbeRecentSuccessKey(apiKeyID int64, platform, model string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(platform) + "\x00" + strings.TrimSpace(model)))
	return openAIHealthProbeRecentSuccessPrefix + strconv.FormatInt(apiKeyID, 10) + ":" + hex.EncodeToString(sum[:])
}

func (h *OpenAIGatewayHandler) tryServeOpenAIHealthProbeRecentSuccess(c *gin.Context, apiKey *service.APIKey, platform, model string) bool {
	if h == nil || h.redisClient == nil || c == nil || apiKey == nil || apiKey.ID <= 0 {
		return false
	}
	profile := openAIHealthProbeRuntime(h.cfg)
	if !profile.RecentSuccessEnabled || profile.RecentSuccessTTLSeconds <= 0 {
		return false
	}
	ctx := context.Background()
	if c.Request != nil {
		ctx = c.Request.Context()
	}
	lookupCtx, cancel := context.WithTimeout(ctx, openAIHealthProbeRecentSuccessLookupTimeout)
	defer cancel()
	value, err := h.redisClient.Get(lookupCtx, openAIHealthProbeRecentSuccessKey(apiKey.ID, platform, model)).Result()
	if err != nil {
		return false
	}
	unix, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	age := time.Now().Unix() - unix
	if err != nil || unix <= 0 || age < 0 || age > int64(profile.RecentSuccessTTLSeconds) {
		return false
	}
	c.Header("X-Sub2API-Health-Probe-Source", "recent-success")
	c.Header("X-Sub2API-Health-Probe-Age", strconv.FormatInt(age, 10))
	c.JSON(http.StatusOK, map[string]any{
		"id": "resp_health_cached_" + uuid.NewString(), "object": "response", "status": "completed",
		"model": model, "output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "MONITOR_OK"}}}},
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
	})
	return true
}

func (h *OpenAIGatewayHandler) recordOpenAIHealthProbeRecentSuccess(apiKeyID int64, platform, model string) {
	if h == nil || h.redisClient == nil || apiKeyID <= 0 {
		return
	}
	profile := openAIHealthProbeRuntime(h.cfg)
	if !profile.RecentSuccessEnabled || profile.RecentSuccessTTLSeconds <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), openAIHealthProbeRecentSuccessWriteTimeout)
	defer cancel()
	_ = h.redisClient.Set(ctx, openAIHealthProbeRecentSuccessKey(apiKeyID, platform, model), strconv.FormatInt(time.Now().Unix(), 10), time.Duration(profile.RecentSuccessTTLSeconds)*time.Second).Err()
}
