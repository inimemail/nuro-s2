package service

import (
	"context"
	"log/slog"
	"math"
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

// Short cooldown is allowed only with positive evidence for every known window.
// Missing, malformed, stale or non-finite data never weakens quota protection.
func cnCodingPlanFrequency429(account *Account, body []byte, now time.Time) bool {
	if account == nil || !account.IsCNProvider() || !account.IsCodingPlan() {
		return false
	}
	text := strings.ToLower(string(body))
	for _, marker := range []string{"额度", "已用尽", "用完", "quota", "usage limit", "insufficient", "balance"} {
		if strings.Contains(text, marker) {
			return false
		}
	}
	frequency := false
	for _, marker := range []string{"速率限制", "请求频率", "访问频率", "请求过于频繁", "请求太频繁", "too many requests", "rate limit exceeded", "rate_limit_exceeded", "ratelimit exceeded"} {
		if strings.Contains(text, marker) {
			frequency = true
			break
		}
	}
	if !frequency {
		return false
	}
	stamp, err := time.Parse(time.RFC3339, cnNormalizeResetTime(account.Extra[cnExtraKey(account.Platform, cnExtraSuffixUsageUpdated)]))
	if err != nil || stamp.After(now) || now.Sub(stamp) > 20*time.Minute {
		return false
	}
	for _, key := range []string{cnExtraSuffix5hUsed, cnExtraSuffixWeeklyUsed} {
		used, ok := cnParseF64(account.Extra[cnExtraKey(account.Platform, key)])
		if !ok || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used >= 90 {
			return false
		}
	}
	return true
}
