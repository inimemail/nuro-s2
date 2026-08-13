package xai

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const GrokQuotaSignalMaxAge = 24 * time.Hour

const (
	grok45ResponsesModel             = "grok-4.5"
	grokHeavyQuotaRequestLimit int64 = 8_300
	grokHeavyQuotaTokenLimit   int64 = 53_000_000
)

func MapJWTSubscriptionTier(tier uint64) string {
	switch tier {
	case 0:
		return "free"
	case 1:
		return "supergrok"
	case 2:
		return "x_basic"
	case 3:
		return "x_premium"
	case 4:
		return "x_premium_plus"
	case 5:
		return "supergrok_heavy"
	case 6:
		return "supergrok_lite"
	case 7:
		return "supergrok_plus"
	default:
		return strconv.FormatUint(tier, 10)
	}
}

func NormalizeSubscriptionTier(raw string) string {
	tier := strings.ToLower(strings.TrimSpace(raw))
	tier = strings.ReplaceAll(tier, "-", "_")
	tier = strings.Join(strings.Fields(tier), "_")
	switch tier {
	case "free", "grok_free", "grokfree", "free_tier", "freetier", "grok_basic", "grokbasic":
		return "free"
	case "supergrok", "grokpro":
		return "supergrok"
	case "supergrok_lite", "supergroklite":
		return "supergrok_lite"
	case "supergrok_heavy", "supergrokheavy":
		return "supergrok_heavy"
	case "supergrok_pro", "supergrokpro":
		return "supergrok_pro"
	case "supergrok_plus", "supergrokplus":
		return "supergrok_plus"
	case "x_basic", "xbasic", "basic":
		return "x_basic"
	case "x_premium", "xpremium":
		return "x_premium"
	case "x_premium_plus", "xpremiumplus", "x_premium+":
		return "x_premium_plus"
	default:
		return tier
	}
}

func SubscriptionTierFromJWT(token string) string {
	claims := DecodeJWTClaims(token)
	raw, ok := claims["tier"]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case float64:
		if value < 0 {
			return ""
		}
		return MapJWTSubscriptionTier(uint64(value))
	case json.Number:
		n, err := value.Int64()
		if err != nil || n < 0 {
			return NormalizeSubscriptionTier(value.String())
		}
		return MapJWTSubscriptionTier(uint64(n))
	case string:
		value = strings.TrimSpace(value)
		if value == "" {
			return ""
		}
		if n, err := strconv.ParseUint(value, 10, 64); err == nil {
			return MapJWTSubscriptionTier(n)
		}
		return NormalizeSubscriptionTier(value)
	default:
		return ""
	}
}

func CanonicalGrokPlan(monthlyLimitCents *float64, subscriptionTier string, quota *QuotaSnapshot) string {
	if plan := resolvePlan(monthlyLimitCents); plan != "" {
		return NormalizeSubscriptionTier(plan)
	}
	normalized := NormalizeSubscriptionTier(subscriptionTier)
	switch normalized {
	case "free", "x_basic":
		return "free"
	case "supergrok_heavy", "supergrok_lite", "supergrok_plus":
		return normalized
	}
	if normalized == "supergrok" || normalized == "supergrok_pro" || normalized == "paid" || normalized == "pro" {
		if hint := Grok45ResponsesPlanHint(quota, time.Time{}); hint != "" {
			return hint
		}
		return "supergrok"
	}
	return ""
}

func IsGrok45ResponsesQuotaModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(StripGrokProviderPrefix(model)))
	return model == grok45ResponsesModel || strings.HasPrefix(model, grok45ResponsesModel+"-")
}

func Grok45ResponsesPlanHint(quota *QuotaSnapshot, now time.Time) string {
	if quota == nil {
		return ""
	}
	if plan := NormalizeSubscriptionTier(quota.PlanFrom45Responses); plan == "supergrok" || plan == "supergrok_heavy" {
		if quotaTimestampFresh(quota.PlanFrom45ResponsesAt, now) {
			return plan
		}
	}
	if !IsGrok45ResponsesQuotaModel(quota.Model) || !IsQuotaSnapshotFresh(quota, now) {
		return ""
	}
	if quotaLooksLikeGrokHeavy(quota) {
		return "supergrok_heavy"
	}
	return ""
}

func (s *QuotaSnapshot) ApplyGrok45ResponsesPlanSignal(previous *QuotaSnapshot) {
	if s == nil {
		return
	}
	observedAt := firstNonEmptyQuotaTime(s.LastHeadersSeenAt, s.UpdatedAt)
	if IsGrok45ResponsesQuotaModel(s.Model) && quotaHasLimitWindow(s) {
		s.PlanFrom45Responses = "supergrok"
		if quotaLooksLikeGrokHeavy(s) {
			s.PlanFrom45Responses = "supergrok_heavy"
		}
		s.PlanFrom45ResponsesAt = observedAt
		return
	}
	if previous != nil && strings.TrimSpace(previous.PlanFrom45Responses) != "" {
		s.PlanFrom45Responses = previous.PlanFrom45Responses
		s.PlanFrom45ResponsesAt = previous.PlanFrom45ResponsesAt
	}
}

func IsQuotaSnapshotFresh(snapshot *QuotaSnapshot, now time.Time) bool {
	if snapshot == nil {
		return false
	}
	return quotaTimestampFresh(firstNonEmptyQuotaTime(snapshot.LastHeadersSeenAt, snapshot.UpdatedAt), now)
}

func quotaTimestampFresh(raw string, now time.Time) bool {
	observedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	age := now.Sub(observedAt)
	return age <= GrokQuotaSignalMaxAge && age >= -5*time.Minute
}

func quotaHasLimitWindow(quota *QuotaSnapshot) bool {
	return quota != nil && ((quota.Requests != nil && quota.Requests.Limit != nil) || (quota.Tokens != nil && quota.Tokens.Limit != nil))
}

func quotaLooksLikeGrokHeavy(quota *QuotaSnapshot) bool {
	var requests, tokens int64
	if quota != nil && quota.Requests != nil && quota.Requests.Limit != nil {
		requests = *quota.Requests.Limit
	}
	if quota != nil && quota.Tokens != nil && quota.Tokens.Limit != nil {
		tokens = *quota.Tokens.Limit
	}
	return requests >= grokHeavyQuotaRequestLimit || tokens >= grokHeavyQuotaTokenLimit
}

func firstNonEmptyQuotaTime(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
