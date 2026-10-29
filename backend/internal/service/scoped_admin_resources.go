package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	apperrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/lib/pq"
)

// A narrow repository hook publishes only after the enclosing transaction
// commits. It uses the existing scheduler snapshot/outbox implementation.
type ScopedAdminAccountRepository interface {
	ScopedAdminCreateRepositories(*dbent.Tx) (AccountRepository, GroupRepository)
	ScopedAdminPublish(context.Context, int64, bool)
}

func scopedBadRequest(message string) error {
	return apperrors.BadRequest("INVALID_SCOPED_REQUEST", message)
}
func decodeScopedJSON(raw []byte, out any) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return scopedBadRequest("A JSON object is required")
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return scopedBadRequest("Null fields are not supported")
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(out); err != nil {
		return scopedBadRequest("Invalid or unsupported fields")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return scopedBadRequest("Exactly one JSON object is required")
	}
	return nil
}
func scopedArg(args *[]any, v any) string {
	*args = append(*args, v)
	return "$" + strconv.Itoa(len(*args))
}

// Keep every dimension in one rule. Combining IDs from one rule with a
// platform/type from another would authorize tuples the owner never granted.
func scopedAccountWhere(p adminaccess.Policy, permission string, args *[]any) string {
	var clauses []string
	for _, r := range p.Rules {
		if !adminaccess.Contains(r.Permissions, permission) {
			continue
		}
		if !r.All && len(r.AccountIDs) == 0 && len(r.GroupIDs) == 0 && !r.Ungrouped {
			continue
		}
		parts := []string{"TRUE"}
		if len(r.Platforms) > 0 {
			parts = append(parts, "a.platform=ANY("+scopedArg(args, pq.Array(r.Platforms))+"::text[])")
		}
		if len(r.AccountTypes) > 0 {
			parts = append(parts, "a.type=ANY("+scopedArg(args, pq.Array(r.AccountTypes))+"::text[])")
		}
		if !r.All {
			var ids []string
			if len(r.AccountIDs) > 0 {
				ids = append(ids, "a.id=ANY("+scopedArg(args, pq.Array(r.AccountIDs))+"::bigint[])")
			}
			if len(r.GroupIDs) > 0 {
				ids = append(ids, "EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=a.id AND ag.group_id=ANY("+scopedArg(args, pq.Array(r.GroupIDs))+"::bigint[]))")
			}
			if r.Ungrouped {
				ids = append(ids, "NOT EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=a.id)")
			}
			if len(ids) == 0 {
				continue
			}
			parts = append(parts, "("+strings.Join(ids, " OR ")+")")
		}
		clauses = append(clauses, "("+strings.Join(parts, " AND ")+")")
	}
	if len(clauses) == 0 {
		return "FALSE"
	}
	return "(" + strings.Join(clauses, " OR ") + ")"
}
func scopedResourceWhere(p adminaccess.Policy, kind, permission string, args *[]any) string {
	var clauses []string
	for _, r := range p.Rules {
		if !adminaccess.Contains(r.Permissions, permission) {
			continue
		}
		parts := []string{}
		ids := r.ProxyIDs
		if kind == "groups" {
			ids = r.GroupIDs
		}
		if !r.All && len(ids) == 0 {
			continue
		}
		if kind == "groups" {
			if len(r.Platforms) > 0 {
				parts = append(parts, "platform=ANY("+scopedArg(args, pq.Array(r.Platforms))+"::text[])")
			}
		}
		if !r.All {
			parts = append(parts, "id=ANY("+scopedArg(args, pq.Array(ids))+"::bigint[])")
		}
		if len(parts) == 0 {
			parts = append(parts, "TRUE")
		}
		clauses = append(clauses, "("+strings.Join(parts, " AND ")+")")
	}
	if len(clauses) == 0 {
		return "FALSE"
	}
	return "(" + strings.Join(clauses, " OR ") + ")"
}

func readScopedJSON(ctx context.Context, q scopedSQL, query string, args ...any) (json.RawMessage, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return nil, err
		}
		return nil, ErrScopedAdminNotFound
	}
	var raw []byte
	if err = rows.Scan(&raw); err != nil {
		return nil, err
	}
	return raw, rows.Err()
}

