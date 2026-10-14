package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type geminiCachedContentRepository struct{ db *sql.DB }

func NewGeminiCachedContentRepository(db *sql.DB) service.GeminiCachedContentRepository {
	return &geminiCachedContentRepository{db: db}
}

func (r *geminiCachedContentRepository) HasCacheState(ctx context.Context) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gemini_cached_contents WHERE state<>'deleted' AND (expires_at>NOW() OR state IN ('pending','reconcile_required'))) OR EXISTS(SELECT 1 FROM gemini_cache_operations WHERE state NOT IN ('settled','released'))`).Scan(&exists)
	return exists, err
}
func cacheDBTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Second)
}
func beginCacheTx(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "SET LOCAL lock_timeout='2s'; SET LOCAL statement_timeout='5s'"); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

type cacheRowScanner interface{ Scan(...any) error }

const cacheOperationColumns = `id,resource_id,user_id,api_key_id,group_id,account_id,subscription_id,kind,COALESCE(idempotency_key_hash,''),fingerprint,state,baseline_expiry,target_expiry,quote,upstream_name,confirmed_tokens,confirmed_expiry,send_started_at,generation,COALESCE(lease_owner,''),lease_until,attempts,last_error_code`

func scanCacheOperation(row cacheRowScanner) (*service.GeminiCacheOperation, error) {
	var o service.GeminiCacheOperation
	var quote []byte
	err := row.Scan(&o.ID, &o.ResourceID, &o.Owner.UserID, &o.Owner.APIKeyID, &o.Owner.GroupID, &o.AccountID, &o.SubscriptionID, &o.Kind, &o.IdempotencyKeyHash, &o.Fingerprint, &o.State, &o.BaselineExpiry, &o.TargetExpiry, &quote, &o.UpstreamName, &o.ConfirmedTokens, &o.ConfirmedExpiry, &o.SendStartedAt, &o.Generation, &o.LeaseOwner, &o.LeaseUntil, &o.Attempts, &o.LastErrorCode)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(quote, &o.Quote); err != nil {
		return nil, err
	}
	return &o, nil
}

const cacheResourceColumns = `id,user_id,api_key_id,group_id,account_id,requested_model,upstream_model,billing_model,upstream_name,display_name,state,token_count,expires_at,paid_until,pricing_context,revision,created_at,updated_at,
 COALESCE((SELECT SUM(c.reserved_amount) FROM gemini_cache_charges c JOIN gemini_cache_operations o ON o.id=c.operation_id WHERE o.resource_id=gemini_cached_contents.id AND c.state IN ('reserved','reconcile_required')),0)::text,
 COALESCE((SELECT SUM(c.actual_amount) FROM gemini_cache_charges c JOIN gemini_cache_operations o ON o.id=c.operation_id WHERE o.resource_id=gemini_cached_contents.id AND c.state='settled'),0)::text,
 COALESCE((SELECT SUM(c.input_cost) FROM gemini_cache_charges c JOIN gemini_cache_operations o ON o.id=c.operation_id WHERE o.resource_id=gemini_cached_contents.id AND c.state='settled'),0)::text,
 COALESCE((SELECT SUM(c.storage_cost) FROM gemini_cache_charges c JOIN gemini_cache_operations o ON o.id=c.operation_id WHERE o.resource_id=gemini_cached_contents.id AND c.state='settled'),0)::text,
 COALESCE((SELECT o.state FROM gemini_cache_operations o WHERE o.resource_id=gemini_cached_contents.id ORDER BY o.created_at DESC,o.id DESC LIMIT 1),'')`

func scanCachedContent(row cacheRowScanner) (*service.GeminiCachedContent, error) {
	var r service.GeminiCachedContent
	var pricing []byte
	err := row.Scan(&r.ID, &r.Owner.UserID, &r.Owner.APIKeyID, &r.Owner.GroupID, &r.AccountID, &r.RequestedModel, &r.UpstreamModel, &r.BillingModel, &r.UpstreamName, &r.DisplayName, &r.State, &r.TokenCount, &r.ExpiresAt, &r.PaidUntil, &pricing, &r.Revision, &r.CreatedAt, &r.UpdatedAt, &r.Billing.ReservedAmount, &r.Billing.SettledAmount, &r.Billing.InputCost, &r.Billing.StorageCost, &r.Billing.OperationState)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(pricing, &r.Pricing); err != nil {
		return nil, err
	}
	return &r, nil
}
func publishGeminiCacheEvent(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	var revision int64
	if err := tx.QueryRowContext(ctx, `UPDATE gemini_cache_view_clock SET revision=revision+1 WHERE id=1 RETURNING revision`).Scan(&revision); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE gemini_cached_contents SET revision=$2 WHERE id=$1`, id, revision); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gemini_cache_events(revision,resource_id) VALUES($1,$2)`, revision, id); err != nil {
		return 0, err
	}
	return revision, nil
}
func (r *geminiCachedContentRepository) Prepare(ctx context.Context, resource *service.GeminiCachedContent, op *service.GeminiCacheOperation, maxResources int) (*service.GeminiCacheOperation, error) {
	if r == nil || r.db == nil || resource == nil || op == nil || op.LeaseOwner == "" {
		return nil, service.ErrGeminiCacheUnavailable
	}
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	tx, err := beginCacheTx(ctx, r.db)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// This advisory lock serializes bounded capacity/idempotency checks only.
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(694208311321144028)"); err != nil {
		return nil, err
	}
	if op.IdempotencyKeyHash != "" {
		var cancelled bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gemini_cache_cancelled_intents WHERE user_id=$1 AND api_key_id=$2 AND kind=$3 AND idempotency_key_hash=$4)`, op.Owner.UserID, op.Owner.APIKeyID, op.Kind, op.IdempotencyKeyHash).Scan(&cancelled); err != nil {
			return nil, err
		}
		if cancelled {
			return nil, service.ErrGeminiCacheRejected
		}
		existing, e := scanCacheOperation(tx.QueryRowContext(ctx, `SELECT `+cacheOperationColumns+` FROM gemini_cache_operations WHERE user_id=$1 AND api_key_id=$2 AND kind=$3 AND idempotency_key_hash=$4`, op.Owner.UserID, op.Owner.APIKeyID, op.Kind, op.IdempotencyKeyHash))
		if e == nil {
			if existing.Fingerprint != op.Fingerprint {
				return nil, service.ErrGeminiCacheConflict
			}
			return existing, nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
	}
	quote, err := json.Marshal(op.Quote)
	if err != nil {
		return nil, err
	}
	pricing, err := json.Marshal(resource.Pricing)
	if err != nil {
		return nil, err
	}
	if op.Kind == "create" {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM gemini_cached_contents WHERE state <> 'deleted' AND (expires_at>NOW() OR state IN ('pending','reconcile_required'))`).Scan(&count); err != nil {
			return nil, err
		}
		var ownerCount int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM gemini_cached_contents WHERE user_id=$1 AND state<>'deleted' AND (expires_at>NOW() OR state IN ('pending','reconcile_required'))`, op.Owner.UserID).Scan(&ownerCount); err != nil {
			return nil, err
		}
		if maxResources <= 0 || count >= maxResources || ownerCount >= min(1000, maxResources) {
			return nil, service.ErrGeminiCacheUnavailable
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO gemini_cached_contents(id,user_id,api_key_id,group_id,account_id,requested_model,upstream_model,billing_model,display_name,state,token_count,expires_at,paid_until,pricing_context) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending',$10,$11,$12,$13)`, resource.ID, op.Owner.UserID, op.Owner.APIKeyID, op.Owner.GroupID, op.AccountID, resource.RequestedModel, resource.UpstreamModel, resource.BillingModel, resource.DisplayName, resource.TokenCount, resource.ExpiresAt, resource.PaidUntil, pricing)
		if err != nil {
			return nil, err
		}
	} else {
		result, e := tx.ExecContext(ctx, `UPDATE gemini_cached_contents SET state='pending',updated_at=NOW() WHERE id=$1 AND revision=$2 AND user_id=$3 AND api_key_id=$4 AND group_id=$5 AND state='active' AND expires_at>NOW()`, resource.ID, resource.Revision, op.Owner.UserID, op.Owner.APIKeyID, op.Owner.GroupID)
		if e != nil {
			return nil, e
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return nil, service.ErrGeminiCacheConflict
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO gemini_cache_operations(id,resource_id,user_id,api_key_id,group_id,account_id,subscription_id,kind,idempotency_key_hash,fingerprint,state,baseline_expiry,target_expiry,quote,generation,lease_owner,lease_until,next_attempt_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,'prepared',$11,$12,$13,1,$14,NOW()+INTERVAL '120 seconds',NOW()+INTERVAL '120 seconds')`, op.ID, resource.ID, op.Owner.UserID, op.Owner.APIKeyID, op.Owner.GroupID, op.AccountID, op.SubscriptionID, op.Kind, op.IdempotencyKeyHash, op.Fingerprint, op.BaselineExpiry, op.TargetExpiry, quote, op.LeaseOwner)
	if err != nil {
		return nil, err
	}
	revision, err := publishGeminiCacheEvent(ctx, tx, resource.ID)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	op.Generation = 1
	op.State = "prepared"
	resource.Revision = revision
	return op, nil
}
func (r *geminiCachedContentRepository) GetForOwner(ctx context.Context, owner service.GeminiCacheOwner, id string) (*service.GeminiCachedContent, error) {
	if !service.ValidGeminiCachePublicID(id) {
		return nil, service.ErrGeminiCacheNotFound
	}
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	item, err := scanCachedContent(r.db.QueryRowContext(ctx, `SELECT `+cacheResourceColumns+` FROM gemini_cached_contents WHERE id=$1 AND user_id=$2 AND api_key_id=$3 AND group_id=$4`, id, owner.UserID, owner.APIKeyID, owner.GroupID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrGeminiCacheNotFound
	}
	return item, err
}
func (r *geminiCachedContentRepository) ListForOwner(ctx context.Context, owner service.GeminiCacheOwner, page string, limit int) ([]service.GeminiCachedContent, string, error) {
	if page != "" && !service.ValidGeminiCachePublicID(page) {
		return nil, "", service.ErrGeminiCacheNotFound
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, `SELECT `+cacheResourceColumns+` FROM gemini_cached_contents WHERE user_id=$1 AND api_key_id=$2 AND group_id=$3 AND state <> 'deleted' AND ($4='' OR id>$4) ORDER BY id LIMIT $5`, owner.UserID, owner.APIKeyID, owner.GroupID, page, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]service.GeminiCachedContent, 0, limit)
	next := ""
	for rows.Next() {
		item, e := scanCachedContent(rows)
		if e != nil {
			return nil, "", e
		}
		if len(items) == limit {
			next = items[len(items)-1].ID
			break
		}
		items = append(items, *item)
	}
	return items, next, rows.Err()
}
func (r *geminiCachedContentRepository) GetOperation(ctx context.Context, id string) (*service.GeminiCacheOperation, error) {
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	return scanCacheOperation(r.db.QueryRowContext(ctx, `SELECT `+cacheOperationColumns+` FROM gemini_cache_operations WHERE id=$1`, id))
}

func (r *geminiCachedContentRepository) FindOperation(ctx context.Context, owner service.GeminiCacheOwner, kind, hash string) (*service.GeminiCacheOperation, error) {
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	op, err := scanCacheOperation(r.db.QueryRowContext(ctx, `SELECT `+cacheOperationColumns+` FROM gemini_cache_operations WHERE user_id=$1 AND api_key_id=$2 AND group_id=$3 AND kind=$4 AND idempotency_key_hash=$5`, owner.UserID, owner.APIKeyID, owner.GroupID, kind, hash))
	if errors.Is(err, sql.ErrNoRows) {
		var cancelled bool
		if err = r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gemini_cache_cancelled_intents WHERE user_id=$1 AND api_key_id=$2 AND kind=$3 AND idempotency_key_hash=$4)`, owner.UserID, owner.APIKeyID, kind, hash).Scan(&cancelled); err != nil {
			return nil, err
		}
		if cancelled {
			return &service.GeminiCacheOperation{State: "released"}, nil
		}
		return nil, nil
	}
	return op, err
}
func requireCacheAffected(result sql.Result) error {
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return service.ErrGeminiCacheLease
	}
	return nil
}
func (r *geminiCachedContentRepository) MarkSendStarted(ctx context.Context, o *service.GeminiCacheOperation) error {
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	// Recheck identity and membership on the management path, including free
	// operations which do not acquire financial owner locks. Exhausted/expired
	// keys may still delete, but cannot create or renew a cache.
	res, err := r.db.ExecContext(ctx, `UPDATE gemini_cache_operations op SET state='send_started',send_started_at=NOW(),updated_at=NOW()
 WHERE op.id=$1 AND op.state='reserved' AND op.generation=$2 AND op.lease_owner=$3
 AND op.lease_until>clock_timestamp()+INTERVAL '45 seconds'
 AND EXISTS (
  SELECT 1 FROM users u JOIN api_keys k ON k.user_id=u.id
  JOIN groups g ON g.id=k.group_id JOIN accounts a ON a.id=op.account_id
  WHERE u.id=op.user_id AND k.id=op.api_key_id AND g.id=op.group_id
  AND u.status='active' AND u.deleted_at IS NULL AND k.deleted_at IS NULL
  AND ((op.kind='delete' AND k.status IN ('active','expired','quota_exhausted'))
    OR (op.kind<>'delete' AND k.status='active' AND (k.expires_at IS NULL OR k.expires_at>NOW())))
  AND g.status='active' AND g.deleted_at IS NULL AND g.platform='gemini'
  AND (op.kind='delete' OR
    (g.subscription_type<>'subscription' AND op.subscription_id IS NULL) OR
    (g.subscription_type='subscription' AND EXISTS(
      SELECT 1 FROM user_subscriptions s WHERE s.id=op.subscription_id
      AND s.user_id=op.user_id AND s.group_id=op.group_id AND s.deleted_at IS NULL
      AND s.status='active' AND s.starts_at<=NOW() AND s.expires_at>NOW())))
  AND (g.subscription_type='subscription' OR (NOT g.is_exclusive AND NOT u.restrict_public_groups)
    OR EXISTS(SELECT 1 FROM user_allowed_groups ug WHERE ug.user_id=u.id AND ug.group_id=g.id))
  AND a.status='active' AND a.deleted_at IS NULL AND a.schedulable AND a.platform='gemini'
  AND EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=a.id AND ag.group_id=g.id)
 )`, o.ID, o.Generation, o.LeaseOwner)
	if err != nil {
		return err
	}
	if err = requireCacheAffected(res); err != nil {
		return err
	}
	o.State = "send_started"
	return nil
}
func (r *geminiCachedContentRepository) Confirm(ctx context.Context, o *service.GeminiCacheOperation, name string, tokens int64, expiry time.Time) error {
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	res, err := r.db.ExecContext(ctx, `UPDATE gemini_cache_operations SET state='upstream_confirmed',upstream_name=$1,confirmed_tokens=$2,confirmed_expiry=$3,updated_at=NOW() WHERE id=$4 AND state IN ('send_started','reconcile_required') AND generation=$5 AND lease_owner=$6 AND lease_until>clock_timestamp()`, name, tokens, expiry, o.ID, o.Generation, o.LeaseOwner)
	if err != nil {
		return err
	}
	if err = requireCacheAffected(res); err != nil {
		return err
	}
	o.State = "upstream_confirmed"
	o.UpstreamName = name
	o.ConfirmedTokens = &tokens
	o.ConfirmedExpiry = &expiry
	return nil
}
func (r *geminiCachedContentRepository) Finish(ctx context.Context, o *service.GeminiCacheOperation, state string) error {
	if state != "settled" && state != "released" && state != "reconcile_required" {
		return errors.New("invalid cache operation state")
	}
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	tx, err := beginCacheTx(ctx, r.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stored, err := scanCacheOperation(tx.QueryRowContext(ctx, `SELECT `+cacheOperationColumns+` FROM gemini_cache_operations WHERE id=$1 FOR UPDATE`, o.ID))
	if err != nil {
		return err
	}
	if stored.State == state {
		return nil
	}
	if stored.Generation != o.Generation || stored.LeaseOwner != o.LeaseOwner || stored.LeaseUntil == nil {
		return service.ErrGeminiCacheLease
	}
	if state == "settled" || state == "released" {
		var chargeState string
		if err = tx.QueryRowContext(ctx, `SELECT state FROM gemini_cache_charges WHERE operation_id=$1`, o.ID).Scan(&chargeState); err != nil {
			return err
		}
		if chargeState != state {
			return service.ErrGeminiCacheConflict
		}
	}
	var res sql.Result
	res, err = tx.ExecContext(ctx, `UPDATE gemini_cache_operations SET state=$1,lease_owner=NULL,lease_until=NULL,updated_at=NOW() WHERE id=$2 AND generation=$3 AND lease_until>clock_timestamp()`, state, o.ID, o.Generation)
	if err != nil {
		return err
	}
	if err = requireCacheAffected(res); err != nil {
		return err
	}
	switch {
	case state == "reconcile_required":
		_, err = tx.ExecContext(ctx, `UPDATE gemini_cached_contents SET state='reconcile_required',updated_at=NOW() WHERE id=$1`, o.ResourceID)
	case state == "settled" && stored.Kind == "delete", state == "released" && stored.Kind == "create":
		_, err = tx.ExecContext(ctx, `UPDATE gemini_cached_contents SET state='deleted',updated_at=NOW() WHERE id=$1`, o.ResourceID)
	case state == "settled":
		if stored.ConfirmedExpiry == nil || stored.ConfirmedTokens == nil {
			return service.ErrGeminiCacheConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE gemini_cached_contents SET state='active',upstream_name=$2,token_count=$3,expires_at=$4,paid_until=GREATEST(paid_until,$4),updated_at=NOW() WHERE id=$1`, o.ResourceID, stored.UpstreamName, *stored.ConfirmedTokens, *stored.ConfirmedExpiry)
	default:
		_, err = tx.ExecContext(ctx, `UPDATE gemini_cached_contents SET state='active',updated_at=NOW() WHERE id=$1`, o.ResourceID)
	}
	if err != nil {
		return err
	}
	if _, err = publishGeminiCacheEvent(ctx, tx, o.ResourceID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	o.State = state
	return nil
}
func (r *geminiCachedContentRepository) Retry(ctx context.Context, o *service.GeminiCacheOperation, code string, next time.Time, reconcile bool) error {
	if len(code) > 64 {
		return errors.New("invalid cache recovery code")
	}
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	tx, err := beginCacheTx(ctx, r.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE gemini_cache_operations SET state=CASE WHEN ($1 AND confirmed_expiry IS NULL) OR attempts>=11 THEN 'reconcile_required' ELSE state END,last_error_code=$2,next_attempt_at=$3,attempts=attempts+1,lease_owner=NULL,lease_until=NULL,updated_at=NOW() WHERE id=$4 AND generation=$5 AND lease_owner=$6 AND lease_until>clock_timestamp()`, reconcile, code, next, o.ID, o.Generation, o.LeaseOwner)
	if err != nil {
		return err
	}
	if err = requireCacheAffected(res); err != nil {
		return err
	}
	if reconcile || o.Attempts >= 11 {
		if _, err = tx.ExecContext(ctx, `UPDATE gemini_cached_contents SET state='reconcile_required',updated_at=NOW() WHERE id=$1`, o.ResourceID); err != nil {
			return err
		}
		if _, err = publishGeminiCacheEvent(ctx, tx, o.ResourceID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (r *geminiCachedContentRepository) ClaimRecovery(ctx context.Context, worker string, limit int, lease time.Duration) ([]service.GeminiCacheOperation, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	if lease < 60*time.Second || lease > 5*time.Minute {
		return nil, errors.New("invalid recovery lease")
	}
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, `WITH candidates AS (SELECT id FROM gemini_cache_operations WHERE state NOT IN ('settled','released') AND attempts<12 AND next_attempt_at<=NOW() AND (lease_until IS NULL OR lease_until<NOW()-INTERVAL '45 seconds') ORDER BY next_attempt_at,id LIMIT $1 FOR UPDATE SKIP LOCKED) UPDATE gemini_cache_operations SET lease_owner=$2,lease_until=NOW()+($3*INTERVAL '1 second'),generation=generation+1,updated_at=NOW() WHERE id IN (SELECT id FROM candidates) RETURNING `+cacheOperationColumns, limit, worker, int64(lease/time.Second))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]service.GeminiCacheOperation, 0, limit)
	for rows.Next() {
		op, e := scanCacheOperation(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, *op)
	}
	return out, rows.Err()
}
func (r *geminiCachedContentRepository) ReadView(ctx context.Context, revision int64, limit int, bootstrap bool) (*service.GeminiCacheViewBatch, error) {
	if limit <= 0 || limit > 100000 {
		return nil, service.ErrGeminiCacheUnavailable
	}
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := &service.GeminiCacheViewBatch{Complete: true}
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM gemini_cache_view_clock WHERE id=1`).Scan(&out.Revision); err != nil {
		return nil, err
	}
	if !bootstrap && revision < out.Revision {
		var oldest int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(revision),$1+1) FROM gemini_cache_events`, out.Revision).Scan(&oldest); err != nil {
			return nil, err
		}
		if revision < oldest-1 {
			bootstrap = true
		}
	}
	if !bootstrap {
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gemini_cache_events WHERE revision>$1 AND revision<=$2 AND resource_id='authorization')`, revision, out.Revision).Scan(&bootstrap); err != nil {
			return nil, err
		}
	}
	out.Reset = bootstrap
	const projection = `SELECT c.id,c.user_id,c.api_key_id,c.group_id,c.account_id,c.requested_model,c.upstream_model,c.upstream_name,
 CASE WHEN u.deleted_at IS NULL AND u.status='active' AND k.deleted_at IS NULL AND k.user_id=c.user_id AND k.group_id=c.group_id AND k.status IN ('active','expired','quota_exhausted')
 AND g.deleted_at IS NULL AND g.status='active' AND g.platform='gemini'
 AND ((g.subscription_type='subscription' AND EXISTS(SELECT 1 FROM user_subscriptions s WHERE s.user_id=c.user_id AND s.group_id=c.group_id AND s.deleted_at IS NULL AND s.status='active' AND s.starts_at<=NOW() AND s.expires_at>NOW()))
 OR (g.subscription_type<>'subscription' AND ((NOT g.is_exclusive AND NOT u.restrict_public_groups) OR EXISTS(SELECT 1 FROM user_allowed_groups ug WHERE ug.user_id=c.user_id AND ug.group_id=c.group_id))))
 AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable AND a.platform='gemini'
 AND EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=c.account_id AND ag.group_id=c.group_id)
 THEN c.state ELSE 'unavailable' END,c.expires_at,c.revision,a.platform,a.type,
 jsonb_build_object('base_url',a.credentials->'base_url','auth_mode',a.credentials->'auth_mode','pool_mode',a.credentials->'pool_mode','gemini_explicit_cache_enabled',a.credentials->'gemini_explicit_cache_enabled','gemini_explicit_cache_max_ttl_seconds',a.credentials->'gemini_explicit_cache_max_ttl_seconds')
 FROM gemini_cached_contents c JOIN users u ON u.id=c.user_id JOIN api_keys k ON k.id=c.api_key_id JOIN groups g ON g.id=c.group_id JOIN accounts a ON a.id=c.account_id`
	query := projection + ` WHERE c.state<>'deleted' AND (c.expires_at>NOW() OR c.state IN ('pending','reconcile_required')) ORDER BY c.id LIMIT $1`
	args := []any{limit + 1}
	if !bootstrap {
		query = projection + ` WHERE c.revision>$1 AND c.revision<=$2 ORDER BY c.revision LIMIT $3`
		args = []any{revision, out.Revision, limit + 1}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b service.GeminiCacheBinding
		var a service.Account
		var credentials []byte
		if err = rows.Scan(&b.ID, &b.Owner.UserID, &b.Owner.APIKeyID, &b.Owner.GroupID, &b.AccountID, &b.RequestedModel, &b.UpstreamModel, &b.UpstreamName, &b.State, &b.ExpiresAt, &b.Revision, &a.Platform, &a.Type, &credentials); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if json.Unmarshal(credentials, &a.Credentials) != nil || !service.IsExplicitCacheAccount(&a) {
			b.State = "unavailable"
		}
		out.Bindings = append(out.Bindings, b)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if len(out.Bindings) > limit {
		out.Complete = false
		out.Bindings = nil
		return out, nil
	}
	if bootstrap || len(out.Bindings) > 0 {
		// Resource changes may introduce a subscription owner without a separate
		// authorization event. Recalculate for either kind of projection change.
		err = tx.QueryRowContext(ctx, `SELECT MIN(boundary) FROM (
 SELECT DISTINCT user_id,group_id FROM gemini_cached_contents
 WHERE state<>'deleted' AND (expires_at>NOW() OR state IN ('pending','reconcile_required'))
 ) owners JOIN user_subscriptions s ON s.user_id=owners.user_id AND s.group_id=owners.group_id
 JOIN groups g ON g.id=owners.group_id
 CROSS JOIN LATERAL (VALUES(s.starts_at),(s.expires_at)) AS times(boundary)
 WHERE s.deleted_at IS NULL AND s.status='active' AND boundary>NOW()
 AND g.subscription_type='subscription' AND g.deleted_at IS NULL`).Scan(&out.NextAuthorizationChange)
		if err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
func (r *geminiCachedContentRepository) LeaseView(ctx context.Context, node string, revision int64, d time.Duration) error {
	if d <= 0 || d > 10*time.Second {
		return service.ErrGeminiCacheUnavailable
	}
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	tx, err := beginCacheTx(ctx, r.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int64
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM gemini_cache_view_clock WHERE id=1 FOR SHARE`).Scan(&current); err != nil {
		return err
	}
	if current != revision {
		return service.ErrGeminiCacheUnavailable
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO gemini_cache_view_nodes(node_id,revision,valid_until) VALUES($1,$2,NOW()+($3*INTERVAL '1 millisecond')) ON CONFLICT(node_id) DO UPDATE SET revision=EXCLUDED.revision,valid_until=EXCLUDED.valid_until,updated_at=NOW()`, node, revision, d.Milliseconds())
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (r *geminiCachedContentRepository) Barrier(ctx context.Context, revision int64) (bool, error) {
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	var allowed bool
	err := r.db.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM gemini_cache_view_nodes WHERE revision<$1 AND valid_until>NOW())`, revision).Scan(&allowed)
	return allowed, err
}

