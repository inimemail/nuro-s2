package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

func blacklistAdmin(ctx context.Context, exec sqlExecutor, actor int64, lock bool) error {
	q := "SELECT role,status FROM users WHERE id=$1 AND deleted_at IS NULL"
	if lock {
		q += " FOR SHARE"
	}
	rows, err := exec.QueryContext(ctx, q, actor)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var role, status string
	if !rows.Next() {
		return infraerrors.Forbidden("ADMIN_REQUIRED", "Administrator access required")
	}
	if err = rows.Scan(&role, &status); err != nil {
		return err
	}
	if role != service.RoleAdmin || status != service.StatusActive {
		return infraerrors.Forbidden("ADMIN_REQUIRED", "Administrator access required")
	}
	return rows.Err()
}
func blacklistRules(ctx context.Context, exec sqlExecutor, lock bool) ([]string, error) {
	q := "SELECT value FROM settings WHERE key=$1"
	if lock {
		q += " FOR SHARE"
	}
	rows, err := exec.QueryContext(ctx, q, service.SettingKeyRegistrationEmailBlacklist)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	raw := "[]"
	if rows.Next() {
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return service.DecodeRegistrationEmailBlacklist(raw)
}
func (r *userRepository) PreviewRegistrationBlacklist(ctx context.Context, actor, after int64) (*service.RegistrationBlacklistPreview, error) {
	if after < 0 {
		return nil, fmt.Errorf("invalid preview position")
	}
	if err := blacklistAdmin(ctx, r.sql, actor, false); err != nil {
		return nil, err
	}
	entries, err := blacklistRules(ctx, r.sql, false)
	if err != nil {
		return nil, err
	}
	result := &service.RegistrationBlacklistPreview{Revision: service.RegistrationBlacklistRevision(entries), Users: []service.RegistrationBlacklistCandidate{}}
	if len(entries) == 0 {
		return result, nil
	}
	// Use the same canonical comparison as registration. Raw login/delivery email
	// is returned unchanged; the final Go check also guards expression drift.
	// Match Go strings.TrimSpace for historical/imported addresses too.
	trimChars := " \t\n\v\f\r\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
	local := "split_part(lower(btrim(email,$3)), '@', 1)"
	base := "CASE WHEN position('+' in " + local + ")>1 THEN split_part(" + local + ",'+',1) ELSE " + local + " END"
	domain := "rtrim(split_part(lower(btrim(email,$3)),'@',2),'.')"
	canonical := "CASE WHEN " + domain + " IN ('gmail.com','googlemail.com') THEN COALESCE(NULLIF(replace((" + base + "),'.',''),''),(" + base + "))||'@gmail.com' ELSE (" + base + ")||'@'||" + domain + " END"
	blocked := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		blocked[e] = struct{}{}
	}
	position := after
	for len(result.Users) < 101 {
		rows, err := r.sql.QueryContext(ctx, "SELECT id,email,updated_at FROM users WHERE deleted_at IS NULL AND role='user' AND status='active' AND id>$1 AND length(email)-length(replace(email,'@',''))=1 AND ("+canonical+")=ANY($2) ORDER BY id LIMIT 101", position, pq.Array(entries), trimChars)
		if err != nil {
			return nil, err
		}
		scanned := 0
		for rows.Next() {
			scanned++
			var u service.RegistrationBlacklistCandidate
			if err = rows.Scan(&u.ID, &u.Email, &u.UpdatedAt); err != nil {
				_ = rows.Close()
				return nil, err
			}
			position = u.ID
			if _, ok := blocked[service.NormalizeEmailForAliasDedup(u.Email)]; ok {
				result.Users = append(result.Users, u)
			}
		}
		if err = rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
		if scanned < 101 {
			break
		}
	}

	result.HasMore = len(result.Users) > 100
	if result.HasMore {
		result.Users = result.Users[:100]
	}
	if len(result.Users) > 0 {
		result.NextID = result.Users[len(result.Users)-1].ID
	}
	return result, nil
}
func (r *userRepository) DisableRegistrationBlacklist(ctx context.Context, actor int64, req service.RegistrationBlacklistDisableRequest) ([]int64, error) {
	if !req.Confirm || len(req.Users) == 0 || len(req.Users) > 100 {
		return nil, fmt.Errorf("invalid confirmed batch")
	}
	batchID, err := uuid.Parse(req.BatchID)
	if err != nil {
		return nil, fmt.Errorf("invalid batch identifier")
	}
	req.BatchID = batchID.String()
	req.Users = slices.Clone(req.Users)
	db, ok := r.sql.(*sql.DB)
	if !ok {
		return nil, service.ErrServiceUnavailable
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = blacklistAdmin(ctx, tx, actor, true); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "registration-blacklist:"+req.BatchID); err != nil {
		return nil, err
	}
	// Stable request fingerprint prevents reusing a successful batch for new users.
	sort.Slice(req.Users, func(i, j int) bool { return req.Users[i].ID < req.Users[j].ID })
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(sum[:])
	rows, err := tx.QueryContext(ctx, "SELECT extra FROM audit_logs WHERE action='admin.users.registration_blacklist.disable' AND actor_user_id=$1 AND extra->>'batch_id'=$2 ORDER BY id LIMIT 1", actor, req.BatchID)
	if err != nil {
		return nil, err
	}
	if rows.Next() {
		var data []byte
		err = rows.Scan(&data)
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		var prior struct {
			Fingerprint string  `json:"fingerprint"`
			IDs         []int64 `json:"ids"`
		}
		if json.Unmarshal(data, &prior) != nil || prior.Fingerprint != fingerprint {
			return nil, fmt.Errorf("batch identifier already used")
		}
		return prior.IDs, tx.Commit()
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	entries, err := blacklistRules(ctx, tx, true)
	if err != nil {
		return nil, err
	}
	conflict := func() error {
		return infraerrors.Conflict("BLACKLIST_PREVIEW_CHANGED", "Rules or users changed; refresh the preview")
	}
	if req.Revision != service.RegistrationBlacklistRevision(entries) {
		return nil, conflict()
	}
	blocked := make(map[string]bool, len(entries))
	for _, e := range entries {
		blocked[e] = true
	}
	ids := make([]int64, 0, len(req.Users))
	before := make([]map[string]any, 0, len(req.Users))
	for _, expected := range req.Users {
		if expected.ID <= 0 || expected.ID == actor || (len(ids) > 0 && ids[len(ids)-1] == expected.ID) {
			return nil, conflict()
		}
		var email, status, role string
		var updated time.Time
		row := tx.QueryRowContext(ctx, "SELECT email,status,role,updated_at FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", expected.ID)
		if err = row.Scan(&email, &status, &role, &updated); err != nil {
			return nil, conflict()
		}
		if email != expected.Email || !updated.Equal(expected.UpdatedAt) || status != service.StatusActive || role != service.RoleUser || !blocked[service.NormalizeEmailForAliasDedup(email)] {
			return nil, conflict()
		}
		res, err := tx.ExecContext(ctx, "UPDATE users SET status='disabled',updated_at=NOW() WHERE id=$1 AND updated_at=$2 AND status='active' AND role='user' AND deleted_at IS NULL", expected.ID, expected.UpdatedAt)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			return nil, conflict()
		}
		ids = append(ids, expected.ID)
		before = append(before, map[string]any{"id": expected.ID, "previous_status": status, "previous_updated_at": updated})
	}
	extra := map[string]any{"batch_id": req.BatchID, "fingerprint": fingerprint, "ids": ids, "before": before, "rules_revision": req.Revision}
	_, err = tx.ExecContext(ctx, auditInsertSQL, auditLogValues(&service.AuditLog{ActorUserID: &actor, ActorRole: service.RoleAdmin, AuthMethod: service.AuditAuthMethodJWT, Action: "admin.users.registration_blacklist.disable", Method: "POST", Path: "/admin/users/registration-blacklist/disable", StatusCode: 200, Extra: extra})...)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}
