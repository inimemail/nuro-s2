package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func usageExportFilterWhere(filters usagestats.UsageLogFilters) ([]string, []any) {
	conditions := make([]string, 0, 9)
	args := make([]any, 0, 9)

	if filters.UserID > 0 {
		conditions = append(conditions, fmt.Sprintf("user_id = $%d", len(args)+1))
		args = append(args, filters.UserID)
	}
	if filters.APIKeyID > 0 {
		conditions = append(conditions, fmt.Sprintf("api_key_id = $%d", len(args)+1))
		args = append(args, filters.APIKeyID)
	}
	if filters.AccountID > 0 {
		conditions = append(conditions, fmt.Sprintf("account_id = $%d", len(args)+1))
		args = append(args, filters.AccountID)
	}
	if filters.GroupID > 0 {
		conditions = append(conditions, fmt.Sprintf("group_id = $%d", len(args)+1))
		args = append(args, filters.GroupID)
	}
	if requestID := strings.TrimSpace(filters.RequestID); requestID != "" {
		conditions = append(conditions, fmt.Sprintf("request_id = $%d", len(args)+1))
		args = append(args, requestID)
	}
	conditions, args = appendRawUsageLogModelWhereCondition(conditions, args, filters.Model)
	conditions, args = appendRequestTypeOrStreamWhereCondition(conditions, args, filters.RequestType, filters.Stream)
	if filters.BillingType != nil {
		conditions = append(conditions, fmt.Sprintf("billing_type = $%d", len(args)+1))
		args = append(args, int16(*filters.BillingType))
	}
	conditions, args = appendUsageLogBillingModeWhereCondition(conditions, args, filters.BillingMode)
	if filters.UpstreamModelMismatch != nil {
		if *filters.UpstreamModelMismatch {
			conditions = append(conditions, "upstream_model_mismatch IS TRUE")
		} else {
			conditions = append(conditions, "upstream_model_mismatch IS FALSE")
		}
	}
	if filters.StartTime != nil {
		conditions = append(conditions, fmt.Sprintf("created_at >= $%d", len(args)+1))
		args = append(args, *filters.StartTime)
	}
	if filters.EndTime != nil {
		conditions = append(conditions, fmt.Sprintf("created_at < $%d", len(args)+1))
		args = append(args, *filters.EndTime)
	}

	return conditions, args
}

// Export queries are owner-scoped and use keyset pagination; the ordinary
// list/count path and immutable usage writes are deliberately unchanged.
func (r *usageLogRepository) ExportUsagePage(ctx context.Context, params pagination.PaginationParams, filters usagestats.UsageLogFilters, highWater int64, after *service.UsageExportPosition) ([]service.UsageLog, int64, error) {
	if filters.UserID <= 0 {
		return nil, 0, fmt.Errorf("usage export requires an owner")
	}
	conditions, args := usageExportFilterWhere(filters)
	if highWater == 0 {
		if after != nil {
			return nil, 0, fmt.Errorf("missing export boundary")
		}
		rows, err := r.sql.QueryContext(ctx, "SELECT COALESCE(MAX(id),0) FROM usage_logs "+buildWhere(conditions), args...)
		if err != nil {
			return nil, 0, err
		}
		if !rows.Next() {
			err = rows.Err()
			_ = rows.Close()
			if err == nil {
				err = fmt.Errorf("missing export boundary row")
			}
			return nil, 0, err
		}
		err = rows.Scan(&highWater)
		if err == nil {
			err = rows.Err()
		}
		closeErr := rows.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, 0, err
		}
	}
	if highWater == 0 {
		return []service.UsageLog{}, 0, nil
	}
	conditions = append(conditions, fmt.Sprintf("id <= $%d", len(args)+1))
	args = append(args, highWater)
	if after != nil {
		op := ">"
		if params.NormalizedSortOrder(pagination.SortOrderDesc) == pagination.SortOrderDesc {
			op = "<"
		}
		switch strings.ToLower(strings.TrimSpace(params.SortBy)) {
		case "created_at":
			conditions = append(conditions, fmt.Sprintf("(created_at,id) %s ($%d,$%d)", op, len(args)+1, len(args)+2))
			args = append(args, after.CreatedAt, after.ID)
		case "model":
			conditions = append(conditions, fmt.Sprintf("(COALESCE(NULLIF(TRIM(requested_model), ''), model),id) %s ($%d,$%d)", op, len(args)+1, len(args)+2))
			args = append(args, after.Model, after.ID)
		default:
			conditions = append(conditions, fmt.Sprintf("id %s $%d", op, len(args)+1))
			args = append(args, after.ID)
		}
	}
	query := "SELECT " + usageLogSelectColumns + " FROM usage_logs " + buildWhere(conditions) + " ORDER BY " + usageLogOrderBy(params) + " LIMIT 1001"
	logs, err := r.queryUsageLogs(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	if err = r.hydrateUsageLogAssociations(ctx, logs); err != nil {
		return nil, 0, err
	}
	return logs, highWater, nil
}
