package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpsScheduledReportV162TemplateExposesSummaryMetrics(t *testing.T) {
	ctx := context.Background()
	svc := NewNotificationEmailService(newNotificationEmailMemorySettingRepo(), nil)

	for _, locale := range []string{"en", "zh"} {
		tmpl, err := svc.GetTemplate(ctx, NotificationEmailEventOpsScheduledReport, locale)
		require.NoError(t, err)
		for _, placeholder := range notificationEmailOpsSummaryPlaceholders {
			require.Contains(t, tmpl.Placeholders, placeholder)
			require.Contains(t, tmpl.HTML, "{{"+placeholder+"}}")
		}
		require.Contains(t, tmpl.Placeholders, "report_detail_display")

		preview, err := svc.PreviewTemplate(ctx, NotificationEmailPreviewInput{
			Event:  NotificationEmailEventOpsScheduledReport,
			Locale: locale,
		})
		require.NoError(t, err)
		require.Contains(t, preview.HTML, "2,374")
		require.Contains(t, preview.HTML, "99.86%")
		require.Contains(t, preview.HTML, "151,260 ms")
		require.Contains(t, preview.HTML, `style="display: none;"`)
	}
}

func TestOpsScheduledReportV162RuntimeDoesNotLeakPreviewMetrics(t *testing.T) {
	ctx := context.Background()
	svc := NewNotificationEmailService(newNotificationEmailMemorySettingRepo(), nil)

	variables := svc.runtimeVariables(ctx, NotificationEmailEventOpsScheduledReport, "en", NotificationEmailSendInput{})
	require.Equal(t, "none", variables["report_summary_display"])
	require.Equal(t, "block", variables["report_detail_display"])
	require.Empty(t, variables["report_html"])
	for _, placeholder := range notificationEmailOpsSummaryPlaceholders {
		if placeholder != "report_summary_display" {
			require.Equal(t, "-", variables[placeholder])
		}
	}
}

func TestOpsScheduledReportV162SummaryVariables(t *testing.T) {
	latencyP50 := 8231
	latencyP99 := 151260
	ttftP50 := 1674
	ttftP99 := 11222
	now := time.Date(2026, time.July, 19, 1, 0, 26, 0, time.UTC)
	report := &opsScheduledReport{
		Name:       "日报",
		ReportType: "daily_summary",
		TimeRange:  24 * time.Hour,
	}
	overview := &OpsDashboardOverview{
		RequestCountTotal:            2374,
		SuccessCount:                 1451,
		ErrorCountSLA:                2,
		BusinessLimitedCount:         921,
		SLA:                          0.9986,
		ErrorRate:                    0.0014,
		UpstreamErrorRate:            0.0028,
		UpstreamErrorCountExcl429529: 4,
		Upstream429Count:             0,
		Upstream529Count:             0,
		TokenConsumed:                121550190,
		Duration:                     OpsPercentiles{P50: &latencyP50, P99: &latencyP99},
		TTFT:                         OpsPercentiles{P50: &ttftP50, P99: &ttftP99},
		QPS:                          OpsRateSummary{Peak: 1.2},
		TPS:                          OpsRateSummary{Peak: 133421.2, Avg: 1406.8},
	}

	variables := opsSummaryReportEmailVariables(report, now, overview, "en")
	require.Equal(t, "Daily summary", variables["report_name"])
	require.Equal(t, "2,374", variables["report_total_requests"])
	require.Equal(t, "99.86%", variables["report_sla"])
	require.Equal(t, "151,260 ms", variables["report_latency_p99"])
	require.Equal(t, "121,550,190", variables["report_tokens"])
	require.Equal(t, "none", variables["report_detail_display"])

	rendered, err := renderNotificationEmail(
		NotificationEmailEventOpsScheduledReport,
		"Report",
		`<section>{{report_html}}</section>`,
		variables,
		map[string]string{"report_html": `<h2>generated summary</h2>`},
	)
	require.NoError(t, err)
	require.Contains(t, rendered.HTML, `<h2>generated summary</h2>`)
}

func TestFormatOpsReportIntegerV162(t *testing.T) {
	require.Equal(t, "2,374", formatOpsReportInteger(2374))
	require.Equal(t, "-1,234", formatOpsReportInteger(-1234))
	require.Equal(t, "42", formatOpsReportInteger(42))
}
