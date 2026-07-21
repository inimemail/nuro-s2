package service

var notificationEmailOpsSummaryPlaceholders = []string{
	"report_summary_display",
	"report_total_requests",
	"report_success_count",
	"report_sla_error_count",
	"report_business_limited_count",
	"report_sla",
	"report_error_rate",
	"report_upstream_error_rate",
	"report_upstream_error_count_excl_429_529",
	"report_upstream_429_count",
	"report_upstream_529_count",
	"report_latency_p50",
	"report_latency_p99",
	"report_ttft_p50",
	"report_ttft_p99",
	"report_tokens",
	"report_qps_current",
	"report_qps_peak",
	"report_qps_avg",
	"report_tps_current",
	"report_tps_peak",
	"report_tps_avg",
}

func init() {
	info := notificationEmailEventDefinitions[NotificationEmailEventOpsScheduledReport]
	placeholders := append([]string{}, notificationEmailCommonPlaceholders...)
	placeholders = append(placeholders, "report_name", "report_type", "report_start_time", "report_end_time")
	placeholders = append(placeholders, notificationEmailOpsSummaryPlaceholders...)
	placeholders = append(placeholders, "report_detail_display", "report_html")
	info.Placeholders = placeholders
	notificationEmailEventDefinitions[NotificationEmailEventOpsScheduledReport] = info

	templates := notificationEmailOfficialTemplates[NotificationEmailEventOpsScheduledReport]
	templates[notificationEmailDefaultLocale] = notificationEmailOfficialTemplate{
		Subject: "[Ops Report] {{report_name}}",
		HTML:    notificationEmailOpsScheduledReportTemplate(notificationEmailDefaultLocale),
	}
	templates[notificationEmailLocaleChinese] = notificationEmailOfficialTemplate{
		Subject: "[运维报表] {{report_name}}",
		HTML:    notificationEmailOpsScheduledReportTemplate(notificationEmailLocaleChinese),
	}
}

func addNotificationEmailOpsSummarySampleVariables(variables map[string]string) {
	variables["report_summary_display"] = "block"
	variables["report_detail_display"] = "none"
	variables["report_total_requests"] = "2,374"
	variables["report_success_count"] = "1,451"
	variables["report_sla_error_count"] = "2"
	variables["report_business_limited_count"] = "921"
	variables["report_sla"] = "99.86%"
	variables["report_error_rate"] = "0.14%"
	variables["report_upstream_error_rate"] = "0.28%"
	variables["report_upstream_error_count_excl_429_529"] = "4"
	variables["report_upstream_429_count"] = "0"
	variables["report_upstream_529_count"] = "0"
	variables["report_latency_p50"] = "8,231 ms"
	variables["report_latency_p99"] = "151,260 ms"
	variables["report_ttft_p50"] = "1,674 ms"
	variables["report_ttft_p99"] = "11,222 ms"
	variables["report_tokens"] = "121,550,190"
	variables["report_qps_current"] = "0.0"
	variables["report_qps_peak"] = "1.2"
	variables["report_qps_avg"] = "0.0"
	variables["report_tps_current"] = "0.0"
	variables["report_tps_peak"] = "133421.2"
	variables["report_tps_avg"] = "1406.8"
}