func (r *geminiCachedContentRepository) Reject(ctx context.Context, o *service.GeminiCacheOperation) error {
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	res, err := r.db.ExecContext(ctx, `UPDATE gemini_cache_operations SET state='reserved',updated_at=NOW() WHERE id=$1 AND generation=$2 AND lease_owner=$3 AND lease_until>clock_timestamp() AND state IN ('send_started','reconcile_required')`, o.ID, o.Generation, o.LeaseOwner)
	if err != nil {
		return err
	}
	if err = requireCacheAffected(res); err != nil {
		return err
	}
	o.State = "reserved"
	return nil
}

// Bounded management maintenance. Readers older than retained events bootstrap
// the full authorized projection; no expired node can acquire a fresh lease.
func (r *geminiCachedContentRepository) PruneCacheMetadata(ctx context.Context) error {
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	if _, err := r.db.ExecContext(ctx, `DELETE FROM gemini_cache_view_nodes WHERE node_id IN (SELECT node_id FROM gemini_cache_view_nodes WHERE valid_until<NOW()-INTERVAL '1 minute' ORDER BY valid_until LIMIT 100)`); err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM gemini_cache_events WHERE revision IN (SELECT revision FROM gemini_cache_events WHERE created_at<NOW()-INTERVAL '1 hour' ORDER BY revision LIMIT 1000)`); err != nil {
		return err
	}
	// A live owner's tombstones must never expire: removing one could allow a
	// delayed request to execute. Only permanently deleted owners can be pruned.
	_, err := r.db.ExecContext(ctx, `WITH candidates AS (
	 SELECT user_id,api_key_id,kind,idempotency_key_hash FROM gemini_cache_cancelled_intents
	 WHERE created_at<NOW()-INTERVAL '1 day' AND
	 (NOT EXISTS(SELECT 1 FROM users WHERE users.id=gemini_cache_cancelled_intents.user_id)
	 OR NOT EXISTS(SELECT 1 FROM api_keys WHERE api_keys.id=gemini_cache_cancelled_intents.api_key_id))
	 ORDER BY created_at LIMIT 100
	) DELETE FROM gemini_cache_cancelled_intents c USING candidates d
	 WHERE c.user_id=d.user_id AND c.api_key_id=d.api_key_id AND c.kind=d.kind AND c.idempotency_key_hash=d.idempotency_key_hash`)
	return err
}
