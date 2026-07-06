package service

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/tidwall/gjson"
)

const (
	anthropicCacheBoostUpstreamAffinitySessionPrefix = "anthropic-pcache-affinity-upstream:"
	anthropicCacheBoostStaticSeedPrefix              = "anthropic_pcache_static_"
	anthropicCacheBoostMinStaticPrefixBytes          = 1024
	anthropicCacheAffinityStickyTTLDefault           = 5 * time.Minute
	anthropicCacheAffinityStickyTTLExtended          = time.Hour
	anthropicCacheBoostAvailabilityCacheTTL          = 2 * time.Second
)

type anthropicCacheAffinityContextKey struct{}

type anthropicCacheAffinityContextValue struct {
	sessionHash string
	ttl         time.Duration
}

type anthropicCacheBoostGroupAvailability struct {
	expiresAt               time.Time
	upstreamPriorityEnabled bool
}

// WithAnthropicCacheAffinitySession stores a cache-affinity session hash in the
// request context. This is deliberately separate from the ordinary sticky
// session hash used for user-session limits and conversation affinity.
func WithAnthropicCacheAffinitySession(ctx context.Context, sessionHash string, ttl time.Duration) context.Context {
	sessionHash = strings.TrimSpace(sessionHash)
	if ctx == nil || !IsAnthropicCacheBoostUpstreamAffinitySessionHash(sessionHash) {
		return ctx
	}
	if ttl <= 0 {
		ttl = anthropicCacheAffinityStickyTTLDefault
	}
	return context.WithValue(ctx, anthropicCacheAffinityContextKey{}, anthropicCacheAffinityContextValue{
		sessionHash: sessionHash,
		ttl:         ttl,
	})
}

func anthropicCacheAffinitySessionFromContext(ctx context.Context) (string, time.Duration) {
	if ctx == nil {
		return "", 0
	}
	v, _ := ctx.Value(anthropicCacheAffinityContextKey{}).(anthropicCacheAffinityContextValue)
	if !IsAnthropicCacheBoostUpstreamAffinitySessionHash(v.sessionHash) {
		return "", 0
	}
	if v.ttl <= 0 {
		v.ttl = anthropicCacheAffinityStickyTTLDefault
	}
	return v.sessionHash, v.ttl
}

func IsAnthropicCacheBoostUpstreamAffinitySessionHash(sessionHash string) bool {
	return strings.HasPrefix(strings.TrimSpace(sessionHash), anthropicCacheBoostUpstreamAffinitySessionPrefix)
}

// DeriveAnthropicCacheBoostUpstreamAffinityHash builds a stable local affinity
// key from static prompt-cache inputs. It excludes user/session metadata and
// volatile user messages so it cannot merge downstream conversations.
func DeriveAnthropicCacheBoostUpstreamAffinityHash(model string, body []byte) string {
	seed, staticBytes := deriveAnthropicCacheBoostStaticPrefixSeed(body)
	if seed == "" || staticBytes < anthropicCacheBoostMinStaticPrefixBytes {
		return ""
	}
	normalizedModel := strings.TrimSpace(model)
	if normalizedModel == "" {
		normalizedModel = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	}
	if normalizedModel == "" {
		normalizedModel = "unknown"
	}
	return anthropicCacheBoostUpstreamAffinitySessionPrefix + hashSensitiveValueForLog(
		strings.Join([]string{
			"strategy", "anthropic-upstream-static-prefix-v1",
			"model", normalizedModel,
			"seed", seed,
		}, "|"),
	)
}

func (s *GatewayService) AnthropicCacheBoostUpstreamPriorityAvailableForGroup(ctx context.Context, groupID *int64, model string) bool {
	return s.anthropicCacheBoostUpstreamPriorityAvailableForGroup(ctx, groupID, model)
}

