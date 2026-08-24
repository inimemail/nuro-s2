package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"golang.org/x/sync/singleflight"
)

type monitorQuotaCacheEntry struct {
	snapshot *domain.MonitorQuotaSnapshot
	expires  time.Time
}

type ChannelMonitorQuotaFetcher struct {
	accounts  AccountRepository
	usage     *AccountUsageService
	cnQuota   *CNProviderQuotaService
	cnBalance *CNProviderBalanceService
	mu        sync.Mutex
	cache     map[int64]monitorQuotaCacheEntry
	flight    singleflight.Group
}

func NewChannelMonitorQuotaFetcher(accounts AccountRepository, usage *AccountUsageService, cnQuota *CNProviderQuotaService, cnBalance *CNProviderBalanceService) *ChannelMonitorQuotaFetcher {
	return &ChannelMonitorQuotaFetcher{accounts: accounts, usage: usage, cnQuota: cnQuota, cnBalance: cnBalance, cache: make(map[int64]monitorQuotaCacheEntry)}
}

func (f *ChannelMonitorQuotaFetcher) LoadAccount(ctx context.Context, id int64) (*Account, error) {
	if f == nil || f.accounts == nil {
		return nil, fmt.Errorf("quota fetcher is not configured")
	}
	return f.accounts.GetByID(ctx, id)
}

func (f *ChannelMonitorQuotaFetcher) Fetch(ctx context.Context, id int64) *domain.MonitorQuotaSnapshot {
	if f == nil {
		return monitorQuotaError("usage", fmt.Errorf("quota fetcher is not configured"), time.Now())
	}
	now := time.Now()
	f.mu.Lock()
	if cached, ok := f.cache[id]; ok && now.Before(cached.expires) {
		f.mu.Unlock()
		copy := *cached.snapshot
		return &copy
	}
	f.mu.Unlock()

	resultCh := f.flight.DoChan("monitor-quota:"+strconv.FormatInt(id, 10), func() (any, error) {
		fetchCtx, cancel := context.WithTimeout(context.Background(), monitorQuotaFetchTimeout)
		defer cancel()
		snapshot := f.fetch(fetchCtx, id, time.Now())
		ttl := monitorQuotaFetchCacheTTL
		if !snapshot.Success {
			ttl = monitorQuotaErrorCacheTTL
		}
		f.mu.Lock()
		f.cache[id] = monitorQuotaCacheEntry{snapshot: snapshot, expires: time.Now().Add(ttl)}
		f.mu.Unlock()
		return snapshot, nil
	})
	select {
	case <-ctx.Done():
		return monitorQuotaError("usage", ctx.Err(), now)
	case result := <-resultCh:
		snapshot, ok := result.Val.(*domain.MonitorQuotaSnapshot)
		if result.Err != nil || !ok || snapshot == nil {
			return monitorQuotaError("usage", result.Err, now)
		}
		copy := *snapshot
		return &copy
	}
}

func (f *ChannelMonitorQuotaFetcher) fetch(ctx context.Context, id int64, now time.Time) *domain.MonitorQuotaSnapshot {
	account, err := f.LoadAccount(ctx, id)
	if err != nil {
		return monitorQuotaError("usage", err, now)
	}
	if account.IsCNProvider() {
		if account.IsCodingPlan() {
			if f.cnQuota == nil {
				return monitorQuotaError("cn_quota", fmt.Errorf("quota service unavailable"), now)
			}
			result, err := f.cnQuota.QueryUsageForAccount(ctx, account)
			if err != nil {
				return monitorQuotaError("cn_quota", err, now)
			}
			snapshot := &domain.MonitorQuotaSnapshot{Source: "cn_quota", Success: result.Success, PlanLevel: result.PlanLevel, CredentialInvalid: result.StatusCode == 401 || result.StatusCode == 403, Error: result.Error, FetchedAt: now}
			for _, tier := range result.Tiers {
				snapshot.Tiers = append(snapshot.Tiers, domain.MonitorQuotaTier{Window: tier.Window, UsedPercent: tier.UsedPercent, ResetAt: tier.ResetAt})
			}
			return snapshot
		}
		if f.cnBalance == nil {
			return monitorQuotaError("cn_balance", fmt.Errorf("balance service unavailable"), now)
		}
		result, err := f.cnBalance.QueryBalanceForAccount(ctx, account)
		if err != nil {
			return monitorQuotaError("cn_balance", err, now)
		}
		snapshot := &domain.MonitorQuotaSnapshot{Source: "cn_balance", Success: result.Success, Balance: &result.Balance, Currency: result.Currency, BalanceLow: result.Success && (!result.Available || result.Balance <= 0), CredentialInvalid: result.StatusCode == 401 || result.StatusCode == 403, Error: result.Error, FetchedAt: now}
		for _, entry := range result.Balances {
			snapshot.Balances = append(snapshot.Balances, domain.MonitorBalance{Currency: entry.Currency, Balance: entry.Balance})
		}
		return snapshot
	}
	if f.usage == nil {
		return monitorQuotaError("usage", fmt.Errorf("usage service unavailable"), now)
	}
	usage, err := f.usage.GetUsageForAccount(ctx, account)
	if err != nil {
		return monitorQuotaError("usage", err, now)
	}
	if usage == nil {
		return monitorQuotaError("usage", fmt.Errorf("usage service returned no data"), now)
	}
	if snapshot := monitorQuotaSnapshotFromUsage(usage, now); snapshot != nil {
		return snapshot
	}
	snapshot := &domain.MonitorQuotaSnapshot{Source: "usage", Success: true, FetchedAt: now}
	appendProgress := func(window, label string, progress *UsageProgress) {
		if progress == nil {
			return
		}
		reset := ""
		if progress.ResetsAt != nil {
			reset = progress.ResetsAt.UTC().Format(time.RFC3339)
		}
		snapshot.Tiers = append(snapshot.Tiers, domain.MonitorQuotaTier{Window: window, Label: label, UsedPercent: progress.Utilization, Used: float64(progress.UsedRequests), Limit: float64(progress.LimitRequests), ResetAt: reset})
	}
	appendProgress("5h", "", usage.FiveHour)
	appendProgress("7d", "", usage.SevenDay)
	appendProgress("7d-sonnet", "", usage.SevenDaySonnet)
	appendProgress("7d-fable", "", usage.SevenDayFable)
	appendProgress("daily", "shared", usage.GeminiSharedDaily)
	appendProgress("daily", "pro", usage.GeminiProDaily)
	appendProgress("daily", "flash", usage.GeminiFlashDaily)
	for model, quota := range usage.AntigravityQuota {
		if quota != nil {
			snapshot.Tiers = append(snapshot.Tiers, domain.MonitorQuotaTier{Window: "total", Label: model, UsedPercent: float64(quota.Utilization), ResetAt: quota.ResetTime})
		}
	}
	return snapshot
}

