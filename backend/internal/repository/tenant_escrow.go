package repository

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// TenantEscrowManager owns soft user/API-key permits held by gateway nodes.
// Account slots remain the hard atomic admission boundary. Escrow is only a
// bounded local permit cache, so losing a node can at worst temporarily reduce
// throughput until its fenced grants are reclaimed.
type TenantEscrowManager struct {
	rdb     *redis.Client
	nodeTTL time.Duration
}

const (
	escrowNodeEpochPrefix = "admission:escrow:node-epoch:"
	escrowNodeLeasePrefix = "admission:escrow:node-lease:"
	escrowHolderPrefix    = "admission:escrow:holder:"
	escrowAllocatedPrefix = "admission:escrow:allocated:"
	escrowNodeTenants     = "admission:escrow:node-tenants:"
	escrowNodeRegistryKey = "admission:escrow:nodes"
	escrowDeadSincePrefix = "admission:escrow:dead-since:"
	escrowReclaimLockKey  = "admission:escrow:reclaim-lock"
)

var ErrEscrowFenced = errors.New("escrow owner epoch fenced")

func NewTenantEscrowManager(rdb *redis.Client, nodeTTL time.Duration) *TenantEscrowManager {
	if nodeTTL <= 0 {
		nodeTTL = 30 * time.Second
	}
	return &TenantEscrowManager{rdb: rdb, nodeTTL: nodeTTL}
}

func (m *TenantEscrowManager) nodeEpochKey(nodeID string) string {
	return escrowNodeEpochPrefix + nodeID
}
func (m *TenantEscrowManager) nodeLeaseKey(nodeID string, epoch uint64) string {
	return escrowNodeLeasePrefix + nodeID + ":" + strconv.FormatUint(epoch, 10)
}
func (m *TenantEscrowManager) holderKey(tenantID string) string {
	return escrowHolderPrefix + "{" + tenantID + "}"
}
func (m *TenantEscrowManager) allocatedKey(tenantID string) string {
	return escrowAllocatedPrefix + "{" + tenantID + "}"
}
func (m *TenantEscrowManager) nodeTenantsKey(nodeID string, epoch uint64) string {
	return escrowNodeTenants + nodeID + ":" + strconv.FormatUint(epoch, 10)
}
func escrowNodeRegistryField(nodeID string, epoch uint64) string {
	return nodeID + "|" + strconv.FormatUint(epoch, 10)
}

func parseEscrowNodeRegistryEntry(field, rawEpoch string) (string, uint64, bool) {
	epoch, err := strconv.ParseUint(rawEpoch, 10, 64)
	if err != nil {
		return "", 0, false
	}
	if separator := strings.LastIndex(field, "|"); separator >= 0 {
		fieldEpoch, parseErr := strconv.ParseUint(field[separator+1:], 10, 64)
		if parseErr != nil || fieldEpoch != epoch {
			return "", 0, false
		}
		field = field[:separator]
	}
	if strings.TrimSpace(field) == "" {
		return "", 0, false
	}
	return field, epoch, true
}

// RegisterNode creates a durable, monotonically increasing owner epoch. A
// restarted process can therefore never reuse a previous node's fencing token.
func (m *TenantEscrowManager) RegisterNode(ctx context.Context, nodeID string) (uint64, error) {
	if m == nil || m.rdb == nil || strings.TrimSpace(nodeID) == "" {
		return 0, errors.New("escrow node identity is not configured")
	}
	epoch, err := m.rdb.Incr(ctx, m.nodeEpochKey(nodeID)).Result()
	if err != nil {
		return 0, fmt.Errorf("allocate escrow node epoch: %w", err)
	}
	if err := m.rdb.Set(ctx, m.nodeLeaseKey(nodeID, uint64(epoch)), strconv.FormatInt(epoch, 10), m.nodeTTL).Err(); err != nil {
		return 0, fmt.Errorf("publish escrow node lease: %w", err)
	}
	if err := m.rdb.HSet(ctx, escrowNodeRegistryKey, escrowNodeRegistryField(nodeID, uint64(epoch)), epoch).Err(); err != nil {
		return 0, fmt.Errorf("register escrow node: %w", err)
	}
	return uint64(epoch), nil
}

