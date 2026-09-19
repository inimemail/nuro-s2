package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type seedanceTaskRepository struct{ db *sql.DB }

func NewSeedanceTaskRepository(db *sql.DB) service.SeedanceTaskRepository {
	return &seedanceTaskRepository{db: db}
}

func (r *seedanceTaskRepository) Reserve(ctx context.Context, task *service.SeedanceTask, limit int) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Separate namespace from other advisory locks; all reservations lock user
	// before account, bounding async inflight independently of HTTP concurrency.
	for _, lock := range []string{fmt.Sprintf("seedance:user:%d", task.UserID), fmt.Sprintf("seedance:account:%d", task.AccountID)} {
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lock); err != nil {
			return err
		}
	}
	var userCount, accountCount int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE user_id=$1), count(*) FILTER (WHERE account_id=$2)
	FROM seedance_tasks WHERE NOT settled AND state NOT IN ('rejected','failed','cancelled','expired','succeeded') AND (user_id=$1 OR account_id=$2)`, task.UserID, task.AccountID).Scan(&userCount, &accountCount)
	if err != nil {
		return err
	}
	if userCount >= 100 || accountCount >= limit {
		return service.ErrSeedanceCapacity
	}
	snapshot, err := json.Marshal(task.Snapshot)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO seedance_tasks (id,user_id,api_key_id,account_id,group_id,snapshot) VALUES ($1,$2,$3,$4,$5,$6)`, task.ID, task.UserID, task.APIKeyID, task.AccountID, task.GroupID, snapshot)
	if err != nil {
		var pgErr *pq.Error
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return service.ErrSeedanceDuplicate
		}
		return err
	}
	return tx.Commit()
}

func (r *seedanceTaskRepository) Submitted(ctx context.Context, id, provider string, body []byte, state string) error {
	if !json.Valid(body) {
		body = []byte(`{}`)
	}
	_, err := r.db.ExecContext(ctx, `UPDATE seedance_tasks SET provider_id=NULLIF($2,''), response=$3,state=$4,updated_at=NOW(),next_poll_at=NOW() WHERE id=$1 AND provider_id IS NULL`, id, provider, body, state)
	return err
}

const seedanceColumns = `id,COALESCE(provider_id,''),user_id,api_key_id,account_id,group_id,COALESCE(terminal_response->>'status',state),snapshot,COALESCE(terminal_response,response,'{}'),created_at,attempts,settled,effects_pending`

func scanSeedance(row interface{ Scan(...any) error }) (*service.SeedanceTask, error) {
	t := &service.SeedanceTask{}
	var snapshot, response []byte
	err := row.Scan(&t.ID, &t.ProviderID, &t.UserID, &t.APIKeyID, &t.AccountID, &t.GroupID, &t.State, &snapshot, &response, &t.CreatedAt, &t.Attempts, &t.Settled, &t.EffectsPending)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrSeedanceTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(snapshot, &t.Snapshot); err != nil {
		return nil, err
	}
	t.Response = response
	return t, nil
}
func (r *seedanceTaskRepository) Owned(ctx context.Context, id string, userID, keyID int64, groupID *int64) (*service.SeedanceTask, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+seedanceColumns+` FROM seedance_tasks WHERE (id=$1 OR provider_id=$1) AND user_id=$2 AND api_key_id=$3 AND group_id IS NOT DISTINCT FROM $4 ORDER BY (id=$1) DESC,created_at DESC LIMIT 2`, id, userID, keyID, groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrSeedanceTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, service.ErrSeedanceTaskNotFound
	}
	task, err := scanSeedance(rows)
	if err != nil {
		return nil, err
	}
	// The globally unique local ID wins. Provider IDs are only guaranteed to
	// be unique within an account, so never silently pick one across accounts.
	if task.ID == id {
		return task, nil
	}
	if rows.Next() {
		return nil, service.ErrSeedanceAmbiguousID
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return task, nil
}
func (r *seedanceTaskRepository) Claim(ctx context.Context) (*service.SeedanceTask, error) {
	return scanSeedance(r.db.QueryRowContext(ctx, `UPDATE seedance_tasks SET next_poll_at=NOW()+INTERVAL '90 seconds',attempts=LEAST(attempts+1,1000000) WHERE id=(SELECT id FROM seedance_tasks WHERE ((provider_id IS NOT NULL AND NOT settled) OR effects_pending) AND next_poll_at<=NOW() ORDER BY next_poll_at FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING `+seedanceColumns))
}
func (r *seedanceTaskRepository) Observe(ctx context.Context, id string, body []byte, state string, delay time.Duration) error {
	if !json.Valid(body) {
		body = nil
	}
	_, err := r.db.ExecContext(ctx, `UPDATE seedance_tasks SET response=COALESCE($2,response),state=CASE WHEN $3='' THEN state ELSE $3 END,updated_at=NOW(),next_poll_at=NOW()+($4 * INTERVAL '1 second') WHERE id=$1 AND NOT settled AND terminal_response IS NULL`, id, body, state, int(delay.Seconds()))
	return err
}