// monitorQuotaSnapshotFromUsage converts the account usage service's
// intentionally degraded diagnostic states into an explicit failed snapshot.
// A nil result means the usage is healthy and can be expanded into tiers.
func monitorQuotaSnapshotFromUsage(usage *UsageInfo, now time.Time) *domain.MonitorQuotaSnapshot {
	if usage == nil || (usage.ErrorCode == "" && strings.TrimSpace(usage.Error) == "" && !usage.IsForbidden && !usage.NeedsReauth) {
		return nil
	}
	message := strings.TrimSpace(usage.Error)
	if message == "" {
		message = strings.TrimSpace(usage.ErrorCode)
	}
	if message == "" {
		message = "quota data unavailable"
	}
	return &domain.MonitorQuotaSnapshot{
		Source:            "usage",
		Success:           false,
		CredentialInvalid: usage.IsForbidden || usage.NeedsReauth || usage.ErrorCode == "forbidden" || usage.ErrorCode == "unauthenticated",
		Error:             truncateMessage(sanitizeErrorMessage(message)),
		FetchedAt:         now,
	}
}

func monitorQuotaError(source string, err error, now time.Time) *domain.MonitorQuotaSnapshot {
	message := "quota fetch failed"
	if err != nil {
		message = truncateMessage(sanitizeErrorMessage(err.Error()))
	}
	lower := strings.ToLower(message)
	credentialInvalid := strings.Contains(lower, "401") || strings.Contains(lower, "403") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "forbidden") || strings.Contains(lower, "authentication")
	return &domain.MonitorQuotaSnapshot{Source: source, Success: false, CredentialInvalid: credentialInvalid, Error: message, FetchedAt: now}
}

func deriveQuotaCheckResult(snapshot *domain.MonitorQuotaSnapshot, model string, checkedAt time.Time) *CheckResult {
	result := &CheckResult{Model: model, CheckedAt: checkedAt}
	if snapshot == nil {
		result.Status = MonitorStatusError
		result.Message = "quota snapshot missing"
		return result
	}
	switch {
	case !snapshot.Success && snapshot.CredentialInvalid:
		result.Status = MonitorStatusFailed
		result.Message = snapshot.Error
	case !snapshot.Success && strings.Contains(snapshot.Error, "linked account not found"):
		result.Status = MonitorStatusDegraded
		result.Message = snapshot.Error
	case !snapshot.Success:
		result.Status = MonitorStatusError
		result.Message = snapshot.Error
	default:
		for _, tier := range snapshot.Tiers {
			if tier.UsedPercent >= monitorQuotaDegradedUsedPercent {
				result.Status = MonitorStatusDegraded
				result.Message = fmt.Sprintf("quota high: %s at %.1f%%", tier.Window, tier.UsedPercent)
				return result
			}
		}
		if snapshot.BalanceLow {
			result.Status = MonitorStatusDegraded
			result.Message = "balance low"
		} else {
			result.Status = MonitorStatusOperational
		}
	}
	return result
}