func scopedAccountProjection(p adminaccess.Policy, args *[]any) string {
	base := `jsonb_build_object('id',a.id,'name',a.name,'platform',a.platform,'type',a.type,'status',a.status)`
	// Credentials are never serialized; only the explicitly authorized mapping
	// subfield may be read, including on idempotent replay.
	mapping := scopedAccountWhere(p, "accounts.models.read", args)
	return base + ` || CASE WHEN ` + mapping + ` THEN jsonb_build_object('model_mapping',COALESCE(a.credentials->'model_mapping','{}'::jsonb)) ELSE '{}'::jsonb END`
}
func (s *ScopedAdminService) readResource(ctx context.Context, q scopedSQL, p adminaccess.Policy, kind string, id int64, page, size int) (json.RawMessage, error) {
	args := []any{}
	var from, where, projection string
	switch kind {
	case "accounts":
		from = "accounts a"
		where = "a.deleted_at IS NULL AND " + scopedAccountWhere(p, "accounts.read", &args)
		projection = scopedAccountProjection(p, &args)
	case "groups":
		from = "groups"
		where = "deleted_at IS NULL AND " + scopedResourceWhere(p, kind, "groups.read", &args)
		projection = `jsonb_build_object('id',id,'name',name,'platform',platform,'status',status)`
	case "proxies":
		from = "proxies"
		where = "deleted_at IS NULL AND " + scopedResourceWhere(p, kind, "proxies.read", &args)
		projection = `jsonb_build_object('id',id,'name',name,'status',status)`
	default:
		return nil, ErrScopedAdminDenied
	}
	idcol := "id"
	if kind == "accounts" {
		idcol = "a.id"
	}
	if id > 0 {
		where += " AND " + idcol + "=" + scopedArg(&args, id)
		return readScopedJSON(ctx, q, "SELECT "+projection+" FROM "+from+" WHERE "+where, args...)
	}
	if page < 1 || page > 1000000 || size < 1 || size > 100 {
		return nil, scopedBadRequest("Invalid pagination")
	}
	limit, offset := scopedArg(&args, size), scopedArg(&args, (page-1)*size)
	query := `WITH authorized AS (SELECT ` + idcol + ` AS resource_id,` + projection + ` AS item FROM ` + from + ` WHERE ` + where + `), page AS (SELECT item FROM authorized ORDER BY resource_id LIMIT ` + limit + ` OFFSET ` + offset + `) SELECT jsonb_build_object('items',COALESCE((SELECT jsonb_agg(item) FROM page),'[]'::jsonb),'total',(SELECT count(*) FROM authorized),'page',` + scopedArg(&args, page) + `::int,'page_size',` + scopedArg(&args, size) + `::int)`
	return readScopedJSON(ctx, q, query, args...)
}
func (s *ScopedAdminService) Resource(ctx context.Context, principal *ScopedAdminPrincipal, method, path, kind string, id int64, page, size int, body []byte, idempotency string) (json.RawMessage, error) {
	if !s.Enabled() || principal == nil {
		return nil, ErrScopedAdminDenied
	}
	permission := adminaccess.RoutePermission(method, path)
	if permission == "" {
		return nil, ErrScopedAdminDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, ErrScopedAdminUnavailable
	}
	defer func() { _ = tx.Rollback() }()
	q := tx.Client()
	if _, err = q.ExecContext(ctx, `SET LOCAL lock_timeout='3s'`); err != nil {
		return nil, ErrScopedAdminUnavailable
	}
	// All writes for a key and its lifecycle changes serialize on this row.
	// A revocation that commits before this lock is always observed.
	lock := " FOR SHARE"
	if method != "GET" {
		lock = " FOR UPDATE"
	}
	key, err := readScopedKey(ctx, q, "id=$1"+lock, principal.Key.ID)
	if err != nil || key.Revision != principal.Key.Revision || !scopedKeyActive(key, principal.ClientIP, time.Now()) || !key.Policy.Has(permission) {
		return nil, ErrScopedAdminDenied
	}
	// Owner demotion, disable and deletion serialize with this operation too.
	owner, ownerErr := readScopedJSON(ctx, q, `SELECT '{}'::jsonb FROM users WHERE id=$1 AND deleted_at IS NULL AND role='admin' AND status='active' FOR SHARE`, key.CreatorID)
	if ownerErr != nil || owner == nil {
		return nil, ErrScopedAdminDenied
	}
	// The key row is already locked; at most one timestamp write per minute.
	if key.LastUsedAt == nil || time.Since(*key.LastUsedAt) >= time.Minute {
		// Shared read locks must not be upgraded (two readers could deadlock).
		if method != "GET" {
			if _, err = q.ExecContext(ctx, `UPDATE scoped_admin_api_keys SET last_used_at=now() WHERE id=$1`, key.ID); err != nil {
				return nil, ErrScopedAdminUnavailable
			}
		}
	}
	if method == "GET" {
		out, e := s.readResource(ctx, q, key.Policy, kind, id, page, size)
		if e != nil {
			return nil, e
		}
		if err = tx.Commit(); err != nil {
			return nil, ErrScopedAdminUnavailable
		}
		if key.LastUsedAt == nil || time.Since(*key.LastUsedAt) >= time.Minute {
			touchCtx, touchCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			_, _ = s.db.ExecContext(touchCtx, `UPDATE scoped_admin_api_keys SET last_used_at=now() WHERE id=$1 AND (last_used_at IS NULL OR last_used_at<now()-interval '1 minute')`, key.ID)
			touchCancel()
		}
		return out, nil
	}
	// Incremental control-plane maintenance, bounded per successful mutation.
	if _, err = q.ExecContext(ctx, `DELETE FROM scoped_admin_operations WHERE ctid IN (SELECT ctid FROM scoped_admin_operations WHERE key_id=$1 AND created_at<now()-interval '30 days' ORDER BY created_at LIMIT 100)`, key.ID); err != nil {
		return nil, ErrScopedAdminUnavailable
	}
	if len(idempotency) < 8 || len(idempotency) > 128 || strings.TrimSpace(idempotency) != idempotency {
		return nil, scopedBadRequest("An Idempotency-Key of 8–128 characters is required")
	}
	for _, c := range idempotency {
		if c < 33 || c > 126 {
			return nil, scopedBadRequest("Invalid Idempotency-Key")
		}
	}
	canonical := new(any)
	if err = decodeScopedJSON(body, canonical); err != nil {
		return nil, err
	}
	canonicalBody, _ := json.Marshal(canonical)
	namespace := method + " " + path
	fingerprint := scopedHash(fmt.Sprintf("%s\x00%d\x00%s", namespace, id, canonicalBody))
	hash := scopedHash(idempotency)
	rows, err := q.QueryContext(ctx, `SELECT fingerprint,resource_id FROM scoped_admin_operations WHERE key_id=$1 AND namespace=$2 AND idempotency_hash=$3`, key.ID, namespace, hash)
	if err != nil {
		return nil, ErrScopedAdminUnavailable
	}
	var oldFingerprint string
	var resourceID int64
	replay := rows.Next()
	if replay {
		err = rows.Scan(&oldFingerprint, &resourceID)
	}
	rowErr := rows.Err()
	_ = rows.Close()
	if err != nil || rowErr != nil {
		return nil, ErrScopedAdminUnavailable
	}
	if replay && oldFingerprint != fingerprint {
		return nil, apperrors.New(409, "IDEMPOTENCY_CONFLICT", "Idempotency key was used with another request")
	}
	txCtx := dbent.NewTxContext(ctx, tx)
	if replay {
		if method == "DELETE" { // No historical DTO is stored or replayed.
			// Deletes are acknowledged only while the current policy still grants
			// their original scope, captured as a minimal tuple in the receipt.
			raw, e := readScopedJSON(ctx, q, `SELECT result FROM scoped_admin_operations WHERE key_id=$1 AND namespace=$2 AND idempotency_hash=$3`, key.ID, namespace, hash)
			if e != nil {
				return nil, ErrScopedAdminUnavailable
			}
			var tuple struct {
				Platform string  `json:"platform"`
				Type     string  `json:"type"`
				Groups   []int64 `json:"groups"`
			}
			if json.Unmarshal(raw, &tuple) != nil || !key.Policy.AccountMatches(permission, resourceID, tuple.Platform, tuple.Type, tuple.Groups) {
				return nil, ErrScopedAdminNotFound
			}
			if err = tx.Commit(); err != nil {
				return nil, ErrScopedAdminUnavailable
			}
			return json.RawMessage(`{"deleted":true}`), nil
		}
		if err = s.authorizeAccountOrGroup(ctx, q, key.Policy, permission, kind, resourceID); err != nil {
			return nil, err
		}
	} else {
		var receipt any
		resourceID, receipt, err = s.writeResource(txCtx, tx, key.Policy, permission, kind, id, method, body)
		if err != nil {
			return nil, err
		}
		result, _ := json.Marshal(receipt)
		if _, err = q.ExecContext(ctx, `INSERT INTO scoped_admin_operations(key_id,namespace,idempotency_hash,fingerprint,resource_id,result) VALUES($1,$2,$3,$4,$5,$6)`, key.ID, namespace, hash, fingerprint, resourceID, string(result)); err != nil {
			return nil, ErrScopedAdminUnavailable
		}
	}
	var out json.RawMessage
	if method == "DELETE" {
		out = json.RawMessage(`{"deleted":true}`)
	} else {
		out, err = s.readResource(ctx, q, key.Policy, kind, resourceID, 1, 1)
		if errors.Is(err, ErrScopedAdminNotFound) {
			out = json.RawMessage(`{"saved":true}`)
			err = nil
		}
		if err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, ErrScopedAdminUnavailable
	}
	return out, nil
}
func (s *ScopedAdminService) authorizeAccountOrGroup(ctx context.Context, q scopedSQL, p adminaccess.Policy, permission, kind string, id int64) error {
	if kind != "accounts" && kind != "groups" {
		return ErrScopedAdminDenied
	}
	if _, err := readScopedJSON(ctx, q, "SELECT '{}'::jsonb FROM "+kind+" WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", id); err != nil {
		return err
	}
	args := []any{}
	from, where := kind, "deleted_at IS NULL AND "
	if kind == "accounts" {
		from = "accounts a"
		where = "a.deleted_at IS NULL AND " + scopedAccountWhere(p, permission, &args) + " AND a.id=" + scopedArg(&args, id)
	} else {
		where += scopedResourceWhere(p, kind, permission, &args) + " AND id=" + scopedArg(&args, id)
	}
	_, err := readScopedJSON(ctx, q, "SELECT '{}'::jsonb FROM "+from+" WHERE "+where, args...)
	return err
}

