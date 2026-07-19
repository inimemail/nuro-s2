package repository

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

type admissionCellDefinition struct {
	id       string
	platform string
	endpoint string
}

type admissionAccountRoute struct {
	cellID   string
	platform string
	cache    *concurrencyCache
}

// cellAwareConcurrencyCache keeps the legacy implementation as an exact
// compatibility path. In Cell mode only account-owned state is placed in an
// append-only owner Cell; tenant state is reserved first and rolled back when
// the account claim fails.
type cellAwareConcurrencyCache struct {
	legacy    *concurrencyCache
	control   *redis.Client
	directory *AccountCellDirectory
	cfg       *config.Config

	mu            sync.RWMutex
	cells         map[string]*concurrencyCache
	cellPlatforms map[string]string
	routes        map[int64]*admissionAccountRoute
	assignments   map[int64]string
	closed        atomic.Bool
	escrow        *localTenantEscrow
}

func newCellAwareConcurrencyCache(control *redis.Client, cfg *config.Config, legacy *concurrencyCache) (service.ConcurrencyCache, error) {
	if cfg == nil || !cfg.Gateway.Admission.Enabled {
		return legacy, nil
	}
	definitions, err := parseAdmissionCellDefinitions(cfg.Gateway.Admission.OpenAICells, cfg.Gateway.Admission.AnthropicCells)
	if err != nil {
		return nil, err
	}
	c := &cellAwareConcurrencyCache{
		legacy:        legacy,
		control:       control,
		directory:     NewAccountCellDirectory(control),
		cfg:           cfg,
		cells:         make(map[string]*concurrencyCache, len(definitions)),
		cellPlatforms: make(map[string]string, len(definitions)),
		routes:        make(map[int64]*admissionAccountRoute),
		assignments:   make(map[int64]string),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, definition := range definitions {
		cell, err := c.openCell(ctx, definition.id, definition.platform, definition.endpoint)
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("open admission Cell %s: %w", definition.id, err)
		}
		if err := c.directory.RegisterCell(ctx, definition.platform, definition.id, definition.endpoint); err != nil {
			c.Close()
			return nil, fmt.Errorf("register admission Cell %s: %w", definition.id, err)
		}
		_ = cell
	}
	if cfg.Gateway.Admission.EscrowEnabled {
		nodeID := strings.TrimSpace(cfg.Gateway.Admission.NodeID)
		if nodeID == "" {
			nodeID, _ = os.Hostname()
		}
		nodeID = strings.TrimSpace(nodeID)
		if nodeID == "" {
			c.Close()
			return nil, errors.New("tenant escrow requires a stable node_id or hostname")
		}
		nodeTTL := time.Duration(cfg.Gateway.Admission.NodeTTLSeconds) * time.Second
		grace := time.Duration(cfg.Gateway.Admission.DeadNodeGraceSeconds) * time.Second
		if grace < legacy.SlotTTL() {
			grace = legacy.SlotTTL()
		}
		c.escrow, err = newLocalTenantEscrow(ctx, NewTenantEscrowManager(control, nodeTTL), nodeID, cfg.Gateway.Admission.EscrowGrantSize, nodeTTL, grace)
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("start tenant escrow: %w", err)
		}
	}
	return c, nil
}

func parseAdmissionCellDefinitions(openAI, anthropic string) ([]admissionCellDefinition, error) {
	result := make([]admissionCellDefinition, 0, 4)
	seen := make(map[string]string)
	for platform, raw := range map[string]string{"openai": openAI, "anthropic": anthropic} {
		for _, item := range strings.Split(raw, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			parts := strings.SplitN(item, "=", 2)
			if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
				return nil, fmt.Errorf("invalid %s admission Cell %q; expected cell-id=redis://host:port/db", platform, item)
			}
			id, endpoint := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			if existing, ok := seen[id]; ok {
				return nil, fmt.Errorf("admission Cell %q is shared by %s and %s", id, existing, platform)
			}
			seen[id] = platform
			result = append(result, admissionCellDefinition{id: id, platform: platform, endpoint: endpoint})
		}
	}
	if len(result) < 2 {
		return nil, errors.New("distributed admission requires at least one OpenAI Cell and one Anthropic Cell")
	}
	sort.Slice(result, func(i, j int) bool { return result[i].id < result[j].id })
	return result, nil
}

