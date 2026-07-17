package securityaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type deleteTokenPayload struct {
	ActorID   int64       `json:"actor_id"`
	Filter    EventFilter `json:"filter"`
	MaxID     int64       `json:"max_id"`
	ExpiresAt time.Time   `json:"expires_at"`
}

type recoverableJob struct {
	ID            int64
	Request       Request
	Hash          string
	Preview       string
	PromptLength  int
	MessageCount  int
	ConfigVersion int64
	HasEvent      bool
}

func (s *Service) claimRecoverableJobs(ctx context.Context, staleBefore time.Time, limit int) ([]recoverableJob, error) {
	if s == nil || s.db == nil || limit <= 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
SELECT j.id, j.request_id, COALESCE(j.user_id,0), j.user_email_snapshot,
       COALESCE(j.api_key_id,0), j.api_key_name_snapshot, j.group_id, j.group_name,
       j.provider, j.endpoint, j.protocol, j.model, j.prompt_hash, j.redacted_preview,
       j.prompt_length, j.message_count, j.stage, j.config_version,
       EXISTS (SELECT 1 FROM prompt_audit_events e WHERE e.job_id=j.id)
FROM prompt_audit_jobs j
WHERE j.status IN ('staging','queued','processing','retry')
  AND j.updated_at < $1
ORDER BY j.updated_at ASC, j.id ASC
FOR UPDATE OF j SKIP LOCKED
LIMIT $2`, staleBefore, limit)
	if err != nil {
		return nil, err
	}
	jobs := make([]recoverableJob, 0, limit)
	for rows.Next() {
		var job recoverableJob
		var groupID sql.NullInt64
		if err := rows.Scan(
			&job.ID, &job.Request.RequestID, &job.Request.UserID, &job.Request.UserEmail,
			&job.Request.APIKeyID, &job.Request.APIKeyName, &groupID, &job.Request.GroupName,
			&job.Request.Provider, &job.Request.Endpoint, &job.Request.Protocol, &job.Request.Model,
			&job.Hash, &job.Preview, &job.PromptLength, &job.MessageCount, &job.Request.Stage,
			&job.ConfigVersion, &job.HasEvent,
		); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if groupID.Valid {
			value := groupID.Int64
			job.Request.GroupID = &value
		}
		jobs = append(jobs, job)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	claimed := make([]recoverableJob, 0, len(jobs))
	for _, job := range jobs {
		if job.HasEvent {
			if _, err := tx.ExecContext(ctx, `
UPDATE prompt_audit_jobs
SET status='done', last_error_code='', processed_at=COALESCE(processed_at,NOW()), updated_at=NOW()
WHERE id=$1`, job.ID); err != nil {
				return nil, err
			}
			continue
		}
		result, err := tx.ExecContext(ctx, `
UPDATE prompt_audit_jobs
SET status='queued', last_error_code='', updated_at=NOW()
WHERE id=$1 AND status IN ('staging','queued','processing','retry')`, job.ID)
		if err != nil {
			return nil, err
		}
		if affected, _ := result.RowsAffected(); affected == 1 {
			claimed = append(claimed, job)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

func (s *Service) createJob(ctx context.Context, req Request, hash, preview string, promptLength, messageCount int, configVersion int64) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("prompt audit database unavailable")
	}
	req = postgresSafeRequest(req)
	preview = sanitizePreviewText(preview)
	var groupID any
	if req.GroupID != nil {
		groupID = *req.GroupID
	}
	var jobID int64
	err := s.db.QueryRowContext(ctx, `
INSERT INTO prompt_audit_jobs (
 request_id, user_id, user_email_snapshot, api_key_id, api_key_name_snapshot,
 group_id, group_name, provider, endpoint, protocol, model, prompt_hash,
 redacted_preview, prompt_length, message_count, stage, config_version, status
) VALUES (
 $1, NULLIF($2, 0), $3, NULLIF($4, 0), $5,
 $6, $7, $8, $9, $10, $11, $12,
 $13, $14, $15, $16, $17, 'staging'
) RETURNING id`,
		req.RequestID, req.UserID, req.UserEmail, req.APIKeyID, req.APIKeyName,
		groupID, req.GroupName, req.Provider, req.Endpoint, req.Protocol, req.Model,
		hash, preview, promptLength, messageCount, req.Stage, configVersion,
	).Scan(&jobID)
	return jobID, err
}

func (s *Service) updateJobStatus(ctx context.Context, jobID int64, status, code string) error {
	if s == nil || s.db == nil || jobID <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE prompt_audit_jobs
SET status=$2, last_error_code=$3,
    attempts=CASE WHEN $2 IN ('processing','retry','failed') THEN attempts+1 ELSE attempts END,
    processing_started_at=CASE WHEN $2='processing' THEN NOW() ELSE processing_started_at END,
    processed_at=CASE WHEN $2 IN ('done','failed') THEN NOW() ELSE processed_at END,
    updated_at=NOW()
WHERE id=$1`, jobID, status, code)
	return err
}

