package repository

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// AccountCellDirectory is the durable ownership directory for admission
// Cells. Assignment is append-only: once an account has an owner Cell, adding
// more Cells never moves its live slots. Online moves remain disabled until a
// claim-side quiesce fence can make drain-and-commit atomic.
type AccountCellDirectory struct {
	rdb *redis.Client
}

const (
	accountCellAssignmentPrefix = "admission:account-cell:assignment:"
	accountCellCatalogKey       = "admission:account-cell:catalog"
	accountCellPlatformCatalog  = "admission:account-cell:catalog:"
	accountCellEndpointKey      = "admission:account-cell:endpoints"
)

var (
	ErrAccountCellBusy              = errors.New("account has active admission leases")
	ErrAccountCellMigrationDisabled = errors.New("online account Cell migration is disabled until owner-Cell quiesce fencing is implemented")
)

func NewAccountCellDirectory(rdb *redis.Client) *AccountCellDirectory {
	return &AccountCellDirectory{rdb: rdb}
}

// RegisterForNewAccounts publishes a provisioned Cell in the append-only
// catalog. It never rewrites an existing account assignment.
func (d *AccountCellDirectory) RegisterForNewAccounts(ctx context.Context, cellID string) error {
	if d == nil || d.rdb == nil || strings.TrimSpace(cellID) == "" {
		return errors.New("account cell directory is not configured")
	}
	return d.rdb.SAdd(ctx, accountCellCatalogKey, strings.TrimSpace(cellID)).Err()
}

func (d *AccountCellDirectory) RegisterPlatformForNewAccounts(ctx context.Context, platform, cellID string) error {
	platform = normalizeAdmissionPlatform(platform)
	cellID = strings.TrimSpace(cellID)
	if d == nil || d.rdb == nil || platform == "" || cellID == "" {
		return errors.New("account Cell platform registration is incomplete")
	}
	return d.rdb.SAdd(ctx, accountCellPlatformCatalog+platform, cellID).Err()
}

// RegisterCell publishes a ready Cell in exactly one platform catalog. The
// endpoint is control-plane metadata; account slot traffic never falls back to
// the control Redis when the assigned Cell is unavailable.
func (d *AccountCellDirectory) RegisterCell(ctx context.Context, platform, cellID, endpoint string) error {
	platform = normalizeAdmissionPlatform(platform)
	cellID = strings.TrimSpace(cellID)
	endpoint = strings.TrimSpace(endpoint)
	if d == nil || d.rdb == nil || platform == "" || cellID == "" || endpoint == "" {
		return errors.New("account cell registration is incomplete")
	}
	_, err := d.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.SAdd(ctx, accountCellPlatformCatalog+platform, cellID)
		pipe.HSet(ctx, accountCellEndpointKey, cellID, endpoint)
		return nil
	})
	return err
}

func (d *AccountCellDirectory) Cells(ctx context.Context, platform string) ([]string, error) {
	platform = normalizeAdmissionPlatform(platform)
	if d == nil || d.rdb == nil || platform == "" {
		return nil, errors.New("account cell directory is not configured")
	}
	cells, err := d.rdb.SMembers(ctx, accountCellPlatformCatalog+platform).Result()
	if err != nil {
		return nil, err
	}
	sort.Strings(cells)
	return cells, nil
}

func (d *AccountCellDirectory) Endpoint(ctx context.Context, cellID string) (string, error) {
	if d == nil || d.rdb == nil || strings.TrimSpace(cellID) == "" {
		return "", errors.New("account cell directory is not configured")
	}
	endpoint, err := d.rdb.HGet(ctx, accountCellEndpointKey, strings.TrimSpace(cellID)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return endpoint, err
}

func (d *AccountCellDirectory) CellBelongsTo(ctx context.Context, cellID, platform string) (bool, error) {
	platform = normalizeAdmissionPlatform(platform)
	if d == nil || d.rdb == nil || strings.TrimSpace(cellID) == "" || platform == "" {
		return false, errors.New("account cell directory is not configured")
	}
	return d.rdb.SIsMember(ctx, accountCellPlatformCatalog+platform, strings.TrimSpace(cellID)).Result()
}

func normalizeAdmissionPlatform(platform string) string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "openai":
		return "openai"
	case "anthropic":
		return "anthropic"
	default:
		return ""
	}
}

func accountCellAssignmentKey(accountID int64) string {
	return accountCellAssignmentPrefix + strconv.FormatInt(accountID, 10)
}

// EnsureAssignment assigns only accounts without an existing assignment. The
// caller supplies the already provisioned Cell list; no hash-ring movement is
// performed when that list grows.
func (d *AccountCellDirectory) EnsureAssignment(ctx context.Context, accountID int64, cells []string) (string, error) {
	return d.ensureAssignment(ctx, accountID, cells, accountCellCatalogKey)
}

func (d *AccountCellDirectory) EnsurePlatformAssignment(ctx context.Context, accountID int64, platform string, cells []string) (string, error) {
	platform = normalizeAdmissionPlatform(platform)
	if platform == "" {
		return "", fmt.Errorf("unsupported admission platform %q", platform)
	}
	return d.ensureAssignment(ctx, accountID, cells, accountCellPlatformCatalog+platform)
}