type scopedAccountPatch struct {
	Name           *string                    `json:"name"`
	Notes          *string                    `json:"notes"`
	Platform       string                     `json:"platform"`
	Type           string                     `json:"type"`
	Credentials    map[string]json.RawMessage `json:"credentials"`
	Status         *string                    `json:"status"`
	Concurrency    *int                       `json:"concurrency"`
	Priority       *int                       `json:"priority"`
	AddGroupIDs    []int64                    `json:"add_group_ids"`
	RemoveGroupIDs []int64                    `json:"remove_group_ids"`
	ProxyMode      string                     `json:"proxy_mode"`
	ProxyID        *int64                     `json:"proxy_id"`
}

func (s *ScopedAdminService) writeResource(ctx context.Context, tx *dbent.Tx, p adminaccess.Policy, permission, kind string, id int64, method string, body []byte) (int64, any, error) {
	q := tx.Client()
	if kind == "groups" {
		var patch struct {
			Name        *string                `json:"name"`
			Description *string                `json:"description"`
			Models      *GroupModelsListConfig `json:"models_list_config"`
		}
		if err := decodeScopedJSON(body, &patch); err != nil {
			return 0, nil, err
		}
		if err := s.authorizeAccountOrGroup(ctx, q, p, permission, kind, id); err != nil {
			return 0, nil, err
		}
		sets := []string{"updated_at=now()"}
		args := []any{}
		if patch.Name != nil {
			if strings.TrimSpace(*patch.Name) == "" || len(*patch.Name) > 100 {
				return 0, nil, scopedBadRequest("Invalid name")
			}
			sets = append(sets, "name="+scopedArg(&args, *patch.Name))
		}
		if patch.Description != nil {
			if len(*patch.Description) > 1000 {
				return 0, nil, scopedBadRequest("Description is too long")
			}
			sets = append(sets, "description="+scopedArg(&args, *patch.Description))
		}
		if patch.Models != nil {
			if len(patch.Models.Models) > 500 {
				return 0, nil, scopedBadRequest("Too many models")
			}
			for _, m := range patch.Models.Models {
				if len(m) > 256 {
					return 0, nil, scopedBadRequest("Invalid model")
				}
			}
			data, _ := json.Marshal(normalizeGroupModelsListConfig(*patch.Models))
			sets = append(sets, "models_list_config="+scopedArg(&args, string(data))+"::jsonb")
		}
		if len(sets) == 1 {
			return 0, nil, scopedBadRequest("No fields supplied")
		}
		_, err := q.ExecContext(ctx, "UPDATE groups SET "+strings.Join(sets, ",")+" WHERE id="+scopedArg(&args, id), args...)
		if err != nil {
			return 0, nil, err
		}
		_, err = q.ExecContext(ctx, `INSERT INTO scheduler_outbox(event_type,group_id) VALUES($1,$2)`, SchedulerOutboxEventGroupChanged, id)
		if err == nil {
			tx.OnCommit(func(next dbent.Committer) dbent.Committer {
				return dbent.CommitFunc(func(commitCtx context.Context, tx *dbent.Tx) error {
					if err := next.Commit(commitCtx, tx); err != nil {
						return err
					}
					if base, ok := s.admin.(*adminServiceImpl); ok && base.authCacheInvalidator != nil {
						publishCtx, cancel := context.WithTimeout(context.WithoutCancel(commitCtx), 5*time.Second)
						defer cancel()
						base.authCacheInvalidator.InvalidateAuthCacheByGroupID(publishCtx, id)
					}
					return nil
				})
			})
		}
		return id, map[string]any{}, err
	}
	if kind != "accounts" {
		return 0, nil, ErrScopedAdminDenied
	}
	if method != "POST" {
		if err := s.authorizeAccountOrGroup(ctx, q, p, permission, kind, id); err != nil {
			return 0, nil, err
		}
	}
	// Parent/shadow transitions and their propagation remain exclusively with
	// the existing interactive administrator paths.
	if id > 0 {
		rows, err := q.QueryContext(ctx, `SELECT parent_account_id IS NOT NULL OR EXISTS(SELECT 1 FROM accounts child WHERE child.parent_account_id=accounts.id AND child.deleted_at IS NULL) FROM accounts WHERE id=$1 AND deleted_at IS NULL`, id)
		if err != nil {
			return 0, nil, err
		}
		var shadow bool
		if rows.Next() {
			err = rows.Scan(&shadow)
		} else {
			err = ErrScopedAdminNotFound
		}
		_ = rows.Close()
		if err != nil {
			return 0, nil, err
		}
		if shadow {
			return 0, nil, ErrScopedAdminDenied
		}
	}
	var patch scopedAccountPatch
	if err := decodeScopedJSON(body, &patch); err != nil {
		return 0, nil, err
	}
	currentPlatform, currentType := "", ""
	var groupIDs []int64
	if id > 0 {
		rows, err := q.QueryContext(ctx, `SELECT platform,type,ARRAY(SELECT group_id FROM account_groups WHERE account_id=$1 ORDER BY group_id) FROM accounts WHERE id=$1`, id)
		if err != nil {
			return 0, nil, err
		}
		if !rows.Next() {
			_ = rows.Close()
			return 0, nil, ErrScopedAdminNotFound
		}
		err = rows.Scan(&currentPlatform, &currentType, pq.Array(&groupIDs))
		_ = rows.Close()
		if err != nil {
			return 0, nil, err
		}
	}
	if method == "DELETE" {
		if string(bytes.TrimSpace(body)) != "{}" {
			return 0, nil, scopedBadRequest("Delete body must be empty")
		}
		if _, err := q.ExecContext(ctx, `UPDATE accounts SET deleted_at=now(),updated_at=now() WHERE id=$1`, id); err != nil {
			return 0, nil, err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM account_groups WHERE account_id=$1`, id); err != nil {
			return 0, nil, err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE account_id=$1`, id); err != nil {
			return 0, nil, err
		}
		if err := s.scopedAccountChanged(ctx, tx, id, groupIDs, true, true); err != nil {
			return 0, nil, err
		}
		return id, map[string]any{"platform": currentPlatform, "type": currentType, "groups": groupIDs}, nil
	}
	if method == "POST" {
		currentPlatform, currentType = patch.Platform, patch.Type
		if !adminaccess.PlatformSupported(currentPlatform) || currentPlatform == PlatformComposite {
			return 0, nil, scopedBadRequest("Unsupported platform")
		}
		if currentType != AccountTypeAPIKey {
			return 0, nil, scopedBadRequest("Scoped creation supports API-key accounts; OAuth onboarding requires an administrator session")
		}
		if !p.AccountMatches(permission, 0, currentPlatform, currentType, patch.AddGroupIDs) {
			return 0, nil, ErrScopedAdminDenied
		}
	} else if patch.Platform != "" || patch.Type != "" {
		return 0, nil, scopedBadRequest("Platform and type cannot be changed")
	}
	allowed := func(perm string) bool {
		if method == "POST" {
			// A new account is authorized against its destination groups, not an
			// intermediate ungrouped state that is never committed.
			return p.AccountMatches(perm, 0, currentPlatform, currentType, patch.AddGroupIDs)
		}
		return p.AccountMatches(perm, id, currentPlatform, currentType, groupIDs)
	}
	sets := []string{"updated_at=now()"}
	args := []any{}
	if patch.Name != nil {
		if !allowed("accounts.notes.write") {
			return 0, nil, ErrScopedAdminDenied
		}
		if strings.TrimSpace(*patch.Name) == "" || len(*patch.Name) > 100 {
			return 0, nil, scopedBadRequest("Invalid account name")
		}
		sets = append(sets, "name="+scopedArg(&args, *patch.Name))
	}
	if patch.Notes != nil {
		if !allowed("accounts.notes.write") {
			return 0, nil, ErrScopedAdminDenied
		}
		if len(*patch.Notes) > 2000 {
			return 0, nil, scopedBadRequest("Notes are too long")
		}
		sets = append(sets, "notes="+scopedArg(&args, *patch.Notes))
	}
	if patch.Status != nil {
		if !allowed("accounts.status.write") {
			return 0, nil, ErrScopedAdminDenied
		}
		// Accounts use "inactive" in the existing admin API and edit form;
		// "disabled" is a user/key status and would not round-trip there.
		if *patch.Status != StatusActive && *patch.Status != "inactive" {
			return 0, nil, scopedBadRequest("Invalid status")
		}
		sets = append(sets, "status="+scopedArg(&args, *patch.Status))
	}
	if patch.Concurrency != nil {
		if !allowed("accounts.schedule.write") {
			return 0, nil, ErrScopedAdminDenied
		}
		if *patch.Concurrency < 1 || *patch.Concurrency > 10000 {
			return 0, nil, scopedBadRequest("Invalid concurrency")
		}
		sets = append(sets, "concurrency="+scopedArg(&args, *patch.Concurrency))
	}
	if patch.Priority != nil {
		if !allowed("accounts.schedule.write") {
			return 0, nil, ErrScopedAdminDenied
		}
		if *patch.Priority < 0 || *patch.Priority > 10000 {
			return 0, nil, scopedBadRequest("Invalid priority")
		}
		sets = append(sets, "priority="+scopedArg(&args, *patch.Priority))
	}
	credentials := map[string]any{}
	for field, raw := range patch.Credentials {
		switch field {
		case "model_mapping":
			if !allowed("accounts.models.write") {
				return 0, nil, ErrScopedAdminDenied
			}
			var mapping map[string]string
			if json.Unmarshal(raw, &mapping) != nil || mapping == nil || len(mapping) > 500 {
				return 0, nil, scopedBadRequest("Invalid model mapping")
			}
			for a, b := range mapping {
				if len(a) == 0 || len(a) > 256 || len(b) == 0 || len(b) > 256 {
					return 0, nil, scopedBadRequest("Invalid model mapping")
				}
			}
			credentials[field] = mapping
		case "api_key", "access_token", "refresh_token", "base_url":
			if !allowed("accounts.credentials.write") {
				return 0, nil, ErrScopedAdminDenied
			}
			var value string
			if json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" || len(value) > 16384 {
				return 0, nil, scopedBadRequest("Invalid credential")
			}
			if field == "base_url" {
				if !allowed("accounts.network.write") {
					return 0, nil, ErrScopedAdminDenied
				}
				u, e := url.Parse(value)
				if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
					return 0, nil, scopedBadRequest("Invalid base URL")
				}
			}
			if field == "api_key" && currentType != AccountTypeAPIKey || field != "api_key" && field != "base_url" && currentType != AccountTypeOAuth && currentType != AccountTypeSetupToken {
				return 0, nil, scopedBadRequest("Credential is not valid for this account type")
			}
			credentials[field] = value
		default:
			return 0, nil, scopedBadRequest("Unsupported credential field")
		}
	}
	if len(credentials) > 0 {
		raw, _ := json.Marshal(credentials)
		sets = append(sets, "credentials=COALESCE(credentials,'{}'::jsonb)||"+scopedArg(&args, string(raw))+"::jsonb")
	}
	// Group edits are incremental: never delete/recreate unaffected bindings.
	groupChanges := append(append([]int64{}, patch.AddGroupIDs...), patch.RemoveGroupIDs...)
	if len(groupChanges) > 0 && !allowed("accounts.groups.write") {
		return 0, nil, ErrScopedAdminDenied
	}
	if len(groupChanges) > 200 {
		return 0, nil, scopedBadRequest("Too many group changes")
	}
	sort.Slice(groupChanges, func(i, j int) bool { return groupChanges[i] < groupChanges[j] })
	for _, g := range groupChanges {
		if g <= 0 {
			return 0, nil, scopedBadRequest("Group IDs must be positive")
		}
		if adminaccess.Contains(patch.AddGroupIDs, g) && adminaccess.Contains(patch.RemoveGroupIDs, g) {
			return 0, nil, scopedBadRequest("Conflicting group changes")
		}
	}
	if len(groupChanges) > 0 {
		rows, err := q.QueryContext(ctx, `SELECT id,platform FROM groups WHERE id=ANY($1::bigint[]) AND deleted_at IS NULL ORDER BY id FOR UPDATE`, pq.Array(groupChanges))
		if err != nil {
			return 0, nil, err
		}
		seen := map[int64]bool{}
		for rows.Next() {
			var g int64
			var platform string
			if err = rows.Scan(&g, &platform); err != nil {
				break
			}
			if !p.GroupMatches("groups.update", g, platform) {
				err = ErrScopedAdminNotFound
				break
			}
			if platform != currentPlatform && platform != PlatformComposite {
				err = ErrScopedAdminDenied
				break
			}
			seen[g] = true
		}
		rowsErr := rows.Err()
		_ = rows.Close()
		if err != nil {
			return 0, nil, err
		}
		if rowsErr != nil {
			return 0, nil, rowsErr
		}
		for _, g := range groupChanges {
			if !seen[g] {
				return 0, nil, ErrScopedAdminNotFound
			}
		}
	}
	if len(patch.AddGroupIDs) > 0 && method != "POST" {
		adapter, ok := s.accounts.(ScopedAdminAccountRepository)
		base, valid := s.admin.(*adminServiceImpl)
		if !ok || !valid {
			return 0, nil, ErrScopedAdminUnavailable
		}
		admin := *base
		admin.accountRepo, admin.groupRepo = adapter.ScopedAdminCreateRepositories(tx)
		if err := admin.checkMixedChannelRisk(ctx, id, currentPlatform, patch.AddGroupIDs); err != nil {
			return 0, nil, err
		}
	}
	proxy, proxyChanged, err := scopedChooseProxy(ctx, q, p, allowed("accounts.network.write"), patch.ProxyMode, patch.ProxyID)
	if err != nil {
		return 0, nil, err
	}
	if proxyChanged {
		sets = append(sets, "proxy_id="+scopedArg(&args, proxy))
	}
	if method == "POST" {
		if patch.Name == nil || len(patch.RemoveGroupIDs) > 0 || patch.Status != nil {
			return 0, nil, scopedBadRequest("Create requires name and cannot remove groups or set status")
		}
		if credentials["api_key"] == nil {
			return 0, nil, scopedBadRequest("API key is required")
		}
		adapter, ok := s.accounts.(ScopedAdminAccountRepository)
		if !ok {
			return 0, nil, ErrScopedAdminUnavailable
		}
		base, ok := s.admin.(*adminServiceImpl)
		if !ok {
			return 0, nil, ErrScopedAdminUnavailable
		}
		admin := *base
		admin.accountRepo, admin.groupRepo = adapter.ScopedAdminCreateRepositories(tx)
		concurrency, priority := 3, 50
		if patch.Concurrency != nil {
			concurrency = *patch.Concurrency
		}
		if patch.Priority != nil {
			priority = *patch.Priority
		}
		account, err := admin.CreateAccount(ctx, &CreateAccountInput{Name: *patch.Name, Notes: patch.Notes, Platform: currentPlatform, Type: currentType, Credentials: credentials, ProxyID: proxy, Concurrency: concurrency, Priority: priority, GroupIDs: patch.AddGroupIDs, SkipDefaultGroupBind: true})
		if err != nil {
			return 0, nil, err
		}
		id = account.ID
	} else {
		if len(sets) == 1 && len(groupChanges) == 0 {
			return 0, nil, scopedBadRequest("No fields supplied")
		}
		if _, err := q.ExecContext(ctx, "UPDATE accounts SET "+strings.Join(sets, ",")+" WHERE id="+scopedArg(&args, id), args...); err != nil {
			return 0, nil, err
		}
		if len(patch.RemoveGroupIDs) > 0 {
			if _, err := q.ExecContext(ctx, `DELETE FROM account_groups WHERE account_id=$1 AND group_id=ANY($2::bigint[])`, id, pq.Array(patch.RemoveGroupIDs)); err != nil {
				return 0, nil, err
			}
		}
		if len(patch.AddGroupIDs) > 0 {
			if _, err := q.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id,priority,created_at) SELECT $1,g,50,now() FROM unnest($2::bigint[]) AS g ON CONFLICT(account_id,group_id) DO NOTHING`, id, pq.Array(patch.AddGroupIDs)); err != nil {
				return 0, nil, err
			}
		}
	}
	affectedGroups := append(append([]int64{}, groupIDs...), patch.AddGroupIDs...)
	if err := s.scopedAccountChanged(ctx, tx, id, affectedGroups, false, method != "POST"); err != nil {
		return 0, nil, err
	}
	return id, map[string]any{}, nil
}
func (s *ScopedAdminService) scopedAccountChanged(ctx context.Context, tx *dbent.Tx, id int64, groupIDs []int64, deleted, enqueue bool) error {
	if enqueue {
		// Consumers cannot discover removed bindings from the current row. Carry
		// both previous and added groups in the same committed outbox event.
		sort.Slice(groupIDs, func(i, j int) bool { return groupIDs[i] < groupIDs[j] })
		unique := make([]int64, 0, len(groupIDs))
		for _, groupID := range groupIDs {
			if groupID > 0 && (len(unique) == 0 || unique[len(unique)-1] != groupID) {
				unique = append(unique, groupID)
			}
		}
		payload, err := json.Marshal(map[string]any{"group_ids": unique})
		if err != nil {
			return err
		}
		if _, err := tx.Client().ExecContext(ctx, `INSERT INTO scheduler_outbox(event_type,account_id,payload) VALUES($1,$2,$3::jsonb)`, SchedulerOutboxEventAccountChanged, id, string(payload)); err != nil {
			return err
		}
	}
	adapter, ok := s.accounts.(ScopedAdminAccountRepository)
	if !ok {
		return ErrScopedAdminUnavailable
	}
	tx.OnCommit(func(next dbent.Committer) dbent.Committer {
		return dbent.CommitFunc(func(commitCtx context.Context, tx *dbent.Tx) error {
			if err := next.Commit(commitCtx, tx); err != nil {
				return err
			}
			publishCtx, cancel := context.WithTimeout(context.WithoutCancel(commitCtx), 5*time.Second)
			defer cancel()
			adapter.ScopedAdminPublish(publishCtx, id, deleted)
			return nil
		})
	})
	return nil
}
func scopedChooseProxy(ctx context.Context, q scopedSQL, p adminaccess.Policy, network bool, mode string, specified *int64) (*int64, bool, error) {
	if mode == "" {
		if specified != nil {
			return nil, false, scopedBadRequest("proxy_id requires proxy_mode")
		}
		return nil, false, nil
	}
	if !network {
		return nil, false, ErrScopedAdminDenied
	}
	if mode == "direct" {
		if specified != nil {
			return nil, false, scopedBadRequest("Conflicting proxy fields")
		}
		return nil, true, nil
	}
	if mode != "specified" && mode != "random" && mode != "least_used" {
		return nil, false, scopedBadRequest("Unknown proxy mode")
	}
	if mode == "specified" {
		if specified == nil || *specified <= 0 || !p.ProxyMatches("proxies.read", *specified) {
			return nil, false, ErrScopedAdminDenied
		}
	} else if specified != nil {
		return nil, false, scopedBadRequest("Conflicting proxy fields")
	}
	args := []any{}
	where := scopedResourceWhere(p, "proxies", "proxies.read", &args)
	if mode == "specified" {
		where += " AND id=" + scopedArg(&args, *specified)
	}
	rows, err := q.QueryContext(ctx, `SELECT id FROM proxies WHERE deleted_at IS NULL AND status='active' AND (expires_at IS NULL OR expires_at>now()) AND `+where+` ORDER BY id LIMIT 1001 FOR UPDATE`, args...)
	if err != nil {
		return nil, false, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	rowErr := rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, false, err
	}
	if rowErr != nil {
		return nil, false, rowErr
	}
	if len(ids) == 0 || len(ids) > 1000 {
		return nil, false, scopedBadRequest("Proxy pool is empty or exceeds 1000 candidates")
	}
	chosen := ids[0]
	if mode == "random" || mode == "least_used" {
		query := `SELECT id FROM unnest($1::bigint[]) AS id ORDER BY random() LIMIT 1`
		if mode == "least_used" {
			query = `SELECT p.id FROM unnest($1::bigint[]) AS p(id) LEFT JOIN accounts a ON a.proxy_id=p.id AND a.deleted_at IS NULL GROUP BY p.id ORDER BY count(a.id),p.id LIMIT 1`
		}
		rows, err = q.QueryContext(ctx, query, pq.Array(ids))
		if err != nil {
			return nil, false, err
		}
		defer func() { _ = rows.Close() }()
		if !rows.Next() {
			if err = rows.Err(); err != nil {
				return nil, false, err
			}
			return nil, false, ErrScopedAdminUnavailable
		}
		if err = rows.Scan(&chosen); err != nil {
			return nil, false, err
		}
		if err = rows.Err(); err != nil {
			return nil, false, err
		}
	}
	return &chosen, true, nil
}