func (c *cellAwareConcurrencyCache) openCell(ctx context.Context, cellID, platform, endpoint string) (*concurrencyCache, error) {
	c.mu.RLock()
	existing := c.cells[cellID]
	existingPlatform := c.cellPlatforms[cellID]
	c.mu.RUnlock()
	if existing != nil {
		if existingPlatform != platform {
			return nil, fmt.Errorf("Cell %s belongs to %s, not %s", cellID, existingPlatform, platform)
		}
		return existing, nil
	}
	options, err := redis.ParseURL(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint: %w", err)
	}
	options.PoolSize = c.cfg.Redis.PoolSize
	options.MinIdleConns = c.cfg.Redis.MinIdleConns
	options.DialTimeout = time.Duration(c.cfg.Redis.DialTimeoutSeconds) * time.Second
	options.ReadTimeout = time.Duration(c.cfg.Redis.ReadTimeoutSeconds) * time.Second
	options.WriteTimeout = time.Duration(c.cfg.Redis.WriteTimeoutSeconds) * time.Second
	if options.Password == "" {
		options.Password = c.cfg.Redis.Password
	}
	if c.cfg.Redis.EnableTLS && options.TLSConfig == nil {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}
	cell := NewConcurrencyCache(client, c.cfg.Gateway.ConcurrencySlotTTLMinutes, c.legacy.waitQueueTTLSeconds).(*concurrencyCache)
	c.mu.Lock()
	if raced := c.cells[cellID]; raced != nil {
		c.mu.Unlock()
		_ = client.Close()
		return raced, nil
	}
	c.cells[cellID] = cell
	c.cellPlatforms[cellID] = platform
	c.mu.Unlock()
	return cell, nil
}

func (c *cellAwareConcurrencyCache) routeForPlatform(ctx context.Context, platform string, accountID int64) (*admissionAccountRoute, error) {
	platform = normalizeAdmissionPlatform(platform)
	if platform == "" {
		return nil, nil
	}
	c.mu.RLock()
	route := c.routes[accountID]
	c.mu.RUnlock()
	if route != nil {
		if route.platform != platform {
			return nil, fmt.Errorf("account %d admission domain changed from %s to %s", accountID, route.platform, platform)
		}
		return route, nil
	}
	c.mu.RLock()
	cellID := c.assignments[accountID]
	c.mu.RUnlock()
	var err error
	if cellID == "" {
		cellID, err = c.directory.EnsurePlatformAssignment(ctx, accountID, platform, nil)
	}
	if err != nil {
		return nil, err
	}
	belongs, err := c.directory.CellBelongsTo(ctx, cellID, platform)
	if err != nil {
		return nil, err
	}
	if !belongs {
		return nil, fmt.Errorf("account %d is assigned to Cell %s outside %s", accountID, cellID, platform)
	}
	c.mu.RLock()
	cell := c.cells[cellID]
	cellPlatform := c.cellPlatforms[cellID]
	c.mu.RUnlock()
	if cell == nil {
		endpoint, err := c.directory.Endpoint(ctx, cellID)
		if err != nil {
			return nil, err
		}
		if endpoint == "" {
			return nil, fmt.Errorf("admission Cell %s has no endpoint", cellID)
		}
		cell, err = c.openCell(ctx, cellID, platform, endpoint)
		if err != nil {
			return nil, err
		}
		cellPlatform = platform
	}
	if cellPlatform != platform {
		return nil, fmt.Errorf("admission Cell %s platform mismatch", cellID)
	}
	route = &admissionAccountRoute{cellID: cellID, platform: platform, cache: cell}
	c.mu.Lock()
	if existing := c.routes[accountID]; existing != nil {
		route = existing
	} else {
		c.routes[accountID] = route
	}
	c.assignments[accountID] = cellID
	c.mu.Unlock()
	// Preserve a cooldown that was written through the legacy compatibility
	// path before this account received its first Cell assignment.
	if ttl, ttlErr := c.legacy.rdb.PTTL(ctx, accountCooldownKey(accountID)).Result(); ttlErr == nil && ttl > 0 {
		_ = route.cache.SetAccountCooldown(ctx, accountID, ttl)
	}
	return route, nil
}