func notificationEmailOpsScheduledReportTemplate(locale string) string {
	if normalizeNotificationLocale(locale) == notificationEmailLocaleChinese {
		return `<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <style>
    body { margin: 0; padding: 24px 12px; background: #f4f6f8; color: #1f2937; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif; }
    .container { width: 100%; max-width: 680px; margin: 0 auto; background: #ffffff; border: 1px solid #dfe7ea; border-radius: 8px; overflow: hidden; }
    .header { padding: 28px 32px 24px; background: #0f766e; color: #ffffff; }
    .eyebrow { margin: 0 0 8px; color: #ccfbf1; font-size: 12px; font-weight: 700; letter-spacing: 0; text-transform: uppercase; }
    h1 { margin: 0; font-size: 26px; line-height: 1.3; }
    .header p { margin: 8px 0 0; color: #e6fffb; font-size: 14px; }
    .content { padding: 28px 32px 32px; }
    .meta { width: 100%; margin: 0 0 20px; border-collapse: collapse; background: #f8fafc; border: 1px solid #e2e8f0; }
    .meta td { padding: 10px 12px; border-bottom: 1px solid #e2e8f0; font-size: 13px; vertical-align: top; }
    .meta tr:last-child td { border-bottom: 0; }
    .meta-label { width: 112px; color: #64748b; font-weight: 600; }
    .section-title { margin: 28px 0 12px; color: #0f172a; font-size: 16px; line-height: 1.4; }
    .metric-grid { width: 100%; border-collapse: separate; border-spacing: 8px; margin: -8px; }
    .metric-cell { width: 50%; padding: 14px 16px; border: 1px solid #e2e8f0; background: #ffffff; vertical-align: top; }
    .metric-label { display: block; color: #64748b; font-size: 12px; line-height: 1.4; }
    .metric-value { display: block; margin-top: 6px; color: #0f172a; font-size: 20px; font-weight: 700; line-height: 1.2; }
    .metric-value.good { color: #15803d; }
    .metric-value.alert { color: #b91c1c; }
    .detail { width: 100%; border-collapse: collapse; }
    .detail td { padding: 10px 12px; border-bottom: 1px solid #e2e8f0; font-size: 13px; }
    .detail td:first-child { width: 56%; color: #475569; }
    .detail td:last-child { color: #0f172a; font-weight: 600; text-align: right; }
    .report-detail { margin-top: 28px; }
    .report-detail:empty { display: none; }
    .footer { padding: 18px 32px; background: #f8fafc; border-top: 1px solid #e2e8f0; color: #64748b; font-size: 12px; line-height: 1.6; }
    @media only screen and (max-width: 620px) {
      body { padding: 0; }
      .container { border: 0; border-radius: 0; }
      .header, .content, .footer { padding-left: 20px; padding-right: 20px; }
      .metric-grid, .metric-grid tbody, .metric-grid tr, .metric-cell { display: block; width: 100% !important; box-sizing: border-box; }
      .metric-cell { margin: 8px 0; }
    }
  </style>
</head>
<body>
  <div class="container">
    <div class="header">
      <p class="eyebrow">运维报表</p>
      <h1>{{report_name}}</h1>
      <p>{{site_name}} 的运行概览</p>
    </div>
    <div class="content">
      <table class="meta" role="presentation">
        <tr><td class="meta-label">报表</td><td>{{report_name}}</td></tr>
        <tr><td class="meta-label">类型</td><td>{{report_type}}</td></tr>
        <tr><td class="meta-label">统计周期</td><td>{{report_start_time}} 至 {{report_end_time}} (UTC)</td></tr>
      </table>

      <div style="display: {{report_summary_display}};">
      <h2 class="section-title">请求概览</h2>
      <table class="metric-grid" role="presentation"><tr>
        <td class="metric-cell"><span class="metric-label">总请求数</span><span class="metric-value">{{report_total_requests}}</span></td>
        <td class="metric-cell"><span class="metric-label">成功请求</span><span class="metric-value good">{{report_success_count}}</span></td>
      </tr><tr>
        <td class="metric-cell"><span class="metric-label">SLA 错误</span><span class="metric-value alert">{{report_sla_error_count}}</span></td>
        <td class="metric-cell"><span class="metric-label">业务限流</span><span class="metric-value">{{report_business_limited_count}}</span></td>
      </tr></table>

      <h2 class="section-title">可靠性</h2>
      <table class="detail" role="presentation">
        <tr><td>SLA</td><td>{{report_sla}}</td></tr>
        <tr><td>错误率</td><td>{{report_error_rate}}</td></tr>
        <tr><td>上游错误率（不含 429 / 529）</td><td>{{report_upstream_error_rate}}</td></tr>
        <tr><td>上游错误（不含 429 / 529）</td><td>{{report_upstream_error_count_excl_429_529}}</td></tr>
        <tr><td>上游 429 / 529</td><td>{{report_upstream_429_count}} / {{report_upstream_529_count}}</td></tr>
      </table>

      <h2 class="section-title">延迟表现</h2>
      <table class="detail" role="presentation">
        <tr><td>请求延迟 p50 / p99</td><td>{{report_latency_p50}} / {{report_latency_p99}}</td></tr>
        <tr><td>首 Token 时间 p50 / p99</td><td>{{report_ttft_p50}} / {{report_ttft_p99}}</td></tr>
      </table>

      <h2 class="section-title">吞吐量</h2>
      <table class="detail" role="presentation">
        <tr><td>Token 消耗</td><td>{{report_tokens}}</td></tr>
        <tr><td>QPS（当前 / 峰值 / 平均）</td><td>{{report_qps_current}} / {{report_qps_peak}} / {{report_qps_avg}}</td></tr>
        <tr><td>TPS（当前 / 峰值 / 平均）</td><td>{{report_tps_current}} / {{report_tps_peak}} / {{report_tps_avg}}</td></tr>
      </table>

      </div>
      <div class="report-detail" style="display: {{report_detail_display}};">{{report_html}}</div>
    </div>
    <div class="footer">此邮件由 {{site_name}} 自动发送，请勿直接回复。</div>
  </div>
</body>
</html>`
	}

	return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <style>
    body { margin: 0; padding: 24px 12px; background: #f4f6f8; color: #1f2937; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    .container { width: 100%; max-width: 680px; margin: 0 auto; background: #ffffff; border: 1px solid #dfe7ea; border-radius: 8px; overflow: hidden; }
    .header { padding: 28px 32px 24px; background: #0f766e; color: #ffffff; }
    .eyebrow { margin: 0 0 8px; color: #ccfbf1; font-size: 12px; font-weight: 700; letter-spacing: 0; text-transform: uppercase; }
    h1 { margin: 0; font-size: 26px; line-height: 1.3; }
    .header p { margin: 8px 0 0; color: #e6fffb; font-size: 14px; }
    .content { padding: 28px 32px 32px; }
    .meta { width: 100%; margin: 0 0 20px; border-collapse: collapse; background: #f8fafc; border: 1px solid #e2e8f0; }
    .meta td { padding: 10px 12px; border-bottom: 1px solid #e2e8f0; font-size: 13px; vertical-align: top; }
    .meta tr:last-child td { border-bottom: 0; }
    .meta-label { width: 112px; color: #64748b; font-weight: 600; }
    .section-title { margin: 28px 0 12px; color: #0f172a; font-size: 16px; line-height: 1.4; }
    .metric-grid { width: 100%; border-collapse: separate; border-spacing: 8px; margin: -8px; }
    .metric-cell { width: 50%; padding: 14px 16px; border: 1px solid #e2e8f0; background: #ffffff; vertical-align: top; }
    .metric-label { display: block; color: #64748b; font-size: 12px; line-height: 1.4; }
    .metric-value { display: block; margin-top: 6px; color: #0f172a; font-size: 20px; font-weight: 700; line-height: 1.2; }
    .metric-value.good { color: #15803d; }
    .metric-value.alert { color: #b91c1c; }
    .detail { width: 100%; border-collapse: collapse; }
    .detail td { padding: 10px 12px; border-bottom: 1px solid #e2e8f0; font-size: 13px; }
    .detail td:first-child { width: 56%; color: #475569; }
    .detail td:last-child { color: #0f172a; font-weight: 600; text-align: right; }
    .report-detail { margin-top: 28px; }
    .report-detail:empty { display: none; }
    .footer { padding: 18px 32px; background: #f8fafc; border-top: 1px solid #e2e8f0; color: #64748b; font-size: 12px; line-height: 1.6; }
    @media only screen and (max-width: 620px) {
      body { padding: 0; }
      .container { border: 0; border-radius: 0; }
      .header, .content, .footer { padding-left: 20px; padding-right: 20px; }
      .metric-grid, .metric-grid tbody, .metric-grid tr, .metric-cell { display: block; width: 100% !important; box-sizing: border-box; }
      .metric-cell { margin: 8px 0; }
    }
  </style>
</head>
<body>
  <div class="container">
    <div class="header">
      <p class="eyebrow">Operations report</p>
      <h1>{{report_name}}</h1>
      <p>{{site_name}} runtime overview</p>
    </div>
    <div class="content">
      <table class="meta" role="presentation">
        <tr><td class="meta-label">Report</td><td>{{report_name}}</td></tr>
        <tr><td class="meta-label">Type</td><td>{{report_type}}</td></tr>
        <tr><td class="meta-label">Reporting period</td><td>{{report_start_time}} to {{report_end_time}} (UTC)</td></tr>
      </table>

      <div style="display: {{report_summary_display}};">
      <h2 class="section-title">Request Overview</h2>
      <table class="metric-grid" role="presentation"><tr>
        <td class="metric-cell"><span class="metric-label">Total Requests</span><span class="metric-value">{{report_total_requests}}</span></td>
        <td class="metric-cell"><span class="metric-label">Successful Requests</span><span class="metric-value good">{{report_success_count}}</span></td>
      </tr><tr>
        <td class="metric-cell"><span class="metric-label">SLA Errors</span><span class="metric-value alert">{{report_sla_error_count}}</span></td>
        <td class="metric-cell"><span class="metric-label">Business Limited</span><span class="metric-value">{{report_business_limited_count}}</span></td>
      </tr></table>

      <h2 class="section-title">Reliability</h2>
      <table class="detail" role="presentation">
        <tr><td>SLA</td><td>{{report_sla}}</td></tr>
        <tr><td>Error Rate</td><td>{{report_error_rate}}</td></tr>
        <tr><td>Upstream Error Rate (excluding 429 / 529)</td><td>{{report_upstream_error_rate}}</td></tr>
        <tr><td>Upstream Errors (excluding 429 / 529)</td><td>{{report_upstream_error_count_excl_429_529}}</td></tr>
        <tr><td>Upstream 429 / 529</td><td>{{report_upstream_429_count}} / {{report_upstream_529_count}}</td></tr>
      </table>

      <h2 class="section-title">Latency</h2>
      <table class="detail" role="presentation">
        <tr><td>Request Latency p50 / p99</td><td>{{report_latency_p50}} / {{report_latency_p99}}</td></tr>
        <tr><td>Time to First Token p50 / p99</td><td>{{report_ttft_p50}} / {{report_ttft_p99}}</td></tr>
      </table>

      <h2 class="section-title">Throughput</h2>
      <table class="detail" role="presentation">
        <tr><td>Tokens Consumed</td><td>{{report_tokens}}</td></tr>
        <tr><td>QPS (current / peak / average)</td><td>{{report_qps_current}} / {{report_qps_peak}} / {{report_qps_avg}}</td></tr>
        <tr><td>TPS (current / peak / average)</td><td>{{report_tps_current}} / {{report_tps_peak}} / {{report_tps_avg}}</td></tr>
      </table>

      </div>
      <div class="report-detail" style="display: {{report_detail_display}};">{{report_html}}</div>
    </div>
    <div class="footer">This email was sent automatically by {{site_name}}. Please do not reply directly.</div>
  </div>
</body>
</html>`
}
