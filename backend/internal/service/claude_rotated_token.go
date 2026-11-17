package service

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

func (s *GatewayService) rotatedClaudeAccessToken(ctx context.Context, account *Account, rejectedToken string, retryDeadlines ...time.Time) string {
	if account == nil || account.Platform != PlatformAnthropic || account.Type != AccountTypeOAuth ||
		s.accountRepo == nil || rejectedToken == "" || ctx.Err() != nil {
		return ""
	}
	deadline := time.Now().Add(time.Second)
	if len(retryDeadlines) > 0 && retryDeadlines[0].Before(deadline) {
		deadline = retryDeadlines[0]
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	latest, err := s.accountRepo.GetByID(ctx, account.ID)
	if err != nil {
		slog.Warn("claude_oauth_401_reload_failed", "account_id", account.ID, "error", err)
		return ""
	}
	if latest == nil || latest.Platform != PlatformAnthropic || latest.Type != AccountTypeOAuth ||
		latest.Status != StatusActive || !latest.Schedulable {
		return ""
	}
	token := strings.TrimSpace(latest.GetCredential("access_token"))
	if token == "" || token == rejectedToken {
		return ""
	}
	if p := s.claudeTokenProvider; p != nil && p.tokenCache != nil {
		if err := p.tokenCache.DeleteAccessToken(ctx, ClaudeTokenCacheKey(latest)); err != nil {
			slog.Warn("claude_oauth_401_invalidate_cache_failed", "account_id", account.ID, "error", err)
		}
	}
	slog.Info("claude_oauth_rotated_token_retry", "account_id", account.ID)
	if ctx.Err() != nil {
		return ""
	}
	return token
}