func (m *TenantEscrowManager) Heartbeat(ctx context.Context, nodeID string, epoch uint64) error {
	current, err := m.rdb.Get(ctx, m.nodeEpochKey(nodeID)).Uint64()
	if err != nil {
		return err
	}
	if current != epoch {
		return ErrEscrowFenced
	}
	return m.rdb.Set(ctx, m.nodeLeaseKey(nodeID, epoch), strconv.FormatUint(epoch, 10), m.nodeTTL).Err()
}

func (m *TenantEscrowManager) NodeAlive(ctx context.Context, nodeID string, epoch uint64) (bool, error) {
	value, err := m.rdb.Get(ctx, m.nodeLeaseKey(nodeID, epoch)).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return value == strconv.FormatUint(epoch, 10), nil
}

func (m *TenantEscrowManager) LiveNodes(ctx context.Context) ([]string, error) {
	nodes, err := m.rdb.HGetAll(ctx, escrowNodeRegistryKey).Result()
	if err != nil {
		return nil, err
	}
	type registryEntry struct {
		nodeID string
		epoch  uint64
	}
	entries := make([]registryEntry, 0, len(nodes))
	leaseKeys := make([]string, 0, len(nodes))
	for registryField, rawEpoch := range nodes {
		nodeID, epoch, ok := parseEscrowNodeRegistryEntry(registryField, rawEpoch)
		if !ok {
			continue
		}
		entries = append(entries, registryEntry{nodeID: nodeID, epoch: epoch})
		leaseKeys = append(leaseKeys, m.nodeLeaseKey(nodeID, epoch))
	}
	if len(entries) == 0 {
		return []string{}, nil
	}
	leases, err := m.rdb.MGet(ctx, leaseKeys...).Result()
	if err != nil {
		return nil, err
	}
	liveSet := make(map[string]struct{}, len(nodes))
	for i, entry := range entries {
		if value, ok := leases[i].(string); ok && value == strconv.FormatUint(entry.epoch, 10) {
			liveSet[entry.nodeID] = struct{}{}
		}
	}
	live := make([]string, 0, len(liveSet))
	for nodeID := range liveSet {
		live = append(live, nodeID)
	}
	sort.Strings(live)
	return live, nil
}

var grantEscrowScript = redis.NewScript(`
if tostring(redis.call('GET', KEYS[3]) or '') ~= ARGV[2] then return -1 end
local holder = redis.call('HGET', KEYS[1], ARGV[1])
local epoch = ARGV[2]
local requested = tonumber(ARGV[3])
local limit = tonumber(ARGV[4])
local current = 0
if holder ~= false then
  local sep = string.find(holder, ':')
  if sep == nil or string.sub(holder, 1, sep - 1) ~= epoch then return -1 end
  current = tonumber(string.sub(holder, sep + 1)) or 0
end
local allocated = tonumber(redis.call('GET', KEYS[2]) or '0')
local available = limit - allocated
if available <= 0 then return 0 end
local grant = requested
if grant > available then grant = available end
redis.call('HSET', KEYS[1], ARGV[1], epoch .. ':' .. (current + grant))
redis.call('INCRBY', KEYS[2], grant)
redis.call('SADD', KEYS[4], ARGV[5])
return grant
`)

// Grant returns the number of permits actually granted. It is atomic per
// tenant and fenced by the caller's durable node epoch.
func (m *TenantEscrowManager) Grant(ctx context.Context, tenantID, nodeID string, epoch uint64, requested, globalLimit int) (int, error) {
	if requested <= 0 || globalLimit <= 0 {
		return 0, nil
	}
	currentEpoch, err := m.rdb.Get(ctx, m.nodeEpochKey(nodeID)).Uint64()
	if err != nil {
		return 0, err
	}
	if currentEpoch != epoch {
		return 0, ErrEscrowFenced
	}
	if alive, err := m.NodeAlive(ctx, nodeID, epoch); err != nil {
		return 0, err
	} else if !alive {
		return 0, ErrEscrowFenced
	}
	result, err := grantEscrowScript.Run(ctx, m.rdb, []string{m.holderKey(tenantID), m.allocatedKey(tenantID), m.nodeEpochKey(nodeID), m.nodeTenantsKey(nodeID, epoch)}, nodeID, epoch, requested, globalLimit, tenantID).Int()
	if err != nil {
		return 0, err
	}
	if result < 0 {
		return 0, ErrEscrowFenced
	}
	return result, nil
}