func (c *cellAwareConcurrencyCache) routeForAssigned(ctx context.Context, accountID int64) (*admissionAccountRoute, error) {
	c.mu.RLock()
	route := c.routes[accountID]
	c.mu.RUnlock()
	if route != nil {
		return route, nil
	}
	c.mu.RLock()
	cellID, known := c.assignments[accountID]
	platform := c.cellPlatforms[cellID]
	c.mu.RUnlock()
	if !known || cellID == "" {
		var err error
		cellID, err = c.directory.Cell(ctx, accountID)
		if err != nil || cellID == "" {
			return nil, err
		}
		c.mu.Lock()
		c.assignments[accountID] = cellID
		platform = c.cellPlatforms[cellID]
		c.mu.Unlock()
	}
	if platform == "" {
		for _, candidate := range []string{"openai", "anthropic"} {
			belongs, belongErr := c.directory.CellBelongsTo(ctx, cellID, candidate)
			if belongErr != nil {
				return nil, belongErr
			}
			if belongs {
				platform = candidate
				break
			}
		}
	}
	if platform == "" {
		return nil, fmt.Errorf("assigned Cell %s has no platform", cellID)
	}
	return c.routeForPlatform(ctx, platform, accountID)
}

func (c *cellAwareConcurrencyCache) AcquireAccountSlotForPlatform(ctx context.Context, platform string, accountID int64, maxConcurrency int, requestID string) (bool, error) {
	route, err := c.routeForPlatform(ctx, platform, accountID)
	if err != nil {
		return false, err
	}
	if route == nil {
		return c.legacy.AcquireAccountSlot(ctx, accountID, maxConcurrency, requestID)
	}
	return route.cache.AcquireAccountSlot(ctx, accountID, maxConcurrency, requestID)
}

func (c *cellAwareConcurrencyCache) AcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
	route, err := c.routeForAssigned(ctx, accountID)
	if err != nil {
		return false, err
	}
	if route == nil {
		return c.legacy.AcquireAccountSlot(ctx, accountID, maxConcurrency, requestID)
	}
	return route.cache.AcquireAccountSlot(ctx, accountID, maxConcurrency, requestID)
}

func (c *cellAwareConcurrencyCache) acquireCandidates(ctx context.Context, candidates []service.AccountSlotCandidate, requestID string) (int64, bool, error) {
	for start := 0; start < len(candidates); {
		candidate := candidates[start]
		if candidate.AccountID <= 0 || candidate.MaxConcurrency <= 0 {
			start++
			continue
		}
		route, err := c.routeForPlatform(ctx, candidate.Platform, candidate.AccountID)
		if err != nil {
			return 0, false, err
		}
		if route == nil {
			// Unknown domains retain the legacy Redis, but only for the current
			// candidate. Passing the remaining slice could let the legacy Lua claim
			// a later OpenAI/Anthropic account outside its owner Cell.
			acquired, acquireErr := c.legacy.AcquireAccountSlot(ctx, candidate.AccountID, candidate.MaxConcurrency, requestID)
			if acquireErr != nil || acquired {
				return candidate.AccountID, acquired, acquireErr
			}
			start++
			continue
		}
		end := start + 1
		for end < len(candidates) {
			next := candidates[end]
			nextRoute, routeErr := c.routeForPlatform(ctx, next.Platform, next.AccountID)
			if routeErr != nil {
				return 0, false, routeErr
			}
			if nextRoute == nil || nextRoute.cellID != route.cellID {
				break
			}
			end++
		}
		accountID, acquired, err := route.cache.AcquireFirstAvailableAccountSlot(ctx, candidates[start:end], requestID)
		if err != nil || acquired {
			return accountID, acquired, err
		}
		start = end
	}
	return 0, false, nil
}

func (c *cellAwareConcurrencyCache) AcquireFirstAvailableAccountSlot(ctx context.Context, candidates []service.AccountSlotCandidate, requestID string) (int64, bool, error) {
	return c.acquireCandidates(ctx, candidates, requestID)
}

