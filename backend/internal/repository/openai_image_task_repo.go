package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type openAIImageTaskRepository struct {
	db  *sql.DB
	sql sqlExecutor
}

func NewOpenAIImageTaskRepository(sqlDB *sql.DB) service.OpenAIImageTaskRepository {
	return &openAIImageTaskRepository{db: sqlDB, sql: sqlDB}
}

const openAIImageTaskSubmitAdvisoryLockID int64 = 0x494D475441534B

func (r *openAIImageTaskRepository) SubmitWithinLimit(ctx context.Context, task *service.OpenAIImageTask, maxUnfinished int) (_ *service.OpenAIImageTask, _ bool, retErr error) {
	if r == nil || r.db == nil {
		return nil, false, errors.New("nil openai image task repository")
	}
	if task == nil {
		return nil, false, errors.New("nil openai image task")
	}
	headersJSON, err := json.Marshal(task.RequestHeaders)
	if err != nil {
		return nil, false, fmt.Errorf("marshal image task headers: %w", err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin image task submission: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, openAIImageTaskSubmitAdvisoryLockID); err != nil {
		return nil, false, fmt.Errorf("lock image task submission: %w", err)
	}
	existing, err := scanOpenAIImageTaskByOwnerAndTaskID(ctx, tx, task.OwnerID, task.ID)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return nil, false, fmt.Errorf("commit existing image task submission: %w", err)
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	if maxUnfinished > 0 {
		var count int64
		if err := scanSingleRow(ctx, tx, `
			SELECT COUNT(*)
			FROM openai_image_tasks
			WHERE status IN ($1, $2)
		`, []any{service.OpenAIImageTaskStatusQueued, service.OpenAIImageTaskStatusRunning}, &count); err != nil {
			return nil, false, err
		}
		if count >= int64(maxUnfinished) {
			return nil, false, service.ErrOpenAIImageTaskQueueFull
		}
	}
	query := `
		INSERT INTO openai_image_tasks (
			owner_id, task_id, api_key_id, user_id, user_concurrency,
			endpoint, model, status, request_body, request_headers
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)
		ON CONFLICT (owner_id, task_id) DO NOTHING
		RETURNING id, owner_id, task_id, api_key_id, user_id, user_concurrency,
			endpoint, model, status, request_body, request_headers,
			response_body, status_code, error_message, locked_by, locked_until,
			attempts, created_at, updated_at, started_at, finished_at
	`
	created, scanErr := scanOpenAIImageTaskWithArgs(ctx, tx, query, []any{
		task.OwnerID,
		task.ID,
		task.APIKeyID,
		task.UserID,
		task.UserConcurrency,
		task.Endpoint,
		task.Model,
		service.OpenAIImageTaskStatusQueued,
		task.RequestBody,
		string(headersJSON),
	})
	if scanErr != nil {
		return nil, false, scanErr
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit image task submission: %w", err)
	}
	return created, true, nil
}

func (r *openAIImageTaskRepository) List(ctx context.Context, ownerID string, ids []string, limit int) ([]*service.OpenAIImageTask, []string, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if len(ids) > 0 {
		rows, err = r.sql.QueryContext(ctx, `
			SELECT id, owner_id, task_id, api_key_id, user_id, user_concurrency,
				endpoint, model, status, request_body, request_headers,
				response_body, status_code, error_message, locked_by, locked_until,
				attempts, created_at, updated_at, started_at, finished_at
			FROM openai_image_tasks
			WHERE owner_id = $1 AND task_id = ANY($2)
			ORDER BY updated_at DESC, id DESC
		`, ownerID, pq.Array(ids))
	} else {
		rows, err = r.sql.QueryContext(ctx, `
			SELECT id, owner_id, task_id, api_key_id, user_id, user_concurrency,
				endpoint, model, status, request_body, request_headers,
				response_body, status_code, error_message, locked_by, locked_until,
				attempts, created_at, updated_at, started_at, finished_at
			FROM openai_image_tasks
			WHERE owner_id = $1
			ORDER BY updated_at DESC, id DESC
			LIMIT $2
		`, ownerID, limit)
	}
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	items, err := scanOpenAIImageTaskRows(rows)
	if err != nil {
		return nil, nil, err
	}
	if len(ids) == 0 {
		return items, nil, nil
	}
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if item != nil {
			seen[item.ID] = struct{}{}
		}
	}
	missing := make([]string, 0)
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			missing = append(missing, id)
		}
	}
	return items, missing, nil
}