func (s *Service) createEvent(ctx context.Context, jobID int64, req Request, hash, preview string, promptLength, messageCount int, result scanResult, configVersion int64, errorCode string) error {
	if s == nil || s.db == nil {
		return errors.New("prompt audit database unavailable")
	}
	req = postgresSafeRequest(req)
	preview = sanitizePreviewText(preview)
	result.Backend = sanitizePreviewText(result.Backend)
	result.Version = sanitizePreviewText(result.Version)
	result.EndpointID = sanitizePreviewText(result.EndpointID)
	categories, err := json.Marshal(result.Categories)
	if err != nil {
		return err
	}
	var groupID any
	if req.GroupID != nil {
		groupID = *req.GroupID
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO prompt_audit_events (
 job_id, request_id, user_id, user_email_snapshot, api_key_id, api_key_name_snapshot,
 group_id, group_name, provider, endpoint, protocol, model, prompt_hash,
 redacted_preview, prompt_length, message_count, stage, decision, risk_level,
 action, categories, scanner_backend, scanner_version, guard_endpoint_id,
 config_version, latency_ms, error_code
) VALUES (
 $1, $2, NULLIF($3,0), $4, NULLIF($5,0), $6,
 $7, $8, $9, $10, $11, $12, $13,
 $14, $15, $16, $17, $18, $19,
 $20, $21::jsonb, $22, $23, $24, $25, $26, $27
)
ON CONFLICT (job_id) DO NOTHING`,
		jobID, req.RequestID, req.UserID, req.UserEmail, req.APIKeyID, req.APIKeyName,
		groupID, req.GroupName, req.Provider, req.Endpoint, req.Protocol, req.Model,
		hash, preview, promptLength, messageCount, req.Stage, result.Decision, result.Risk,
		result.Action, string(categories), result.Backend, result.Version, result.EndpointID,
		configVersion, result.LatencyMS, errorCode,
	)
	return err
}

func postgresSafeRequest(req Request) Request {
	req.RequestID = sanitizePreviewText(req.RequestID)
	req.UserEmail = sanitizePreviewText(req.UserEmail)
	req.APIKeyName = sanitizePreviewText(req.APIKeyName)
	req.GroupName = sanitizePreviewText(req.GroupName)
	req.Provider = sanitizePreviewText(req.Provider)
	req.Endpoint = sanitizePreviewText(req.Endpoint)
	req.Protocol = sanitizePreviewText(req.Protocol)
	req.Model = sanitizePreviewText(req.Model)
	req.Stage = sanitizePreviewText(req.Stage)
	return req
}

func (s *Service) ListEvents(ctx context.Context, filter EventFilter) (*EventList, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("prompt audit database unavailable")
	}
	if filter.Page <= 0 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 {
		filter.PageSize = 20
	}
	if filter.PageSize > MaxEventPageSize {
		filter.PageSize = MaxEventPageSize
	}
	if err := validateEventFilter(filter); err != nil {
		return nil, err
	}
	whereSQL, args := eventFilterWhere(filter)
	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM prompt_audit_events WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, err
	}
	queryArgs := append([]any(nil), args...)
	queryArgs = append(queryArgs, filter.PageSize, (filter.Page-1)*filter.PageSize)
	rows, err := s.db.QueryContext(ctx, `
SELECT id, request_id, COALESCE(user_id,0), user_email_snapshot,
       COALESCE(api_key_id,0), api_key_name_snapshot, group_id, group_name,
       provider, endpoint, protocol, model, prompt_hash, redacted_preview,
       prompt_length, message_count, stage, decision, risk_level, action,
       categories, scanner_backend, scanner_version, guard_endpoint_id,
       latency_ms, error_code, created_at
FROM prompt_audit_events
WHERE `+whereSQL+`
ORDER BY created_at DESC, id DESC
LIMIT $`+fmt.Sprint(len(queryArgs)-1)+` OFFSET $`+fmt.Sprint(len(queryArgs)), queryArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]Event, 0, filter.PageSize)
	for rows.Next() {
		item, scanErr := scanEvent(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &EventList{Items: items, Total: total, Page: filter.Page, PageSize: filter.PageSize}, nil
}

func eventFilterWhere(filter EventFilter) (string, []any) {
	where := []string{"1=1"}
	args := make([]any, 0, 8)
	add := func(format string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(format, len(args)))
	}
	if value := strings.TrimSpace(filter.Decision); value != "" {
		add("decision=$%d", value)
	}
	if value := strings.TrimSpace(filter.RiskLevel); value != "" {
		add("risk_level=$%d", value)
	}
	if filter.GroupID != nil {
		add("group_id=$%d", *filter.GroupID)
	}
	if filter.UserID != nil {
		add("user_id=$%d", *filter.UserID)
	}
	if value := strings.TrimSpace(filter.Search); value != "" {
		args = append(args, value)
		placeholder := "$" + fmt.Sprint(len(args))
		where = append(where, "(request_id ILIKE '%' || "+placeholder+" || '%' OR prompt_hash ILIKE '%' || "+placeholder+" || '%' OR user_email_snapshot ILIKE '%' || "+placeholder+" || '%')")
	}
	return strings.Join(where, " AND "), args
}

func validateEventFilter(filter EventFilter) error {
	if filter.Page < 0 || filter.PageSize < 0 || filter.PageSize > MaxEventPageSize {
		return infraerrors.BadRequest("PROMPT_AUDIT_FILTER_INVALID", "event filter is invalid")
	}
	if (filter.GroupID != nil && *filter.GroupID <= 0) || (filter.UserID != nil && *filter.UserID <= 0) {
		return infraerrors.BadRequest("PROMPT_AUDIT_FILTER_INVALID", "event filter is invalid")
	}
	if len([]rune(strings.TrimSpace(filter.Search))) > MaxEventSearchRunes {
		return infraerrors.BadRequest("PROMPT_AUDIT_SEARCH_TOO_LONG", "event search is too long")
	}
	if value := strings.TrimSpace(filter.Decision); value != "" {
		switch value {
		case "pass", "flag", "critical", "unavailable":
		default:
			return infraerrors.BadRequest("PROMPT_AUDIT_FILTER_INVALID", "event filter is invalid")
		}
	}
	if value := strings.TrimSpace(filter.RiskLevel); value != "" {
		switch value {
		case "low", "medium", "high", "critical", "unknown":
		default:
			return infraerrors.BadRequest("PROMPT_AUDIT_FILTER_INVALID", "event filter is invalid")
		}
	}
	return nil
}

type rowScanner interface{ Scan(...any) error }

func scanEvent(row rowScanner) (Event, error) {
	var item Event
	var groupID sql.NullInt64
	var categories []byte
	err := row.Scan(
		&item.ID, &item.RequestID, &item.UserID, &item.UserEmail,
		&item.APIKeyID, &item.APIKeyName, &groupID, &item.GroupName,
		&item.Provider, &item.Endpoint, &item.Protocol, &item.Model,
		&item.PromptHash, &item.Preview, &item.PromptLength, &item.MessageCount,
		&item.Stage, &item.Decision, &item.RiskLevel, &item.Action,
		&categories, &item.ScannerBackend, &item.ScannerVersion, &item.EndpointID,
		&item.LatencyMS, &item.ErrorCode, &item.CreatedAt,
	)
	if err != nil {
		return Event{}, err
	}
	if groupID.Valid {
		value := groupID.Int64
		item.GroupID = &value
	}
	item.Categories = []string{}
	_ = json.Unmarshal(categories, &item.Categories)
	return item, nil
}

func (s *Service) GetEvent(ctx context.Context, id int64) (*Event, error) {
	if s == nil || s.db == nil || id <= 0 {
		return nil, sql.ErrNoRows
	}
	row := s.db.QueryRowContext(ctx, `
SELECT id, request_id, COALESCE(user_id,0), user_email_snapshot,
       COALESCE(api_key_id,0), api_key_name_snapshot, group_id, group_name,
       provider, endpoint, protocol, model, prompt_hash, redacted_preview,
       prompt_length, message_count, stage, decision, risk_level, action,
       categories, scanner_backend, scanner_version, guard_endpoint_id,
       latency_ms, error_code, created_at
FROM prompt_audit_events WHERE id=$1`, id)
	item, err := scanEvent(row)
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *Service) DeleteEvent(ctx context.Context, id int64) (bool, error) {
	if s == nil || s.db == nil || id <= 0 {
		return false, nil
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM prompt_audit_events WHERE id=$1", id)
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	return count > 0, nil
}

func (s *Service) DeleteEvents(ctx context.Context, ids []int64) (int64, error) {
	if s == nil || s.db == nil || len(ids) == 0 {
		return 0, nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		args = append(args, id)
		placeholders = append(placeholders, "$"+fmt.Sprint(len(args)))
	}
	if len(args) == 0 {
		return 0, nil
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM prompt_audit_events WHERE id IN ("+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Service) cleanupExpired(ctx context.Context, retentionDays int) error {
	if s == nil || s.db == nil {
		return nil
	}
	if retentionDays < 1 || retentionDays > MaxRetentionDays {
		retentionDays = DefaultRetentionDays
	}
	cutoff := time.Now().UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	if _, err := s.db.ExecContext(ctx, "DELETE FROM prompt_audit_events WHERE created_at < $1", cutoff); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
DELETE FROM prompt_audit_jobs j
WHERE j.created_at < $1
  AND NOT EXISTS (SELECT 1 FROM prompt_audit_events e WHERE e.job_id=j.id)`, cutoff)
	return err
}