func (c *cellAwareConcurrencyCache) AcquireFirstAvailableUserAccountSlots(ctx context.Context, userID int64, userMaxConcurrency int, candidates []service.AccountSlotCandidate, userRequestID, accountRequestID string) (int64, bool, error) {
	if c.escrow != nil {
		acquired, err := c.escrow.Acquire(ctx, "user:"+strconv.FormatInt(userID, 10), userMaxConcurrency, userRequestID)
		if err != nil || !acquired {
			return 0, false, err
		}
		accountID, accountAcquired, accountErr := c.acquireCandidates(ctx, candidates, accountRequestID)
		if accountErr != nil || !accountAcquired {
			c.escrow.Release(userRequestID)
			return 0, false, accountErr
		}
		return accountID, true, nil
	}
	userAcquired, err := c.legacy.AcquireUserSlot(ctx, userID, userMaxConcurrency, userRequestID)
	if err != nil || !userAcquired {
		return 0, false, err
	}
	accountID, accountAcquired, accountErr := c.acquireCandidates(ctx, candidates, accountRequestID)
	if accountErr != nil || !accountAcquired {
		_ = c.legacy.ReleaseUserSlot(context.WithoutCancel(ctx), userID, userRequestID)
		return 0, false, accountErr
	}
	return accountID, true, nil
}

func (c *cellAwareConcurrencyCache) ReleaseAccountSlot(ctx context.Context, accountID int64, requestID string) error {
	route, err := c.routeForAssigned(ctx, accountID)
	if err != nil {
		return err
	}
	if route == nil {
		return c.legacy.ReleaseAccountSlot(ctx, accountID, requestID)
	}
	return route.cache.ReleaseAccountSlot(ctx, accountID, requestID)
}

func (c *cellAwareConcurrencyCache) AcquireUserSlot(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
	if c.escrow != nil {
		return c.escrow.Acquire(ctx, "user:"+strconv.FormatInt(userID, 10), maxConcurrency, requestID)
	}
	return c.legacy.AcquireUserSlot(ctx, userID, maxConcurrency, requestID)
}

func (c *cellAwareConcurrencyCache) ReleaseUserSlot(ctx context.Context, userID int64, requestID string) error {
	if c.escrow != nil && c.escrow.Release(requestID) {
		return nil
	}
	return c.legacy.ReleaseUserSlot(ctx, userID, requestID)
}

func (c *cellAwareConcurrencyCache) GetUserConcurrency(ctx context.Context, userID int64) (int, error) {
	if c.escrow != nil {
		return c.escrow.InUse("user:" + strconv.FormatInt(userID, 10)), nil
	}
	return c.legacy.GetUserConcurrency(ctx, userID)
}

func (c *cellAwareConcurrencyCache) RefreshSlot(ctx context.Context, kind string, entityID int64, requestID string) error {
	if kind == "user" && c.escrow != nil && c.escrow.Owns(requestID) {
		return nil
	}
	if kind != "account" {
		return c.legacy.RefreshSlot(ctx, kind, entityID, requestID)
	}
	route, err := c.routeForAssigned(ctx, entityID)
	if err != nil {
		return err
	}
	if route == nil {
		return c.legacy.RefreshSlot(ctx, kind, entityID, requestID)
	}
	return route.cache.RefreshSlot(ctx, kind, entityID, requestID)
}

func (c *cellAwareConcurrencyCache) RefreshSlots(ctx context.Context, renewals []service.ConcurrencySlotRenewal) error {
	groups := make(map[*concurrencyCache][]service.ConcurrencySlotRenewal)
	for _, renewal := range renewals {
		if renewal.Kind == "user" && c.escrow != nil && c.escrow.Owns(renewal.RequestID) {
			continue
		}
		cache := c.legacy
		if renewal.Kind == "account" {
			route, err := c.routeForAssigned(ctx, renewal.EntityID)
			if err != nil {
				return err
			}
			if route != nil {
				cache = route.cache
			}
		}
		groups[cache] = append(groups[cache], renewal)
	}
	var refreshErrors []error
	for cache, grouped := range groups {
		if err := cache.RefreshSlots(ctx, grouped); err != nil {
			refreshErrors = append(refreshErrors, err)
		}
	}
	return errors.Join(refreshErrors...)
}

