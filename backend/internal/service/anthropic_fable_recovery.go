package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"time"
)

type ownedModelRateLimitClearer interface {
	ClearModelRateLimitIfOwned(context.Context, int64, string, []byte) error
}

func recoverAnthropicFableWindowLimit(ctx context.Context, repo AccountRepository, account *Account, progress *UsageProgress) {
	if account == nil || account.Platform != PlatformAnthropic || progress == nil || progress.UtilizationUnknown || math.IsNaN(progress.Utilization) || math.IsInf(progress.Utilization, 0) || progress.Utilization < 0 || progress.Utilization >= 100 {
		return
	}
	// Missing, malformed or already expired windows cannot prove current quota
	// recovery, even when they carry a numeric utilization below 100 percent.
	if progress.ResetsAt == nil || !progress.ResetsAt.After(time.Now()) {
		return
	}
	limits, _ := account.Extra[modelRateLimitsKey].(map[string]any)
	observed, _ := limits[anthropicFableRateLimitKey].(map[string]any)
	if observed["reason"] != anthropicFableWindowReason {
		return
	}
	if owner, _ := observed["owner_token"].(string); owner == "" {
		return
	}
	clearer, ok := repo.(ownedModelRateLimitClearer)
	if !ok {
		return
	}
	expected, err := json.Marshal(observed)
	if err != nil {
		return
	}
	if err := clearer.ClearModelRateLimitIfOwned(ctx, account.ID, anthropicFableRateLimitKey, expected); err != nil {
		slog.Warn("anthropic_fable_recovery_failed", "account_id", account.ID, "error", err)
	}
}
