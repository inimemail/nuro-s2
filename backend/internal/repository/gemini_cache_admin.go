package repository

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"strings"
)

func (r *geminiCachedContentRepository) CurrentViewRevision(ctx context.Context) (int64, error) {
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	var revision int64
	err := r.db.QueryRowContext(ctx, `SELECT revision FROM gemini_cache_view_clock WHERE id=1`).Scan(&revision)
	return revision, err
}
func (r *geminiCachedContentRepository) ListReconcile(ctx context.Context, page string, limit int) ([]service.GeminiCacheReconcileRow, string, error) {
	if page != "" && !service.ValidGeminiCachePublicID(page) {
		return nil, "", service.ErrGeminiCacheNotFound
	}
	limit = min(max(limit, 1), 100)
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, `SELECT o.id,o.resource_id,o.user_id,o.api_key_id,o.group_id,o.kind,o.state,o.attempts,o.last_error_code,COALESCE(c.reserved_amount::text,o.quote->>'amount'),o.confirmed_tokens,COALESCE(c.actual_amount::text,''),COALESCE(GREATEST(0,c.actual_amount-c.reserved_amount)::text,'0') FROM gemini_cache_operations o LEFT JOIN gemini_cache_charges c ON c.operation_id=o.id WHERE o.state NOT IN ('settled','released') AND ($1='' OR o.id>$1) ORDER BY o.id LIMIT $2`, page, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]service.GeminiCacheReconcileRow, 0, limit)
	next := ""
	for rows.Next() {
		var row service.GeminiCacheReconcileRow
		if err = rows.Scan(&row.OperationID, &row.ResourceName, &row.UserID, &row.APIKeyID, &row.GroupID, &row.Kind, &row.State, &row.Attempts, &row.ErrorCode, &row.ReservedAmount, &row.ConfirmedTokens, &row.ActualAmount, &row.DeficitAmount); err != nil {
			return nil, "", err
		}
		if len(items) == limit {
			next = items[len(items)-1].OperationID
			break
		}
		row.ResourceName = "cachedContents/" + row.ResourceName
		items = append(items, row)
	}
	return items, next, rows.Err()
}
func (r *geminiCachedContentRepository) RequeueReconcile(ctx context.Context, id string, actor int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if !service.ValidGeminiCachePublicID(id) || actor <= 0 || len(reason) < 3 || len(reason) > 256 {
		return service.ErrGeminiCacheConflict
	}
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	tx, err := beginCacheTx(ctx, r.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE gemini_cache_operations SET attempts=0,next_attempt_at=NOW(),updated_at=NOW() WHERE id=$1 AND state NOT IN ('settled','released') AND (state='reconcile_required' OR attempts>=12) AND (lease_until IS NULL OR lease_until<NOW()-INTERVAL '45 seconds')`, id)
	if err != nil {
		return err
	}
	if err = requireCacheAffected(result); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO gemini_cache_admin_actions(operation_id,actor_user_id,action,reason) VALUES($1,$2,'recheck',$3)`, id, actor, reason); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *geminiCachedContentRepository) CancelUnrecordedOperation(ctx context.Context, owner service.GeminiCacheOwner, kind, hash string) error {
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	tx, err := beginCacheTx(ctx, r.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(694208311321144028)"); err != nil {
		return err
	}
	var recorded bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gemini_cache_operations WHERE user_id=$1 AND api_key_id=$2 AND kind=$3 AND idempotency_key_hash=$4)`, owner.UserID, owner.APIKeyID, kind, hash).Scan(&recorded); err != nil {
		return err
	}
	if recorded {
		return service.ErrGeminiCacheConflict
	}
	// Keep repeat cancellations idempotent, even once an owner's budget is full.
	var cancelled bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gemini_cache_cancelled_intents WHERE user_id=$1 AND api_key_id=$2 AND kind=$3 AND idempotency_key_hash=$4)`, owner.UserID, owner.APIKeyID, kind, hash).Scan(&cancelled); err != nil {
		return err
	}
	if cancelled {
		return nil
	}
	var keyCount, userCount int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(*) FILTER (WHERE api_key_id=$2) FROM gemini_cache_cancelled_intents WHERE user_id=$1`, owner.UserID, owner.APIKeyID).Scan(&userCount, &keyCount); err != nil {
		return err
	}
	if keyCount >= 1000 || userCount >= 10000 {
		return service.ErrGeminiCacheQuota
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO gemini_cache_cancelled_intents(user_id,api_key_id,kind,idempotency_key_hash) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, owner.UserID, owner.APIKeyID, kind, hash); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *geminiCachedContentRepository) AuthorizationRevisionAfter(ctx context.Context, before int64) (int64, error) {
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	var revision int64
	err := r.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM gemini_cache_events WHERE resource_id='authorization' AND revision>$1`, before).Scan(&revision)
	return revision, err
}