func (s *GatewayService) anthropicCacheBoostUpstreamPriorityAvailableForGroup(ctx context.Context, groupID *int64, model string) bool {
	if s == nil {
		return false
	}
	if s.schedulerSnapshot == nil && s.accountRepo == nil {
		return false
	}
	normalizedModel := strings.TrimSpace(model)
	cacheKey := strconv.FormatInt(derefGroupID(groupID), 10) + ":" + normalizedModel
	now := time.Now()
	if raw, ok := s.anthropicCacheBoostGroupAvailability.Load(cacheKey); ok {
		state, _ := raw.(anthropicCacheBoostGroupAvailability)
		if !state.expiresAt.IsZero() && now.Before(state.expiresAt) {
			return state.upstreamPriorityEnabled
		}
	}
	state := anthropicCacheBoostGroupAvailability{
		expiresAt: now.Add(anthropicCacheBoostAvailabilityCacheTTL),
	}
	accounts, useMixed, err := s.listSchedulableAccounts(ctx, groupID, PlatformAnthropic, false)
	if err == nil {
		for i := range accounts {
			account := &accounts[i]
			if !account.IsAnthropicCacheBoostUpstreamHitPriorityEnabled() ||
				!s.isAccountSchedulableForSelection(account) ||
				s.isAnthropicPoolAccountSoftCooling(account) ||
				!s.isAccountAllowedForPlatform(account, PlatformAnthropic, useMixed) ||
				(groupID != nil && accountHasGroupMetadata(account) && !s.isAccountInGroup(account, groupID)) ||
				(normalizedModel != "" && !s.isModelSupportedByAccountWithContext(ctx, account, normalizedModel)) ||
				!account.IsSchedulableForModelWithContext(ctx, normalizedModel) ||
				!s.isAccountSchedulableForQuota(account) {
				continue
			}
			state.upstreamPriorityEnabled = true
			break
		}
	}
	s.anthropicCacheBoostGroupAvailability.Store(cacheKey, state)
	return state.upstreamPriorityEnabled
}

func deriveAnthropicCacheBoostStaticPrefixSeed(body []byte) (string, int) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return "", 0
	}

	var b strings.Builder
	staticBytes := 0
	appendString := func(label, value string, countStatic bool) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		_, _ = b.WriteString("|")
		_, _ = b.WriteString(label)
		_, _ = b.WriteString("=")
		_, _ = b.WriteString(value)
		if countStatic {
			staticBytes += len(value)
		}
	}
	appendJSON := func(label string, value gjson.Result, countStatic bool) {
		if !value.Exists() || value.Raw == "" || value.Raw == "null" || value.Raw == "[]" || value.Raw == "{}" {
			return
		}
		appendString(label, normalizeCompatSeedJSON(json.RawMessage(value.Raw)), countStatic)
	}

	appendString("model", gjson.GetBytes(body, "model").String(), false)
	appendJSON("system", gjson.GetBytes(body, "system"), true)
	appendJSON("tools", gjson.GetBytes(body, "tools"), true)

	if msgs := gjson.GetBytes(body, "messages"); msgs.Exists() && msgs.IsArray() {
		msgs.ForEach(func(_, msg gjson.Result) bool {
			role := strings.TrimSpace(msg.Get("role").String())
			if role != "system" && role != "developer" {
				return true
			}
			appendJSON("message_"+role, msg.Get("content"), true)
			return true
		})
	}

	if instr := gjson.GetBytes(body, "instructions"); instr.Exists() {
		appendJSON("instructions", instr, true)
	}
	if inp := gjson.GetBytes(body, "input"); inp.Exists() && inp.IsArray() {
		inp.ForEach(func(_, item gjson.Result) bool {
			role := strings.TrimSpace(item.Get("role").String())
			if role != "system" && role != "developer" {
				return true
			}
			appendJSON("input_"+role, item.Get("content"), true)
			return true
		})
	}

	if staticBytes == 0 || b.Len() == 0 {
		return "", staticBytes
	}
	return anthropicCacheBoostStaticSeedPrefix + strings.TrimPrefix(b.String(), "|"), staticBytes
}

func DeriveAnthropicCacheAffinityStickyTTL(body []byte) time.Duration {
	includeMessageTTL := len(body) < anthropicCacheBoostAggressiveMinBodyBytes
	if anthropicBodyHasCacheControlTTL(body, cacheTTLTarget1h, includeMessageTTL) {
		return anthropicCacheAffinityStickyTTLExtended
	}
	return anthropicCacheAffinityStickyTTLDefault
}

func anthropicBodyHasCacheControlTTL(body []byte, ttl string, includeMessages bool) bool {
	if len(body) == 0 || strings.TrimSpace(ttl) == "" || !gjson.ValidBytes(body) {
		return false
	}
	found := false
	check := func(value gjson.Result) {
		if found {
			return
		}
		cc := value.Get("cache_control")
		if cc.Exists() && cc.Get("type").String() == "ephemeral" && cc.Get("ttl").String() == ttl {
			found = true
		}
	}

	topCC := gjson.GetBytes(body, "cache_control")
	if topCC.Exists() && topCC.Get("type").String() == "ephemeral" && topCC.Get("ttl").String() == ttl {
		return true
	}

	system := gjson.GetBytes(body, "system")
	if system.IsArray() {
		system.ForEach(func(_, block gjson.Result) bool {
			check(block)
			return !found
		})
	}

	if includeMessages {
		messages := gjson.GetBytes(body, "messages")
		if messages.IsArray() {
			messages.ForEach(func(_, msg gjson.Result) bool {
				content := msg.Get("content")
				if content.IsArray() {
					content.ForEach(func(_, block gjson.Result) bool {
						check(block)
						return !found
					})
				}
				return !found
			})
		}
	}

	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			check(tool)
			return !found
		})
	}

	return found
}

