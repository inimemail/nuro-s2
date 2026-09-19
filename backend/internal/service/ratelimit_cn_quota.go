package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

func isCNProviderQuotaExhausted403(account *Account, body []byte, message string) bool {
	if account == nil || !account.IsCNProvider() || !account.IsCodingPlan() {
		return false
	}
	message = strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(message, "usage limit") || strings.Contains(message, "quota will reset") ||
		strings.EqualFold(gjson.GetBytes(body, "error.type").String(), "access_terminated_error")
}

// A confirmed exhausted weekly window must not be released at an earlier 5h
// reset. Missing window evidence uses the existing bounded temporary cooldown.
func cnQuota403Reset(account *Account, now time.Time) time.Time {
	until := now.Add(time.Duration(openAI403CooldownMinutesDefault) * time.Minute)
	if account == nil {
		return until
	}
	var exhaustedReset time.Time
	for _, window := range []struct{ used, reset string }{
		{cnExtraSuffix5hUsed, cnExtraSuffix5hReset},
		{cnExtraSuffixWeeklyUsed, cnExtraSuffixWeeklyReset},
	} {
		used, ok := cnParseF64(account.Extra[cnExtraKey(account.Platform, window.used)])
		reset, resetErr := parseSchedulingTime(cnNormalizeResetTime(account.Extra[cnExtraKey(account.Platform, window.reset)]))
		if ok && used >= 100 && resetErr == nil && reset.After(now) && reset.After(exhaustedReset) {
			exhaustedReset = reset
		}
	}
	if !exhaustedReset.IsZero() {
		return exhaustedReset
	}
	return until
}

func (s *RateLimitService) handleCNQuota403(ctx context.Context, account *Account) {
	const reason = "cn_quota_exhausted"
	until := cnQuota403Reset(account, time.Now())
	s.notifyAccountSchedulingBlocked(account, until, reason)
	if err := s.accountRepo.SetRateLimited(ctx, account.ID, until); err == nil {
		return
	} else {
		slog.Warn("cn_quota_rate_limit_failed", "account_id", account.ID, "error", err)
	}
	// Never turn recoverable quota exhaustion into an authentication failure.
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
		slog.Warn("cn_quota_temp_pause_failed", "account_id", account.ID, "error", err)
	}
}
