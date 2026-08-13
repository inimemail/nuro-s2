package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

type GrokQuotaFetcher struct{}

func NewGrokQuotaFetcher() *GrokQuotaFetcher {
	return &GrokQuotaFetcher{}
}

func grokBillingSnapshotFromExtra(extra map[string]any) (*xai.BillingSummary, error) {
	if extra == nil {
		return nil, nil
	}
	raw, ok := extra[grokBillingExtraKey]
	if !ok || raw == nil {
		return nil, nil
	}
	if snapshot, ok := raw.(*xai.BillingSummary); ok {
		return snapshot, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal grok billing snapshot: %w", err)
	}
	var out xai.BillingSummary
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (f *GrokQuotaFetcher) BuildUsageInfo(account *Account) *UsageInfo {
	now := time.Now()
	usage := &UsageInfo{
		Source:             "passive",
		UpdatedAt:          &now,
		GrokFreeTokenLimit: xai.GrokFreeRolling24hTokenLimit,
	}
	if account == nil {
		usage.ErrorCode = "quota_unknown"
		usage.Error = "Quota is not available yet"
		return usage
	}

	billing, _ := grokBillingSnapshotFromExtra(account.Extra)
	if billing != nil {
		usage.GrokBilling = billing
		if billing.Plan != "" {
			usage.SubscriptionTier = billing.Plan
			usage.SubscriptionTierRaw = billing.Plan
		}
		if parsedAt, parseErr := time.Parse(time.RFC3339, billing.UpdatedAt); parseErr == nil {
			usage.UpdatedAt = &parsedAt
		}
		usage.GrokLastStatusCode = billing.StatusCode
		usage.GrokQuotaSnapshotState = "billing_observed"
	}
	snapshot, err := grokQuotaSnapshotFromExtra(account.Extra)
	if err != nil || snapshot == nil {
		applyGrokResolvedSubscriptionTier(usage, account, billing, nil)
		if billing == nil {
			usage.ErrorCode = "quota_unknown"
			usage.Error = "Quota is not available yet"
		}
		return usage
	}
	applyGrokResolvedSubscriptionTier(usage, account, billing, snapshot)

	if parsedAt, err := time.Parse(time.RFC3339, snapshot.UpdatedAt); err == nil {
		usage.UpdatedAt = &parsedAt
	}
	usage.GrokRequestQuota = snapshot.Requests
	usage.GrokTokenQuota = snapshot.Tokens
	usage.GrokRetryAfterSeconds = snapshot.RetryAfterSeconds
	if usage.SubscriptionTier == "" {
		usage.SubscriptionTier = snapshot.SubscriptionTier
		usage.SubscriptionTierRaw = snapshot.SubscriptionTier
	}
	usage.GrokEntitlementStatus = snapshot.EntitlementStatus
	usage.GrokLastQuotaProbeAt = snapshot.LastProbeAt
	usage.GrokLastHeadersSeenAt = snapshot.LastHeadersSeenAt
	if snapshot.StatusCode >= http.StatusBadRequest || usage.GrokLastStatusCode == 0 {
		usage.GrokLastStatusCode = snapshot.StatusCode
	}
	if snapshot.HasObservedHeaders() {
		usage.GrokQuotaSnapshotState = "observed"
	} else {
		usage.GrokQuotaSnapshotState = "no_headers"
		usage.ErrorCode = "quota_unknown"
		usage.Error = "No xAI quota headers observed on the latest Grok probe"
	}

	switch snapshot.StatusCode {
	case 401:
		usage.NeedsReauth = true
		usage.ErrorCode = "unauthenticated"
	case 403:
		usage.IsForbidden = true
		usage.ForbiddenType = "forbidden"
		usage.ErrorCode = "forbidden"
		if usage.GrokEntitlementStatus == "" {
			usage.GrokEntitlementStatus = "forbidden"
		}
	case 429:
		usage.ErrorCode = "rate_limited"
	}
	return usage
}

func applyGrokResolvedSubscriptionTier(usage *UsageInfo, account *Account, billing *xai.BillingSummary, snapshot *xai.QuotaSnapshot) {
	if usage == nil || account == nil {
		return
	}
	if jwtTier := xai.SubscriptionTierFromJWT(account.GetCredential("access_token")); jwtTier != "" {
		usage.SubscriptionTier = jwtTier
		usage.SubscriptionTierRaw = jwtTier
		return
	}
	signal := strings.TrimSpace(account.GetCredential("subscription_tier"))
	if signal == "" && snapshot != nil {
		signal = strings.TrimSpace(snapshot.SubscriptionTier)
	}
	if signal == "" && billing != nil {
		signal = strings.TrimSpace(billing.Plan)
	}
	var monthlyLimit *float64
	if billing != nil {
		monthlyLimit = billing.MonthlyLimitCents
	}
	if plan := xai.CanonicalGrokPlan(monthlyLimit, signal, snapshot); plan != "" {
		usage.SubscriptionTier = plan
		usage.SubscriptionTierRaw = plan
		return
	}
	if usage.SubscriptionTier == "" && signal != "" {
		usage.SubscriptionTier = signal
		usage.SubscriptionTierRaw = signal
	}
}

func stampGrokQuotaSnapshotForPlan(account *Account, snapshot *xai.QuotaSnapshot, model string) {
	if snapshot == nil {
		return
	}
	if strings.TrimSpace(snapshot.Model) == "" && strings.TrimSpace(model) != "" {
		snapshot.Model = xai.ResolveGrokTextResponsesModelID(model)
	}
	var previous *xai.QuotaSnapshot
	if account != nil {
		previous, _ = grokQuotaSnapshotFromExtra(account.Extra)
	}
	snapshot.ApplyGrok45ResponsesPlanSignal(previous)
}

func grokQuotaSnapshotFromExtra(extra map[string]any) (*xai.QuotaSnapshot, error) {
	if extra == nil {
		return nil, nil
	}
	raw, ok := extra[grokQuotaSnapshotExtraKey]
	if !ok || raw == nil {
		return nil, nil
	}
	switch snapshot := raw.(type) {
	case *xai.QuotaSnapshot:
		return snapshot, nil
	case xai.QuotaSnapshot:
		return &snapshot, nil
	case map[string]any:
		data, err := json.Marshal(snapshot)
		if err != nil {
			return nil, err
		}
		var out xai.QuotaSnapshot
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, err
		}
		return &out, nil
	default:
		data, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("marshal grok quota snapshot: %w", err)
		}
		var out xai.QuotaSnapshot
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, err
		}
		return &out, nil
	}
}