func (c *cellAwareConcurrencyCache) SlotTTL() time.Duration { return c.legacy.SlotTTL() }

func (c *cellAwareConcurrencyCache) accountCache(ctx context.Context, accountID int64) (*concurrencyCache, error) {
	c.mu.RLock()
	route := c.routes[accountID]
	c.mu.RUnlock()
	if route == nil {
		var err error
		route, err = c.routeForAssigned(ctx, accountID)
		if err != nil {
			return nil, err
		}
	}
	if route == nil {
		return c.legacy, nil
	}
	return route.cache, nil
}

func (c *cellAwareConcurrencyCache) GetAccountConcurrency(ctx context.Context, accountID int64) (int, error) {
	cache, err := c.accountCache(ctx, accountID)
	if err != nil {
		return 0, err
	}
	return cache.GetAccountConcurrency(ctx, accountID)
}

func (c *cellAwareConcurrencyCache) GetAccountConcurrencyBatch(ctx context.Context, accountIDs []int64) (map[int64]int, error) {
	if err := c.primeAssignments(ctx, accountIDs); err != nil {
		return nil, err
	}
	result := make(map[int64]int, len(accountIDs))
	groups := make(map[*concurrencyCache][]int64)
	for _, accountID := range accountIDs {
		cache, err := c.accountCache(ctx, accountID)
		if err != nil {
			return nil, err
		}
		groups[cache] = append(groups[cache], accountID)
	}
	for cache, ids := range groups {
		counts, err := cache.GetAccountConcurrencyBatch(ctx, ids)
		if err != nil {
			return nil, err
		}
		for id, count := range counts {
			result[id] = count
		}
	}
	return result, nil
}

func (c *cellAwareConcurrencyCache) GetAccountsLoadBatch(ctx context.Context, accounts []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	ids := make([]int64, len(accounts))
	for i, account := range accounts {
		ids[i] = account.ID
	}
	if err := c.primeAssignments(ctx, ids); err != nil {
		return nil, err
	}
	result := make(map[int64]*service.AccountLoadInfo, len(accounts))
	groups := make(map[*concurrencyCache][]service.AccountWithConcurrency)
	for _, account := range accounts {
		cache, err := c.accountCache(ctx, account.ID)
		if err != nil {
			return nil, err
		}
		groups[cache] = append(groups[cache], account)
	}
	for cache, grouped := range groups {
		loads, err := cache.GetAccountsLoadBatch(ctx, grouped)
		if err != nil {
			return nil, err
		}
		for id, load := range loads {
			result[id] = load
		}
	}
	return result, nil
}

func (c *cellAwareConcurrencyCache) primeAssignments(ctx context.Context, accountIDs []int64) error {
	missing := make([]int64, 0, len(accountIDs))
	c.mu.RLock()
	for _, accountID := range accountIDs {
		if _, routed := c.routes[accountID]; routed {
			continue
		}
		if _, known := c.assignments[accountID]; !known {
			missing = append(missing, accountID)
		}
	}
	c.mu.RUnlock()
	if len(missing) == 0 {
		return nil
	}
	assignments, err := c.directory.AssignmentsFor(ctx, missing)
	if err != nil {
		return err
	}
	c.mu.Lock()
	for _, accountID := range missing {
		// Do not cache a missing assignment forever. Another node may assign the
		// account after this read; retaining an empty value would keep this
		// process on the legacy Redis until restart.
		if cellID := assignments[accountID]; cellID != "" {
			c.assignments[accountID] = cellID
		}
	}
	c.mu.Unlock()
	return nil
}

func (c *cellAwareConcurrencyCache) IncrementAccountWaitCount(ctx context.Context, accountID int64, maxWait int) (bool, error) {
	cache, err := c.accountCache(ctx, accountID)
	if err != nil {
		return false, err
	}
	return cache.IncrementAccountWaitCount(ctx, accountID, maxWait)
}