func (d *AccountCellDirectory) ensureAssignment(ctx context.Context, accountID int64, cells []string, catalogKey string) (string, error) {
	if d == nil || d.rdb == nil || accountID <= 0 {
		return "", errors.New("account cell directory is not configured")
	}
	// Existing ownership is authoritative even if the new-account catalog is
	// temporarily unavailable during a Cell rollout.
	key := accountCellAssignmentKey(accountID)
	if existing, err := d.rdb.Get(ctx, key).Result(); err == nil {
		return existing, nil
	} else if !errors.Is(err, redis.Nil) {
		return "", fmt.Errorf("read account cell assignment: %w", err)
	}
	clean := make([]string, 0, len(cells))
	for _, cell := range cells {
		if cell = strings.TrimSpace(cell); cell != "" {
			clean = append(clean, cell)
		}
	}
	if len(clean) == 0 {
		catalog, err := d.rdb.SMembers(ctx, catalogKey).Result()
		if err != nil {
			return "", fmt.Errorf("read account Cell catalog: %w", err)
		}
		clean = catalog
	}
	sort.Strings(clean)
	unique := clean[:0]
	for _, cell := range clean {
		if len(unique) == 0 || unique[len(unique)-1] != cell {
			unique = append(unique, cell)
		}
	}
	clean = unique
	if len(clean) == 0 {
		return "", errors.New("no admission cells configured")
	}
	// A stable initial choice is sufficient because the assignment is frozen;
	// it is not a ring and is never recomputed for existing accounts.
	idx := int(accountID % int64(len(clean)))
	chosen := clean[idx]
	ok, err := d.rdb.SetNX(ctx, key, chosen, 0).Result()
	if err != nil {
		return "", fmt.Errorf("assign account cell: %w", err)
	}
	if !ok {
		chosen, err = d.rdb.Get(ctx, key).Result()
		if err != nil {
			return "", fmt.Errorf("read raced account cell assignment: %w", err)
		}
	}
	return chosen, nil
}

func (d *AccountCellDirectory) Cell(ctx context.Context, accountID int64) (string, error) {
	if d == nil || d.rdb == nil {
		return "", errors.New("account cell directory is not configured")
	}
	cell, err := d.rdb.Get(ctx, accountCellAssignmentKey(accountID)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return cell, err
}

// Assignments is used to build an immutable in-process routing snapshot. It is
// never called from the request path.
func (d *AccountCellDirectory) Assignments(ctx context.Context) (map[int64]string, error) {
	if d == nil || d.rdb == nil {
		return nil, errors.New("account cell directory is not configured")
	}
	result := make(map[int64]string)
	var cursor uint64
	for {
		keys, next, err := d.rdb.Scan(ctx, cursor, accountCellAssignmentPrefix+"*", 1000).Result()
		if err != nil {
			return nil, err
		}
		if len(keys) > 0 {
			values, err := d.rdb.MGet(ctx, keys...).Result()
			if err != nil {
				return nil, err
			}
			for i, key := range keys {
				accountID, parseErr := strconv.ParseInt(strings.TrimPrefix(key, accountCellAssignmentPrefix), 10, 64)
				if parseErr != nil || values[i] == nil {
					continue
				}
				cellID, ok := values[i].(string)
				if ok && cellID != "" {
					result[accountID] = cellID
				}
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return result, nil
}

func (d *AccountCellDirectory) AssignmentsFor(ctx context.Context, accountIDs []int64) (map[int64]string, error) {
	result := make(map[int64]string, len(accountIDs))
	if d == nil || d.rdb == nil || len(accountIDs) == 0 {
		return result, nil
	}
	keys := make([]string, len(accountIDs))
	for i, accountID := range accountIDs {
		keys[i] = accountCellAssignmentKey(accountID)
	}
	values, err := d.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	for i, value := range values {
		if cellID, ok := value.(string); ok && cellID != "" {
			result[accountIDs[i]] = cellID
		}
	}
	return result, nil
}

// BeginMigration intentionally fails closed. Reading the control Redis cannot
// prove that the owner Cell has no live leases, and without a claim-side
// quiesce fence a new lease could race the ownership update. New Cells accept
// only new account assignments; existing ownership remains frozen.
func (d *AccountCellDirectory) BeginMigration(ctx context.Context, accountID int64, targetCell string, activeLeases int) (uint64, error) {
	if activeLeases != 0 {
		return 0, ErrAccountCellBusy
	}
	targetCell = strings.TrimSpace(targetCell)
	if targetCell == "" {
		return 0, errors.New("target Cell is empty")
	}
	assignment, err := d.Cell(ctx, accountID)
	if err != nil {
		return 0, err
	}
	if assignment == "" {
		return 0, errors.New("account has no Cell assignment")
	}
	if assignment == targetCell {
		return 0, nil
	}
	return 0, ErrAccountCellMigrationDisabled
}

// CommitMigration remains fail-closed for the same reason as BeginMigration.
func (d *AccountCellDirectory) CommitMigration(ctx context.Context, accountID int64, targetCell string, epoch uint64, activeLeases int) error {
	if activeLeases != 0 {
		return ErrAccountCellBusy
	}
	return ErrAccountCellMigrationDisabled
}