func (s *GatewayService) anthropicCacheAffinityAccountIDFromContext(ctx context.Context, groupID *int64) int64 {
	sessionHash, _ := anthropicCacheAffinitySessionFromContext(ctx)
	if sessionHash == "" || s == nil || s.cache == nil {
		return 0
	}
	accountID, err := s.cache.GetSessionAccountID(ctx, derefGroupID(groupID), sessionHash)
	if err != nil {
		return 0
	}
	return accountID
}

func (s *GatewayService) bindAnthropicCacheAffinitySessionForAccount(ctx context.Context, groupID *int64, account *Account) {
	if s == nil || s.cache == nil || account == nil || !account.IsAnthropicCacheBoostUpstreamHitPriorityEnabled() {
		return
	}
	sessionHash, ttl := anthropicCacheAffinitySessionFromContext(ctx)
	if sessionHash == "" {
		return
	}
	if s.shouldInjectAnthropicCacheTTL1h(ctx, account) {
		ttl = anthropicCacheAffinityStickyTTLExtended
	}
	if ttl <= 0 {
		ttl = anthropicCacheAffinityStickyTTLDefault
	}
	_ = s.cache.SetSessionAccountID(ctx, derefGroupID(groupID), sessionHash, account.ID, ttl)
}

func (s *GatewayService) bindAnthropicCacheAffinitySessionForAccountID(ctx context.Context, groupID *int64, accountID int64) {
	if accountID <= 0 {
		return
	}
	sessionHash, _ := anthropicCacheAffinitySessionFromContext(ctx)
	if sessionHash == "" || s == nil || (s.schedulerSnapshot == nil && s.accountRepo == nil) {
		return
	}
	account, err := s.getSchedulableAccount(ctx, accountID)
	if err != nil {
		return
	}
	s.bindAnthropicCacheAffinitySessionForAccount(ctx, groupID, account)
}

func selectLayeredAccountWithLoadAndAnthropicAffinity(
	accounts []accountWithLoad,
	healthStats *accountRuntimeHealthStats,
	cfg config.GatewaySchedulingConfig,
	preferOAuth bool,
	now time.Time,
	affinityAccountID int64,
	affinityActive bool,
) *accountWithLoad {
	if len(accounts) == 0 {
		return nil
	}
	candidates := filterByMinPriority(accounts)
	candidates = filterByNonPoolModeIfPresent(candidates)
	candidates = filterByAccountHealthBand(candidates, healthStats)
	if cfg.PreferSoonestReset {
		candidates = filterBySoonestReset(candidates, now)
	}
	if affinityActive {
		if selected := selectAnthropicCacheAffinityCandidate(candidates, affinityAccountID, preferOAuth); selected != nil {
			return selected
		}
	}
	return selectByLRU(candidates, preferOAuth)
}

func selectLayeredAccountWithAnthropicAffinity(accounts []*Account, healthStats *accountRuntimeHealthStats, cfg config.GatewaySchedulingConfig, preferOAuth bool, now time.Time, affinityAccountID int64, affinityActive bool) *Account {
	selected := selectLayeredAccountWithLoadAndAnthropicAffinity(accountPointersToNeutralLoads(accounts), healthStats, cfg, preferOAuth, now, affinityAccountID, affinityActive)
	if selected == nil {
		return nil
	}
	return selected.account
}

func selectAnthropicCacheAffinityCandidate(accounts []accountWithLoad, affinityAccountID int64, preferOAuth bool) *accountWithLoad {
	if len(accounts) == 0 {
		return nil
	}
	if affinityAccountID > 0 {
		for i := range accounts {
			acc := accounts[i].account
			if acc != nil && acc.ID == affinityAccountID && acc.IsAnthropicCacheBoostUpstreamHitPriorityEnabled() {
				return &accounts[i]
			}
		}
	}
	enabled := make([]accountWithLoad, 0, len(accounts))
	for _, item := range accounts {
		if item.account != nil && item.account.IsAnthropicCacheBoostUpstreamHitPriorityEnabled() {
			enabled = append(enabled, item)
		}
	}
	if len(enabled) == 0 {
		return nil
	}
	return selectByLRU(enabled, preferOAuth)
}
