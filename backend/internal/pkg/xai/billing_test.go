package xai

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func billingFloat(value float64) *float64 {
	return &value
}

func TestBuildBillingSummaryWeekly(t *testing.T) {
	summary := BuildBillingSummary(&BillingConfig{
		CurrentPeriod:      &BillingPeriod{Type: "weekly", Start: "2026-07-13", End: "2026-07-20"},
		CreditUsagePercent: billingFloat(37.5),
		ProductUsage: []BillingProductUsage{
			{Product: "grok", UsagePercent: billingFloat(25)},
			{Product: "  ", UsagePercent: billingFloat(99)},
		},
	})

	require.NotNil(t, summary)
	require.Equal(t, "weekly", summary.PeriodType)
	require.Equal(t, "2026-07-13", summary.PeriodStart)
	require.Equal(t, "2026-07-20", summary.PeriodEnd)
	require.InDelta(t, 37.5, *summary.UsagePercent, 1e-12)
	require.Len(t, summary.ProductUsage, 1)
	require.Equal(t, "grok", summary.ProductUsage[0].Product)
}

func TestBuildBillingSummaryMonthlyParsesCentsAndPlan(t *testing.T) {
	summary := BuildBillingSummary(&BillingConfig{
		MonthlyLimit:       json.RawMessage(`{"val":"15000"}`),
		Used:               json.RawMessage(`18000`),
		BillingPeriodStart: "2026-07-01",
		BillingPeriodEnd:   "2026-08-01",
	})

	require.NotNil(t, summary)
	require.Equal(t, "monthly", summary.PeriodType)
	require.Equal(t, "SuperGrok", summary.Plan)
	require.InDelta(t, 15000, *summary.MonthlyLimitCents, 1e-12)
	require.InDelta(t, 18000, *summary.UsedCents, 1e-12)
	require.InDelta(t, 15000, *summary.IncludedUsedCents, 1e-12)
	require.InDelta(t, 100, *summary.UsedPercent, 1e-12)
}

func TestBuildBillingSummaryKeepsSuccessfulEmptyConfigObservable(t *testing.T) {
	summary := BuildBillingSummary(&BillingConfig{})

	require.NotNil(t, summary)
	require.Empty(t, summary.Plan)
	require.Nil(t, summary.MonthlyLimitCents)
	require.Nil(t, summary.UsagePercent)
}

func TestMergeBillingProbeResultRetainsFailedWindow(t *testing.T) {
	previous := &BillingSummary{
		MonthlyLimitCents: billingFloat(15000),
		UsedPercent:       billingFloat(40),
		Plan:              "SuperGrok",
		UpdatedAt:         "2026-07-14T00:00:00Z",
	}
	weekly := &BillingSummary{
		PeriodType:   "weekly",
		UsagePercent: billingFloat(12),
	}

	merged := MergeBillingProbeResult(previous, weekly, nil, true, false)

	require.NotNil(t, merged)
	require.True(t, merged.Partial)
	require.Equal(t, []string{"monthly"}, merged.FailedWindows)
	require.InDelta(t, 12, *merged.UsagePercent, 1e-12)
	require.InDelta(t, 15000, *merged.MonthlyLimitCents, 1e-12)
	require.Equal(t, "SuperGrok", merged.Plan)
	require.NotEmpty(t, merged.WeeklyUpdatedAt)
}

func TestStampBillingSummaryAddsMetadata(t *testing.T) {
	summary := StampBillingSummary(&BillingSummary{}, 200, "billing_probe")

	require.Equal(t, 200, summary.StatusCode)
	require.Equal(t, "billing_probe", summary.Source)
	require.NotEmpty(t, summary.FetchedAt)
	require.Equal(t, summary.FetchedAt, summary.UpdatedAt)
}

func TestBuildBillingURLWithValidatorUsesValidatedBaseURL(t *testing.T) {
	got, err := BuildBillingURLWithValidator("https://relay.example/v1", true, func(raw string) (string, error) {
		require.Equal(t, "https://relay.example/v1", raw)
		return raw, nil
	})
	require.NoError(t, err)
	require.Equal(t, "https://relay.example/v1"+BillingWeeklyPath, got)
}

func TestBuildBillingURLWithValidatorRejectsInvalidBaseURL(t *testing.T) {
	_, err := BuildBillingURLWithValidator("https://relay.example/v1", false, func(string) (string, error) {
		return "", fmt.Errorf("rejected")
	})
	require.Error(t, err)
}
