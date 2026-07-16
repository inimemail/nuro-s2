package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type auditLogRepository struct{ db *sql.DB }

func NewAuditLogRepository(db *sql.DB) service.AuditLogRepository { return &auditLogRepository{db: db} }

const auditInsertSQL = `INSERT INTO audit_logs
(created_at, actor_user_id, actor_email, actor_role, auth_method, credential_masked,
 action, method, path, request_id, client_ip, user_agent, request_body, status_code, latency_ms, extra)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`

func auditLogValues(entry *service.AuditLog) []any {
	extra := "{}"
	if len(entry.Extra) > 0 {
		if raw, err := json.Marshal(entry.Extra); err == nil {
			extra = string(raw)
		}
	}
	created := entry.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	var actor any
	if entry.ActorUserID != nil && *entry.ActorUserID > 0 {
		actor = *entry.ActorUserID
	}
	return []any{created.UTC(), actor, auditTruncate(entry.ActorEmail, 255), auditTruncate(entry.ActorRole, 32),
		auditTruncate(entry.AuthMethod, 32), auditTruncate(entry.CredentialMasked, 160), auditTruncate(entry.Action, 128),
		auditTruncate(entry.Method, 16), auditTruncate(entry.Path, 512), auditTruncate(entry.RequestID, 64),
		auditTruncate(entry.ClientIP, 64), auditTruncate(entry.UserAgent, 512), entry.RequestBody,
		entry.StatusCode, entry.LatencyMs, extra}
}

func (r *auditLogRepository) BatchInsert(ctx context.Context, entries []*service.AuditLog) (int64, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("nil audit log repository")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, auditInsertSQL)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	defer stmt.Close()
	var inserted int64
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		if _, err := stmt.ExecContext(ctx, auditLogValues(entry)...); err != nil {
			_ = tx.Rollback()
			return inserted, err
		}
		inserted++
	}
	if err := tx.Commit(); err != nil {
		return inserted, err
	}
	return inserted, nil
}

func (r *auditLogRepository) Insert(ctx context.Context, entry *service.AuditLog) error {
	if r == nil || r.db == nil || entry == nil {
		return fmt.Errorf("invalid audit log insert")
	}
	_, err := r.db.ExecContext(ctx, auditInsertSQL, auditLogValues(entry)...)
	return err
}

func auditWhere(filter *service.AuditLogFilter) (string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	add := func(expr string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(expr, len(args)))
	}
	if filter.StartTime != nil {
		add("l.created_at >= $%d", filter.StartTime.UTC())
	}
	if filter.EndTime != nil {
		add("l.created_at <= $%d", filter.EndTime.UTC())
	}
	if filter.ActorUserID != nil {
		add("l.actor_user_id = $%d", *filter.ActorUserID)
	}
	if filter.ActorEmail != "" {
		add("l.actor_email ILIKE $%d", "%"+filter.ActorEmail+"%")
	}
	if filter.AuthMethod != "" {
		add("l.auth_method = $%d", filter.AuthMethod)
	}
	if filter.Action != "" {
		add("l.action = $%d", filter.Action)
	}
	if filter.Method != "" {
		add("l.method = $%d", strings.ToUpper(filter.Method))
	}
	if filter.ClientIP != "" {
		add("l.client_ip = $%d", filter.ClientIP)
	}
	if filter.Success != nil {
		if *filter.Success {
			clauses = append(clauses, "l.status_code < 400")
		} else {
			clauses = append(clauses, "l.status_code >= 400")
		}
	}
	if filter.Query != "" {
		args = append(args, "%"+filter.Query+"%")
		n := len(args)
		clauses = append(clauses, fmt.Sprintf("(l.path ILIKE $%d OR l.action ILIKE $%d OR l.actor_email ILIKE $%d)", n, n, n))
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

const auditSelect = `l.id, l.created_at, l.actor_user_id, l.actor_email, l.actor_role, l.auth_method,
l.credential_masked, l.action, l.method, l.path, l.request_id, l.client_ip, l.user_agent,
l.request_body, l.status_code, l.latency_ms, l.extra::text`

type auditScanner func(...any) error

func scanAudit(scanner auditScanner) (*service.AuditLog, error) {
	entry := &service.AuditLog{}
	var actor sql.NullInt64
	var extra string
	if err := scanner(&entry.ID, &entry.CreatedAt, &actor, &entry.ActorEmail, &entry.ActorRole, &entry.AuthMethod,
		&entry.CredentialMasked, &entry.Action, &entry.Method, &entry.Path, &entry.RequestID, &entry.ClientIP,
		&entry.UserAgent, &entry.RequestBody, &entry.StatusCode, &entry.LatencyMs, &extra); err != nil {
		return nil, err
	}
	if actor.Valid {
		value := actor.Int64
		entry.ActorUserID = &value
	}
	if extra != "" && extra != "{}" {
		_ = json.Unmarshal([]byte(extra), &entry.Extra)
	}
	return entry, nil
}

func (r *auditLogRepository) List(ctx context.Context, filter *service.AuditLogFilter) (*service.AuditLogList, error) {
	if filter == nil {
		filter = &service.AuditLogFilter{}
	}
	page, size := filter.Page, filter.PageSize
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = 50
	}
	if size > 200 {
		size = 200
	}
	where, args := auditWhere(filter)
	var total int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_logs l "+where, args...).Scan(&total); err != nil {
		return nil, err
	}
	queryArgs := append(append([]any{}, args...), size, (page-1)*size)
	query := "SELECT " + auditSelect + " FROM audit_logs l " + where + fmt.Sprintf(" ORDER BY l.created_at DESC, l.id DESC LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	rows, err := r.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]*service.AuditLog, 0, size)
	for rows.Next() {
		item, err := scanAudit(rows.Scan)
		if err != nil {
			return nil, err
		}
		item.RequestBody = ""
		items = append(items, item)
	}
	return &service.AuditLogList{Logs: items, Total: total, Page: page, PageSize: size}, rows.Err()
}

func (r *auditLogRepository) GetByID(ctx context.Context, id int64) (*service.AuditLog, error) {
	item, err := scanAudit(r.db.QueryRowContext(ctx, "SELECT "+auditSelect+" FROM audit_logs l WHERE l.id=$1", id).Scan)
	if err == sql.ErrNoRows {
		return nil, service.ErrAuditLogNotFound
	}
	return item, err
}

func (r *auditLogRepository) Count(ctx context.Context) (int64, error) {
	var count int64
	err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_logs").Scan(&count)
	return count, err
}

func (r *auditLogRepository) TruncateAll(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, "TRUNCATE TABLE audit_logs")
	return err
}

func (r *auditLogRepository) DeleteBefore(ctx context.Context, cutoff time.Time, size int) (int64, error) {
	if size <= 0 {
		size = 5000
	}
	result, err := r.db.ExecContext(ctx, `WITH batch AS (SELECT id FROM audit_logs WHERE created_at < $1 ORDER BY id LIMIT $2) DELETE FROM audit_logs WHERE id IN (SELECT id FROM batch)`, cutoff.UTC(), size)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func auditTruncate(value string, max int) string {
	runes := []rune(strings.TrimSpace(value))
	for len(string(runes)) > max {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}
