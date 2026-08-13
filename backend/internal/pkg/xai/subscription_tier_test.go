package xai

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testJWTWithPayload(payload string) string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
}

func TestSubscriptionTierFromJWTMapsNumericAndNamedTiers(t *testing.T) {
	require.Equal(t, "supergrok_heavy", SubscriptionTierFromJWT(testJWTWithPayload(`{"tier":5}`)))
	require.Equal(t, "x_premium_plus", SubscriptionTierFromJWT(testJWTWithPayload(`{"tier":"x-premium-plus"}`)))
	require.Empty(t, SubscriptionTierFromJWT(testJWTWithPayload(`{"other":"value"}`)))
}

func TestGrok45ResponsesPlanHintRequiresFreshSignal(t *testing.T) {
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	requests := grokHeavyQuotaRequestLimit
	quota := &QuotaSnapshot{
		Model:             grok45ResponsesModel,
		Requests:          &QuotaWindow{Limit: &requests},
		LastHeadersSeenAt: now.Add(-GrokQuotaSignalMaxAge + time.Minute).Format(time.RFC3339),
	}
	require.Equal(t, "supergrok_heavy", Grok45ResponsesPlanHint(quota, now))

	quota.LastHeadersSeenAt = now.Add(-GrokQuotaSignalMaxAge - time.Minute).Format(time.RFC3339)
	require.Empty(t, Grok45ResponsesPlanHint(quota, now))
}

func TestApplyGrok45ResponsesPlanSignalPreservesTimestampWithoutRefreshingIt(t *testing.T) {
	observedAt := "2026-08-12T08:00:00Z"
	previous := &QuotaSnapshot{
		PlanFrom45Responses:   "supergrok_heavy",
		PlanFrom45ResponsesAt: observedAt,
	}
	snapshot := &QuotaSnapshot{Model: "grok-4.6", UpdatedAt: "2026-08-13T08:00:00Z"}
	snapshot.ApplyGrok45ResponsesPlanSignal(previous)
	require.Equal(t, "supergrok_heavy", snapshot.PlanFrom45Responses)
	require.Equal(t, observedAt, snapshot.PlanFrom45ResponsesAt)
	require.Empty(t, Grok45ResponsesPlanHint(snapshot, time.Date(2026, time.August, 13, 9, 0, 0, 0, time.UTC)), "a carried signal must not gain freshness from snapshot updated_at")
}