func (c *cellAwareConcurrencyCache) DecrementAccountWaitCount(ctx context.Context, accountID int64) error {
	cache, err := c.accountCache(ctx, accountID)
	if err != nil {
		return err
	}
	return cache.DecrementAccountWaitCount(ctx, accountID)
}

func (c *cellAwareConcurrencyCache) GetAccountWaitingCount(ctx context.Context, accountID int64) (int, error) {
	cache, err := c.accountCache(ctx, accountID)
	if err != nil {
		return 0, err
	}
	return cache.GetAccountWaitingCount(ctx, accountID)
}

func (c *cellAwareConcurrencyCache) SetAccountCooldown(ctx context.Context, accountID int64, ttl time.Duration) error {
	cache, err := c.accountCache(ctx, accountID)
	if err != nil {
		return err
	}
	return cache.SetAccountCooldown(ctx, accountID, ttl)
}

func (c *cellAwareConcurrencyCache) ClearAccountCooldown(ctx context.Context, accountID int64) error {
	cache, err := c.accountCache(ctx, accountID)
	if err != nil {
		return err
	}
	return cache.ClearAccountCooldown(ctx, accountID)
}

func (c *cellAwareConcurrencyCache) CleanupExpiredAccountSlots(ctx context.Context, accountID int64) error {
	cache, err := c.accountCache(ctx, accountID)
	if err != nil {
		return err
	}
	return cache.CleanupExpiredAccountSlots(ctx, accountID)
}