var releaseEscrowScript = redis.NewScript(`
if tostring(redis.call('GET', KEYS[3]) or '') ~= ARGV[2] then return -1 end
local holder = redis.call('HGET', KEYS[1], ARGV[1])
if holder == false then return 0 end
local sep = string.find(holder, ':')
if sep == nil or string.sub(holder, 1, sep - 1) ~= ARGV[2] then return -1 end
local current = tonumber(string.sub(holder, sep + 1)) or 0
local release = tonumber(ARGV[3])
if release > current then release = current end
local remaining = current - release
if remaining == 0 then
  redis.call('HDEL', KEYS[1], ARGV[1])
  redis.call('SREM', KEYS[4], ARGV[4])
else
  redis.call('HSET', KEYS[1], ARGV[1], ARGV[2] .. ':' .. remaining)
end
local allocated = tonumber(redis.call('GET', KEYS[2]) or '0') - release
if allocated <= 0 then
  redis.call('DEL', KEYS[2])
else
  redis.call('SET', KEYS[2], allocated)
end
if redis.call('HLEN', KEYS[1]) == 0 then redis.call('DEL', KEYS[1]) end
return release
`)

func (m *TenantEscrowManager) Release(ctx context.Context, tenantID, nodeID string, epoch uint64, permits int) error {
	if permits <= 0 {
		return nil
	}
	currentEpoch, err := m.rdb.Get(ctx, m.nodeEpochKey(nodeID)).Uint64()
	if err != nil {
		return err
	}
	if currentEpoch != epoch {
		return ErrEscrowFenced
	}
	result, err := releaseEscrowScript.Run(ctx, m.rdb, []string{m.holderKey(tenantID), m.allocatedKey(tenantID), m.nodeEpochKey(nodeID), m.nodeTenantsKey(nodeID, epoch)}, nodeID, epoch, permits, tenantID).Int()
	if err != nil {
		return err
	}
	if result < 0 {
		return ErrEscrowFenced
	}
	return nil
}

var reclaimEscrowScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[3]) ~= 0 then return 0 end
local holder = redis.call('HGET', KEYS[1], ARGV[1])
if holder == false then return 0 end
local sep = string.find(holder, ':')
if sep == nil or string.sub(holder, 1, sep - 1) ~= ARGV[2] then return 0 end
local count = tonumber(string.sub(holder, sep + 1)) or 0
redis.call('HDEL', KEYS[1], ARGV[1])
redis.call('SREM', KEYS[4], ARGV[3])
local allocated = tonumber(redis.call('GET', KEYS[2]) or '0')
allocated = allocated - count
if allocated <= 0 then
  redis.call('DEL', KEYS[2])
else
  redis.call('SET', KEYS[2], allocated)