func (s *Service) CreateDeletePreview(ctx context.Context, filter EventFilter, actorID int64) (*DeletePreview, error) {
	if s == nil || s.db == nil || s.encryptor == nil || actorID <= 0 {
		return nil, errors.New("prompt audit delete preview unavailable")
	}
	if err := validateEventFilter(filter); err != nil {
		return nil, err
	}
	whereSQL, args := eventFilterWhere(filter)
	var count, maxID int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(MAX(id),0) FROM prompt_audit_events WHERE "+whereSQL, args...).Scan(&count, &maxID); err != nil {
		return nil, err
	}
	expiresAt := time.Now().UTC().Add(5 * time.Minute)
	payload := deleteTokenPayload{ActorID: actorID, Filter: filter, MaxID: maxID, ExpiresAt: expiresAt}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	token, err := s.encryptor.Encrypt(string(raw))
	if err != nil {
		return nil, errors.New("prompt audit delete preview unavailable")
	}
	return &DeletePreview{Count: count, MaxID: maxID, ExpiresAt: expiresAt, Token: token}, nil
}

func (s *Service) DeleteByFilter(ctx context.Context, token string, actorID int64) (int64, error) {
	payload, err := s.verifyDeleteToken(token, actorID)
	if err != nil {
		return 0, err
	}
	whereSQL, args := eventFilterWhere(payload.Filter)
	args = append(args, payload.MaxID)
	whereSQL += " AND id <= $" + fmt.Sprint(len(args))
	result, err := s.db.ExecContext(ctx, "DELETE FROM prompt_audit_events WHERE "+whereSQL, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Service) verifyDeleteToken(token string, actorID int64) (deleteTokenPayload, error) {
	if s == nil || s.encryptor == nil || strings.TrimSpace(token) == "" {
		return deleteTokenPayload{}, errors.New("delete confirmation token invalid")
	}
	raw, err := s.encryptor.Decrypt(strings.TrimSpace(token))
	if err != nil {
		return deleteTokenPayload{}, errors.New("delete confirmation token invalid")
	}
	var payload deleteTokenPayload
	if json.Unmarshal([]byte(raw), &payload) != nil || payload.ActorID != actorID || time.Now().UTC().After(payload.ExpiresAt) {
		return deleteTokenPayload{}, errors.New("delete confirmation token expired or invalid")
	}
	return payload, nil
}
