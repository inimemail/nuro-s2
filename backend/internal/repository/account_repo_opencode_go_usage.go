package repository

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const openCodeGoUsageEligibleSQL = `type='apikey' AND (
 (platform='opencode_go' AND LOWER(COALESCE(NULLIF(credentials->>'account_mode',''),extra->>'account_mode','go'))<>'zen'
 AND (extra->>'cn_billing_mode'='coding_plan' OR (COALESCE(extra->>'cn_billing_mode','') NOT IN ('payg','coding_plan') AND credentials->>'account_mode' IN ('coding','coding_plan')))) OR
 (platform IN ('openai','anthropic','kimi','zhipu','deepseek','minimax') AND
 LOWER(RTRIM(BTRIM(credentials->>'base_url'),'/')) IN ('https://opencode.ai/zen/go','https://opencode.ai/zen/go/v1','https://opencode.ai/zen/go/anthropic')))`

func openCodeUsageEpochSQL(field string) string {
	return `CASE WHEN extra #>> '{opencode_go_usage_snapshot,` + field + `}' ~ '^[0-9]{1,10}$' THEN (extra #>> '{opencode_go_usage_snapshot,` + field + `}')::bigint ELSE 0 END`
}
func (r *accountRepository) ListDueOpenCodeGoUsageAccounts(ctx context.Context, now time.Time, debounce, maxWait time.Duration, limit int) ([]*service.Account, error) {
	if limit <= 0 {
		return nil, nil
	}
	limit = min(limit, 20)
	rows, err := r.sql.QueryContext(ctx, `WITH eligible AS (
 SELECT id,created_at,last_used_at,`+openCodeUsageEpochSQL("last_attempt_at")+` AS attempted,`+openCodeUsageEpochSQL("next_refresh_at")+` AS next_refresh
 FROM accounts WHERE deleted_at IS NULL AND status='active' AND `+openCodeGoUsageEligibleSQL+`
 AND extra @> '{"opencode_go_usage_auto_refresh":true}'::jsonb AND last_used_at IS NOT NULL
 ) SELECT id FROM eligible
 WHERE last_used_at > to_timestamp(attempted) AND $1>=to_timestamp(next_refresh)
 AND $1>=LEAST(last_used_at+make_interval(secs=>$2::double precision),GREATEST(created_at,to_timestamp(attempted))+make_interval(secs=>$3::double precision))
 ORDER BY attempted,last_used_at,id LIMIT $4`, now.UTC(), debounce.Seconds(), maxWait.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return r.GetByIDs(ctx, ids)
}
func (r *accountRepository) FindRecentOpenCodeGoUsage(ctx context.Context, scope string, since int64) (*service.OpenCodeGoUsageSnapshot, error) {
	rows, err := r.sql.QueryContext(ctx, `SELECT extra->'opencode_go_usage_snapshot' FROM accounts
 WHERE deleted_at IS NULL AND extra->>'opencode_go_usage_scope'=$1 AND `+openCodeUsageEpochSQL("last_attempt_at")+` >= $2
 ORDER BY `+openCodeUsageEpochSQL("last_attempt_at")+` DESC LIMIT 1`, scope, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var raw []byte
	if err := rows.Scan(&raw); err != nil {
		return nil, err
	}
	var snapshot service.OpenCodeGoUsageSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}
func (r *accountRepository) SaveOpenCodeGoUsageSnapshot(ctx context.Context, a *service.Account, scope string, snapshot *service.OpenCodeGoUsageSnapshot) (bool, error) {
	if a == nil || snapshot == nil {
		return false, errors.New("missing usage snapshot")
	}
	credentials, err := json.Marshal(a.Credentials)
	if err != nil {
		return false, err
	}
	patch, err := json.Marshal(map[string]any{service.OpenCodeGoUsageSnapshotKey: snapshot, service.OpenCodeGoUsageScopeKey: scope, service.OpenCodeGoUsageIdentityKey: service.OpenCodeGoUsageIdentity(a)})
	if err != nil {
		return false, err
	}
	var proxyVersion any
	if a.Proxy != nil {
		proxyVersion = a.Proxy.UpdatedAt
	}
	// updated_at provides a conservative ABA fence for credentials/proxy edits.
	// Last-used flushes can reject a stale sample, never revive a disabled auto flag.
	result, err := r.sql.ExecContext(ctx, `UPDATE accounts SET extra=COALESCE(extra,'{}'::jsonb)||$1::jsonb
 WHERE id=$2 AND deleted_at IS NULL AND platform=$3 AND type=$4 AND credentials=$5::jsonb
 AND proxy_id IS NOT DISTINCT FROM $6 AND updated_at=$7
 AND COALESCE(extra->>'opencode_go_usage_source','configured')=$8
 AND COALESCE(extra->'opencode_go_usage_auto_refresh','false'::jsonb)=$9::jsonb
 AND ($6::bigint IS NULL OR EXISTS (SELECT 1 FROM proxies p WHERE p.id=$6 AND p.deleted_at IS NULL AND p.updated_at=$10))`, string(patch), a.ID, a.Platform, a.Type, string(credentials), a.ProxyID, a.UpdatedAt, service.OpenCodeGoUsageSource(a), strconv.FormatBool(a.Extra[service.OpenCodeGoUsageAutoKey] == true), proxyVersion)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}
func (r *accountRepository) ConfigureOpenCodeGoUsage(ctx context.Context, a *service.Account, enabled bool, source string) (bool, error) {
	patch, err := json.Marshal(map[string]any{service.OpenCodeGoUsageAutoKey: enabled, service.OpenCodeGoUsageSourceKey: source})
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `UPDATE accounts SET updated_at=NOW(), extra=(COALESCE(extra,'{}'::jsonb) || $1::jsonb)
 - CASE WHEN COALESCE(extra->>'opencode_go_usage_source','configured')<>$2 THEN ARRAY['opencode_go_usage_snapshot','opencode_go_usage_scope','opencode_go_usage_identity']::text[] ELSE ARRAY[]::text[] END
 WHERE id=$3 AND deleted_at IS NULL AND updated_at=$4`, string(patch), source, a.ID, a.UpdatedAt)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &a.ID, nil, nil); err != nil {
		return true, err
	}
	return true, nil
}