// Freeze the first verified terminal response before attempting billing or
// deleting the provider task. A concurrent stale poll cannot overwrite it.
func (r *seedanceTaskRepository) SaveTerminal(ctx context.Context, task *service.SeedanceTask) error {
	var body []byte
	err := r.db.QueryRowContext(ctx, `UPDATE seedance_tasks SET terminal_response=COALESCE(terminal_response,$2),state=COALESCE(terminal_response->>'status',$2::jsonb->>'status',state),updated_at=NOW() WHERE id=$1 RETURNING terminal_response,state`, task.ID, []byte(task.Response)).Scan(&body, &task.State)
	if err != nil {
		return err
	}
	task.Response = body
	return nil
}
func (r *seedanceTaskRepository) Settle(ctx context.Context, task *service.SeedanceTask, cmd *service.UsageBillingCommand, usage *service.UsageLog) (*service.UsageBillingApplyResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var settled bool
	if err = tx.QueryRowContext(ctx, `SELECT settled FROM seedance_tasks WHERE id=$1 FOR UPDATE`, task.ID).Scan(&settled); err != nil {
		return nil, err
	}
	if settled {
		return &service.UsageBillingApplyResult{}, nil
	}
	cmd.Normalize()
	billing := &usageBillingRepository{db: r.db}
	applied, err := billing.claimUsageBillingKey(ctx, tx, cmd)
	if err != nil {
		return nil, err
	}
	result := &service.UsageBillingApplyResult{Applied: applied}
	if applied {
		if err = billing.applyUsageBillingEffects(ctx, tx, cmd, result); err != nil {
			return nil, err
		}
		if _, err = (&usageLogRepository{}).createSingle(ctx, tx, usage); err != nil {
			return nil, err
		}
		if !task.Snapshot.PlatformQuotaFlusher && task.Snapshot.SubscriptionID == nil && usage.ActualCost > 0 && task.Snapshot.Platform != "" {
			now := time.Now()
			// Match existing calendar day/week and rolling 30-day month semantics.
			_, err = tx.ExecContext(ctx, `UPDATE user_platform_quotas SET
			 daily_usage_usd=CASE WHEN daily_window_start=$4 THEN daily_usage_usd+$3 ELSE $3 END,
			 weekly_usage_usd=CASE WHEN weekly_window_start=$5 THEN weekly_usage_usd+$3 ELSE $3 END,
			 monthly_usage_usd=CASE WHEN monthly_window_start IS NULL OR monthly_window_start<=$6-INTERVAL '30 days' THEN $3 ELSE monthly_usage_usd+$3 END,
			 daily_window_start=$4,weekly_window_start=$5,
			 monthly_window_start=CASE WHEN monthly_window_start IS NULL OR monthly_window_start<=$6-INTERVAL '30 days' THEN $6 ELSE monthly_window_start END,
			 updated_at=$6 WHERE user_id=$1 AND platform=$2 AND deleted_at IS NULL AND (daily_limit_usd IS NOT NULL OR weekly_limit_usd IS NOT NULL OR monthly_limit_usd IS NOT NULL)`, task.UserID, task.Snapshot.Platform, usage.ActualCost, timezone.StartOfDay(now), timezone.StartOfWeek(now), now)
			if err != nil {
				return nil, err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE seedance_tasks SET settled=TRUE,effects_pending=TRUE,state=$2,response=$3,updated_at=NOW(),next_poll_at=NOW() WHERE id=$1`, task.ID, task.State, []byte(task.Response))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
func (r *seedanceTaskRepository) CompleteEffects(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE seedance_tasks SET effects_pending=FALSE WHERE id=$1 AND settled`, id)
	return err
}
