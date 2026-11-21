package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *opsRepository) GetOpenAITokenStats(ctx context.Context, filter *service.OpsOpenAITokenStatsFilter) (*service.OpsOpenAITokenStatsResponse, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil ops repository")
	}
	if filter == nil {
		return nil, fmt.Errorf("nil filter")
	}
	if filter.StartTime.IsZero() || filter.EndTime.IsZero() {
		return nil, fmt.Errorf("start_time/end_time required")
	}
	// 允许 start_time == end_time（结果为空），与 service 层校验口径保持一致。
	if filter.StartTime.After(filter.EndTime) {
		return nil, fmt.Errorf("start_time must be <= end_time")
	}

	dashboardFilter := &service.OpsDashboardFilter{
		StartTime: filter.StartTime.UTC(),
		EndTime:   filter.EndTime.UTC(),
		Platform:  strings.TrimSpace(strings.ToLower(filter.Platform)),
		GroupID:   filter.GroupID,
	}

	join, where, baseArgs, next := buildUsageWhere(dashboardFilter, dashboardFilter.StartTime, dashboardFilter.EndTime, 1)
	if strings.TrimSpace(strings.ToLower(filter.Scope)) != "all" {
		where += " AND ul.model LIKE 'gpt%'"
	}

	baseCTE := `
WITH stats AS MATERIALIZED (
  SELECT
    ul.model AS model,
    COUNT(*)::bigint AS request_count,
    ROUND(
      1000.0 * (SUM(ul.output_tokens::numeric) FILTER (WHERE ul.duration_ms > 0 AND ul.output_tokens > 0))
      / (SUM(ul.duration_ms::numeric) FILTER (WHERE ul.duration_ms > 0 AND ul.output_tokens > 0)),
      2
    )::float8 AS avg_tokens_per_sec,
    ROUND(AVG(ul.first_token_ms)::numeric, 2)::float8 AS avg_first_token_ms,
    COALESCE(SUM(ul.output_tokens), 0)::bigint AS total_output_tokens,
    COALESCE(ROUND(AVG(ul.duration_ms)::numeric, 0), 0)::bigint AS avg_duration_ms,
    COUNT(CASE WHEN ul.first_token_ms IS NOT NULL THEN 1 END)::bigint AS requests_with_first_token
  FROM usage_logs ul
  ` + join + `
  ` + where + `
  GROUP BY ul.model
)
`

	pageSQL := `
SELECT *
FROM stats
ORDER BY request_count DESC, model ASC`

	args := make([]any, 0, len(baseArgs)+2)
	args = append(args, baseArgs...)

	if filter.IsTopNMode() {
		pageSQL += fmt.Sprintf("\nLIMIT $%d", next)
		args = append(args, filter.TopN)
	} else {
		offset := (filter.Page - 1) * filter.PageSize
		pageSQL += fmt.Sprintf("\nLIMIT $%d OFFSET $%d", next, next+1)
		args = append(args, filter.PageSize, offset)
	}

	querySQL := baseCTE + ", page AS (" + pageSQL + `
)
SELECT
  totals.total,
  page.model,
  COALESCE(page.request_count, 0),
  page.avg_tokens_per_sec,
  page.avg_first_token_ms,
  COALESCE(page.total_output_tokens, 0),
  COALESCE(page.avg_duration_ms, 0),
  COALESCE(page.requests_with_first_token, 0)
FROM (SELECT COUNT(*) AS total FROM stats) totals
LEFT JOIN page ON TRUE
ORDER BY page.request_count DESC, page.model ASC`
	var total int64

	rows, err := r.db.QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]*service.OpsOpenAITokenStatsItem, 0, 32)
	for rows.Next() {
		item := &service.OpsOpenAITokenStatsItem{}
		var model sql.NullString
		var avgTPS sql.NullFloat64
		var avgFirstToken sql.NullFloat64
		if err := rows.Scan(
			&total,
			&model,
			&item.RequestCount,
			&avgTPS,
			&avgFirstToken,
			&item.TotalOutputTokens,
			&item.AvgDurationMs,
			&item.RequestsWithFirstToken,
		); err != nil {
			return nil, err
		}
		if !model.Valid {
			continue
		}
		item.Model = model.String
		if avgTPS.Valid {
			v := avgTPS.Float64
			item.AvgTokensPerSec = &v
		}
		if avgFirstToken.Valid {
			v := avgFirstToken.Float64
			item.AvgFirstTokenMs = &v
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	resp := &service.OpsOpenAITokenStatsResponse{
		TimeRange: strings.TrimSpace(filter.TimeRange),
		StartTime: dashboardFilter.StartTime,
		EndTime:   dashboardFilter.EndTime,
		Platform:  dashboardFilter.Platform,
		GroupID:   dashboardFilter.GroupID,
		Items:     items,
		Total:     total,
	}
	if filter.IsTopNMode() {
		topN := filter.TopN
		resp.TopN = &topN
	} else {
		resp.Page = filter.Page
		resp.PageSize = filter.PageSize
	}
	return resp, nil
}
