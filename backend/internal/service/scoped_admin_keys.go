package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	apperrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const ScopedAdminKeyPrefix = "sk-admin-v1-"

var ErrScopedAdminDenied = apperrors.New(403, "SCOPED_ADMIN_DENIED", "Operation is outside this key's permissions")
var ErrScopedAdminNotFound = apperrors.New(404, "NOT_FOUND", "Resource not found")
var ErrScopedAdminUnavailable = apperrors.New(503, "SCOPED_ADMIN_UNAVAILABLE", "Scoped administration is unavailable")

type ScopedAdminKey struct {
	ID          int64              `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Prefix      string             `json:"prefix"`
	Enabled     bool               `json:"enabled"`
	ExpiresAt   *time.Time         `json:"expires_at"`
	IPAllowlist []string           `json:"ip_allowlist"`
	Policy      adminaccess.Policy `json:"policy"`
	Revision    int64              `json:"revision"`
	RPM         int                `json:"rpm"`
	CreatorID   int64              `json:"creator_id"`
	CreatedAt   time.Time          `json:"created_at"`
	LastUsedAt  *time.Time         `json:"last_used_at"`
	RevokedAt   *time.Time         `json:"revoked_at"`
}
type ScopedAdminPrincipal struct {
	Key      ScopedAdminKey
	ClientIP string
}
type ScopedAdminKeyInput struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Enabled     bool               `json:"enabled"`
	ExpiresAt   *time.Time         `json:"expires_at"`
	IPAllowlist []string           `json:"ip_allowlist"`
	Policy      adminaccess.Policy `json:"policy"`
	RPM         int                `json:"rpm"`
	Revision    int64              `json:"revision"`
}
type scopedSQL interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}
type ScopedAdminService struct {
	cfg      *config.Config
	db       *sql.DB
	client   *dbent.Client
	admin    AdminService
	accounts AccountRepository
}

// Construction deliberately performs no I/O: the disabled gate works without
// the new tables and introduces no work in the inference path.
func NewScopedAdminService(cfg *config.Config, db *sql.DB, client *dbent.Client, admin AdminService, accounts AccountRepository) *ScopedAdminService {
	return &ScopedAdminService{cfg: cfg, db: db, client: client, admin: admin, accounts: accounts}
}
func (s *ScopedAdminService) Enabled() bool {
	return s != nil && s.cfg != nil && s.cfg.Security.ScopedAdminAPIKeysEnabled
}
func scopedHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func newScopedSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return ScopedAdminKeyPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

const scopedKeyColumns = `id,name,description,key_prefix,enabled,expires_at,ip_allowlist,policy,policy_revision,rpm,creator_id,created_at,revoked_at,last_used_at`

func scanScopedKey(rows *sql.Rows) (ScopedAdminKey, error) {
	var k ScopedAdminKey
	var ips, policy []byte
	err := rows.Scan(&k.ID, &k.Name, &k.Description, &k.Prefix, &k.Enabled, &k.ExpiresAt, &ips, &policy, &k.Revision, &k.RPM, &k.CreatorID, &k.CreatedAt, &k.RevokedAt, &k.LastUsedAt)
	if err != nil {
		return k, err
	}
	if err = json.Unmarshal(ips, &k.IPAllowlist); err != nil {
		return k, err
	}
	err = json.Unmarshal(policy, &k.Policy)
	return k, err
}
func readScopedKey(ctx context.Context, q scopedSQL, where string, args ...any) (ScopedAdminKey, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+scopedKeyColumns+` FROM scoped_admin_api_keys WHERE deleted_at IS NULL AND `+where, args...)
	if err != nil {
		return ScopedAdminKey{}, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return ScopedAdminKey{}, err
		}
		return ScopedAdminKey{}, sql.ErrNoRows
	}
	k, err := scanScopedKey(rows)
	return k, err
}
func scopedKeyActive(k ScopedAdminKey, ip string, now time.Time) bool {
	return k.Enabled && k.RevokedAt == nil && (k.ExpiresAt == nil || k.ExpiresAt.After(now)) && k.Policy.Validate() == nil && adminaccess.IPAllowed(k.IPAllowlist, ip)
}
func (s *ScopedAdminService) Authenticate(ctx context.Context, secret, ip string) (*ScopedAdminPrincipal, error) {
	if !s.Enabled() || !strings.HasPrefix(secret, ScopedAdminKeyPrefix) || len(secret) != len(ScopedAdminKeyPrefix)+43 {
		return nil, ErrScopedAdminDenied
	}
	k, err := readScopedKey(ctx, s.db, "key_hash=$1", scopedHash(secret))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrScopedAdminDenied
	}
	if err != nil {
		return nil, ErrScopedAdminUnavailable
	}
	if !scopedKeyActive(k, ip, time.Now()) {
		return nil, ErrScopedAdminDenied
	}
	return &ScopedAdminPrincipal{Key: k, ClientIP: ip}, nil
}
func validateScopedInput(in *ScopedAdminKeyInput) error {
	in.Name = strings.TrimSpace(in.Name)
	if len(in.Name) == 0 || len(in.Name) > 100 || len(in.Description) > 1000 || in.RPM < 1 || in.RPM > 600 {
		return apperrors.BadRequest("INVALID_SCOPED_KEY", "Name, description or rate limit is invalid")
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(time.Now()) {
		return apperrors.BadRequest("INVALID_SCOPED_KEY", "Expiration must be in the future")
	}
	if err := in.Policy.Validate(); err != nil {
		return apperrors.BadRequest("INVALID_SCOPED_POLICY", err.Error())
	}
	if err := adminaccess.ValidateIPPolicy(in.IPAllowlist); err != nil {
		return apperrors.BadRequest("INVALID_SCOPED_IP", err.Error())
	}
	if in.IPAllowlist == nil {
		in.IPAllowlist = []string{}
	}
	return nil
}

type ScopedAdminKeyPage struct {
	Items        []ScopedAdminKey `json:"items"`
	NextBeforeID int64            `json:"next_before_id"`
}

func (s *ScopedAdminService) ListKeys(ctx context.Context, beforeID int64) (*ScopedAdminKeyPage, error) {
	if !s.Enabled() {
		return nil, ErrScopedAdminNotFound
	}
	if beforeID < 0 {
		return nil, apperrors.BadRequest("INVALID_SCOPED_CURSOR", "Invalid key list cursor")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+scopedKeyColumns+` FROM scoped_admin_api_keys WHERE deleted_at IS NULL AND ($1::bigint=0 OR id<$1) ORDER BY id DESC LIMIT 201`, beforeID)
	if err != nil {
		return nil, ErrScopedAdminUnavailable
	}
	defer func() { _ = rows.Close() }()
	out := []ScopedAdminKey{}
	for rows.Next() {
		k, e := scanScopedKey(rows)
		if e != nil {
			return nil, ErrScopedAdminUnavailable
		}
		out = append(out, k)
	}
	if rows.Err() != nil {
		return nil, ErrScopedAdminUnavailable
	}
	page := &ScopedAdminKeyPage{Items: out}
	if len(out) > 200 {
		page.Items = out[:200]
		page.NextBeforeID = out[199].ID
	}
	return page, nil
}
func (s *ScopedAdminService) CreateKey(ctx context.Context, creator int64, in ScopedAdminKeyInput) (ScopedAdminKey, string, error) {
	if !s.Enabled() {
		return ScopedAdminKey{}, "", ErrScopedAdminNotFound
	}
	if err := validateScopedInput(&in); err != nil {
		return ScopedAdminKey{}, "", err
	}
	secret, err := newScopedSecret()
	if err != nil {
		return ScopedAdminKey{}, "", ErrScopedAdminUnavailable
	}
	policy, _ := json.Marshal(in.Policy)
	ips, _ := json.Marshal(in.IPAllowlist)
	rows, err := s.db.QueryContext(ctx, `INSERT INTO scoped_admin_api_keys(name,description,key_prefix,key_hash,enabled,expires_at,ip_allowlist,policy,rpm,creator_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING `+scopedKeyColumns, in.Name, in.Description, secret[:len(ScopedAdminKeyPrefix)+8], scopedHash(secret), in.Enabled, in.ExpiresAt, string(ips), string(policy), in.RPM, creator)
	if err != nil {
		return ScopedAdminKey{}, "", ErrScopedAdminUnavailable
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return ScopedAdminKey{}, "", ErrScopedAdminUnavailable
	}
	k, err := scanScopedKey(rows)
	if err != nil {
		return k, "", ErrScopedAdminUnavailable
	}
	return k, secret, nil
}
func (s *ScopedAdminService) UpdateKey(ctx context.Context, id int64, in ScopedAdminKeyInput) error {
	if !s.Enabled() {
		return ErrScopedAdminNotFound
	}
	if err := validateScopedInput(&in); err != nil {
		return err
	}
	if in.Revision < 1 {
		return apperrors.BadRequest("REVISION_REQUIRED", "Current revision is required")
	}
	policy, _ := json.Marshal(in.Policy)
	ips, _ := json.Marshal(in.IPAllowlist)
	result, err := s.db.ExecContext(ctx, `UPDATE scoped_admin_api_keys SET name=$1,description=$2,enabled=$3,expires_at=$4,ip_allowlist=$5,policy=$6,rpm=$7,policy_revision=policy_revision+1,updated_at=now() WHERE id=$8 AND policy_revision=$9 AND revoked_at IS NULL AND deleted_at IS NULL`, in.Name, in.Description, in.Enabled, in.ExpiresAt, string(ips), string(policy), in.RPM, id, in.Revision)
	return scopedMutationResult(result, err)
}
func scopedMutationResult(result sql.Result, err error) error {
	if err != nil {
		return ErrScopedAdminUnavailable
	}
	n, err := result.RowsAffected()
	if err != nil {
		return ErrScopedAdminUnavailable
	}
	if n != 1 {
		return apperrors.New(409, "SCOPED_KEY_CHANGED", "Key was changed or is no longer available; reload the list")
	}
	return nil
}
func (s *ScopedAdminService) ChangeKey(ctx context.Context, id, revision int64, action string) (string, error) {
	if !s.Enabled() {
		return "", ErrScopedAdminNotFound
	}
	if revision < 1 {
		return "", apperrors.BadRequest("REVISION_REQUIRED", "Current revision is required")
	}
	var secret string
	var err error
	var result sql.Result
	switch action {
	case "rotate":
		secret, err = newScopedSecret()
		if err != nil {
			return "", ErrScopedAdminUnavailable
		}
		result, err = s.db.ExecContext(ctx, `UPDATE scoped_admin_api_keys SET key_hash=$1,key_prefix=$2,policy_revision=policy_revision+1,updated_at=now() WHERE id=$3 AND policy_revision=$4 AND revoked_at IS NULL AND deleted_at IS NULL`, scopedHash(secret), secret[:len(ScopedAdminKeyPrefix)+8], id, revision)
	case "revoke":
		result, err = s.db.ExecContext(ctx, `UPDATE scoped_admin_api_keys SET enabled=false,revoked_at=now(),policy_revision=policy_revision+1,updated_at=now() WHERE id=$1 AND policy_revision=$2 AND revoked_at IS NULL AND deleted_at IS NULL`, id, revision)
	case "delete":
		result, err = s.db.ExecContext(ctx, `UPDATE scoped_admin_api_keys SET enabled=false,revoked_at=COALESCE(revoked_at,now()),deleted_at=now(),policy_revision=policy_revision+1,updated_at=now() WHERE id=$1 AND policy_revision=$2 AND deleted_at IS NULL`, id, revision)
	default:
		return "", ErrScopedAdminDenied
	}
	if err = scopedMutationResult(result, err); err != nil {
		return "", err
	}
	return secret, nil
}