func (r *openAIImageTaskRepository) ClaimNext(ctx context.Context, workerID string, lockFor time.Duration) (*service.OpenAIImageTask, error) {
	if lockFor <= 0 {
		lockFor = 30 * time.Minute
	}
	query := `
		WITH next AS (
			SELECT id
			FROM openai_image_tasks
			WHERE status = $1
				OR (status = $2 AND locked_until IS NOT NULL AND locked_until < NOW())
			ORDER BY created_at ASC, id ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE openai_image_tasks AS tasks
		SET status = $3,
			locked_by = $4,
			locked_until = NOW() + ($5 * interval '1 second'),
			attempts = attempts + 1,
			started_at = COALESCE(started_at, NOW()),
			updated_at = NOW(),
			error_message = ''
		FROM next
		WHERE tasks.id = next.id
		RETURNING tasks.id, tasks.owner_id, tasks.task_id, tasks.api_key_id, tasks.user_id, tasks.user_concurrency,
			tasks.endpoint, tasks.model, tasks.status, tasks.request_body, tasks.request_headers,
			tasks.response_body, tasks.status_code, tasks.error_message, tasks.locked_by, tasks.locked_until,
			tasks.attempts, tasks.created_at, tasks.updated_at, tasks.started_at, tasks.finished_at
	`
	task, err := r.scanTask(ctx, query, []any{
		service.OpenAIImageTaskStatusQueued,
		service.OpenAIImageTaskStatusRunning,
		service.OpenAIImageTaskStatusRunning,
		workerID,
		int(lockFor.Seconds()),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return task, err
}

func (r *openAIImageTaskRepository) MarkSuccess(ctx context.Context, dbID int64, statusCode int, response []byte) error {
	_, err := r.sql.ExecContext(ctx, `
		UPDATE openai_image_tasks
		SET status = $1,
			status_code = $2,
			response_body = $3,
			error_message = '',
			locked_by = '',
			locked_until = NULL,
			finished_at = NOW(),
			updated_at = NOW()
		WHERE id = $4
	`, service.OpenAIImageTaskStatusSuccess, statusCode, response, dbID)
	return err
}

func (r *openAIImageTaskRepository) MarkError(ctx context.Context, dbID int64, statusCode int, message string, response []byte) error {
	_, err := r.sql.ExecContext(ctx, `
		UPDATE openai_image_tasks
		SET status = $1,
			status_code = $2,
			response_body = $3,
			error_message = $4,
			locked_by = '',
			locked_until = NULL,
			finished_at = NOW(),
			updated_at = NOW()
		WHERE id = $5
	`, service.OpenAIImageTaskStatusError, statusCode, response, message, dbID)
	return err
}

func (r *openAIImageTaskRepository) CountUnfinished(ctx context.Context) (int64, error) {
	var count int64
	err := scanSingleRow(ctx, r.sql, `
		SELECT COUNT(*)
		FROM openai_image_tasks
		WHERE status IN ($1, $2)
	`, []any{service.OpenAIImageTaskStatusQueued, service.OpenAIImageTaskStatusRunning}, &count)
	return count, err
}

func (r *openAIImageTaskRepository) CleanupFinished(ctx context.Context, olderThan time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	res, err := r.sql.ExecContext(ctx, `
		WITH old AS (
			SELECT id
			FROM openai_image_tasks
			WHERE status IN ($1, $2)
				AND finished_at IS NOT NULL
				AND finished_at < $3
			ORDER BY finished_at ASC
			LIMIT $4
		)
		DELETE FROM openai_image_tasks
		USING old
		WHERE openai_image_tasks.id = old.id
	`, service.OpenAIImageTaskStatusSuccess, service.OpenAIImageTaskStatusError, olderThan, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *openAIImageTaskRepository) getByOwnerAndTaskID(ctx context.Context, ownerID, taskID string) (*service.OpenAIImageTask, error) {
	return scanOpenAIImageTaskByOwnerAndTaskID(ctx, r.sql, ownerID, taskID)
}

func scanOpenAIImageTaskByOwnerAndTaskID(ctx context.Context, q sqlQueryer, ownerID, taskID string) (*service.OpenAIImageTask, error) {
	return scanOpenAIImageTaskWithArgs(ctx, q, `
		SELECT id, owner_id, task_id, api_key_id, user_id, user_concurrency,
			endpoint, model, status, request_body, request_headers,
			response_body, status_code, error_message, locked_by, locked_until,
			attempts, created_at, updated_at, started_at, finished_at
		FROM openai_image_tasks
		WHERE owner_id = $1 AND task_id = $2
	`, []any{ownerID, taskID})
}

func (r *openAIImageTaskRepository) scanTask(ctx context.Context, query string, args []any) (*service.OpenAIImageTask, error) {
	return scanOpenAIImageTaskWithArgs(ctx, r.sql, query, args)
}

func scanOpenAIImageTaskWithArgs(ctx context.Context, q sqlQueryer, query string, args []any) (*service.OpenAIImageTask, error) {
	var task service.OpenAIImageTask
	if err := scanOpenAIImageTask(ctx, q, query, args, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

func scanOpenAIImageTaskRows(rows *sql.Rows) ([]*service.OpenAIImageTask, error) {
	items := make([]*service.OpenAIImageTask, 0)
	for rows.Next() {
		var task service.OpenAIImageTask
		if err := scanOpenAIImageTaskFromScanner(rows, &task); err != nil {
			return nil, err
		}
		items = append(items, &task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

type openAIImageTaskScanner interface {
	Scan(dest ...any) error
}

func scanOpenAIImageTask(ctx context.Context, q sqlQueryer, query string, args []any, task *service.OpenAIImageTask) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if err := scanOpenAIImageTaskFromScanner(rows, task); err != nil {
		return err
	}
	return rows.Err()
}

func scanOpenAIImageTaskFromScanner(scanner openAIImageTaskScanner, task *service.OpenAIImageTask) error {
	var headersJSON []byte
	var response sql.Null[[]byte]
	var lockedUntil sql.NullTime
	var startedAt sql.NullTime
	var finishedAt sql.NullTime
	if err := scanner.Scan(
		&task.DBID,
		&task.OwnerID,
		&task.ID,
		&task.APIKeyID,
		&task.UserID,
		&task.UserConcurrency,
		&task.Endpoint,
		&task.Model,
		&task.Status,
		&task.RequestBody,
		&headersJSON,
		&response,
		&task.StatusCode,
		&task.ErrorMessage,
		&task.LockedBy,
		&lockedUntil,
		&task.Attempts,
		&task.CreatedAt,
		&task.UpdatedAt,
		&startedAt,
		&finishedAt,
	); err != nil {
		return err
	}
	if len(headersJSON) > 0 {
		if err := json.Unmarshal(headersJSON, &task.RequestHeaders); err != nil {
			return fmt.Errorf("parse image task headers: %w", err)
		}
	}
	if response.Valid {
		task.Response = append([]byte(nil), response.V...)
	}
	if lockedUntil.Valid {
		task.LockedUntil = &lockedUntil.Time
	}
	if startedAt.Valid {
		task.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		task.FinishedAt = &finishedAt.Time
	}
	return nil
}
