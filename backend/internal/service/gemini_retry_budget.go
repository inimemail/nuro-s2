package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
)

func geminiSameAccountMaxAttempts(cfg *config.Config) int {
	if cfg == nil || cfg.Gateway.MaxSameAccountRetriesGemini == nil {
		return geminiMaxRetries
	}
	n := *cfg.Gateway.MaxSameAccountRetriesGemini
	if n < 0 || n > 10 {
		return geminiMaxRetries // Config.Validate rejects this on startup.
	}
	return n + 1
}

func geminiBoundPoolRetryable(ctx context.Context, account *Account, status int) bool {
	return isGeminiCacheBoundRequest(ctx) && account != nil && account.IsPoolMode() &&
		(status == 429 || status >= 500 && status <= 599) && account.IsPoolModeRetryableStatus(status)
}

func (s *GeminiMessagesCompatService) shouldRetryGeminiUpstreamErrorForRequest(ctx context.Context, account *Account, status int) bool {
	if isGeminiCacheBoundRequest(ctx) && account != nil && account.IsPoolMode() {
		return geminiBoundPoolRetryable(ctx, account, status)
	}
	return s.shouldRetryGeminiUpstreamError(account, status)
}