end
if redis.call('HLEN', KEYS[1]) == 0 then redis.call('DEL', KEYS[1]) end
return 1
`)

// Reclaim removes all permits held by a dead node. The lease must already be
// expired, so an in-flight healthy node cannot be reclaimed early.
func (m *TenantEscrowManager) ReclaimDeadNode(ctx context.Context, tenantID, nodeID string, epoch uint64) (bool, error) {
	if alive, err := m.NodeAlive(ctx, nodeID, epoch); err != nil {
		return false, err
	} else if alive {
		return false, nil
	}
	result, err := reclaimEscrowScript.Run(ctx, m.rdb, []string{
		m.holderKey(tenantID), m.allocatedKey(tenantID), m.nodeLeaseKey(nodeID, epoch), m.nodeTenantsKey(nodeID, epoch),
	}, nodeID, epoch, tenantID).Int()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

// ReclaimExpiredNodes waits through the configured grace period before
// reclaiming grants. The grace must be at least the maximum in-flight request
// lease in deployments that require strict no-oversell semantics.
func (m *TenantEscrowManager) ReclaimExpiredNodes(ctx context.Context, grace time.Duration) (int, error) {
	if m == nil || m.rdb == nil {
		return 0, errors.New("escrow manager is not configured")
	}
	if grace < 0 {
		grace = 0
	}
	locked, err := m.rdb.SetNX(ctx, escrowReclaimLockKey, time.Now().UnixNano(), m.nodeTTL).Result()
	if err != nil || !locked {
		return 0, err
	}
	nodes, err := m.rdb.HGetAll(ctx, escrowNodeRegistryKey).Result()
	if err != nil {
		return 0, err
	}
	reclaimed := 0
	now := time.Now()
	for registryField, rawEpoch := range nodes {
		nodeID, epoch, ok := parseEscrowNodeRegistryEntry(registryField, rawEpoch)
		if !ok {
			continue
		}
		alive, aliveErr := m.NodeAlive(ctx, nodeID, epoch)
		if aliveErr != nil {
			return reclaimed, aliveErr
		}
		deadKey := escrowDeadSincePrefix + nodeID + ":" + strconv.FormatUint(epoch, 10)
		if alive {
			_ = m.rdb.Del(ctx, deadKey).Err()
			continue
		}
		deadAt, deadErr := m.rdb.Get(ctx, deadKey).Int64()
		if errors.Is(deadErr, redis.Nil) {
			_ = m.rdb.SetNX(ctx, deadKey, now.Unix(), grace+24*time.Hour).Err()
			continue
		}
		if deadErr != nil || now.Sub(time.Unix(deadAt, 0)) < grace {
			continue
		}
		tenants, tenantErr := m.rdb.SMembers(ctx, m.nodeTenantsKey(nodeID, epoch)).Result()
		if tenantErr != nil {
			return reclaimed, tenantErr
		}
		for _, tenantID := range tenants {
			ok, reclaimErr := m.ReclaimDeadNode(ctx, tenantID, nodeID, epoch)
			if reclaimErr != nil {
				return reclaimed, reclaimErr
			}
			if ok {
				reclaimed++
			}
		}
		remaining, countErr := m.rdb.SCard(ctx, m.nodeTenantsKey(nodeID, epoch)).Result()
		if countErr == nil && remaining == 0 {
			_, _ = m.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.HDel(ctx, escrowNodeRegistryKey, registryField)
				pipe.Del(ctx, m.nodeTenantsKey(nodeID, epoch), deadKey)
				return nil
			})
		}
	}
	return reclaimed, nil
}

func (m *TenantEscrowManager) UnregisterNode(ctx context.Context, nodeID string, epoch uint64) error {
	remaining, err := m.rdb.SCard(ctx, m.nodeTenantsKey(nodeID, epoch)).Result()
	if err != nil {
		return err
	}
	if remaining != 0 {
		return nil
	}
	_, err = m.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Del(ctx, m.nodeLeaseKey(nodeID, epoch), m.nodeTenantsKey(nodeID, epoch))
		pipe.HDel(ctx, escrowNodeRegistryKey, escrowNodeRegistryField(nodeID, epoch))
		return nil
	})
	return err
}

// StableHolderSet keeps low-limit tenants on a small, deterministic subset of
// nodes. This avoids handing 5 permits to 50 nodes as fractional local state.
func StableHolderSet(tenantID string, limit int, nodes []string) []string {
	if limit <= 0 || len(nodes) == 0 {
		return nil
	}
	unique := make([]string, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		if strings.TrimSpace(node) == "" {
			continue
		}
		if _, exists := seen[node]; exists {
			continue
		}
		seen[node] = struct{}{}
		unique = append(unique, node)
	}
	sort.Strings(unique)
	if len(unique) == 0 {
		return nil
	}
	count := limit
	if count > len(unique) {
		count = len(unique)
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(tenantID))
	start := int(h.Sum64() % uint64(len(unique)))
	result := make([]string, 0, count)
	for i := 0; len(result) < count && i < len(unique); i++ {
		result = append(result, unique[(start+i)%len(unique)])
	}
	return result
}