func (c *cellAwareConcurrencyCache) CleanupExpiredAccountSlotKeys(ctx context.Context) error {
	c.mu.RLock()
	cells := make([]*concurrencyCache, 0, len(c.cells)+1)
	cells = append(cells, c.legacy)
	for _, cell := range c.cells {
		cells = append(cells, cell)
	}
	c.mu.RUnlock()
	for _, cell := range cells {
		if err := cell.CleanupExpiredAccountSlotKeys(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Cross-process prefix cleanup is intentionally disabled. Lease TTL and
// fencing are the only safe owners during rolling restarts.
func (c *cellAwareConcurrencyCache) CleanupStaleProcessSlots(context.Context, string) error {
	return nil
}

func (c *cellAwareConcurrencyCache) IncrementWaitCount(ctx context.Context, userID int64, maxWait int) (bool, error) {
	return c.legacy.IncrementWaitCount(ctx, userID, maxWait)
}
func (c *cellAwareConcurrencyCache) DecrementWaitCount(ctx context.Context, userID int64) error {
	return c.legacy.DecrementWaitCount(ctx, userID)
}
func (c *cellAwareConcurrencyCache) GetUsersLoadBatch(ctx context.Context, users []service.UserWithConcurrency) (map[int64]*service.UserLoadInfo, error) {
	if c.escrow == nil {
		return c.legacy.GetUsersLoadBatch(ctx, users)
	}
	result := make(map[int64]*service.UserLoadInfo, len(users))
	for _, user := range users {
		current := c.escrow.InUse("user:" + strconv.FormatInt(user.ID, 10))
		load := 0
		if user.MaxConcurrency > 0 {
			load = current * 100 / user.MaxConcurrency
		}
		result[user.ID] = &service.UserLoadInfo{UserID: user.ID, CurrentConcurrency: current, LoadRate: load}
	}
	return result, nil
}

func (c *cellAwareConcurrencyCache) TrackAPIKeySlot(ctx context.Context, apiKeyID int64, requestID string) error {
	return c.legacy.TrackAPIKeySlot(ctx, apiKeyID, requestID)
}
func (c *cellAwareConcurrencyCache) ReleaseAPIKeySlot(ctx context.Context, apiKeyID int64, requestID string) error {
	return c.legacy.ReleaseAPIKeySlot(ctx, apiKeyID, requestID)
}
func (c *cellAwareConcurrencyCache) GetAPIKeyConcurrencyBatch(ctx context.Context, apiKeyIDs []int64) (map[int64]int, error) {
	return c.legacy.GetAPIKeyConcurrencyBatch(ctx, apiKeyIDs)
}

func (c *cellAwareConcurrencyCache) Close() error {
	if c == nil || !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	if c.escrow != nil {
		c.escrow.Close()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var firstErr error
	for _, cell := range c.cells {
		if cell != nil && cell.rdb != nil {
			if err := cell.rdb.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	c.cells = nil
	return firstErr
}

type localEscrowState struct {
	mu       sync.Mutex
	limit    int
	grants   int
	inUse    int
	lastUsed time.Time
}

type localTenantEscrow struct {
	manager  *TenantEscrowManager
	nodeID   string
	epoch    uint64
	grantMax int
	nodeTTL  time.Duration
	grace    time.Duration
	idleTTL  time.Duration

	mu         sync.RWMutex
	requestsMu sync.RWMutex
	states     map[string]*localEscrowState
	requests   map[string]string
	validTo    atomic.Int64
	liveNodes  atomic.Value
	stop       chan struct{}
	done       chan struct{}
	maintDone  chan struct{}
}

func newLocalTenantEscrow(ctx context.Context, manager *TenantEscrowManager, nodeID string, grantMax int, nodeTTL, grace time.Duration) (*localTenantEscrow, error) {
	epoch, err := manager.RegisterNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	idleTTL := 2 * nodeTTL
	if idleTTL < time.Minute {
		idleTTL = time.Minute
	}
	e := &localTenantEscrow{manager: manager, nodeID: nodeID, epoch: epoch, grantMax: grantMax, nodeTTL: nodeTTL, grace: grace, idleTTL: idleTTL, states: make(map[string]*localEscrowState), requests: make(map[string]string), stop: make(chan struct{}), done: make(chan struct{}), maintDone: make(chan struct{})}
	nodes, err := manager.LiveNodes(ctx)
	if err != nil {
		_ = manager.UnregisterNode(context.WithoutCancel(ctx), nodeID, epoch)
		return nil, fmt.Errorf("load escrow node registry: %w", err)
	}
	e.liveNodes.Store(nodes)
	e.validTo.Store(time.Now().Add(nodeTTL).UnixNano())
	go e.heartbeat()
	go e.maintain()
	return e, nil
}

func (e *localTenantEscrow) heartbeat() {
	defer close(e.done)
	interval := e.nodeTTL / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), interval)
			err := e.manager.Heartbeat(ctx, e.nodeID, e.epoch)
			if err == nil {
				e.validTo.Store(time.Now().Add(e.nodeTTL).UnixNano())
			}
			cancel()
		case <-e.stop:
			return
		}
	}
}

func (e *localTenantEscrow) maintain() {
	defer close(e.maintDone)
	interval := e.nodeTTL
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			timeout := min(e.nodeTTL, 5*time.Second)
			if timeout < time.Second {
				timeout = time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			_, _ = e.manager.ReclaimExpiredNodes(ctx, e.grace)
			if nodes, err := e.manager.LiveNodes(ctx); err == nil {
				e.liveNodes.Store(nodes)
			}
			e.releaseIdleStates(ctx, time.Now().Add(-e.idleTTL))
			cancel()
		case <-e.stop:
			return
		}
	}
}

func (e *localTenantEscrow) currentLiveNodes() []string {
	if e == nil {
		return nil
	}
	nodes, _ := e.liveNodes.Load().([]string)
	return nodes
}

func (e *localTenantEscrow) state(tenantID string) *localEscrowState {
	e.mu.RLock()
	state := e.states[tenantID]
	e.mu.RUnlock()
	if state != nil {
		return state
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if state = e.states[tenantID]; state == nil {
		state = &localEscrowState{lastUsed: time.Now()}
		e.states[tenantID] = state
	}
	return state
}

func (e *localTenantEscrow) Acquire(ctx context.Context, tenantID string, limit int, requestID string) (bool, error) {
	if limit <= 0 {
		return true, nil
	}
	if time.Now().UnixNano() >= e.validTo.Load() {
		return false, ErrEscrowFenced
	}
	state := e.state(tenantID)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.lastUsed = time.Now()
	e.requestsMu.RLock()
	_, duplicate := e.requests[requestID]
	e.requestsMu.RUnlock()
	if duplicate {
		return true, nil
	}
	// A reduced tenant limit must return unused grants to the global pool.
	// Active requests are never revoked; they drain naturally before the node
	// can converge below the new limit.
	retained := limit
	if retained < state.inUse {
		retained = state.inUse
	}
	if excess := state.grants - retained; excess > 0 {
		if err := e.manager.Release(ctx, tenantID, e.nodeID, e.epoch, excess); err != nil {
			return false, err
		}
		state.grants -= excess
	}
	state.limit = limit
	usable := min(state.grants, limit)
	if state.inUse >= usable {
		request := min(e.grantMax, limit-state.grants)
		if limit <= e.grantMax {
			holders := StableHolderSet(tenantID, limit, e.currentLiveNodes())
			isHolder := false
			for _, holder := range holders {
				if holder == e.nodeID {
					isHolder = true
					break
				}
			}
			if !isHolder {
				return false, nil
			}
			request = 1
		}
		if request <= 0 {
			return false, nil
		}
		granted, err := e.manager.Grant(ctx, tenantID, e.nodeID, e.epoch, request, limit)
		if err != nil {
			return false, err
		}
		state.grants += granted
		usable = min(state.grants, limit)
	}
	if state.inUse >= usable {
		return false, nil
	}
	e.requestsMu.Lock()
	if _, duplicate := e.requests[requestID]; duplicate {
		e.requestsMu.Unlock()
		return true, nil
	}
	state.inUse++
	e.requests[requestID] = tenantID
	e.requestsMu.Unlock()
	return true, nil
}

func (e *localTenantEscrow) Release(requestID string) bool {
	e.requestsMu.Lock()
	tenantID, ok := e.requests[requestID]
	if ok {
		delete(e.requests, requestID)
	}
	e.requestsMu.Unlock()
	e.mu.RLock()
	state := e.states[tenantID]
	e.mu.RUnlock()
	if !ok || state == nil {
		return false
	}
	state.mu.Lock()
	if state.inUse > 0 {
		state.inUse--
	}
	state.lastUsed = time.Now()
	state.mu.Unlock()
	return true
}

func (e *localTenantEscrow) releaseIdleStates(ctx context.Context, cutoff time.Time) {
	e.mu.RLock()
	type candidate struct {
		tenant string
		state  *localEscrowState
	}
	candidates := make([]candidate, 0, len(e.states))
	for tenant, state := range e.states {
		candidates = append(candidates, candidate{tenant: tenant, state: state})
	}
	e.mu.RUnlock()

	for _, item := range candidates {
		state := item.state
		state.mu.Lock()
		if state.inUse != 0 || state.lastUsed.After(cutoff) {
			state.mu.Unlock()
			continue
		}
		if state.grants > 0 {
			if err := e.manager.Release(ctx, item.tenant, e.nodeID, e.epoch, state.grants); err != nil {
				state.mu.Unlock()
				continue
			}
			state.grants = 0
		}
		e.mu.Lock()
		if e.states[item.tenant] == state && state.inUse == 0 {
			delete(e.states, item.tenant)
		}
		e.mu.Unlock()
		state.mu.Unlock()
	}
}

func (e *localTenantEscrow) Owns(requestID string) bool {
	e.requestsMu.RLock()
	_, ok := e.requests[requestID]
	e.requestsMu.RUnlock()
	return ok
}

func (e *localTenantEscrow) InUse(tenantID string) int {
	e.mu.RLock()
	state := e.states[tenantID]
	e.mu.RUnlock()
	if state == nil {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.inUse
}

func (e *localTenantEscrow) Close() {
	select {
	case <-e.stop:
		return
	default:
		close(e.stop)
	}
	<-e.done
	<-e.maintDone
	e.mu.RLock()
	type release struct {
		tenant string
		count  int
	}
	releases := make([]release, 0, len(e.states))
	for tenant, state := range e.states {
		state.mu.Lock()
		unused := state.grants - state.inUse
		state.grants -= unused
		state.mu.Unlock()
		if unused > 0 {
			releases = append(releases, release{tenant: tenant, count: unused})
		}
	}
	e.mu.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, item := range releases {
		_ = e.manager.Release(ctx, item.tenant, e.nodeID, e.epoch, item.count)
	}
	_ = e.manager.UnregisterNode(ctx, e.nodeID, e.epoch)
}
