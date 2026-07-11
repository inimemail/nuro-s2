package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/cespare/xxhash/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

const (
	// ChatGPT internal API for OAuth accounts
	chatgptCodexURL = "https://chatgpt.com/backend-api/codex/responses"
	// OpenAI Platform API for API Key accounts (fallback)
	openaiPlatformAPIURL            = "https://api.openai.com/v1/responses"
	openaiPlatformAPIInputTokensURL = "https://api.openai.com/v1/responses/input_tokens"
	openaiStickySessionTTL          = time.Hour // 粘性会话TTL
	codexCLIUserAgent               = "codex_cli_rs/0.144.1"
	// codex_cli_only 拒绝时单个请求头日志长度上限（字符）
	codexCLIOnlyHeaderValueMaxBytes = 256

	// OpenAIParsedRequestBodyKey 缓存 handler 侧已解析的请求体，避免重复解析。
	OpenAIParsedRequestBodyKey = "openai_parsed_request_body"
	// OpenAI WS Mode 失败后的重连次数上限（不含首次尝试）。
	// 与 Codex 客户端保持一致：失败后最多重连 5 次。
	openAIWSReconnectRetryLimit = 5
	// OpenAI WS Mode 重连退避默认值（可由配置覆盖）。
	openAIWSRetryBackoffInitialDefault       = 120 * time.Millisecond
	openAIWSRetryBackoffMaxDefault           = 2 * time.Second
	openAIWSRetryJitterRatioDefault          = 0.2
	openAICompactSessionSeedKey              = "openai_compact_session_seed"
	openAIPromptCacheBoostMinBodyBytes       = 16 * 1024
	openAIPromptCacheBoostAffinityStickyTTL  = 24 * time.Hour
	openAIPromptCacheBoostAggressiveCacheTTL = 2 * time.Second
	codexCLIVersion                          = "0.144.1"
	// Codex 限额快照仅用于后台展示/诊断，不需要每个成功请求都立即落库。
	openAICodexSnapshotPersistMinInterval = 30 * time.Second
	openAIOAuth401RefreshRetryKey         = "openai_oauth_401_refresh_retry"
)

// OpenAI allowed headers whitelist (for non-passthrough).
var openaiAllowedHeaders = map[string]bool{
	"accept-language":       true,
	"content-type":          true,
	"conversation_id":       true,
	"user-agent":            true,
	"originator":            true,
	"session_id":            true,
	"x-codex-turn-state":    true,
	"x-codex-turn-metadata": true,
}

// OpenAI passthrough allowed headers whitelist.
// 透传模式下仅放行这些低风险请求头，避免将非标准/环境噪声头传给上游触发风控。
var openaiPassthroughAllowedHeaders = map[string]bool{
	"accept":                true,
	"accept-language":       true,
	"content-type":          true,
	"conversation_id":       true,
	"openai-beta":           true,
	"user-agent":            true,
	"originator":            true,
	"session_id":            true,
	"x-codex-turn-state":    true,
	"x-codex-turn-metadata": true,
}

// codex_cli_only 拒绝时记录的请求头白名单（仅用于诊断日志，不参与上游透传）
var codexCLIOnlyDebugHeaderWhitelist = []string{
	"User-Agent",
	"Content-Type",
	"Accept",
	"Accept-Language",
	"OpenAI-Beta",
	"Originator",
	"Session_ID",
	"Conversation_ID",
	"X-Request-ID",
	"X-Client-Request-ID",
	"X-Forwarded-For",
	"X-Real-IP",
}

// OpenAICodexUsageSnapshot represents Codex API usage limits from response headers
type OpenAICodexUsageSnapshot struct {
	PrimaryUsedPercent          *float64 `json:"primary_used_percent,omitempty"`
	PrimaryResetAfterSeconds    *int     `json:"primary_reset_after_seconds,omitempty"`
	PrimaryWindowMinutes        *int     `json:"primary_window_minutes,omitempty"`
	SecondaryUsedPercent        *float64 `json:"secondary_used_percent,omitempty"`
	SecondaryResetAfterSeconds  *int     `json:"secondary_reset_after_seconds,omitempty"`
	SecondaryWindowMinutes      *int     `json:"secondary_window_minutes,omitempty"`
	PrimaryOverSecondaryPercent *float64 `json:"primary_over_secondary_percent,omitempty"`
	UpdatedAt                   string   `json:"updated_at,omitempty"`
}

// NormalizedCodexLimits contains normalized 5h/7d rate limit data
type NormalizedCodexLimits struct {
	Used5hPercent   *float64
	Reset5hSeconds  *int
	Window5hMinutes *int
	Used7dPercent   *float64
	Reset7dSeconds  *int
	Window7dMinutes *int
}

// Normalize converts primary/secondary fields to canonical 5h/7d fields.
// Strategy: Compare window_minutes to determine which is 5h vs 7d.
// Returns nil if snapshot is nil or has no useful data.
func (s *OpenAICodexUsageSnapshot) Normalize() *NormalizedCodexLimits {
	if s == nil {
		return nil
	}

	result := &NormalizedCodexLimits{}

	primaryMins := 0
	secondaryMins := 0
	hasPrimaryWindow := false
	hasSecondaryWindow := false

	if s.PrimaryWindowMinutes != nil {
		primaryMins = *s.PrimaryWindowMinutes
		hasPrimaryWindow = true
	}
	if s.SecondaryWindowMinutes != nil {
		secondaryMins = *s.SecondaryWindowMinutes
		hasSecondaryWindow = true
	}

	// Determine mapping based on window_minutes
	use5hFromPrimary := false
	use7dFromPrimary := false

	if hasPrimaryWindow && hasSecondaryWindow {
		// Both known: smaller window is 5h, larger is 7d
		if primaryMins < secondaryMins {
			use5hFromPrimary = true
		} else {
			use7dFromPrimary = true
		}
	} else if hasPrimaryWindow {
		// Only primary known: classify by threshold (<=360 min = 6h -> 5h window)
		if primaryMins <= 360 {
			use5hFromPrimary = true
		} else {
			use7dFromPrimary = true
		}
	} else if hasSecondaryWindow {
		// Only secondary known: classify by threshold
		if secondaryMins <= 360 {
			// 5h from secondary, so primary (if any data) is 7d
			use7dFromPrimary = true
		} else {
			// 7d from secondary, so primary (if any data) is 5h
			use5hFromPrimary = true
		}
	} else {
		// No window_minutes: fall back to legacy assumption (primary=7d, secondary=5h)
		use7dFromPrimary = true
	}

	// Assign values
	if use5hFromPrimary {
		result.Used5hPercent = s.PrimaryUsedPercent
		result.Reset5hSeconds = s.PrimaryResetAfterSeconds
		result.Window5hMinutes = s.PrimaryWindowMinutes
		result.Used7dPercent = s.SecondaryUsedPercent
		result.Reset7dSeconds = s.SecondaryResetAfterSeconds
		result.Window7dMinutes = s.SecondaryWindowMinutes
	} else if use7dFromPrimary {
		result.Used7dPercent = s.PrimaryUsedPercent
		result.Reset7dSeconds = s.PrimaryResetAfterSeconds
		result.Window7dMinutes = s.PrimaryWindowMinutes
		result.Used5hPercent = s.SecondaryUsedPercent
		result.Reset5hSeconds = s.SecondaryResetAfterSeconds
		result.Window5hMinutes = s.SecondaryWindowMinutes
	}

	return result
}

// OpenAIUsage represents OpenAI API response usage
type OpenAIUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	ImageOutputTokens        int `json:"image_output_tokens,omitempty"`
}

// OpenAIForwardResult represents the result of forwarding
type OpenAIForwardResult struct {
	RequestID  string
	ResponseID string
	Usage      OpenAIUsage
	Model      string // 原始模型（用于响应和日志显示）
	// BillingModel is the model used for cost calculation.
	// When non-empty, CalculateCost uses this instead of Model.
	// This is set by the Anthropic Messages conversion path where
	// the mapped upstream model differs from the client-facing model.
	BillingModel string
	// UpstreamModel is the actual model sent to the upstream provider after mapping.
	// Empty when no mapping was applied (requested model was used as-is).
	UpstreamModel string
	// ServiceTier records the OpenAI Responses API service tier, e.g. "priority" / "flex".
	// Nil means the request did not specify a recognized tier.
	ServiceTier *string
	// ReasoningEffort is extracted from request body (reasoning.effort) or derived from model suffix.
	// Stored for usage records display; nil means not provided / not applicable.
	ReasoningEffort      *string
	Stream               bool
	OpenAIWSMode         bool
	ResponseHeaders      http.Header
	Duration             time.Duration
	FirstTokenMs         *int
	UpstreamHeaderMs     *int
	UpstreamFirstByteMs  *int
	FirstClientFlushMs   *int
	EdgePrepareMs        *int
	EdgeQueueWaitMs      *int
	EdgeRelayStartMs     *int
	EdgeFallbackReason   *string
	EdgeRetryCount       *int
	ClientDisconnect     bool
	ImageCount           int
	ImageSize            string
	ImageInputSize       string
	ImageOutputSize      string
	ImageOutputSizes     []string
	ImageSizeSource      string
	ImageSizeBreakdown   map[string]int
	VideoCount           int
	VideoResolution      string
	VideoDurationSeconds int
	wsReplayInput        []json.RawMessage
	wsReplayInputExists  bool
}

type OpenAIWSRetryMetricsSnapshot struct {
	RetryAttemptsTotal            int64 `json:"retry_attempts_total"`
	RetryBackoffMsTotal           int64 `json:"retry_backoff_ms_total"`
	RetryExhaustedTotal           int64 `json:"retry_exhausted_total"`
	NonRetryableFastFallbackTotal int64 `json:"non_retryable_fast_fallback_total"`
}

type OpenAICompatibilityFallbackMetricsSnapshot struct {
	SessionHashLegacyReadFallbackTotal int64   `json:"session_hash_legacy_read_fallback_total"`
	SessionHashLegacyReadFallbackHit   int64   `json:"session_hash_legacy_read_fallback_hit"`
	SessionHashLegacyDualWriteTotal    int64   `json:"session_hash_legacy_dual_write_total"`
	SessionHashLegacyReadHitRate       float64 `json:"session_hash_legacy_read_hit_rate"`

	MetadataLegacyFallbackIsMaxTokensOneHaikuTotal int64 `json:"metadata_legacy_fallback_is_max_tokens_one_haiku_total"`
	MetadataLegacyFallbackThinkingEnabledTotal     int64 `json:"metadata_legacy_fallback_thinking_enabled_total"`
	MetadataLegacyFallbackPrefetchedStickyAccount  int64 `json:"metadata_legacy_fallback_prefetched_sticky_account_total"`
	MetadataLegacyFallbackPrefetchedStickyGroup    int64 `json:"metadata_legacy_fallback_prefetched_sticky_group_total"`
	MetadataLegacyFallbackSingleAccountRetryTotal  int64 `json:"metadata_legacy_fallback_single_account_retry_total"`
	MetadataLegacyFallbackAccountSwitchCountTotal  int64 `json:"metadata_legacy_fallback_account_switch_count_total"`
	MetadataLegacyFallbackTotal                    int64 `json:"metadata_legacy_fallback_total"`
}

type openAIWSRetryMetrics struct {
	retryAttempts            atomic.Int64
	retryBackoffMs           atomic.Int64
	retryExhausted           atomic.Int64
	nonRetryableFastFallback atomic.Int64
}

type accountWriteThrottle struct {
	minInterval time.Duration
	mu          sync.Mutex
	lastByID    map[int64]time.Time
}

const accountWriteThrottleMaxEntries = 4096

func newAccountWriteThrottle(minInterval time.Duration) *accountWriteThrottle {
	return &accountWriteThrottle{
		minInterval: minInterval,
		lastByID:    make(map[int64]time.Time),
	}
}

func (t *accountWriteThrottle) Allow(id int64, now time.Time) bool {
	if t == nil || id <= 0 || t.minInterval <= 0 {
		return true
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if last, ok := t.lastByID[id]; ok && now.Sub(last) < t.minInterval {
		return false
	}
	t.lastByID[id] = now

	if len(t.lastByID) > accountWriteThrottleMaxEntries {
		cutoff := now.Add(-4 * t.minInterval)
		for accountID, writtenAt := range t.lastByID {
			if writtenAt.Before(cutoff) {
				delete(t.lastByID, accountID)
			}
		}
	}
	if len(t.lastByID) > accountWriteThrottleMaxEntries {
		type throttleEntry struct {
			accountID int64
			written   time.Time
		}
		entries := make([]throttleEntry, 0, len(t.lastByID))
		for accountID, writtenAt := range t.lastByID {
			entries = append(entries, throttleEntry{accountID: accountID, written: writtenAt})
		}
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].written.Before(entries[j].written)
		})
		for i := 0; i < len(entries)-accountWriteThrottleMaxEntries; i++ {
			delete(t.lastByID, entries[i].accountID)
		}
	}

	return true
}

var defaultOpenAICodexSnapshotPersistThrottle = newAccountWriteThrottle(openAICodexSnapshotPersistMinInterval)
var defaultOpenAIPromptCacheHitRateLogThrottle = newAccountWriteThrottle(5 * time.Minute)

// ErrNoAvailableCompactAccounts indicates the request needs /responses/compact
// support but no compatible account is available.
var ErrNoAvailableCompactAccounts = errors.New("no available OpenAI accounts support /responses/compact")

// OpenAIGatewayService handles OpenAI API gateway operations
type OpenAIGatewayService struct {
	accountRepo           AccountRepository
	usageLogRepo          UsageLogRepository
	usageBillingRepo      UsageBillingRepository
	userRepo              UserRepository
	userSubRepo           UserSubscriptionRepository
	cache                 GatewayCache
	cfg                   *config.Config
	codexDetector         CodexClientRestrictionDetector
	schedulerSnapshot     *SchedulerSnapshotService
	concurrencyService    *ConcurrencyService
	billingService        *BillingService
	rateLimitService      *RateLimitService
	billingCacheService   *BillingCacheService
	userGroupRateResolver *userGroupRateResolver
	httpUpstream          HTTPUpstream
	deferredService       *DeferredService
	openAITokenProvider   *OpenAITokenProvider
	grokTokenProvider     *GrokTokenProvider
	toolCorrector         *CodexToolCorrector
	openaiWSResolver      OpenAIWSProtocolResolver
	resolver              *ModelPricingResolver
	channelService        *ChannelService
	balanceNotifyService  *BalanceNotifyService
	settingService        *SettingService
	tlsFPProfileService   *TLSFingerprintProfileService
	userPlatformQuotaRepo UserPlatformQuotaRepository

	openaiWSPoolOnce              sync.Once
	openaiWSStateStoreOnce        sync.Once
	openaiAccountStatsOnce        sync.Once
	openaiSchedulerOnce           sync.Once
	openaiWSPassthroughDialerOnce sync.Once
	openaiWSPool                  *openAIWSConnPool
	openaiWSStateStore            OpenAIWSStateStore
	openaiScheduler               OpenAIAccountScheduler
	openaiWSPassthroughDialer     openAIWSClientDialer
	openaiAccountStats            *openAIAccountRuntimeStats

	openaiWSFallbackUntil                        sync.Map // key: int64(accountID), value: time.Time
	openaiAccountRuntimeBlockUntil               sync.Map // key: int64(accountID), value: time.Time
	openaiPoolSoftCooldownUntil                  sync.Map // key: int64(accountID), value: time.Time
	openaiPoolSoftCooldownContext                sync.Map // key: int64(accountID), value: openAIPoolSoftCooldownContext
	openaiPoolSoftCooldownFailureCount           sync.Map // key: int64(accountID), value: int
	openaiPoolRecoveryProbeInFlight              sync.Map // key: int64(accountID), value: struct{}
	openaiPoolRecoveryProbeFailureCount          sync.Map // key: int64(accountID), value: int
	openaiPoolRecoveryProbeAdminKickAt           sync.Map // key: int64(accountID), value: time.Time
	openaiCodexAutoResetInFlight                 sync.Map // key: int64(accountID), value: struct{}
	openaiCodexAutoResetCooldownUntil            sync.Map // key: int64(accountID), value: time.Time
	openaiOAuth429WindowStartUnixNano            atomic.Int64
	openaiOAuth429WindowCount                    atomic.Int64
	openaiWSRetryMetrics                         openAIWSRetryMetrics
	responseHeaderFilter                         *responseheaders.CompiledHeaderFilter
	codexSnapshotThrottle                        *accountWriteThrottle
	openaiPromptCacheBoostDisabledUntil          sync.Map // key: int64(accountID), value: time.Time
	openaiPromptCacheBoostGroupAvailabilityCache sync.Map // key: string(group:model), value: promptCacheBoostGroupAvailability
	openaiCompatSessionResponses                 sync.Map
	openaiCompatAnthropicDigestSessions          sync.Map
	openaiCyberPolicySessionBlocks               sync.Map // key: platform:accountID:anchorType:anchorHash, value: cyberPolicySessionBlock
	openaiFirstTokenTimeoutPlaceholderGuard      openAIFirstTokenTimeoutPlaceholderGuard
}

type promptCacheBoostGroupAvailability struct {
	expiresAt               time.Time
	aggressiveEnabled       bool
	upstreamPriorityEnabled bool
	keyOptimizationEnabled  bool
}

// NewOpenAIGatewayService creates a new OpenAIGatewayService
func NewOpenAIGatewayService(
	accountRepo AccountRepository,
	usageLogRepo UsageLogRepository,
	usageBillingRepo UsageBillingRepository,
	userRepo UserRepository,
	userSubRepo UserSubscriptionRepository,
	userGroupRateRepo UserGroupRateRepository,
	cache GatewayCache,
	cfg *config.Config,
	schedulerSnapshot *SchedulerSnapshotService,
	concurrencyService *ConcurrencyService,
	billingService *BillingService,
	rateLimitService *RateLimitService,
	billingCacheService *BillingCacheService,
	httpUpstream HTTPUpstream,
	deferredService *DeferredService,
	openAITokenProvider *OpenAITokenProvider,
	resolver *ModelPricingResolver,
	channelService *ChannelService,
	balanceNotifyService *BalanceNotifyService,
	settingService *SettingService,
	tlsFPProfileService *TLSFingerprintProfileService,
	userPlatformQuotaRepo UserPlatformQuotaRepository,
) *OpenAIGatewayService {
	svc := &OpenAIGatewayService{
		accountRepo:         accountRepo,
		usageLogRepo:        usageLogRepo,
		usageBillingRepo:    usageBillingRepo,
		userRepo:            userRepo,
		userSubRepo:         userSubRepo,
		cache:               cache,
		cfg:                 cfg,
		codexDetector:       NewOpenAICodexClientRestrictionDetector(cfg),
		schedulerSnapshot:   schedulerSnapshot,
		concurrencyService:  concurrencyService,
		billingService:      billingService,
		rateLimitService:    rateLimitService,
		billingCacheService: billingCacheService,
		userGroupRateResolver: newUserGroupRateResolver(
			userGroupRateRepo,
			nil,
			resolveUserGroupRateCacheTTL(cfg),
			nil,
			"service.openai_gateway",
		),
		httpUpstream:          httpUpstream,
		deferredService:       deferredService,
		openAITokenProvider:   openAITokenProvider,
		toolCorrector:         NewCodexToolCorrector(),
		openaiWSResolver:      NewOpenAIWSProtocolResolver(cfg),
		resolver:              resolver,
		channelService:        channelService,
		balanceNotifyService:  balanceNotifyService,
		settingService:        settingService,
		tlsFPProfileService:   tlsFPProfileService,
		userPlatformQuotaRepo: userPlatformQuotaRepo,
		responseHeaderFilter:  compileResponseHeaderFilter(cfg),
		codexSnapshotThrottle: newAccountWriteThrottle(openAICodexSnapshotPersistMinInterval),
	}
	if rateLimitService != nil {
		rateLimitService.SetAccountRuntimeBlocker(svc)
	}
	if openAITokenProvider != nil {
		openAITokenProvider.SetAccountRuntimeBlocker(svc)
	}
	svc.logOpenAIWSModeBootstrap()
	return svc
}

func (s *OpenAIGatewayService) SetGrokTokenProvider(provider *GrokTokenProvider) {
	if s == nil {
		return
	}
	s.grokTokenProvider = provider
}

// ResolveChannelMapping 解析渠道级模型映射（代理到 ChannelService）
func (s *OpenAIGatewayService) ResolveChannelMapping(ctx context.Context, groupID int64, model string) ChannelMappingResult {
	if s.channelService == nil {
		return ChannelMappingResult{MappedModel: model}
	}
	return s.channelService.ResolveChannelMapping(ctx, groupID, model)
}

// IsModelRestricted 检查模型是否被渠道限制（代理到 ChannelService）
func (s *OpenAIGatewayService) IsModelRestricted(ctx context.Context, groupID int64, model string) bool {
	if s.channelService == nil {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, groupID, model)
}

// ResolveChannelMappingAndRestrict 解析渠道映射。
// 模型限制检查已移至调度阶段，restricted 始终返回 false。
func (s *OpenAIGatewayService) ResolveChannelMappingAndRestrict(ctx context.Context, groupID *int64, model string) (ChannelMappingResult, bool) {
	if s.channelService == nil {
		return ChannelMappingResult{MappedModel: model}, false
	}
	return s.channelService.ResolveChannelMappingAndRestrict(ctx, groupID, model)
}

func (s *OpenAIGatewayService) isCodexImageGenerationBridgeEnabled(ctx context.Context, account *Account, apiKey *APIKey) bool {
	if override := account.CodexImageGenerationBridgeOverride(); override != nil {
		return *override
	}
	if s != nil && s.channelService != nil && apiKey != nil && apiKey.GroupID != nil {
		ch, err := s.channelService.GetChannelForGroup(ctx, *apiKey.GroupID)
		if err != nil {
			slog.Warn("failed to resolve codex image generation bridge channel override", "group_id", *apiKey.GroupID, "error", err)
		} else if override := ch.CodexImageGenerationBridgeOverride(PlatformOpenAI); override != nil {
			return *override
		}
	}
	return s != nil && s.cfg != nil && s.cfg.Gateway.CodexImageGenerationBridgeEnabled
}

func (s *OpenAIGatewayService) checkChannelPricingRestriction(ctx context.Context, groupID *int64, requestedModel string) bool {
	if groupID == nil || s.channelService == nil || requestedModel == "" {
		return false
	}
	mapping := s.channelService.ResolveChannelMapping(ctx, *groupID, requestedModel)
	billingModel := billingModelForRestriction(mapping.BillingModelSource, requestedModel, mapping.MappedModel)
	if billingModel == "" {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, *groupID, billingModel)
}

func (s *OpenAIGatewayService) isUpstreamModelRestrictedByChannel(ctx context.Context, groupID int64, account *Account, requestedModel string, requireCompact bool) bool {
	if s.channelService == nil {
		return false
	}
	upstreamModel := resolveOpenAIAccountUpstreamModelForRequest(account, requestedModel, requireCompact, s.openAICompactModelFallback())
	if upstreamModel == "" {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, groupID, upstreamModel)
}

func (s *OpenAIGatewayService) needsUpstreamChannelRestrictionCheck(ctx context.Context, groupID *int64) bool {
	if groupID == nil || s.channelService == nil {
		return false
	}
	ch, err := s.channelService.GetChannelForGroup(ctx, *groupID)
	if err != nil {
		slog.Warn("failed to check openai channel upstream restriction", "group_id", *groupID, "error", err)
		return false
	}
	if ch == nil || !ch.RestrictModels {
		return false
	}
	return ch.BillingModelSource == BillingModelSourceUpstream
}

// ReplaceModelInBody 替换请求体中的 JSON model 字段（通用 gjson/sjson 实现）。
func (s *OpenAIGatewayService) ReplaceModelInBody(body []byte, newModel string) []byte {
	return ReplaceModelInBody(body, newModel)
}

func (s *OpenAIGatewayService) getCodexSnapshotThrottle() *accountWriteThrottle {
	if s != nil && s.codexSnapshotThrottle != nil {
		return s.codexSnapshotThrottle
	}
	return defaultOpenAICodexSnapshotPersistThrottle
}

func (s *OpenAIGatewayService) billingDeps() *billingDeps {
	return &billingDeps{
		accountRepo:           s.accountRepo,
		userRepo:              s.userRepo,
		userSubRepo:           s.userSubRepo,
		billingCacheService:   s.billingCacheService,
		deferredService:       s.deferredService,
		balanceNotifyService:  s.balanceNotifyService,
		userPlatformQuotaRepo: s.userPlatformQuotaRepo,
	}
}

// CloseOpenAIWSPool 关闭 OpenAI WebSocket 连接池的后台 worker 和空闲连接。
// 应在应用优雅关闭时调用。
func (s *OpenAIGatewayService) CloseOpenAIWSPool() {
	if s != nil && s.openaiWSPool != nil {
		s.openaiWSPool.Close()
	}
}

func (s *OpenAIGatewayService) logOpenAIWSModeBootstrap() {
	if s == nil || s.cfg == nil {
		return
	}
	wsCfg := s.cfg.Gateway.OpenAIWS
	logOpenAIWSModeInfo(
		"bootstrap enabled=%v oauth_enabled=%v apikey_enabled=%v force_http=%v responses_websockets_v2=%v responses_websockets=%v payload_log_sample_rate=%.3f event_flush_batch_size=%d event_flush_interval_ms=%d prewarm_cooldown_ms=%d retry_backoff_initial_ms=%d retry_backoff_max_ms=%d retry_jitter_ratio=%.3f retry_total_budget_ms=%d ws_read_limit_bytes=%d",
		wsCfg.Enabled,
		wsCfg.OAuthEnabled,
		wsCfg.APIKeyEnabled,
		wsCfg.ForceHTTP,
		wsCfg.ResponsesWebsocketsV2,
		wsCfg.ResponsesWebsockets,
		wsCfg.PayloadLogSampleRate,
		wsCfg.EventFlushBatchSize,
		wsCfg.EventFlushIntervalMS,
		wsCfg.PrewarmCooldownMS,
		wsCfg.RetryBackoffInitialMS,
		wsCfg.RetryBackoffMaxMS,
		wsCfg.RetryJitterRatio,
		wsCfg.RetryTotalBudgetMS,
		openAIWSMessageReadLimitBytes,
	)
}

func (s *OpenAIGatewayService) getCodexClientRestrictionDetector() CodexClientRestrictionDetector {
	if s != nil && s.codexDetector != nil {
		return s.codexDetector
	}
	var cfg *config.Config
	if s != nil {
		cfg = s.cfg
	}
	return NewOpenAICodexClientRestrictionDetector(cfg)
}

func (s *OpenAIGatewayService) getOpenAIWSProtocolResolver() OpenAIWSProtocolResolver {
	if s != nil && s.openaiWSResolver != nil {
		return s.openaiWSResolver
	}
	var cfg *config.Config
	if s != nil {
		cfg = s.cfg
	}
	return NewOpenAIWSProtocolResolver(cfg)
}

func classifyOpenAIWSReconnectReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var fallbackErr *openAIWSFallbackError
	if !errors.As(err, &fallbackErr) || fallbackErr == nil {
		return "", false
	}
	reason := strings.TrimSpace(fallbackErr.Reason)
	if reason == "" {
		return "", false
	}

	baseReason := strings.TrimPrefix(reason, "prewarm_")

	switch baseReason {
	case "policy_violation",
		"message_too_big",
		"upgrade_required",
		"ws_unsupported",
		"auth_failed",
		"invalid_encrypted_content",
		"previous_response_not_found":
		return reason, false
	}

	switch baseReason {
	case "read_event",
		"write_request",
		"write",
		"acquire_timeout",
		"acquire_conn",
		"conn_queue_full",
		"dial_failed",
		"upstream_5xx",
		"event_error",
		"error_event",
		"upstream_error_event",
		"ws_connection_limit_reached",
		"missing_final_response":
		return reason, true
	default:
		return reason, false
	}
}

func resolveOpenAIWSFallbackErrorResponse(err error) (statusCode int, errType string, clientMessage string, upstreamMessage string, ok bool) {
	if err == nil {
		return 0, "", "", "", false
	}
	var fallbackErr *openAIWSFallbackError
	if !errors.As(err, &fallbackErr) || fallbackErr == nil {
		return 0, "", "", "", false
	}

	reason := strings.TrimSpace(fallbackErr.Reason)
	reason = strings.TrimPrefix(reason, "prewarm_")
	if reason == "" {
		return 0, "", "", "", false
	}

	var dialErr *openAIWSDialError
	if fallbackErr.Err != nil && errors.As(fallbackErr.Err, &dialErr) && dialErr != nil {
		if dialErr.StatusCode > 0 {
			statusCode = dialErr.StatusCode
		}
		if dialErr.Err != nil {
			upstreamMessage = sanitizeUpstreamErrorMessage(strings.TrimSpace(dialErr.Err.Error()))
		}
	}

	switch reason {
	case "invalid_encrypted_content":
		if statusCode == 0 {
			statusCode = http.StatusBadRequest
		}
		errType = "invalid_request_error"
		if upstreamMessage == "" {
			upstreamMessage = "encrypted content could not be verified"
		}
	case "previous_response_not_found":
		if statusCode == 0 {
			statusCode = http.StatusBadRequest
		}
		errType = "invalid_request_error"
		if upstreamMessage == "" {
			upstreamMessage = "previous response not found"
		}
	case "upgrade_required":
		if statusCode == 0 {
			statusCode = http.StatusUpgradeRequired
		}
	case "ws_unsupported":
		if statusCode == 0 {
			statusCode = http.StatusBadRequest
		}
	case "auth_failed":
		if statusCode == 0 {
			statusCode = http.StatusUnauthorized
		}
	case "upstream_rate_limited":
		if statusCode == 0 {
			statusCode = http.StatusTooManyRequests
		}
	default:
		if statusCode == 0 {
			return 0, "", "", "", false
		}
	}

	if upstreamMessage == "" && fallbackErr.Err != nil {
		upstreamMessage = sanitizeUpstreamErrorMessage(strings.TrimSpace(fallbackErr.Err.Error()))
	}
	if upstreamMessage == "" {
		switch reason {
		case "upgrade_required":
			upstreamMessage = "upstream websocket upgrade required"
		case "ws_unsupported":
			upstreamMessage = "upstream websocket not supported"
		case "auth_failed":
			upstreamMessage = "upstream authentication failed"
		case "upstream_rate_limited":
			upstreamMessage = "upstream rate limit exceeded, please retry later"
		default:
			upstreamMessage = "Upstream request failed"
		}
	}

	if errType == "" {
		if statusCode == http.StatusTooManyRequests {
			errType = "rate_limit_error"
		} else {
			errType = "upstream_error"
		}
	}
	clientMessage = upstreamMessage
	return statusCode, errType, clientMessage, upstreamMessage, true
}

func (s *OpenAIGatewayService) writeOpenAIWSFallbackErrorResponse(c *gin.Context, account *Account, wsErr error) bool {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return false
	}
	statusCode, errType, clientMessage, upstreamMessage, ok := resolveOpenAIWSFallbackErrorResponse(wsErr)
	if !ok {
		return false
	}
	if strings.TrimSpace(clientMessage) == "" {
		clientMessage = "Upstream request failed"
	}
	if strings.TrimSpace(upstreamMessage) == "" {
		upstreamMessage = clientMessage
	}

	setOpsUpstreamError(c, statusCode, upstreamMessage, "")
	if account != nil {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: statusCode,
			Kind:               "ws_error",
			Message:            upstreamMessage,
		})
	}
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": clientMessage,
		},
	})
	return true
}

func (s *OpenAIGatewayService) openAIWSRetryBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}

	initial := openAIWSRetryBackoffInitialDefault
	maxBackoff := openAIWSRetryBackoffMaxDefault
	jitterRatio := openAIWSRetryJitterRatioDefault
	if s != nil && s.cfg != nil {
		wsCfg := s.cfg.Gateway.OpenAIWS
		if wsCfg.RetryBackoffInitialMS > 0 {
			initial = time.Duration(wsCfg.RetryBackoffInitialMS) * time.Millisecond
		}
		if wsCfg.RetryBackoffMaxMS > 0 {
			maxBackoff = time.Duration(wsCfg.RetryBackoffMaxMS) * time.Millisecond
		}
		if wsCfg.RetryJitterRatio >= 0 {
			jitterRatio = wsCfg.RetryJitterRatio
		}
	}
	if initial <= 0 {
		return 0
	}
	if maxBackoff <= 0 {
		maxBackoff = initial
	}
	if maxBackoff < initial {
		maxBackoff = initial
	}
	if jitterRatio < 0 {
		jitterRatio = 0
	}
	if jitterRatio > 1 {
		jitterRatio = 1
	}

	shift := attempt - 1
	if shift < 0 {
		shift = 0
	}
	backoff := initial
	if shift > 0 {
		backoff = initial * time.Duration(1<<shift)
	}
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	if jitterRatio <= 0 {
		return backoff
	}
	jitter := time.Duration(float64(backoff) * jitterRatio)
	if jitter <= 0 {
		return backoff
	}
	delta := time.Duration(rand.Int63n(int64(jitter)*2+1)) - jitter
	withJitter := backoff + delta
	if withJitter < 0 {
		return 0
	}
	return withJitter
}

func (s *OpenAIGatewayService) openAIWSRetryTotalBudget() time.Duration {
	if s != nil && s.cfg != nil {
		ms := s.cfg.Gateway.OpenAIWS.RetryTotalBudgetMS
		if ms <= 0 {
			return 0
		}
		return time.Duration(ms) * time.Millisecond
	}
	return 0
}

func (s *OpenAIGatewayService) recordOpenAIWSRetryAttempt(backoff time.Duration) {
	if s == nil {
		return
	}
	s.openaiWSRetryMetrics.retryAttempts.Add(1)
	if backoff > 0 {
		s.openaiWSRetryMetrics.retryBackoffMs.Add(backoff.Milliseconds())
	}
}

func (s *OpenAIGatewayService) recordOpenAIWSRetryExhausted() {
	if s == nil {
		return
	}
	s.openaiWSRetryMetrics.retryExhausted.Add(1)
}

func (s *OpenAIGatewayService) recordOpenAIWSNonRetryableFastFallback() {
	if s == nil {
		return
	}
	s.openaiWSRetryMetrics.nonRetryableFastFallback.Add(1)
}

func (s *OpenAIGatewayService) SnapshotOpenAIWSRetryMetrics() OpenAIWSRetryMetricsSnapshot {
	if s == nil {
		return OpenAIWSRetryMetricsSnapshot{}
	}
	return OpenAIWSRetryMetricsSnapshot{
		RetryAttemptsTotal:            s.openaiWSRetryMetrics.retryAttempts.Load(),
		RetryBackoffMsTotal:           s.openaiWSRetryMetrics.retryBackoffMs.Load(),
		RetryExhaustedTotal:           s.openaiWSRetryMetrics.retryExhausted.Load(),
		NonRetryableFastFallbackTotal: s.openaiWSRetryMetrics.nonRetryableFastFallback.Load(),
	}
}

func SnapshotOpenAICompatibilityFallbackMetrics() OpenAICompatibilityFallbackMetricsSnapshot {
	legacyReadFallbackTotal, legacyReadFallbackHit, legacyDualWriteTotal := openAIStickyCompatStats()
	isMaxTokensOneHaiku, thinkingEnabled, prefetchedStickyAccount, prefetchedStickyGroup, singleAccountRetry, accountSwitchCount := RequestMetadataFallbackStats()

	readHitRate := float64(0)
	if legacyReadFallbackTotal > 0 {
		readHitRate = float64(legacyReadFallbackHit) / float64(legacyReadFallbackTotal)
	}
	metadataFallbackTotal := isMaxTokensOneHaiku + thinkingEnabled + prefetchedStickyAccount + prefetchedStickyGroup + singleAccountRetry + accountSwitchCount

	return OpenAICompatibilityFallbackMetricsSnapshot{
		SessionHashLegacyReadFallbackTotal: legacyReadFallbackTotal,
		SessionHashLegacyReadFallbackHit:   legacyReadFallbackHit,
		SessionHashLegacyDualWriteTotal:    legacyDualWriteTotal,
		SessionHashLegacyReadHitRate:       readHitRate,

		MetadataLegacyFallbackIsMaxTokensOneHaikuTotal: isMaxTokensOneHaiku,
		MetadataLegacyFallbackThinkingEnabledTotal:     thinkingEnabled,
		MetadataLegacyFallbackPrefetchedStickyAccount:  prefetchedStickyAccount,
		MetadataLegacyFallbackPrefetchedStickyGroup:    prefetchedStickyGroup,
		MetadataLegacyFallbackSingleAccountRetryTotal:  singleAccountRetry,
		MetadataLegacyFallbackAccountSwitchCountTotal:  accountSwitchCount,
		MetadataLegacyFallbackTotal:                    metadataFallbackTotal,
	}
}

func (s *OpenAIGatewayService) detectCodexClientRestriction(c *gin.Context, account *Account, body []byte) CodexClientRestrictionDetectionResult {
	var globalAllowedClients []string
	var policy CodexCLIOnlyPolicy
	if account != nil && account.IsCodexCLIOnlyEnabled() && s != nil && s.settingService != nil {
		ctx := context.Background()
		if c != nil && c.Request != nil {
			ctx = c.Request.Context()
		}
		if s.settingService.IsOpenAIAllowClaudeCodeCodexPluginEnabled(ctx) {
			globalAllowedClients = []string{openai.AllowedClientClaudeCode}
		}
		policy = s.settingService.GetCodexCLIOnlyPolicy(ctx)
	}
	return s.getCodexClientRestrictionDetector().DetectWithPolicy(c, account, globalAllowedClients, policy, body)
}

func getAPIKeyIDFromContext(c *gin.Context) int64 {
	if c == nil {
		return 0
	}
	v, exists := c.Get("api_key")
	if !exists {
		return 0
	}
	apiKey, ok := v.(*APIKey)
	if !ok || apiKey == nil {
		return 0
	}
	return apiKey.ID
}

// isolateOpenAISessionID 将 apiKeyID 混入 session 标识符，
// 确保不同 API Key 的用户即使使用相同的原始 session_id/conversation_id，
// 到达上游的标识符也不同，防止跨用户会话碰撞。
func isolateOpenAISessionID(apiKeyID int64, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	h := xxhash.New()
	_, _ = fmt.Fprintf(h, "k%d:", apiKeyID)
	_, _ = h.WriteString(raw)
	return fmt.Sprintf("%016x", h.Sum64())
}

func logCodexCLIOnlyDetection(ctx context.Context, c *gin.Context, account *Account, apiKeyID int64, result CodexClientRestrictionDetectionResult, body []byte) {
	if !result.Enabled {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	accountID := int64(0)
	if account != nil {
		accountID = account.ID
	}
	fields := []zap.Field{
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", accountID),
		zap.Bool("codex_cli_only_enabled", result.Enabled),
		zap.Bool("codex_official_client_match", result.Matched),
		zap.String("reject_reason", result.Reason),
	}
	if apiKeyID > 0 {
		fields = append(fields, zap.Int64("api_key_id", apiKeyID))
	}
	if !result.Matched {
		fields = appendCodexCLIOnlyRejectedRequestFields(fields, c, body)
	}
	log := logger.FromContext(ctx).With(fields...)
	if result.Matched {
		log.Info("OpenAI codex_cli_only 放行请求")
		return
	}
	log.Warn("OpenAI codex_cli_only 拒绝非官方客户端请求")
}

func appendCodexCLIOnlyRejectedRequestFields(fields []zap.Field, c *gin.Context, body []byte) []zap.Field {
	if c == nil || c.Request == nil {
		return fields
	}

	req := c.Request
	requestModel, requestStream, promptCacheKey := extractOpenAIRequestMetaFromBody(body)
	fields = append(fields,
		zap.String("request_method", strings.TrimSpace(req.Method)),
		zap.String("request_path", strings.TrimSpace(req.URL.Path)),
		zap.String("request_query", strings.TrimSpace(req.URL.RawQuery)),
		zap.String("request_host", strings.TrimSpace(req.Host)),
		zap.String("request_client_ip", strings.TrimSpace(ip.GetClientIP(c))),
		zap.String("request_remote_addr", strings.TrimSpace(req.RemoteAddr)),
		zap.String("request_user_agent", strings.TrimSpace(req.Header.Get("User-Agent"))),
		zap.String("request_content_type", strings.TrimSpace(req.Header.Get("Content-Type"))),
		zap.Int64("request_content_length", req.ContentLength),
		zap.Bool("request_stream", requestStream),
	)
	if requestModel != "" {
		fields = append(fields, zap.String("request_model", requestModel))
	}
	if promptCacheKey != "" {
		fields = append(fields, zap.String("request_prompt_cache_key_sha256", hashSensitiveValueForLog(promptCacheKey)))
	}

	if headers := snapshotCodexCLIOnlyHeaders(req.Header); len(headers) > 0 {
		fields = append(fields, zap.Any("request_headers", headers))
	}
	fields = append(fields, zap.Int("request_body_size", len(body)))
	return fields
}

func snapshotCodexCLIOnlyHeaders(header http.Header) map[string]string {
	if len(header) == 0 {
		return nil
	}
	result := make(map[string]string, len(codexCLIOnlyDebugHeaderWhitelist))
	for _, key := range codexCLIOnlyDebugHeaderWhitelist {
		value := strings.TrimSpace(header.Get(key))
		if value == "" {
			continue
		}
		result[strings.ToLower(key)] = truncateString(value, codexCLIOnlyHeaderValueMaxBytes)
	}
	return result
}

func hashSensitiveValueForLog(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func logOpenAIInstructionsRequiredDebug(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	upstreamStatusCode int,
	upstreamMsg string,
	requestBody []byte,
	upstreamBody []byte,
) {
	msg := strings.TrimSpace(upstreamMsg)
	if !isOpenAIInstructionsRequiredError(upstreamStatusCode, msg, upstreamBody) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	accountID := int64(0)
	accountName := ""
	if account != nil {
		accountID = account.ID
		accountName = strings.TrimSpace(account.Name)
	}

	userAgent := ""
	originator := ""
	if c != nil {
		userAgent = strings.TrimSpace(c.GetHeader("User-Agent"))
		originator = strings.TrimSpace(c.GetHeader("originator"))
	}

	fields := []zap.Field{
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", accountID),
		zap.String("account_name", accountName),
		zap.Int("upstream_status_code", upstreamStatusCode),
		zap.String("upstream_error_message", msg),
		zap.String("request_user_agent", userAgent),
		zap.Bool("codex_official_client_match", openai.IsCodexOfficialClientByHeaders(userAgent, originator)),
	}
	fields = appendCodexCLIOnlyRejectedRequestFields(fields, c, requestBody)

	logger.FromContext(ctx).With(fields...).Warn("OpenAI 上游返回 Instructions are required，已记录请求详情用于排查")
}

func isOpenAIInstructionsRequiredError(upstreamStatusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if upstreamStatusCode != http.StatusBadRequest {
		return false
	}

	hasInstructionRequired := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "instructions are required") {
			return true
		}
		if strings.Contains(lower, "required parameter: 'instructions'") {
			return true
		}
		if strings.Contains(lower, "required parameter: instructions") {
			return true
		}
		if strings.Contains(lower, "missing required parameter") && strings.Contains(lower, "instructions") {
			return true
		}
		return strings.Contains(lower, "instruction") && strings.Contains(lower, "required")
	}

	if hasInstructionRequired(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}

	errMsg := gjson.GetBytes(upstreamBody, "error.message").String()
	errMsgLower := strings.ToLower(strings.TrimSpace(errMsg))
	errCode := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.code").String()))
	errParam := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.param").String()))
	errType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.type").String()))

	if errParam == "instructions" {
		return true
	}
	if hasInstructionRequired(errMsg) {
		return true
	}
	if strings.Contains(errCode, "missing_required_parameter") && strings.Contains(errMsgLower, "instructions") {
		return true
	}
	if strings.Contains(errType, "invalid_request") && strings.Contains(errMsgLower, "instructions") && strings.Contains(errMsgLower, "required") {
		return true
	}

	return false
}

func isOpenAITransientProcessingError(upstreamStatusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if upstreamStatusCode != http.StatusBadRequest {
		return false
	}

	match := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "an error occurred while processing your request") {
			return true
		}
		if strings.Contains(lower, "selected model is at capacity") {
			return true
		}
		return strings.Contains(lower, "you can retry your request") &&
			strings.Contains(lower, "help.openai.com") &&
			strings.Contains(lower, "request id")
	}

	if match(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}
	if match(gjson.GetBytes(upstreamBody, "error.message").String()) {
		return true
	}
	return match(string(upstreamBody))
}

func isOpenAIContextWindowError(upstreamMsg string, upstreamBody []byte) bool {
	match := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "context_too_large") || strings.Contains(lower, "context_length_exceeded") {
			return true
		}
		if strings.Contains(lower, "maximum context length") || strings.Contains(lower, "max context length") {
			return true
		}
		hasExceeded := strings.Contains(lower, "exceed") || strings.Contains(lower, "too large") || strings.Contains(lower, "too long")
		if strings.Contains(lower, "context window") && hasExceeded {
			return true
		}
		if strings.Contains(lower, "context length") && hasExceeded {
			return true
		}
		return strings.Contains(lower, "token limit") &&
			strings.Contains(lower, "context") &&
			hasExceeded
	}

	if match(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}
	for _, path := range []string{
		"error.message",
		"response.error.message",
		"message",
		"error.code",
		"response.error.code",
		"code",
	} {
		if match(gjson.GetBytes(upstreamBody, path).String()) {
			return true
		}
	}
	return match(string(upstreamBody))
}

// ExtractSessionID extracts the raw session ID from headers or body without hashing.
// Used by ForwardAsAnthropic to pass as prompt_cache_key for upstream cache.
func (s *OpenAIGatewayService) ExtractSessionID(c *gin.Context, body []byte) string {
	if c == nil {
		return ""
	}
	sessionID := strings.TrimSpace(c.GetHeader("session_id"))
	if sessionID == "" {
		sessionID = strings.TrimSpace(c.GetHeader("conversation_id"))
	}
	if sessionID == "" && len(body) > 0 {
		sessionID = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	return sessionID
}

func explicitOpenAISessionID(c *gin.Context, body []byte) string {
	if sessionID := explicitOpenAIHeaderSessionID(c); sessionID != "" {
		return sessionID
	}
	return explicitOpenAIBodyPromptCacheKey(body)
}

func explicitOpenAIHeaderSessionID(c *gin.Context) string {
	if c == nil {
		return ""
	}

	sessionID := strings.TrimSpace(c.GetHeader("session_id"))
	if sessionID == "" {
		sessionID = strings.TrimSpace(c.GetHeader("conversation_id"))
	}
	return sessionID
}

func explicitOpenAIBodyPromptCacheKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
}

// GenerateExplicitSessionHash generates a sticky-session hash only from explicit
// client session signals. It intentionally skips content-derived fallback and is
// used by stateless endpoints such as /v1/images.
func (s *OpenAIGatewayService) GenerateExplicitSessionHash(c *gin.Context, body []byte) string {
	sessionID := explicitOpenAISessionID(c, body)
	if sessionID == "" {
		return ""
	}

	currentHash, legacyHash := deriveOpenAISessionHashes(sessionID)
	attachOpenAILegacySessionHashToGin(c, legacyHash)
	return currentHash
}

// GeneratePromptCacheBoostAffinitySessionHash generates a sticky hash used only
// for prompt-cache affinity. Explicit client session signals win on the basic
// path; group-aware callers may upgrade body prompt_cache_key to upstream-cache
// affinity when that safer mode is enabled for the group.
func (s *OpenAIGatewayService) GeneratePromptCacheBoostAffinitySessionHash(c *gin.Context, body []byte, model string) string {
	if c == nil || len(body) == 0 || explicitOpenAISessionID(c, body) != "" {
		return ""
	}
	return DeriveOpenAIPromptCacheBoostAffinityHash(model, body)
}

func (s *OpenAIGatewayService) GeneratePromptCacheBoostAffinitySessionHashForGroup(ctx context.Context, c *gin.Context, groupID *int64, body []byte, model string) string {
	return s.GeneratePromptCacheBoostAffinitySessionHashForGroupWithMapped(ctx, c, groupID, body, model, nil, "")
}

func (s *OpenAIGatewayService) GeneratePromptCacheBoostAffinitySessionHashForGroupWithMapped(ctx context.Context, c *gin.Context, groupID *int64, body []byte, model string, mappedBody []byte, mappedModel string) string {
	if c == nil || len(body) == 0 || explicitOpenAIHeaderSessionID(c) != "" {
		return ""
	}
	upstreamBody := body
	upstreamModel := model
	if len(mappedBody) > 0 {
		upstreamBody = mappedBody
	}
	if strings.TrimSpace(mappedModel) != "" {
		upstreamModel = mappedModel
	}
	if promptCacheKey := explicitOpenAIBodyPromptCacheKey(body); promptCacheKey != "" {
		if s != nil && s.schedulerSnapshot != nil && s.openAIPromptCacheBoostUpstreamPriorityAvailableForGroup(ctx, groupID, upstreamModel) {
			return DeriveOpenAIPromptCacheBoostExplicitKeyUpstreamAffinityHash(upstreamModel, promptCacheKey)
		}
		return ""
	}
	if s == nil || s.schedulerSnapshot == nil || !openAIPromptCacheBoostBodyMayBenefitFromAggressive(body) {
		return DeriveOpenAIPromptCacheBoostAffinityHash(model, body)
	}
	if s.openAIPromptCacheBoostUpstreamPriorityAvailableForGroup(ctx, groupID, upstreamModel) {
		if s.openAIPromptCacheBoostKeyOptimizationAvailableForGroup(ctx, groupID, upstreamModel) {
			if optimizedHash := DeriveOpenAIPromptCacheBoostOptimizedAffinityHash(upstreamModel, upstreamBody); optimizedHash != "" {
				return optimizedHash
			}
		}
		if upstreamHash := DeriveOpenAIPromptCacheBoostUpstreamAffinityHash(upstreamModel, upstreamBody); upstreamHash != "" {
			return upstreamHash
		}
	}
	if s.openAIPromptCacheBoostAggressiveAvailableForGroup(ctx, groupID, model) {
		if aggressiveHash := DeriveOpenAIPromptCacheBoostAggressiveAffinityHash(model, body); aggressiveHash != "" {
			return aggressiveHash
		}
	}
	return DeriveOpenAIPromptCacheBoostAffinityHash(model, body)
}

func (s *OpenAIGatewayService) openAIPromptCacheBoostUpstreamPriorityAvailableForGroup(ctx context.Context, groupID *int64, model string) bool {
	return s.openAIPromptCacheBoostAvailabilityForGroup(ctx, groupID, model).upstreamPriorityEnabled
}

func (s *OpenAIGatewayService) openAIPromptCacheBoostAggressiveAvailableForGroup(ctx context.Context, groupID *int64, model string) bool {
	return s.openAIPromptCacheBoostAvailabilityForGroup(ctx, groupID, model).aggressiveEnabled
}

func (s *OpenAIGatewayService) openAIPromptCacheBoostKeyOptimizationAvailableForGroup(ctx context.Context, groupID *int64, model string) bool {
	return s.openAIPromptCacheBoostAvailabilityForGroup(ctx, groupID, model).keyOptimizationEnabled
}

func (s *OpenAIGatewayService) openAIPromptCacheBoostAvailabilityForGroup(ctx context.Context, groupID *int64, model string) promptCacheBoostGroupAvailability {
	if s == nil || s.schedulerSnapshot == nil {
		return promptCacheBoostGroupAvailability{}
	}
	cacheKey := strconv.FormatInt(derefGroupID(groupID), 10) + ":" + NormalizeOpenAICompatRequestedModel(model)
	now := time.Now()
	if raw, ok := s.openaiPromptCacheBoostGroupAvailabilityCache.Load(cacheKey); ok {
		state, _ := raw.(promptCacheBoostGroupAvailability)
		if !state.expiresAt.IsZero() && now.Before(state.expiresAt) {
			return state
		}
	}
	state := promptCacheBoostGroupAvailability{
		expiresAt: now.Add(openAIPromptCacheBoostAggressiveCacheTTL),
	}
	keyOptimizationCompatible := true
	keyOptimizationSeen := false
	accounts, _, err := s.schedulerSnapshot.ListSchedulableAccounts(ctx, groupID, PlatformOpenAI, false)
	if err == nil {
		for i := range accounts {
			account := &accounts[i]
			if !isOpenAIAccountEligibleForRequest(ctx, account, model, false, OpenAIEndpointCapabilityChatCompletions, "") ||
				s.isOpenAIPoolAccountSoftCooling(account) ||
				s.isOpenAIAccountRuntimeBlocked(account) {
				continue
			}
			if account.IsOpenAIPromptCacheBoostAggressive() {
				state.aggressiveEnabled = true
			}
			if account.IsOpenAIPromptCacheBoostUpstreamHitPriorityEnabled() {
				state.upstreamPriorityEnabled = true
				if account.IsOpenAIPromptCacheKeyOptimizationEnabled() {
					keyOptimizationSeen = true
				} else {
					keyOptimizationCompatible = false
				}
			}
			if state.aggressiveEnabled && state.upstreamPriorityEnabled && !keyOptimizationCompatible {
				break
			}
		}
	}
	state.keyOptimizationEnabled = keyOptimizationSeen && keyOptimizationCompatible
	s.openaiPromptCacheBoostGroupAvailabilityCache.Store(cacheKey, state)
	return state
}

func (s *OpenAIGatewayService) GeneratePromptCacheBoostAffinitySessionHashForAccount(c *gin.Context, body []byte, model string, account *Account) string {
	if c == nil || len(body) == 0 || explicitOpenAISessionID(c, body) != "" {
		return ""
	}
	return deriveOpenAIPromptCacheBoostAffinityHashForAccount(account, model, body)
}

func (s *OpenAIGatewayService) openAIStickySessionTTLForHash(sessionHash string, fallback time.Duration) time.Duration {
	if IsOpenAIPromptCacheBoostAggressiveAffinitySessionHash(sessionHash) ||
		IsOpenAIPromptCacheBoostUpstreamAffinitySessionHash(sessionHash) ||
		IsOpenAIPromptCacheBoostOptimizedAffinitySessionHash(sessionHash) {
		return openAIPromptCacheBoostAffinityStickyTTL
	}
	if fallback > 0 {
		return fallback
	}
	return openaiStickySessionTTL
}

// GenerateSessionHash generates a sticky-session hash for OpenAI requests.
//
// Priority:
//  1. Header: session_id
//  2. Header: conversation_id
//  3. Body:   prompt_cache_key (opencode)
//  4. Body:   content-based fallback (model + system + tools + first user message)
func (s *OpenAIGatewayService) GenerateSessionHash(c *gin.Context, body []byte) string {
	if c == nil {
		return ""
	}

	sessionID := explicitOpenAISessionID(c, body)
	if sessionID == "" && len(body) > 0 {
		sessionID = deriveOpenAIContentSessionSeed(body)
	}
	if sessionID == "" {
		return ""
	}

	currentHash, legacyHash := deriveOpenAISessionHashes(sessionID)
	attachOpenAILegacySessionHashToGin(c, legacyHash)
	return currentHash
}

// GenerateSessionHashWithFallback 先按常规信号生成会话哈希；
// 当未携带 session_id/conversation_id/prompt_cache_key 时，使用 fallbackSeed 生成稳定哈希。
// 该方法用于 WS ingress，避免会话信号缺失时发生跨账号漂移。
func (s *OpenAIGatewayService) GenerateSessionHashWithFallback(c *gin.Context, body []byte, fallbackSeed string) string {
	sessionHash := s.GenerateSessionHash(c, body)
	if sessionHash != "" {
		return sessionHash
	}

	seed := strings.TrimSpace(fallbackSeed)
	if seed == "" {
		return ""
	}

	currentHash, legacyHash := deriveOpenAISessionHashes(seed)
	attachOpenAILegacySessionHashToGin(c, legacyHash)
	return currentHash
}

func resolveOpenAIUpstreamOriginator(c *gin.Context, isOfficialClient bool) string {
	if c != nil {
		if originator := strings.TrimSpace(c.GetHeader("originator")); originator != "" {
			return originator
		}
	}
	if isOfficialClient {
		return "codex_cli_rs"
	}
	return "opencode"
}

// BindStickySession sets session -> account binding with standard TTL.
func (s *OpenAIGatewayService) BindStickySession(ctx context.Context, groupID *int64, sessionHash string, accountID int64) error {
	if sessionHash == "" || accountID <= 0 {
		return nil
	}
	if IsOpenAIPromptCacheBoostAffinitySessionHash(sessionHash) {
		if !s.isOpenAIPromptCacheBoostAffinityAccountBindable(ctx, sessionHash, accountID) {
			return nil
		}
	}
	ttl := openaiStickySessionTTL
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds > 0 {
		ttl = time.Duration(s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds) * time.Second
	}
	return s.setStickySessionAccountID(ctx, groupID, sessionHash, accountID, s.openAIStickySessionTTLForHash(sessionHash, ttl))
}

// SelectAccount selects an OpenAI account with sticky session support
func (s *OpenAIGatewayService) SelectAccount(ctx context.Context, groupID *int64, sessionHash string) (*Account, error) {
	return s.SelectAccountForModel(ctx, groupID, sessionHash, "")
}

// SelectAccountForModel selects an account supporting the requested model
func (s *OpenAIGatewayService) SelectAccountForModel(ctx context.Context, groupID *int64, sessionHash string, requestedModel string) (*Account, error) {
	return s.SelectAccountForModelWithExclusions(ctx, groupID, sessionHash, requestedModel, nil)
}

// SelectAccountForModelWithExclusions selects an account supporting the requested model while excluding specified accounts.
// SelectAccountForModelWithExclusions 选择支持指定模型的账号，同时排除指定的账号。
func (s *OpenAIGatewayService) SelectAccountForModelWithExclusions(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}) (*Account, error) {
	return s.selectAccountForModelWithExclusions(s.withOpenAIQuotaAutoPauseContext(ctx), groupID, sessionHash, requestedModel, excludedIDs, false, 0, "", "", PlatformOpenAI)
}

// noAvailableOpenAISelectionError builds the standard "no account available" error
// while preserving the compact-specific error when applicable.
func noAvailableOpenAISelectionError(requestedModel string, compactBlocked bool) error {
	if compactBlocked {
		return ErrNoAvailableCompactAccounts
	}
	if requestedModel != "" {
		return fmt.Errorf("no available OpenAI accounts supporting model: %s", requestedModel)
	}
	return errors.New("no available OpenAI accounts")
}

// openAICompactSupportTier classifies an OpenAI account by compact capability.
// 0 = explicitly unsupported, 1 = unknown / not yet probed, 2 = explicitly supported.
func openAICompactSupportTier(account *Account) int {
	if account == nil || !account.IsOpenAI() {
		return 0
	}
	supported, known := account.OpenAICompactSupportKnown()
	if !known {
		return 1
	}
	if supported {
		return 2
	}
	return 0
}

// isOpenAIAccountEligibleForRequest centralises the schedulable / OpenAI / model /
// compact-support checks used during account selection.
func normalizeOpenAICompatibleRequestPlatform(platform string) string {
	switch strings.TrimSpace(platform) {
	case PlatformGrok:
		return PlatformGrok
	default:
		return PlatformOpenAI
	}
}

func isOpenAICompatibleRequestPlatformAccount(account *Account, requestPlatform string) bool {
	if account == nil {
		return false
	}
	return account.Platform == normalizeOpenAICompatibleRequestPlatform(requestPlatform)
}

func isOpenAIAccountEligibleForRequest(ctx context.Context, account *Account, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability, requiredImageCapability OpenAIImagesCapability, requestPlatform ...string) bool {
	platform := PlatformOpenAI
	if len(requestPlatform) > 0 {
		platform = requestPlatform[0]
	}
	if account == nil || !isOpenAICompatibleRequestPlatformAccount(account, platform) || !account.IsSchedulableForModelWithContext(ctx, requestedModel) {
		return false
	}
	if paused, reason := shouldAutoPauseOpenAIAccountByQuota(ctx, account); paused {
		// Debug level: this fires per-candidate on the scheduling hot path, so Info
		// would amplify into log spam once several accounts cross the threshold.
		slog.Debug("account_auto_paused_by_quota",
			"account_id", account.ID,
			"window", reason.window,
			"threshold", reason.threshold,
			"utilization", reason.utilization,
		)
		return false
	}
	if requestedModel != "" && !account.IsModelSupported(requestedModel) {
		return false
	}
	if !account.SupportsOpenAIEndpointCapability(requiredCapability) {
		return false
	}
	if !account.SupportsOpenAIImageCapability(requiredImageCapability) {
		return false
	}
	if !account.MatchesOpenAIImagePoolRequest(ctx, requestedModel, requiredImageCapability) {
		return false
	}
	if requireCompact && openAICompactSupportTier(account) == 0 {
		return false
	}
	return true
}

func (s *OpenAIGatewayService) parentAccountLookup(ctx context.Context) func(int64) *Account {
	return func(id int64) *Account {
		if s == nil || s.accountRepo == nil {
			return nil
		}
		account, _ := s.accountRepo.GetByID(ctx, id)
		return account
	}
}

type openAIQuotaAutoPauseDecision struct {
	window      string
	threshold   float64
	utilization float64
}

func shouldAutoPauseOpenAIAccountByQuota(ctx context.Context, account *Account) (bool, openAIQuotaAutoPauseDecision) {
	if account == nil || !account.IsOpenAI() {
		return false, openAIQuotaAutoPauseDecision{}
	}
	// Per-account explicit-disable flags must take precedence over the global default.
	// Without these, leaving the account threshold blank means "use global default",
	// so an admin has no way to exempt a single account from auto-pause once a global
	// default exists. The disable flag is per-window so an account can opt out of
	// only 5h or only 7d auto-pause.
	disabled5h := resolveAccountExtraBool(account.Extra, "auto_pause_5h_disabled")
	disabled7d := resolveAccountExtraBool(account.Extra, "auto_pause_7d_disabled")
	threshold5h, threshold7d := resolveOpenAIQuotaAutoPauseThresholds(ctx, account)
	now := time.Now()
	if !disabled5h && threshold5h > 0 {
		if utilization, ok := resolveOpenAIQuotaUtilization(account.Extra, "5h", now); ok && utilization >= threshold5h {
			return true, openAIQuotaAutoPauseDecision{window: "5h", threshold: threshold5h, utilization: utilization}
		}
	}
	if !disabled7d && threshold7d > 0 {
		if utilization, ok := resolveOpenAIQuotaUtilization(account.Extra, "7d", now); ok && utilization >= threshold7d {
			return true, openAIQuotaAutoPauseDecision{window: "7d", threshold: threshold7d, utilization: utilization}
		}
	}
	return false, openAIQuotaAutoPauseDecision{}
}

// resolveAccountExtraBool reads a bool-like value from account extra, tolerating
// the few shapes JSON unmarshalling may produce (real bool, "true"/"false"
// strings, 0/1 numbers).
func resolveAccountExtraBool(extra map[string]any, key string) bool {
	if len(extra) == 0 {
		return false
	}
	value, ok := extra[key]
	if !ok || value == nil {
		return false
	}
	switch v := value.(type) {
	case bool:
		return v
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(v))
		return err == nil && parsed
	case float64:
		return v != 0
	case float32:
		return v != 0
	case int:
		return v != 0
	case int64:
		return v != 0
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i != 0
		}
	}
	return false
}

func resolveOpenAIQuotaAutoPauseThresholds(ctx context.Context, account *Account) (float64, float64) {
	threshold5h, _ := resolveAccountExtraNumber(account.Extra, "auto_pause_5h_threshold")
	threshold7d, _ := resolveAccountExtraNumber(account.Extra, "auto_pause_7d_threshold")
	threshold5h = clamp01(threshold5h)
	threshold7d = clamp01(threshold7d)
	if threshold5h > 0 && threshold7d > 0 {
		return threshold5h, threshold7d
	}
	settings := openAIQuotaAutoPauseSettingsFromContext(ctx)
	if threshold5h <= 0 {
		threshold5h = clamp01(settings.DefaultThreshold5h)
	}
	if threshold7d <= 0 {
		threshold7d = clamp01(settings.DefaultThreshold7d)
	}
	return threshold5h, threshold7d
}

func resolveAccountExtraNumber(extra map[string]any, keys ...string) (float64, bool) {
	if len(extra) == 0 {
		return 0, false
	}
	for _, key := range keys {
		value, ok := extra[key]
		if !ok || value == nil {
			continue
		}
		switch v := value.(type) {
		case float64:
			return v, true
		case float32:
			return float64(v), true
		case int:
			return float64(v), true
		case int64:
			return float64(v), true
		case json.Number:
			parsed, err := v.Float64()
			if err == nil {
				return parsed, true
			}
		case string:
			parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

// resolveOpenAIQuotaUtilization returns the current utilization ratio (0..1) for the
// given Codex usage window. ok=false means there is no usable signal to pause on:
// either no snapshot exists, or the window has already rolled over so the cached
// percentage is stale. The stale guard matters because a paused account stops
// receiving requests, so its snapshot is never refreshed from upstream headers —
// without this check an old used_percent would keep the account paused forever even
// after the real window reset.
func resolveOpenAIQuotaUtilization(extra map[string]any, window string, now time.Time) (float64, bool) {
	usedPercent := readOpenAIQuotaUsedPercent(extra, window)
	if usedPercent <= 0 {
		return 0, false
	}
	if openAIQuotaWindowReset(extra, window, now) {
		return 0, false
	}
	return usedPercent / 100, true
}

// openAIQuotaWindowReset reports whether the Codex usage window's reset time has
// already passed relative to now. It prefers the absolute codex_<window>_reset_at
// timestamp and falls back to codex_<window>_reset_after_seconds anchored at
// codex_usage_updated_at, mirroring AccountUsageService's window-progress logic.
func openAIQuotaWindowReset(extra map[string]any, window string, now time.Time) bool {
	if len(extra) == 0 {
		return false
	}
	if resetAtRaw, ok := extra["codex_"+window+"_reset_at"]; ok {
		if resetAt, err := parseTime(fmt.Sprint(resetAtRaw)); err == nil {
			return !now.Before(resetAt)
		}
	}
	resetAfter := parseExtraInt(extra["codex_"+window+"_reset_after_seconds"])
	if resetAfter <= 0 {
		return false
	}
	base := now
	if updatedRaw, ok := extra["codex_usage_updated_at"]; ok {
		if updatedAt, err := parseTime(fmt.Sprint(updatedRaw)); err == nil {
			base = updatedAt
		}
	}
	resetAt := base.Add(time.Duration(resetAfter) * time.Second)
	return !now.Before(resetAt)
}

func readOpenAIQuotaUsedPercent(extra map[string]any, window string) float64 {
	if len(extra) == 0 {
		return 0
	}
	if value, ok := resolveAccountExtraNumber(extra, "codex_"+window+"_used_percent"); ok {
		return value
	}
	return 0
}

type openAIQuotaAutoPauseCtxKey struct{}

func withOpenAIQuotaAutoPauseSettings(ctx context.Context, settings OpsOpenAIAccountQuotaAutoPauseSettings) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openAIQuotaAutoPauseCtxKey{}, settings)
}

func openAIQuotaAutoPauseSettingsFromContext(ctx context.Context) OpsOpenAIAccountQuotaAutoPauseSettings {
	if ctx == nil {
		return OpsOpenAIAccountQuotaAutoPauseSettings{}
	}
	settings, _ := ctx.Value(openAIQuotaAutoPauseCtxKey{}).(OpsOpenAIAccountQuotaAutoPauseSettings)
	return settings
}

func (s *OpenAIGatewayService) withOpenAIQuotaAutoPauseContext(ctx context.Context) context.Context {
	if s == nil || s.settingService == nil {
		return ctx
	}
	return withOpenAIQuotaAutoPauseSettings(ctx, s.settingService.GetOpenAIQuotaAutoPauseSettings(ctx))
}

// prioritizeOpenAICompactAccounts re-orders a slice so that accounts with known
// compact support are tried first, followed by unknown, then explicitly unsupported.
// The relative order within each tier is preserved.
func prioritizeOpenAICompactAccounts(accounts []*Account) []*Account {
	if len(accounts) == 0 {
		return nil
	}
	supported := make([]*Account, 0, len(accounts))
	unknown := make([]*Account, 0, len(accounts))
	unsupported := make([]*Account, 0, len(accounts))
	for _, account := range accounts {
		switch openAICompactSupportTier(account) {
		case 2:
			supported = append(supported, account)
		case 1:
			unknown = append(unknown, account)
		default:
			unsupported = append(unsupported, account)
		}
	}
	out := make([]*Account, 0, len(accounts))
	out = append(out, supported...)
	out = append(out, unknown...)
	out = append(out, unsupported...)
	return out
}

func (s *OpenAIGatewayService) orderOpenAIPoolCoolingAccountsLast(accounts []*Account, requestedModel string) []*Account {
	if len(accounts) == 0 {
		return accounts
	}
	active := make([]*Account, 0, len(accounts))
	probeDue := make([]*Account, 0)
	for _, account := range accounts {
		if s.isOpenAIPoolAccountSoftCooling(account) {
			if s.isOpenAIPoolAccountSoftCooldownDue(account) {
				if s.clearOpenAIPoolSoftCooldownIfRecoveryProbeDisabled(context.Background(), account, requestedModel) {
					active = append(active, account)
					continue
				}
				probeDue = append(probeDue, account)
			}
			continue
		}
		active = append(active, account)
	}
	if len(active) == len(accounts) {
		return accounts
	}
	s.maybeStartOpenAIPoolRecoveryProbes(probeDue, requestedModel)
	return active
}

func (s *OpenAIGatewayService) orderOpenAIPoolCoolingLoadedAccountsLast(accounts []accountWithLoad, requestedModel string) []accountWithLoad {
	if len(accounts) == 0 {
		return accounts
	}
	active := make([]accountWithLoad, 0, len(accounts))
	probeDue := make([]*Account, 0)
	for _, item := range accounts {
		if s.isOpenAIPoolAccountSoftCooling(item.account) {
			if s.isOpenAIPoolAccountSoftCooldownDue(item.account) {
				if s.clearOpenAIPoolSoftCooldownIfRecoveryProbeDisabled(context.Background(), item.account, requestedModel) {
					active = append(active, item)
					continue
				}
				probeDue = append(probeDue, item.account)
			}
			continue
		}
		active = append(active, item)
	}
	if len(active) == len(accounts) {
		return accounts
	}
	s.maybeStartOpenAIPoolRecoveryProbes(probeDue, requestedModel)
	return active
}

func (s *OpenAIGatewayService) maybeStartOpenAIPoolRecoveryProbes(accounts []*Account, requestedModel string) {
	if s == nil || len(accounts) == 0 {
		return
	}
	for _, account := range accounts {
		s.maybeStartOpenAIPoolRecoveryProbe(context.Background(), account, requestedModel)
	}
}

func (s *OpenAIGatewayService) sortOpenAIPoolCooldownProbeAccounts(accounts []*Account) {
	sort.SliceStable(accounts, func(i, j int) bool {
		return s.lessOpenAIPoolCooldownProbeAccount(accounts[i], accounts[j])
	})
}

func (s *OpenAIGatewayService) sortOpenAIPoolCooldownProbeLoadedAccounts(accounts []accountWithLoad) {
	preferSoonestReset := s.schedulingConfig().PreferSoonestReset
	now := time.Time{}
	if preferSoonestReset {
		now = time.Now()
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		a, b := accounts[i], accounts[j]
		if a.account.Priority != b.account.Priority {
			return a.account.Priority < b.account.Priority
		}
		aUntil, aCooling := s.openAIPoolAccountSoftCooldownUntil(a.account)
		bUntil, bCooling := s.openAIPoolAccountSoftCooldownUntil(b.account)
		if aCooling != bCooling {
			return !aCooling
		}
		if aCooling && bCooling && !aUntil.Equal(bUntil) {
			return aUntil.Before(bUntil)
		}
		if a.loadInfo.LoadRate != b.loadInfo.LoadRate {
			return a.loadInfo.LoadRate < b.loadInfo.LoadRate
		}
		if a.loadInfo.WaitingCount != b.loadInfo.WaitingCount {
			return a.loadInfo.WaitingCount < b.loadInfo.WaitingCount
		}
		if preferSoonestReset {
			if less, ok := accountSoonestResetLess(a.account, b.account, now); ok {
				return less
			}
		}
		switch {
		case a.account.LastUsedAt == nil && b.account.LastUsedAt != nil:
			return true
		case a.account.LastUsedAt != nil && b.account.LastUsedAt == nil:
			return false
		case a.account.LastUsedAt != nil && b.account.LastUsedAt != nil:
			if !a.account.LastUsedAt.Equal(*b.account.LastUsedAt) {
				return a.account.LastUsedAt.Before(*b.account.LastUsedAt)
			}
		}
		return a.account.ID < b.account.ID
	})
}

func (s *OpenAIGatewayService) lessOpenAIPoolCooldownProbeAccount(a, b *Account) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	aUntil, aCooling := s.openAIPoolAccountSoftCooldownUntil(a)
	bUntil, bCooling := s.openAIPoolAccountSoftCooldownUntil(b)
	if aCooling != bCooling {
		return !aCooling
	}
	if aCooling && bCooling && !aUntil.Equal(bUntil) {
		return aUntil.Before(bUntil)
	}
	switch {
	case a.LastUsedAt == nil && b.LastUsedAt != nil:
		return true
	case a.LastUsedAt != nil && b.LastUsedAt == nil:
		return false
	case a.LastUsedAt != nil && b.LastUsedAt != nil:
		if !a.LastUsedAt.Equal(*b.LastUsedAt) {
			return a.LastUsedAt.Before(*b.LastUsedAt)
		}
	}
	return a.ID < b.ID
}

// resolveOpenAIAccountUpstreamModelForRequest resolves the upstream model that
// would be sent for a given request, honouring compact-only mappings when the
// caller is on the /responses/compact path.
func resolveOpenAIAccountUpstreamModelForRequest(account *Account, requestedModel string, requireCompact bool, compactFallback string) string {
	upstreamModel := resolveOpenAIForwardModel(account, requestedModel, "")
	if upstreamModel == "" {
		return ""
	}
	if requireCompact {
		return resolveOpenAICompactForwardModelWithFallback(account, upstreamModel, compactFallback)
	}
	return upstreamModel
}

func (s *OpenAIGatewayService) openAICompactModelFallback() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	return s.cfg.Gateway.OpenAICompactModel
}

func (s *OpenAIGatewayService) resolveOpenAICompactForwardModel(account *Account, model string) string {
	return resolveOpenAICompactForwardModelWithFallback(account, model, s.openAICompactModelFallback())
}

func (s *OpenAIGatewayService) selectAccountForModelWithExclusions(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, stickyAccountID int64, requiredCapability OpenAIEndpointCapability, requiredImageCapability OpenAIImagesCapability, requestPlatform string) (*Account, error) {
	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		slog.Warn("channel pricing restriction blocked request",
			"group_id", derefGroupID(groupID),
			"model", requestedModel)
		return nil, fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, requestedModel)
	}

	// 1. 尝试粘性会话命中
	// Try sticky session hit
	if account := s.tryStickySessionHit(ctx, groupID, sessionHash, requestedModel, excludedIDs, requireCompact, stickyAccountID, requiredCapability, requiredImageCapability, requestPlatform); account != nil {
		return account, nil
	}

	// 2. 获取可调度的 OpenAI 账号
	// Get schedulable OpenAI accounts
	accounts, err := s.listSchedulableAccountsForPlatform(ctx, groupID, requestPlatform)
	if err != nil {
		return nil, fmt.Errorf("query accounts failed: %w", err)
	}

	// 3. 按优先级 + LRU 选择最佳账号
	// Select by priority + LRU
	selected, compactBlocked := s.selectBestAccount(ctx, groupID, accounts, requestedModel, excludedIDs, requireCompact, requiredCapability, requiredImageCapability, requestPlatform)

	if selected == nil {
		return nil, noAvailableOpenAISelectionError(requestedModel, compactBlocked)
	}

	hydrated, err := s.hydrateSelectedAccount(ctx, selected)
	if err != nil {
		return nil, err
	}

	// 4. 设置粘性会话绑定
	// Set sticky session binding
	if sessionHash != "" {
		_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, selected.ID, s.openAIStickySessionTTLForHash(sessionHash, openaiStickySessionTTL))
	}

	return hydrated, nil
}

// tryStickySessionHit 尝试从粘性会话获取账号。
// 如果命中且账号可用则返回账号；如果账号不可用则清理会话并返回 nil。
//
// tryStickySessionHit attempts to get account from sticky session.
// Returns account if hit and usable; clears session and returns nil if account is unavailable.
func (s *OpenAIGatewayService) tryStickySessionHit(ctx context.Context, groupID *int64, sessionHash, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, stickyAccountID int64, requiredCapability OpenAIEndpointCapability, requiredImageCapability OpenAIImagesCapability, requestPlatform string) *Account {
	if sessionHash == "" && stickyAccountID <= 0 {
		return nil
	}
	isPromptCacheAffinity := IsOpenAIPromptCacheBoostAffinitySessionHash(sessionHash)

	accountID := stickyAccountID
	if accountID <= 0 {
		var err error
		accountID, err = s.getStickySessionAccountID(ctx, groupID, sessionHash)
		if err != nil || accountID <= 0 {
			return nil
		}
	}

	if _, excluded := excludedIDs[accountID]; excluded {
		return nil
	}

	account, err := s.getSchedulableAccount(ctx, accountID)
	if err != nil {
		return nil
	}

	// 检查账号是否需要清理粘性会话
	// Check if sticky session should be cleared
	if shouldClearStickySession(account, requestedModel) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}

	// 验证账号是否可用于当前请求
	// Verify account is usable for current request
	if !isOpenAIAccountEligibleForRequest(ctx, account, requestedModel, false, requiredCapability, requiredImageCapability, requestPlatform) {
		return nil
	}
	if s.isOpenAIAccountRuntimeBlocked(account) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}
	account = s.recheckSelectedOpenAIAccountFromDB(ctx, account, requestedModel, requireCompact, requiredCapability, requiredImageCapability, requestPlatform)
	if account == nil {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}
	if !s.latestOpenAIAccountMatchesGroup(ctx, account, groupID) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}
	if isPromptCacheAffinity && !s.isOpenAIPromptCacheBoostAffinityHashUsableForAccount(sessionHash, account) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}
	if groupID != nil && s.needsUpstreamChannelRestrictionCheck(ctx, groupID) &&
		s.isUpstreamModelRestrictedByChannel(ctx, *groupID, account, requestedModel, requireCompact) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}
	if s.hasHigherPriorityOpenAIAccountAvailable(ctx, groupID, account, requestedModel, requireCompact, requiredCapability, requiredImageCapability, OpenAIUpstreamTransportAny, requestPlatform) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}
	if s.hasSamePriorityNonPoolOpenAIAccountAvailable(ctx, groupID, account, requestedModel, requireCompact, requiredCapability, requiredImageCapability, OpenAIUpstreamTransportAny, requestPlatform) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}

	// 刷新会话 TTL 并返回账号
	// Refresh session TTL and return account
	_ = s.refreshStickySessionTTL(ctx, groupID, sessionHash, s.openAIStickySessionTTLForHash(sessionHash, openaiStickySessionTTL))
	return account
}

// selectBestAccount 从候选账号中选择最佳账号（优先级 + LRU）。
// 返回 nil 表示无可用账号。
//
// selectBestAccount selects the best account from candidates (priority + LRU).
// Returns nil if no available account. The second return reports whether at
// least one candidate was filtered out solely because it lacks compact support
// (only meaningful when requireCompact=true).
func (s *OpenAIGatewayService) selectBestAccount(ctx context.Context, groupID *int64, accounts []Account, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, requiredCapability OpenAIEndpointCapability, requiredImageCapability OpenAIImagesCapability, requestPlatform ...string) (*Account, bool) {
	var selected *Account
	selectedCompactTier := -1
	compactBlocked := false
	needsUpstreamCheck := s.needsUpstreamChannelRestrictionCheck(ctx, groupID)
	probeDue := make([]*Account, 0)
	rng := newOpenAISelectionRNG(nextOpenAIAccountBalanceSeed())
	selectedRankSeen := 0

	for i := range accounts {
		acc := &accounts[i]

		// 跳过被排除的账号
		// Skip excluded accounts
		if _, excluded := excludedIDs[acc.ID]; excluded {
			continue
		}

		fresh := s.resolveFreshSchedulableOpenAIAccount(ctx, acc, requestedModel, false, requiredCapability, requiredImageCapability, requestPlatform...)
		if fresh == nil {
			continue
		}
		fresh = s.recheckSelectedOpenAIAccountFromDB(ctx, fresh, requestedModel, false, requiredCapability, requiredImageCapability, requestPlatform...)
		if fresh == nil {
			continue
		}
		if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, fresh, requestedModel, requireCompact) {
			continue
		}
		compactTier := 0
		if requireCompact {
			compactTier = openAICompactSupportTier(fresh)
			if compactTier == 0 {
				compactBlocked = true
				continue
			}
		}
		if s.isOpenAIPoolAccountSoftCooling(fresh) {
			if s.isOpenAIPoolAccountSoftCooldownDue(fresh) && s.clearOpenAIPoolSoftCooldownIfRecoveryProbeDisabled(ctx, fresh, requestedModel) {
				// Probe disabled: the expired soft cooldown was cleared, so this account can participate now.
			} else {
				if s.isOpenAIPoolAccountSoftCooldownDue(fresh) {
					probeDue = append(probeDue, fresh)
				}
				continue
			}
		}

		// 选择优先级最高且最久未使用的账号
		// Select highest priority and least recently used
		if selected == nil {
			selected = fresh
			selectedCompactTier = compactTier
			selectedRankSeen = 1
			continue
		}

		// compact 模式下高 tier 优先；同 tier 内才比较 priority/LRU。
		if requireCompact && compactTier != selectedCompactTier {
			if compactTier > selectedCompactTier {
				selected = fresh
				selectedCompactTier = compactTier
				selectedRankSeen = 1
			}
			continue
		}

		if fresh.Priority < selected.Priority {
			selected = fresh
			selectedCompactTier = compactTier
			selectedRankSeen = 1
			continue
		}
		if fresh.Priority == selected.Priority {
			if s.isBetterAccount(fresh, selected) {
				selected = fresh
				selectedCompactTier = compactTier
				selectedRankSeen = 1
				continue
			}
			if sameOpenAIAccountLastUsedTie(fresh, selected) {
				selectedRankSeen++
				if rng.nextUint64()%uint64(selectedRankSeen) == 0 {
					selected = fresh
					selectedCompactTier = compactTier
				}
			}
		}
	}
	s.maybeStartOpenAIPoolRecoveryProbes(probeDue, requestedModel)

	return selected, compactBlocked
}

// isBetterAccount 判断 candidate 是否比 current 更优。
// 规则：优先级更高（数值更小）优先；同优先级时，未使用过的优先，其次是最久未使用的。
//
// isBetterAccount checks if candidate is better than current.
// Rules: higher priority (lower value) wins; same priority: never used > least recently used.
func (s *OpenAIGatewayService) isBetterAccount(candidate, current *Account) bool {
	// 优先级更高（数值更小）
	// Higher priority (lower value)
	if candidate.Priority < current.Priority {
		return true
	}
	if candidate.Priority > current.Priority {
		return false
	}

	if less, ok := nonPoolAccountBeforePool(candidate, current); ok {
		return less
	}

	// 同优先级，比较最后使用时间
	// Same priority, compare last used time
	switch {
	case candidate.LastUsedAt == nil && current.LastUsedAt != nil:
		// candidate 从未使用，优先
		return true
	case candidate.LastUsedAt != nil && current.LastUsedAt == nil:
		// current 从未使用，保持
		return false
	case candidate.LastUsedAt == nil && current.LastUsedAt == nil:
		// 都未使用，保持
		return false
	default:
		// 都使用过，选择最久未使用的
		return candidate.LastUsedAt.Before(*current.LastUsedAt)
	}
}

// SelectAccountWithLoadAwareness selects an account with load-awareness and wait plan.
func (s *OpenAIGatewayService) SelectAccountWithLoadAwareness(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}) (*AccountSelectionResult, error) {
	return s.selectAccountWithLoadAwareness(s.withOpenAIQuotaAutoPauseContext(ctx), groupID, sessionHash, requestedModel, excludedIDs, false, 0, "", "", PlatformOpenAI, -1)
}

func (s *OpenAIGatewayService) selectAccountWithLoadAwareness(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, stickyAccountID int64, requiredCapability OpenAIEndpointCapability, requiredImageCapability OpenAIImagesCapability, requestPlatform string, lockedPriority int) (*AccountSelectionResult, error) {
	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		slog.Warn("channel pricing restriction blocked request",
			"group_id", derefGroupID(groupID),
			"model", requestedModel)
		return nil, fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, requestedModel)
	}

	cfg := s.schedulingConfig()
	needsUpstreamCheck := s.needsUpstreamChannelRestrictionCheck(ctx, groupID)
	stickyBusyPreserve := false
	if stickyAccountID <= 0 && sessionHash != "" && s.cache != nil {
		if accountID, err := s.getStickySessionAccountID(ctx, groupID, sessionHash); err == nil {
			stickyAccountID = accountID
		}
	}
	if s.concurrencyService == nil || !cfg.LoadBatchEnabled {
		effectiveExcludedIDs := cloneExcludedAccountIDs(excludedIDs)
		var fallbackWaitAccount *Account
		for {
			selectionSessionHash := sessionHash
			if stickyBusyPreserve {
				selectionSessionHash = ""
			}
			account, err := s.selectAccountForModelWithExclusions(ctx, groupID, selectionSessionHash, requestedModel, effectiveExcludedIDs, requireCompact, stickyAccountID, requiredCapability, requiredImageCapability, requestPlatform)
			if err != nil {
				if fallbackWaitAccount != nil && errors.Is(err, ErrNoAvailableAccounts) {
					return s.newSelectionResult(ctx, fallbackWaitAccount, false, nil, &AccountWaitPlan{
						AccountID:      fallbackWaitAccount.ID,
						MaxConcurrency: fallbackWaitAccount.Concurrency,
						Timeout:        cfg.FallbackWaitTimeout,
						MaxWaiting:     cfg.FallbackMaxWaiting,
					})
				}
				return nil, err
			}
			if fallbackWaitAccount == nil {
				fallbackWaitAccount = account
			}
			result, err := s.tryAcquireAccountSlot(ctx, account.ID, account.Concurrency)
			if err == nil && result != nil && result.Acquired {
				return s.newAcquiredSelectionResult(ctx, account, result.ReleaseFunc)
			}
			if stickyAccountID > 0 && stickyAccountID == account.ID && s.concurrencyService != nil {
				stickyBusyPreserve = true
			}
			if s.concurrencyService == nil {
				return s.newSelectionResult(ctx, account, false, nil, &AccountWaitPlan{
					AccountID:      account.ID,
					MaxConcurrency: account.Concurrency,
					Timeout:        cfg.FallbackWaitTimeout,
					MaxWaiting:     cfg.FallbackMaxWaiting,
				})
			}
			if effectiveExcludedIDs == nil {
				effectiveExcludedIDs = make(map[int64]struct{})
			}
			effectiveExcludedIDs[account.ID] = struct{}{}
		}
	}

	accounts, err := s.listSchedulableAccountsForPlatform(ctx, groupID, requestPlatform)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, ErrNoAvailableAccounts
	}

	isExcluded := func(accountID int64) bool {
		if excludedIDs == nil {
			return false
		}
		_, excluded := excludedIDs[accountID]
		return excluded
	}

	// ============ Layer 1: Sticky session ============
	if sessionHash != "" || stickyAccountID > 0 {
		accountID := stickyAccountID
		if accountID > 0 && !isExcluded(accountID) {
			account, err := s.getSchedulableAccount(ctx, accountID)
			if err == nil {
				isPromptCacheAffinity := IsOpenAIPromptCacheBoostAffinitySessionHash(sessionHash)
				clearSticky := shouldClearStickySession(account, requestedModel)
				if clearSticky {
					if sessionHash != "" {
						_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
					}
				} else if isPromptCacheAffinity && !s.isOpenAIPromptCacheBoostAffinityHashUsableForAccount(sessionHash, account) {
					if sessionHash != "" {
						_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
					}
					clearSticky = true
				}
				if !clearSticky && openAIAccountMatchesLockedPriority(account, lockedPriority) &&
					isOpenAIAccountEligibleForRequest(ctx, account, requestedModel, false, requiredCapability, requiredImageCapability, requestPlatform) {
					account = s.recheckSelectedOpenAIAccountFromDB(ctx, account, requestedModel, requireCompact, requiredCapability, requiredImageCapability, requestPlatform)
					if account == nil {
						if sessionHash != "" {
							_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
						}
					} else if !s.latestOpenAIAccountMatchesGroup(ctx, account, groupID) {
						if sessionHash != "" {
							_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
						}
					} else if s.isOpenAIAccountRuntimeBlocked(account) {
						if sessionHash != "" {
							_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
						}
					} else if isPromptCacheAffinity && !s.isOpenAIPromptCacheBoostAffinityHashUsableForAccount(sessionHash, account) {
						if sessionHash != "" {
							_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
						}
					} else if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, account, requestedModel, requireCompact) {
						if sessionHash != "" {
							_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
						}
					} else if !parentHealthyForShadow(account, s.parentAccountLookup(ctx)) {
						if sessionHash != "" {
							_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
						}
					} else if s.hasHigherPriorityOpenAIAccountAvailable(ctx, groupID, account, requestedModel, requireCompact, requiredCapability, requiredImageCapability, OpenAIUpstreamTransportAny, requestPlatform) {
						if sessionHash != "" {
							_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
						}
					} else {
						result, err := s.tryAcquireAccountSlot(ctx, accountID, account.Concurrency)
						if err == nil && result != nil && result.Acquired {
							selection, selectErr := s.newAcquiredSelectionResult(ctx, account, result.ReleaseFunc)
							if selectErr != nil {
								return nil, selectErr
							}
							if sessionHash != "" {
								_ = s.refreshStickySessionTTL(ctx, groupID, sessionHash, s.openAIStickySessionTTLForHash(sessionHash, openaiStickySessionTTL))
							}
							return selection, nil
						}
						stickyBusyPreserve = true
					}
				}
			}
		}
	}

	// ============ Layer 2: Load-aware selection ============
	baseCandidateCount := 0
	candidates := make([]*Account, 0, len(accounts))
	parentCacheL2 := make(map[int64]*Account)
	parentLookupL2 := func(id int64) *Account {
		if account, ok := parentCacheL2[id]; ok {
			return account
		}
		account := s.parentAccountLookup(ctx)(id)
		parentCacheL2[id] = account
		return account
	}
	for i := range accounts {
		acc := &accounts[i]
		if isExcluded(acc.ID) {
			continue
		}
		if groupID != nil && accountHasGroupMetadata(acc) && !openAIStickyAccountMatchesGroup(acc, groupID) {
			continue
		}
		// Scheduler snapshots can be temporarily stale (bucket rebuild is throttled);
		// re-check schedulability here so recently rate-limited/overloaded accounts
		// are not selected again before the bucket is rebuilt.
		if !isOpenAIAccountEligibleForRequest(ctx, acc, requestedModel, false, requiredCapability, requiredImageCapability, requestPlatform) {
			continue
		}
		if !openAIAccountMatchesLockedPriority(acc, lockedPriority) {
			continue
		}
		if !parentHealthyForShadow(acc, parentLookupL2) {
			continue
		}
		if s.isOpenAIAccountRuntimeBlocked(acc) {
			continue
		}
		if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, acc, requestedModel, requireCompact) {
			continue
		}
		baseCandidateCount++
		candidates = append(candidates, acc)
	}

	if len(candidates) == 0 {
		return nil, ErrNoAvailableAccounts
	}

	accountLoads := make([]AccountWithConcurrency, 0, len(candidates))
	for _, acc := range candidates {
		accountLoads = append(accountLoads, AccountWithConcurrency{
			ID:             acc.ID,
			MaxConcurrency: acc.EffectiveLoadFactor(),
		})
	}

	tryAcquireFromLoadMap := func(loadMap map[int64]*AccountLoadInfo) (*AccountSelectionResult, bool, error) {
		var available []accountWithLoad
		for _, acc := range candidates {
			loadInfo := loadMap[acc.ID]
			loadInfoMissing := loadInfo == nil
			if loadInfo == nil {
				loadInfo = &AccountLoadInfo{AccountID: acc.ID}
			}
			if loadInfo.LoadRate < 100 {
				available = append(available, accountWithLoad{
					account:         acc,
					loadInfo:        loadInfo,
					loadInfoMissing: loadInfoMissing,
				})
			}
		}

		if len(available) == 0 {
			return nil, false, nil
		}

		preferSoonestReset := cfg.PreferSoonestReset
		now := time.Now()
		healthCandidates := s.openAIAccountWithLoadHealthCandidates(available, requestedModel)
		healthScores := buildOpenAIAccountCandidateHealthScores(healthCandidates)
		for i := range available {
			if available[i].account == nil {
				continue
			}
			available[i].healthScore = healthScores[available[i].account.ID]
			available[i].hasHealthScore = true
		}
		sort.SliceStable(available, func(i, j int) bool {
			a, b := available[i], available[j]
			if a.account.Priority != b.account.Priority {
				return a.account.Priority < b.account.Priority
			}
			if less, ok := nonPoolAccountBeforePool(a.account, b.account); ok {
				return less
			}
			if less, ok := openAIHealthScoreLess(healthScores[a.account.ID], healthScores[b.account.ID]); ok {
				return less
			}
			if preferSoonestReset {
				if less, ok := accountSoonestResetLess(a.account, b.account, now); ok {
					return less
				}
			}
			switch {
			case a.account.LastUsedAt == nil && b.account.LastUsedAt != nil:
				return true
			case a.account.LastUsedAt != nil && b.account.LastUsedAt == nil:
				return false
			case a.account.LastUsedAt == nil && b.account.LastUsedAt == nil:
				return false
			default:
				return a.account.LastUsedAt.Before(*b.account.LastUsedAt)
			}
		})
		shuffleOpenAIAccountLoadTiesWithReset(available, preferSoonestReset)
		prioritizeOpenAIPromptCacheUpstreamLoadTies(available, sessionHash, preferSoonestReset)
		available = s.orderOpenAIPoolCoolingLoadedAccountsLast(available, requestedModel)

		selectionOrder := make([]accountWithLoad, 0, len(available))
		if requireCompact {
			appendTier := func(out []accountWithLoad, tier int) []accountWithLoad {
				for _, item := range available {
					if openAICompactSupportTier(item.account) == tier {
						out = append(out, item)
					}
				}
				return out
			}
			selectionOrder = appendTier(selectionOrder, 2)
			selectionOrder = appendTier(selectionOrder, 1)
			// tier 0 候选作为兜底追加：DB recheck 时若发现 cache tier 0 实际
			// 已升级为 1/2（探测刚跑完，cache 尚未刷新），仍可正常命中。
			selectionOrder = appendTier(selectionOrder, 0)
		} else {
			selectionOrder = append(selectionOrder, available...)
		}
		selectionOrder = s.orderOpenAIPoolCoolingLoadedAccountsLast(selectionOrder, requestedModel)

		for _, item := range selectionOrder {
			fresh := s.resolveFreshSchedulableOpenAIAccount(ctx, item.account, requestedModel, false, requiredCapability, requiredImageCapability, requestPlatform)
			if fresh == nil {
				continue
			}
			fresh = s.recheckSelectedOpenAIAccountFromDB(ctx, fresh, requestedModel, requireCompact, requiredCapability, requiredImageCapability, requestPlatform)
			if fresh == nil {
				continue
			}
			if !openAIAccountMatchesLockedPriority(fresh, lockedPriority) {
				continue
			}
			if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, fresh, requestedModel, requireCompact) {
				continue
			}
			result, err := s.tryAcquireAccountSlot(ctx, fresh.ID, fresh.Concurrency)
			if err == nil && result != nil && result.Acquired {
				selection, selectErr := s.newAcquiredSelectionResult(ctx, fresh, result.ReleaseFunc)
				if selectErr != nil {
					return nil, true, selectErr
				}
				if sessionHash != "" && !stickyBusyPreserve {
					_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, fresh.ID, s.openAIStickySessionTTLForHash(sessionHash, openaiStickySessionTTL))
				}
				return selection, true, nil
			}
		}
		return nil, true, nil
	}

	loadMap, err := s.concurrencyService.GetAccountsLoadBatch(ctx, accountLoads)
	if err != nil {
		ordered := append([]*Account(nil), candidates...)
		sortAccountsByPriorityPoolAndLastUsed(ordered, false)
		ordered = s.orderOpenAIPoolCoolingAccountsLast(ordered, requestedModel)
		if requireCompact {
			ordered = prioritizeOpenAICompactAccounts(ordered)
			ordered = s.orderOpenAIPoolCoolingAccountsLast(ordered, requestedModel)
		}
		for _, acc := range ordered {
			fresh := s.resolveFreshSchedulableOpenAIAccount(ctx, acc, requestedModel, false, requiredCapability, requiredImageCapability, requestPlatform)
			if fresh == nil {
				continue
			}
			fresh = s.recheckSelectedOpenAIAccountFromDB(ctx, fresh, requestedModel, requireCompact, requiredCapability, requiredImageCapability, requestPlatform)
			if fresh == nil {
				continue
			}
			if !openAIAccountMatchesLockedPriority(fresh, lockedPriority) {
				continue
			}
			if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, fresh, requestedModel, requireCompact) {
				continue
			}
			result, err := s.tryAcquireAccountSlot(ctx, fresh.ID, fresh.Concurrency)
			if err == nil && result != nil && result.Acquired {
				selection, selectErr := s.newAcquiredSelectionResult(ctx, fresh, result.ReleaseFunc)
				if selectErr != nil {
					return nil, selectErr
				}
				if sessionHash != "" && !stickyBusyPreserve {
					_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, fresh.ID, s.openAIStickySessionTTLForHash(sessionHash, openaiStickySessionTTL))
				}
				return selection, nil
			}
		}
	} else {
		if selection, attempted, selectErr := tryAcquireFromLoadMap(loadMap); selectErr != nil {
			return nil, selectErr
		} else if selection != nil {
			return selection, nil
		} else if attempted {
			if freshLoadMap, loadErr := s.concurrencyService.GetAccountsLoadBatchFresh(ctx, accountLoads); loadErr == nil {
				if selection, _, selectErr := tryAcquireFromLoadMap(freshLoadMap); selectErr != nil {
					return nil, selectErr
				} else if selection != nil {
					return selection, nil
				}
			}
		}
	}

	// ============ Layer 3: Fallback wait ============
	candidates = s.orderOpenAIWaitCandidates(candidates, requestedModel, requireCompact, cfg)
	for _, acc := range candidates {
		fresh := s.resolveFreshSchedulableOpenAIAccount(ctx, acc, requestedModel, false, requiredCapability, requiredImageCapability, requestPlatform)
		if fresh == nil {
			continue
		}
		fresh = s.recheckSelectedOpenAIAccountFromDB(ctx, fresh, requestedModel, requireCompact, requiredCapability, requiredImageCapability, requestPlatform)
		if fresh == nil {
			continue
		}
		if !openAIAccountMatchesLockedPriority(fresh, lockedPriority) {
			continue
		}
		if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, fresh, requestedModel, requireCompact) {
			continue
		}
		return s.newSelectionResult(ctx, fresh, false, nil, &AccountWaitPlan{
			AccountID:      fresh.ID,
			MaxConcurrency: fresh.Concurrency,
			Timeout:        cfg.FallbackWaitTimeout,
			MaxWaiting:     cfg.FallbackMaxWaiting,
		})
	}

	if requireCompact && baseCandidateCount > 0 {
		return nil, ErrNoAvailableCompactAccounts
	}
	return nil, ErrNoAvailableAccounts
}

func (s *OpenAIGatewayService) listSchedulableAccounts(ctx context.Context, groupID *int64) ([]Account, error) {
	if s.schedulerSnapshot != nil {
		accounts, _, err := s.schedulerSnapshot.ListSchedulableAccounts(ctx, groupID, PlatformOpenAI, false)
		return accounts, err
	}
	var accounts []Account
	var err error
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		accounts, err = s.accountRepo.ListSchedulableByPlatform(ctx, PlatformOpenAI)
	} else if groupID != nil {
		accounts, err = s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, PlatformOpenAI)
	} else {
		accounts, err = s.accountRepo.ListSchedulableUngroupedByPlatform(ctx, PlatformOpenAI)
	}
	if err != nil {
		return nil, fmt.Errorf("query accounts failed: %w", err)
	}
	return accounts, nil
}

func (s *OpenAIGatewayService) orderOpenAIWaitCandidates(candidates []*Account, requestedModel string, requireCompact bool, cfg config.GatewaySchedulingConfig) []*Account {
	if len(candidates) <= 1 {
		return candidates
	}
	items := make([]accountWithLoad, 0, len(candidates))
	for _, account := range candidates {
		if account == nil {
			continue
		}
		items = append(items, accountWithLoad{
			account:  account,
			loadInfo: &AccountLoadInfo{AccountID: account.ID},
		})
	}
	if len(items) == 0 {
		return candidates
	}

	healthScores := s.openAIAccountWithLoadHealthScores(items, requestedModel)
	now := time.Time{}
	if cfg.PreferSoonestReset {
		now = time.Now()
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.account.Priority != b.account.Priority {
			return a.account.Priority < b.account.Priority
		}
		if less, ok := nonPoolAccountBeforePool(a.account, b.account); ok {
			return less
		}
		if less, ok := openAIHealthScoreLess(healthScores[a.account.ID], healthScores[b.account.ID]); ok {
			return less
		}
		if cfg.PreferSoonestReset {
			if less, ok := accountSoonestResetLess(a.account, b.account, now); ok {
				return less
			}
		}
		switch {
		case a.account.LastUsedAt == nil && b.account.LastUsedAt != nil:
			return true
		case a.account.LastUsedAt != nil && b.account.LastUsedAt == nil:
			return false
		case a.account.LastUsedAt != nil && b.account.LastUsedAt != nil:
			if !a.account.LastUsedAt.Equal(*b.account.LastUsedAt) {
				return a.account.LastUsedAt.Before(*b.account.LastUsedAt)
			}
		}
		return a.account.ID < b.account.ID
	})

	ordered := make([]*Account, 0, len(items))
	for _, item := range items {
		ordered = append(ordered, item.account)
	}
	if requireCompact {
		ordered = prioritizeOpenAICompactAccounts(ordered)
	}
	return s.orderOpenAIPoolCoolingAccountsLast(ordered, requestedModel)
}

func (s *OpenAIGatewayService) openAIAccountWithLoadHealthScores(accounts []accountWithLoad, requestedModel string) map[int64]float64 {
	return buildOpenAIAccountCandidateHealthScores(s.openAIAccountWithLoadHealthCandidates(accounts, requestedModel))
}

func (s *OpenAIGatewayService) openAIAccountWithLoadHealthCandidates(accounts []accountWithLoad, requestedModel string) []openAIAccountCandidateScore {
	if len(accounts) == 0 {
		return nil
	}
	candidates := make([]openAIAccountCandidateScore, 0, len(accounts))
	for _, item := range accounts {
		if item.account == nil {
			continue
		}
		errorRate, ttft, hasTTFT := 0.0, 0.0, false
		sampleCount, ttftSampleCount, lastUpdated := int64(0), int64(0), time.Time{}
		if s != nil {
			transport := s.getOpenAIWSProtocolResolver().Resolve(item.account).Transport
			if stats := s.getOpenAIAccountRuntimeStats(); stats != nil {
				errorRate, ttft, hasTTFT, sampleCount, ttftSampleCount, lastUpdated = stats.snapshotForRouteWithMeta(item.account.ID, requestedModel, transport)
			}
		}
		candidates = append(candidates, openAIAccountCandidateScore{
			account:         item.account,
			loadInfo:        item.loadInfo,
			loadInfoMissing: item.loadInfoMissing,
			errorRate:       errorRate,
			ttft:            ttft,
			hasTTFT:         hasTTFT,
			sampleCount:     sampleCount,
			ttftSampleCount: ttftSampleCount,
			lastUpdated:     lastUpdated,
		})
	}
	return candidates
}

func (s *OpenAIGatewayService) prioritizeOpenAIHealthProbeLoadedAccounts(accounts []accountWithLoad, healthByID map[int64]openAIAccountCandidateScore, now time.Time) []accountWithLoad {
	if len(accounts) <= 1 || s == nil {
		return accounts
	}
	stats := s.getOpenAIAccountRuntimeStats()
	if stats == nil {
		return accounts
	}
	candidates := make([]openAIAccountCandidateScore, 0, len(accounts))
	for _, item := range accounts {
		if item.account == nil {
			continue
		}
		candidate := healthByID[item.account.ID]
		candidate.account = item.account
		candidate.loadInfo = item.loadInfo
		candidate.loadInfoMissing = item.loadInfoMissing
		candidates = append(candidates, candidate)
	}
	prioritized := prioritizeOpenAIHealthProbeCandidate(candidates, stats, now)
	if len(prioritized) == 0 || len(prioritized) != len(candidates) || prioritized[0].account == nil || candidates[0].account == nil || prioritized[0].account.ID == candidates[0].account.ID {
		return accounts
	}
	selectedID := prioritized[0].account.ID
	selectedIdx := -1
	for i, item := range accounts {
		if item.account != nil && item.account.ID == selectedID {
			selectedIdx = i
			break
		}
	}
	if selectedIdx <= 0 {
		return accounts
	}
	ordered := append([]accountWithLoad(nil), accounts...)
	selected := ordered[selectedIdx]
	copy(ordered[1:selectedIdx+1], ordered[0:selectedIdx])
	ordered[0] = selected
	return ordered
}

func (s *OpenAIGatewayService) tryAcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int) (*AcquireResult, error) {
	if s.concurrencyService == nil {
		return &AcquireResult{Acquired: true, ReleaseFunc: func() {}}, nil
	}
	return s.concurrencyService.AcquireAccountSlot(ctx, accountID, maxConcurrency)
}

func (s *OpenAIGatewayService) resolveFreshSchedulableOpenAIAccount(ctx context.Context, account *Account, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability, requiredImageCapability OpenAIImagesCapability, requestPlatform ...string) *Account {
	if account == nil {
		return nil
	}

	fresh := account
	if s.schedulerSnapshot != nil {
		current, err := s.getSchedulableAccount(ctx, account.ID)
		if err != nil || current == nil {
			return nil
		}
		fresh = current
	}

	if !isOpenAIAccountEligibleForRequest(ctx, fresh, requestedModel, requireCompact, requiredCapability, requiredImageCapability, requestPlatform...) {
		return nil
	}
	if !parentHealthyForShadow(fresh, s.parentAccountLookup(ctx)) {
		return nil
	}
	if s.isOpenAIPoolAccountSoftCooling(fresh) {
		if s.isOpenAIPoolAccountSoftCooldownDue(fresh) {
			if s.clearOpenAIPoolSoftCooldownIfRecoveryProbeDisabled(ctx, fresh, requestedModel) {
				if s.isOpenAIAccountRuntimeBlocked(fresh) {
					return nil
				}
				return fresh
			}
			s.maybeStartOpenAIPoolRecoveryProbe(context.Background(), fresh, requestedModel)
		}
		return nil
	}
	if s.isOpenAIAccountRuntimeBlocked(fresh) {
		return nil
	}
	return fresh
}

func (s *OpenAIGatewayService) recheckSelectedOpenAIAccountFromDB(ctx context.Context, account *Account, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability, requiredImageCapability OpenAIImagesCapability, requestPlatform ...string) *Account {
	if account == nil {
		return nil
	}
	if s.schedulerSnapshot == nil {
		if !isOpenAIAccountEligibleForRequest(ctx, account, requestedModel, requireCompact, requiredCapability, requiredImageCapability, requestPlatform...) {
			return nil
		}
		if !parentHealthyForShadow(account, s.parentAccountLookup(ctx)) {
			return nil
		}
		if s.isOpenAIPoolAccountSoftCooling(account) {
			if s.isOpenAIPoolAccountSoftCooldownDue(account) {
				if s.clearOpenAIPoolSoftCooldownIfRecoveryProbeDisabled(ctx, account, requestedModel) {
					if s.isOpenAIAccountRuntimeBlocked(account) {
						return nil
					}
					return account
				}
				s.maybeStartOpenAIPoolRecoveryProbe(context.Background(), account, requestedModel)
			}
			return nil
		}
		if s.isOpenAIAccountRuntimeBlocked(account) {
			return nil
		}
		return account
	}

	latest, err := s.schedulerSnapshot.GetAccount(ctx, account.ID)
	if err != nil || latest == nil {
		if s.accountRepo == nil {
			latest = account
		} else {
			latest, err = s.accountRepo.GetByID(ctx, account.ID)
			if err != nil || latest == nil {
				return nil
			}
		}
	}
	if !isOpenAIAccountEligibleForRequest(ctx, latest, requestedModel, requireCompact, requiredCapability, requiredImageCapability, requestPlatform...) {
		return nil
	}
	if !parentHealthyForShadow(latest, s.parentAccountLookup(ctx)) {
		return nil
	}
	if s.isOpenAIPoolAccountSoftCooling(latest) {
		if s.isOpenAIPoolAccountSoftCooldownDue(latest) {
			if s.clearOpenAIPoolSoftCooldownIfRecoveryProbeDisabled(ctx, latest, requestedModel) {
				if s.isOpenAIAccountRuntimeBlocked(latest) {
					return nil
				}
				return latest
			}
			s.maybeStartOpenAIPoolRecoveryProbe(context.Background(), latest, requestedModel)
		}
		return nil
	}
	if s.isOpenAIAccountRuntimeBlocked(latest) {
		return nil
	}
	return latest
}

func (s *OpenAIGatewayService) getSchedulableAccount(ctx context.Context, accountID int64) (*Account, error) {
	var (
		account *Account
		err     error
	)
	if s.schedulerSnapshot != nil {
		account, err = s.schedulerSnapshot.GetAccount(ctx, accountID)
	} else {
		account, err = s.accountRepo.GetByID(ctx, accountID)
	}
	if err != nil || account == nil {
		return account, err
	}
	return account, nil
}

func (s *OpenAIGatewayService) hydrateSelectedAccount(ctx context.Context, account *Account) (*Account, error) {
	if account == nil || s.schedulerSnapshot == nil {
		return account, nil
	}
	hydrated, err := s.schedulerSnapshot.GetAccount(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	if hydrated == nil {
		return nil, fmt.Errorf("selected openai account %d not found during hydration", account.ID)
	}
	return hydrated, nil
}

func (s *OpenAIGatewayService) newSelectionResult(ctx context.Context, account *Account, acquired bool, release func(), waitPlan *AccountWaitPlan) (*AccountSelectionResult, error) {
	hydrated, err := s.hydrateSelectedAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	return &AccountSelectionResult{
		Account:     hydrated,
		Acquired:    acquired,
		ReleaseFunc: release,
		WaitPlan:    waitPlan,
	}, nil
}

func (s *OpenAIGatewayService) newAcquiredSelectionResult(ctx context.Context, account *Account, release func()) (*AccountSelectionResult, error) {
	selection, err := s.newSelectionResult(ctx, account, true, release, nil)
	if err != nil && release != nil {
		release()
	}
	return selection, err
}

func (s *OpenAIGatewayService) schedulingConfig() config.GatewaySchedulingConfig {
	if s.cfg != nil {
		return s.cfg.Gateway.Scheduling
	}
	return config.GatewaySchedulingConfig{
		StickySessionMaxWaiting:           3,
		StickySessionWaitTimeout:          45 * time.Second,
		FallbackWaitTimeout:               30 * time.Second,
		FallbackMaxWaiting:                100,
		LoadBatchEnabled:                  true,
		PreferSoonestReset:                false,
		CandidateSlotArbiterMaxCandidates: 16,
		SlotCleanupInterval:               30 * time.Second,
	}
}

func (s *OpenAIGatewayService) openAIAccountSlotArbiterEnabled() bool {
	cfg := s.schedulingConfig()
	return cfg.CandidateSlotArbiterEnabled && cfg.CandidateSlotArbiterMaxCandidates > 0
}

func (s *OpenAIGatewayService) openAIAccountSlotArbiterMaxCandidates() int {
	cfg := s.schedulingConfig()
	if cfg.CandidateSlotArbiterMaxCandidates <= 0 {
		return 0
	}
	return cfg.CandidateSlotArbiterMaxCandidates
}

func (s *OpenAIGatewayService) publishOpenAISchedulingRuntimeEvent(ctx context.Context, eventType SchedulerEventType, accountID int64, reason string) {
	if s == nil || s.schedulerSnapshot == nil || accountID <= 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	eventCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	s.schedulerSnapshot.publishEvent(eventCtx, SchedulerEvent{
		Type:      eventType,
		AccountID: accountID,
		Reason:    reason,
	})
}

// GetAccessToken gets the access token for an OpenAI account
func (s *OpenAIGatewayService) GetAccessToken(ctx context.Context, account *Account) (string, string, error) {
	if account != nil && account.IsShadow() {
		credentialAccount, err := resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil {
			return "", "", err
		}
		account = credentialAccount
	}
	if account != nil && account.Platform == PlatformGrok {
		if account.Type != AccountTypeOAuth {
			return "", "", fmt.Errorf("unsupported grok account type: %s", account.Type)
		}
		if s.grokTokenProvider != nil {
			accessToken, err := s.grokTokenProvider.GetAccessToken(ctx, account)
			if err != nil {
				return "", "", err
			}
			return accessToken, "oauth", nil
		}
		accessToken := account.GetGrokAccessToken()
		if accessToken == "" {
			return "", "", errors.New("access_token not found in credentials")
		}
		return accessToken, "oauth", nil
	}
	switch account.Type {
	case AccountTypeOAuth:
		// 使用 TokenProvider 获取缓存的 token
		if s.openAITokenProvider != nil {
			accessToken, err := s.openAITokenProvider.GetAccessToken(ctx, account)
			if err != nil {
				return "", "", err
			}
			return accessToken, "oauth", nil
		}
		// 降级：TokenProvider 未配置时直接从账号读取
		accessToken := account.GetOpenAIAccessToken()
		if accessToken == "" {
			return "", "", errors.New("access_token not found in credentials")
		}
		return accessToken, "oauth", nil
	case AccountTypeAPIKey:
		apiKey := account.GetOpenAIApiKey()
		if apiKey == "" {
			return "", "", errors.New("api_key not found in credentials")
		}
		return apiKey, "apikey", nil
	default:
		return "", "", fmt.Errorf("unsupported account type: %s", account.Type)
	}
}

func (s *OpenAIGatewayService) tryRecoverOpenAIOAuth401(ctx context.Context, c *gin.Context, account *Account, statusCode int, body []byte) (*Account, string, bool) {
	if s == nil || s.openAITokenProvider == nil || account == nil || c == nil {
		return nil, "", false
	}
	if statusCode != http.StatusUnauthorized || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return nil, "", false
	}
	if retried, _ := c.Get(openAIOAuth401RefreshRetryKey); retried == true {
		return nil, "", false
	}
	upstreamCode := strings.ToLower(strings.TrimSpace(extractUpstreamErrorCode(body)))
	if upstreamCode == "token_invalidated" || upstreamCode == "token_revoked" {
		return nil, "", false
	}

	c.Set(openAIOAuth401RefreshRetryKey, true)
	refreshedAccount, token, err := s.openAITokenProvider.ForceRefresh(ctx, account)
	if err != nil {
		slog.Warn("openai_oauth_401_force_refresh_failed",
			"account_id", account.ID,
			"error", err,
			"upstream_code", upstreamCode,
		)
		if isNonRetryableRefreshError(err) {
			s.markOpenAIOAuthReauthorizationRequired(ctx, s.openAIOAuthReauthorizationAccount(ctx, account), err)
		}
		return nil, "", false
	}
	if refreshedAccount == nil {
		refreshedAccount = account
	}
	credentialAccountID := refreshedAccount.ID
	if account.IsCredentialShadow() {
		refreshedAccount = account
	}
	slog.Info("openai_oauth_401_force_refresh_succeeded",
		"account_id", account.ID,
		"credential_account_id", credentialAccountID,
		"upstream_code", upstreamCode,
	)
	return refreshedAccount, token, true
}

func (s *OpenAIGatewayService) openAIOAuthReauthorizationAccount(ctx context.Context, account *Account) *Account {
	if s == nil || account == nil || !account.IsCredentialShadow() {
		return account
	}
	credentialAccount, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil || credentialAccount == nil {
		slog.Warn("openai_oauth_reauthorization_parent_resolve_failed",
			"account_id", account.ID,
			"error", err,
		)
		return account
	}
	return credentialAccount
}

func (s *OpenAIGatewayService) markOpenAIOAuthReauthorizationRequired(ctx context.Context, account *Account, err error) {
	if s == nil || s.accountRepo == nil || account == nil {
		return
	}
	if s.rateLimitService != nil {
		s.rateLimitService.notifyAccountSchedulingBlocked(account, time.Time{}, "openai_oauth_refresh_non_retryable")
	}
	msg := "OpenAI OAuth refresh failed permanently; reauthorization required"
	if err != nil {
		msg += ": " + sanitizeUpstreamErrorMessage(err.Error())
	}
	if setErr := s.accountRepo.SetError(ctx, account.ID, msg); setErr != nil {
		slog.Warn("openai_oauth_refresh_set_error_failed", "account_id", account.ID, "error", setErr)
		return
	}
	slog.Warn("openai_oauth_reauthorization_required", "account_id", account.ID, "error", msg)
}

func (s *OpenAIGatewayService) shouldFailoverUpstreamError(statusCode int) bool {
	switch statusCode {
	case 401, 402, 403, 429, 529:
		return true
	default:
		return statusCode >= 500
	}
}

func (s *OpenAIGatewayService) shouldFailoverOpenAIUpstreamResponse(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if isOpenAIContextWindowError(upstreamMsg, upstreamBody) {
		return false
	}
	if statusCode == http.StatusBadRequest {
		return isOpenAITransientProcessingError(statusCode, upstreamMsg, upstreamBody)
	}
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusPaymentRequired || statusCode == http.StatusForbidden ||
		statusCode == http.StatusTooManyRequests || statusCode == 529 || statusCode >= 500 {
		return true
	}
	return false
}

func (s *OpenAIGatewayService) IsOpenAIPoolDownstreamModelLimitProtectionEnabled(ctx context.Context) bool {
	return s == nil || s.settingService == nil || s.settingService.IsOpenAIPoolDownstreamModelLimitProtectionEnabled(ctx)
}

func (s *OpenAIGatewayService) classifyOpenAIPoolFailover(ctx context.Context, account *Account, statusCode int, upstreamMsg string, upstreamBody []byte) openAIPoolFailoverDecision {
	return classifyOpenAIPoolFailoverWithModelLimitProtection(
		account,
		statusCode,
		upstreamMsg,
		upstreamBody,
		s.IsOpenAIPoolDownstreamModelLimitProtectionEnabled(ctx),
	)
}

func (s *OpenAIGatewayService) shouldFailoverOpenAIAccountResponse(ctx context.Context, account *Account, statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if account != nil && account.IsOpenAI() && account.IsPoolMode() {
		return s.classifyOpenAIPoolFailover(ctx, account, statusCode, upstreamMsg, upstreamBody).Failover
	}
	return s.shouldFailoverOpenAIUpstreamResponse(statusCode, upstreamMsg, upstreamBody)
}

func (s *OpenAIGatewayService) handleFailoverSideEffects(ctx context.Context, resp *http.Response, account *Account, requestedModel ...string) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if len(requestedModel) > 0 {
		s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, requestedModel[0])
		return
	}
	s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body)
}

func (s *OpenAIGatewayService) tryAutoConsumeOpenAICodexResetCredit(ctx context.Context, account *Account, headers http.Header, responseBody []byte) bool {
	if s == nil || account == nil || !account.IsOpenAIOAuth() || account.Platform != PlatformOpenAI {
		return false
	}
	if account.IsShadow() {
		return false
	}
	if account.Extra == nil {
		return false
	}

	mode := openAICodexAutoResetModeFromExtra(account.Extra)
	if mode == openAICodexAutoResetModeOff {
		return false
	}

	if untilRaw, ok := s.openaiCodexAutoResetCooldownUntil.Load(account.ID); ok {
		if until, ok := untilRaw.(time.Time); ok && time.Now().Before(until) {
			return false
		}
	}
	if _, loaded := s.openaiCodexAutoResetInFlight.LoadOrStore(account.ID, struct{}{}); loaded {
		return false
	}
	defer s.openaiCodexAutoResetInFlight.Delete(account.ID)

	if !s.isOpenAICodexAutoResetEligible(mode, headers) {
		return false
	}

	payload, err := json.Marshal(map[string]string{"redeem_request_id": uuid.NewString()})
	if err != nil {
		s.openaiCodexAutoResetCooldownUntil.Store(account.ID, time.Now().Add(30*time.Second))
		return false
	}

	resp, err := s.doOpenAIWhamRequest(ctx, account, http.MethodPost, openAIWhamConsumeURL, bytes.NewReader(payload))
	if err != nil {
		s.openaiCodexAutoResetCooldownUntil.Store(account.ID, time.Now().Add(30*time.Second))
		slog.Warn("openai_codex_auto_reset_consume_failed", "account_id", account.ID, "error", err)
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.openaiCodexAutoResetCooldownUntil.Store(account.ID, time.Now().Add(30*time.Second))
		slog.Warn("openai_codex_auto_reset_consume_status_failed", "account_id", account.ID, "status", resp.StatusCode)
		return false
	}

	updates, err := s.fetchOpenAICodexResetCreditUpdates(ctx, account)
	if err != nil || len(updates) == 0 {
		updates = buildOpenAICodexResetCreditOptimisticUpdates(account, time.Now())
	}
	if len(updates) > 0 && s.accountRepo != nil {
		if updateErr := s.accountRepo.UpdateExtra(ctx, account.ID, updates); updateErr != nil {
			slog.Warn("openai_codex_auto_reset_update_extra_failed", "account_id", account.ID, "error", updateErr)
		}
	}
	mergeAccountExtra(account, updates)
	if s.rateLimitService != nil {
		if err := s.rateLimitService.ClearRateLimit(ctx, account.ID); err != nil {
			slog.Warn("openai_codex_auto_reset_clear_rate_limit_failed", "account_id", account.ID, "error", err)
		}
	}
	s.openaiCodexAutoResetCooldownUntil.Store(account.ID, time.Now().Add(time.Minute))
	_ = responseBody
	return true
}

func (s *OpenAIGatewayService) isOpenAICodexAutoResetEligible(mode string, headers http.Header) bool {
	if mode != openAICodexAutoResetModeShort && mode != openAICodexAutoResetModeLong {
		return false
	}
	used, ok := openAICodexUsedPercentFromHeaders(headers, mode)
	if !ok {
		return false
	}
	return used >= 100
}

func openAICodexUsedPercentFromHeaders(headers http.Header, mode string) (float64, bool) {
	if headers == nil {
		return 0, false
	}
	snapshot := ParseCodexRateLimitHeaders(headers)
	if snapshot == nil {
		return 0, false
	}
	normalized := snapshot.Normalize()
	if normalized == nil {
		return 0, false
	}
	switch mode {
	case openAICodexAutoResetModeShort:
		return openAICodexShortWindowUsedPercent(normalized)
	case openAICodexAutoResetModeLong:
		return openAICodexLongWindowUsedPercent(normalized)
	}
	return 0, false
}

func openAICodexShortWindowUsedPercent(normalized *NormalizedCodexLimits) (float64, bool) {
	if normalized == nil {
		return 0, false
	}
	if normalized.Window5hMinutes != nil && normalized.Window7dMinutes != nil {
		if *normalized.Window5hMinutes <= *normalized.Window7dMinutes {
			if normalized.Used5hPercent != nil {
				return *normalized.Used5hPercent, true
			}
		} else if normalized.Used7dPercent != nil {
			return *normalized.Used7dPercent, true
		}
	}
	if normalized.Used5hPercent != nil {
		return *normalized.Used5hPercent, true
	}
	return 0, false
}

func openAICodexLongWindowUsedPercent(normalized *NormalizedCodexLimits) (float64, bool) {
	if normalized == nil {
		return 0, false
	}
	if normalized.Window5hMinutes != nil && normalized.Window7dMinutes != nil {
		if *normalized.Window5hMinutes > *normalized.Window7dMinutes {
			if normalized.Used5hPercent != nil {
				return *normalized.Used5hPercent, true
			}
		} else if normalized.Used7dPercent != nil {
			return *normalized.Used7dPercent, true
		}
	}
	if normalized.Used7dPercent != nil {
		return *normalized.Used7dPercent, true
	}
	return 0, false
}

func (s *OpenAIGatewayService) fetchOpenAICodexResetCreditUpdates(ctx context.Context, account *Account) (map[string]any, error) {
	resp, err := s.doOpenAIWhamRequest(ctx, account, http.MethodGet, openAIWhamUsageURL, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, fmt.Errorf("read openai wham usage response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openai wham usage returned status %d", resp.StatusCode)
	}

	updates := make(map[string]any)
	if snapshot := ParseCodexRateLimitHeaders(resp.Header); snapshot != nil {
		if normalized := snapshot.Normalize(); normalized != nil {
			if normalized.Used5hPercent != nil {
				updates["codex_5h_used_percent"] = *normalized.Used5hPercent
			}
			if normalized.Reset5hSeconds != nil {
				updates["codex_5h_reset_after_seconds"] = *normalized.Reset5hSeconds
			}
			if normalized.Window5hMinutes != nil {
				updates["codex_5h_window_minutes"] = *normalized.Window5hMinutes
			}
			if normalized.Used7dPercent != nil {
				updates["codex_7d_used_percent"] = *normalized.Used7dPercent
			}
			if normalized.Reset7dSeconds != nil {
				updates["codex_7d_reset_after_seconds"] = *normalized.Reset7dSeconds
			}
			if normalized.Window7dMinutes != nil {
				updates["codex_7d_window_minutes"] = *normalized.Window7dMinutes
			}
		}
	}
	resetUpdates, supported, ok := extractOpenAICodexResetCreditUpdates(body, time.Now())
	if !ok {
		if len(updates) > 0 {
			return updates, nil
		}
		return nil, nil
	}
	for k, v := range resetUpdates {
		updates[k] = v
	}
	updates["codex_reset_credits_supported"] = supported
	return updates, nil
}

func (s *OpenAIGatewayService) doOpenAIWhamRequest(ctx context.Context, account *Account, method, url string, body io.Reader) (*http.Response, error) {
	if account == nil || !account.IsOpenAIOAuth() {
		return nil, fmt.Errorf("account does not support OpenAI Codex reset credits")
	}
	accessToken, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	chatgptAccountID, err := openAIWhamChatGPTAccountID(account)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("create openai wham request: %w", err)
	}
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Originator", openAIWhamOriginator)
	req.Header.Set("OAI-Language", "zh-CN")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-Mode", "no-cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Priority", "u=4, i")
	req.Header.Set("User-Agent", codexCLIUserAgent)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if customUA := account.GetOpenAIUserAgent(); customUA != "" {
		req.Header.Set("User-Agent", customUA)
	}
	req.Header.Set("chatgpt-account-id", chatgptAccountID)
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	client, err := httppool.GetClient(httppool.Options{
		ProxyURL:              proxyURL,
		Timeout:               15 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("build openai wham client: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai wham request failed: %w", err)
	}
	return resp, nil
}

// Forward forwards request to OpenAI API
func (s *OpenAIGatewayService) Forward(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	restrictionResult := s.detectCodexClientRestriction(c, account, body)
	apiKeyID := getAPIKeyIDFromContext(c)
	logCodexCLIOnlyDetection(ctx, c, account, apiKeyID, restrictionResult, body)
	if restrictionResult.Enabled && !restrictionResult.Matched {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"type":    "forbidden_error",
				"message": CodexClientRestrictionMessage(restrictionResult),
			},
		})
		return nil, errors.New("codex_cli_only restriction: only codex official clients are allowed")
	}

	originalBody := body
	reqModel, reqStream, promptCacheKey := extractOpenAIRequestMetaFromBody(body)
	originalModel := reqModel

	if account != nil && account.Platform == PlatformGrok {
		return s.forwardGrokResponses(ctx, c, account, body, originalModel, reqStream, startTime)
	}

	if account.Type == AccountTypeAPIKey && !openai_compat.ShouldUseResponsesAPI(account.Extra) {
		return s.forwardResponsesViaRawChatCompletions(ctx, c, account, body)
	}

	compatMessagesBridge := isOpenAICompatMessagesBridgeBody(body)
	setOpenAICompatMessagesBridgeContext(c, compatMessagesBridge)

	isCodexCLI := openai.IsCodexOfficialClientByHeaders(c.GetHeader("User-Agent"), c.GetHeader("originator")) || (s.cfg != nil && s.cfg.Gateway.ForceCodexCLI)
	codexImageGenerationExplicitToolPolicy := codexImageGenerationExplicitToolPolicyAllow
	if isCodexCLI && account != nil {
		codexImageGenerationExplicitToolPolicy = account.CodexImageGenerationExplicitToolPolicy()
	}
	wsDecision := s.getOpenAIWSProtocolResolver().Resolve(account)
	clientTransport := GetOpenAIClientTransport(c)
	// 仅允许 WS 入站请求走 WS 上游，避免出现 HTTP -> WS 协议混用。
	wsDecision = resolveOpenAIWSDecisionByClientTransport(wsDecision, clientTransport)
	if account.IsOpenAIUpstreamStrongIsolationEnabled() && wsDecision.Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		wsDecision = openAIWSHTTPDecision("upstream_strong_isolation")
	}
	if c != nil {
		c.Set("openai_ws_transport_decision", string(wsDecision.Transport))
		c.Set("openai_ws_transport_reason", wsDecision.Reason)
	}
	if wsDecision.Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		logOpenAIWSModeDebug(
			"selected account_id=%d account_type=%s transport=%s reason=%s model=%s stream=%v",
			account.ID,
			account.Type,
			normalizeOpenAIWSLogValue(string(wsDecision.Transport)),
			normalizeOpenAIWSLogValue(wsDecision.Reason),
			reqModel,
			reqStream,
		)
	}
	// 当前仅支持 WSv2；WSv1 命中时直接返回错误，避免出现“配置可开但行为不确定”。
	if wsDecision.Transport == OpenAIUpstreamTransportResponsesWebsocket {
		if c != nil {
			MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{
					"type":    "invalid_request_error",
					"message": "OpenAI WSv1 is temporarily unsupported. Please enable responses_websockets_v2.",
				},
			})
		}
		return nil, errors.New("openai ws v1 is temporarily unsupported; use ws v2")
	}
	passthroughEnabled := account.IsOpenAIPassthroughEnabled()
	if passthroughEnabled {
		if isCodexCLI && codexImageGenerationExplicitToolPolicy == codexImageGenerationExplicitToolPolicyStrip {
			strippedBody, changed, stripErr := stripOpenAIImageGenerationToolsFromRawPayload(body)
			if stripErr != nil {
				return nil, stripErr
			}
			if changed {
				body = strippedBody
				originalBody = strippedBody
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Stripped /responses image_generation tool for Codex client by account policy")
			}
		}
		// 透传分支只需要轻量提取字段，避免热路径全量 Unmarshal。
		reasoningEffort := extractOpenAIReasoningEffortFromBody(body, reqModel)
		return s.forwardOpenAIPassthrough(ctx, c, account, originalBody, reqModel, reasoningEffort, reqStream, startTime)
	}

	reqBody, err := getOpenAIRequestBodyMap(c, body)
	if err != nil {
		return nil, err
	}

	if v, ok := reqBody["model"].(string); ok {
		reqModel = v
		originalModel = reqModel
	}
	if v, ok := reqBody["stream"].(bool); ok {
		reqStream = v
	}
	if promptCacheKey == "" {
		if v, ok := reqBody["prompt_cache_key"].(string); ok {
			promptCacheKey = strings.TrimSpace(v)
		}
	}

	// Track if body needs re-serialization
	bodyModified := false
	// 单字段补丁快速路径：只要整个变更集最终可归约为同一路径的 set/delete，就避免全量 Marshal。
	patchDisabled := false
	patchHasOp := false
	patchDelete := false
	patchPath := ""
	var patchValue any
	markPatchSet := func(path string, value any) {
		if strings.TrimSpace(path) == "" {
			patchDisabled = true
			return
		}
		if patchDisabled {
			return
		}
		if !patchHasOp {
			patchHasOp = true
			patchDelete = false
			patchPath = path
			patchValue = value
			return
		}
		if patchDelete || patchPath != path {
			patchDisabled = true
			return
		}
		patchValue = value
	}
	markPatchDelete := func(path string) {
		if strings.TrimSpace(path) == "" {
			patchDisabled = true
			return
		}
		if patchDisabled {
			return
		}
		if !patchHasOp {
			patchHasOp = true
			patchDelete = true
			patchPath = path
			return
		}
		if !patchDelete || patchPath != path {
			patchDisabled = true
		}
	}
	disablePatch := func() {
		patchDisabled = true
	}

	apiKey := getAPIKeyFromContext(c)
	imageGenerationAllowed := GroupAllowsImageGeneration(nil)
	if apiKey != nil {
		imageGenerationAllowed = GroupAllowsImageGeneration(apiKey.Group)
	}
	codexImageGenerationBridgeEnabled := isCodexCLI &&
		imageGenerationAllowed &&
		codexImageGenerationExplicitToolPolicy != codexImageGenerationExplicitToolPolicyStrip &&
		s.isCodexImageGenerationBridgeEnabled(ctx, account, apiKey)
	if isCodexCLI && codexImageGenerationExplicitToolPolicy == codexImageGenerationExplicitToolPolicyStrip {
		if stripOpenAIImageGenerationTools(reqBody) {
			bodyModified = true
			disablePatch()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Stripped /responses image_generation tool for Codex client by account policy")
		}
	}
	if IsImageGenerationIntentMap(openAIResponsesEndpoint, reqModel, reqBody) && !imageGenerationAllowed {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"type":    "permission_error",
				"message": ImageGenerationPermissionMessage(),
			},
		})
		return nil, errors.New("image generation disabled for group")
	}

	// /responses/compact 不支持 image_generation bridge 注入的 tool_choice 等参数。
	isCompactRequest := isOpenAIResponsesCompactPath(c)

	if account.Type == AccountTypeAPIKey && normalizeOpenAIResponsesStringInputMap(reqBody) {
		bodyModified = true
		disablePatch()
	}

	// 非透传模式下，instructions 为空时注入默认指令。
	if isInstructionsEmpty(reqBody) && !compatMessagesBridge {
		reqBody["instructions"] = "You are a helpful coding assistant."
		bodyModified = true
		markPatchSet("instructions", "You are a helpful coding assistant.")
	}

	if !isCompactRequest && codexImageGenerationBridgeEnabled && ensureOpenAIResponsesImageGenerationTool(reqBody) {
		bodyModified = true
		disablePatch()
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Injected /responses image_generation tool for Codex client")
	}

	if normalizeOpenAIResponsesImageGenerationTools(reqBody) {
		bodyModified = true
		disablePatch()
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Normalized /responses image_generation tool payload")
	}
	if !isCompactRequest && codexImageGenerationBridgeEnabled && applyCodexImageGenerationBridgeInstructions(reqBody) {
		bodyModified = true
		disablePatch()
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Added Codex image_generation bridge instructions")
	}

	// 对所有请求执行模型映射（包含 Codex CLI）。
	billingModel := account.GetMappedModel(reqModel)
	if billingModel != reqModel {
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Model mapping applied: %s -> %s (account: %s, isCodexCLI: %v)", reqModel, billingModel, account.Name, isCodexCLI)
		reqBody["model"] = billingModel
		bodyModified = true
		markPatchSet("model", billingModel)
	}
	upstreamModel := billingModel
	if imageGenerationAllowed && normalizeOpenAIResponsesImageOnlyModel(reqBody) {
		bodyModified = true
		disablePatch()
		if model, ok := reqBody["model"].(string); ok {
			upstreamModel = strings.TrimSpace(model)
		}
		logger.LegacyPrintf(
			"service.openai_gateway",
			"[OpenAI] Normalized /responses image-only model request inbound_model=%s image_model=%s upstream_model=%s",
			reqModel,
			billingModel,
			upstreamModel,
		)
	}
	if err := validateOpenAIResponsesImageModel(reqBody, upstreamModel); err != nil {
		setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"type":    "invalid_request_error",
				"message": err.Error(),
				"param":   "model",
			},
		})
		return nil, err
	}
	if hasOpenAIImageGenerationTool(reqBody) {
		logger.LegacyPrintf(
			"service.openai_gateway",
			"[OpenAI] /responses image_generation request inbound_model=%s mapped_model=%s account_type=%s",
			reqModel,
			upstreamModel,
			account.Type,
		)
	}
	if err := validateCodexSparkInput(reqBody, upstreamModel); err != nil {
		setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"type":    "invalid_request_error",
				"message": err.Error(),
				"param":   "input",
			},
		})
		return nil, err
	}

	// Compact-only model 映射：仅在 /responses/compact 路径生效，且优先级高于
	// OAuth 模型规范化（避免 OAuth 规范化覆盖 compact-only 自定义模型）。
	compactMapped := false
	if isCompactRequest {
		compactMappedModel := s.resolveOpenAICompactForwardModel(account, billingModel)
		if compactMappedModel != "" && compactMappedModel != billingModel {
			compactMapped = true
			upstreamModel = compactMappedModel
			reqBody["model"] = compactMappedModel
			bodyModified = true
			markPatchSet("model", compactMappedModel)
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Compact model mapping applied: %s -> %s (account: %s, isCodexCLI: %v)", billingModel, compactMappedModel, account.Name, isCodexCLI)
		}
	}

	// OpenAI OAuth 账号走 ChatGPT internal Codex endpoint，需要将模型名规范化为
	// 上游可识别的 Codex/GPT 系列。API Key 账号则应保留原始/映射后的模型名，
	// 以兼容自定义 base_url 的 OpenAI-compatible 上游。
	if model, ok := reqBody["model"].(string); ok {
		if !compactMapped {
			upstreamModel = normalizeOpenAIModelForUpstream(account, model)
			if upstreamModel != "" && upstreamModel != model {
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Upstream model resolved: %s -> %s (account: %s, type: %s, isCodexCLI: %v)",
					model, upstreamModel, account.Name, account.Type, isCodexCLI)
				reqBody["model"] = upstreamModel
				bodyModified = true
				markPatchSet("model", upstreamModel)
			}
		}

		// 移除 gpt-5.2-codex 以下的版本 verbosity 参数
		// 确保高版本模型向低版本模型映射不报错
		if !SupportsVerbosity(upstreamModel) {
			if text, ok := reqBody["text"].(map[string]any); ok {
				delete(text, "verbosity")
			}
		}
	}

	// 规范化 reasoning.effort 参数（minimal -> none），与上游允许值对齐。
	if reasoning, ok := reqBody["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort == "minimal" {
			reasoning["effort"] = "none"
			bodyModified = true
			markPatchSet("reasoning.effort", "none")
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Normalized reasoning.effort: minimal -> none (account: %s)", account.Name)
		}
	}

	if account.Type == AccountTypeOAuth {
		codexResult := codexTransformResult{}
		if compatMessagesBridge {
			codexResult = applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{
				IsCodexCLI:              isCodexCLI,
				IsCompact:               isCompactRequest,
				SkipDefaultInstructions: true,
				PreserveToolCallIDs:     true,
			})
			ensureCodexOAuthInstructionsField(reqBody)
			bodyModified = true
			disablePatch()
		} else {
			codexResult = applyCodexOAuthTransform(reqBody, isCodexCLI, isCompactRequest)
		}
		if codexResult.Modified {
			bodyModified = true
			disablePatch()
		}
		if applyCodexClientMetadata(reqBody, account) {
			bodyModified = true
			disablePatch()
		}
		if codexResult.NormalizedModel != "" {
			upstreamModel = codexResult.NormalizedModel
		}
		if codexResult.PromptCacheKey != "" {
			promptCacheKey = codexResult.PromptCacheKey
		}
	}
	if isCompactRequest && account.Type == AccountTypeOAuth && normalizeOpenAICodexCompactReasoningEffortMap(reqBody, upstreamModel) {
		bodyModified = true
		markPatchSet("reasoning.effort", "xhigh")
	}

	promptCacheBoostKeyInjected := false
	promptCacheBoostRetentionInjected := false
	if s.isOpenAIPromptCacheBoostRuntimeEnabled(account) {
		if promptCacheKey == "" && s.isOpenAIPromptCacheBoostKeyRuntimeEnabled(account) {
			if generatedKey := deriveOpenAIVirtualPromptCacheKey(account, upstreamModel, body); generatedKey != "" {
				promptCacheKey = generatedKey
				if existing, ok := reqBody["prompt_cache_key"].(string); !ok || strings.TrimSpace(existing) == "" {
					reqBody["prompt_cache_key"] = generatedKey
					bodyModified = true
					promptCacheBoostKeyInjected = true
					disablePatch()
				}
			}
		}
		if s.isOpenAIPromptCacheBoostRetentionRuntimeEnabled(account) {
			if existing, ok := reqBody["prompt_cache_retention"].(string); !ok || strings.TrimSpace(existing) == "" {
				reqBody["prompt_cache_retention"] = "24h"
				bodyModified = true
				promptCacheBoostRetentionInjected = true
				disablePatch()
			}
		}
	}

	// Handle max_output_tokens based on platform and account type
	if !isCodexCLI {
		if maxOutputTokens, hasMaxOutputTokens := reqBody["max_output_tokens"]; hasMaxOutputTokens {
			switch account.Platform {
			case PlatformOpenAI:
				// For OpenAI API Key, remove max_output_tokens (not supported)
				// For OpenAI OAuth (Responses API), keep it (supported)
				if account.Type == AccountTypeAPIKey {
					delete(reqBody, "max_output_tokens")
					bodyModified = true
					markPatchDelete("max_output_tokens")
				}
			case PlatformAnthropic:
				// For Anthropic (Claude), convert to max_tokens
				delete(reqBody, "max_output_tokens")
				markPatchDelete("max_output_tokens")
				if _, hasMaxTokens := reqBody["max_tokens"]; !hasMaxTokens {
					reqBody["max_tokens"] = maxOutputTokens
					disablePatch()
				}
				bodyModified = true
			case PlatformGemini:
				// For Gemini, remove (will be handled by Gemini-specific transform)
				delete(reqBody, "max_output_tokens")
				bodyModified = true
				markPatchDelete("max_output_tokens")
			default:
				// For unknown platforms, remove to be safe
				delete(reqBody, "max_output_tokens")
				bodyModified = true
				markPatchDelete("max_output_tokens")
			}
		}

		// Also handle max_completion_tokens (similar logic)
		if _, hasMaxCompletionTokens := reqBody["max_completion_tokens"]; hasMaxCompletionTokens {
			if account.Type == AccountTypeAPIKey || account.Platform != PlatformOpenAI {
				delete(reqBody, "max_completion_tokens")
				bodyModified = true
				markPatchDelete("max_completion_tokens")
			}
		}

		// Remove unsupported fields (not supported by upstream OpenAI API)
		unsupportedFields := []string{"safety_identifier"}
		for _, unsupportedField := range unsupportedFields {
			if _, has := reqBody[unsupportedField]; has {
				delete(reqBody, unsupportedField)
				bodyModified = true
				markPatchDelete(unsupportedField)
			}
		}
		if !account.IsOpenAIPromptCacheBoostEnabled() {
			if _, has := reqBody["prompt_cache_retention"]; has {
				delete(reqBody, "prompt_cache_retention")
				bodyModified = true
				markPatchDelete("prompt_cache_retention")
			}
		}
	}

	if account.IsOpenAIUpstreamStrongIsolationEnabled() {
		if applyOpenAIUpstreamStrongIsolationMap(reqBody, true) {
			bodyModified = true
			disablePatch()
		}
	}

	// 仅在 WSv2 模式保留 previous_response_id，其他模式（HTTP/WSv1）统一过滤。
	// 注意：该规则同样适用于 Codex CLI 请求，避免 WSv1 向上游透传不支持字段。
	if wsDecision.Transport != OpenAIUpstreamTransportResponsesWebsocketV2 {
		if _, has := reqBody["previous_response_id"]; has {
			delete(reqBody, "previous_response_id")
			bodyModified = true
			markPatchDelete("previous_response_id")
		}
	}

	if sanitizeEmptyBase64InputImagesInOpenAIRequestBodyMap(reqBody) {
		bodyModified = true
		disablePatch()
	}

	// Apply OpenAI fast policy (参照 Claude BetaPolicy 的 fast-mode 过滤)：
	// 针对 body 的 service_tier 字段（"priority" 即 fast，"flex"），按策略
	// 执行 filter（删除字段）或 block（拒绝请求）。对 gpt-5.5 等模型屏蔽
	// fast 时在此生效。
	//
	// 注意：
	//   1. 此处统一使用 upstreamModel（已经过 GetMappedModel +
	//      normalizeOpenAIModelForUpstream + Codex OAuth normalize），与
	//      chat-completions / messages 入口保持一致，避免不同入口因为模型
	//      维度不同而出现 whitelist 命中差异。
	//   2. action=pass 时也要把 raw "fast" 归一化为 "priority" 写回 body，
	//      否则 native /responses 入口透传 "fast" 给上游会被拒。chat-
	//      completions 入口由 normalizeResponsesBodyServiceTier 完成同一
	//      行为，这里手工实现等效逻辑。
	if rawTier, ok := reqBody["service_tier"].(string); ok {
		if normTier := normalizedOpenAIServiceTierValue(rawTier); normTier != "" {
			action, errMsg := s.evaluateOpenAIFastPolicy(ctx, account, upstreamModel, normTier)
			switch action {
			case BetaPolicyActionBlock:
				msg := errMsg
				if msg == "" {
					msg = fmt.Sprintf("openai service_tier=%s is not allowed for model %s", normTier, upstreamModel)
				}
				blocked := &OpenAIFastBlockedError{Message: msg}
				writeOpenAIFastPolicyBlockedResponse(c, blocked)
				return nil, blocked
			case BetaPolicyActionFilter:
				delete(reqBody, "service_tier")
				bodyModified = true
				disablePatch()
			default:
				// pass：若客户端传的是别名 "fast"，归一化为 "priority"
				// 后写回 body，确保上游收到的是其能识别的规范值。
				if normTier != rawTier {
					reqBody["service_tier"] = normTier
					bodyModified = true
					markPatchSet("service_tier", normTier)
				}
			}
		}
	}

	if IsImageGenerationIntentMap(openAIResponsesEndpoint, reqModel, reqBody) && !imageGenerationAllowed {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"type":    "permission_error",
				"message": ImageGenerationPermissionMessage(),
			},
		})
		return nil, errors.New("image generation disabled for group")
	}
	imageBillingModel := ""
	imageSizeTier := ""
	imageInputSize := ""
	if IsImageGenerationIntentMap(openAIResponsesEndpoint, reqModel, reqBody) {
		var imageCfgErr error
		imageCfg, imageCfgErr := resolveOpenAIResponsesImageBillingConfigDetailed(reqBody, billingModel)
		if imageCfgErr != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, imageCfgErr.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{
					"type":    "invalid_request_error",
					"message": imageCfgErr.Error(),
					"param":   "size",
				},
			})
			return nil, imageCfgErr
		}
		imageBillingModel = imageCfg.Model
		imageSizeTier = imageCfg.SizeTier
		imageInputSize = imageCfg.InputSize
	}

	// Re-serialize body only if modified
	if bodyModified {
		serializedByPatch := false
		if !patchDisabled && patchHasOp {
			var patchErr error
			if patchDelete {
				body, patchErr = sjson.DeleteBytes(body, patchPath)
			} else {
				body, patchErr = sjson.SetBytes(body, patchPath, patchValue)
			}
			if patchErr == nil {
				serializedByPatch = true
			}
		}
		if !serializedByPatch {
			var marshalErr error
			body, marshalErr = json.Marshal(reqBody)
			if marshalErr != nil {
				return nil, fmt.Errorf("serialize request body: %w", marshalErr)
			}
		}
	}

	// Get access token
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	// 命中 WS 时仅走 WebSocket Mode；不再自动回退 HTTP。
	if wsDecision.Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		wsReqBody := reqBody
		if len(reqBody) > 0 {
			wsReqBody = make(map[string]any, len(reqBody))
			for k, v := range reqBody {
				wsReqBody[k] = v
			}
		}
		_, hasPreviousResponseID := wsReqBody["previous_response_id"]
		logOpenAIWSModeDebug(
			"forward_start account_id=%d account_type=%s model=%s stream=%v has_previous_response_id=%v",
			account.ID,
			account.Type,
			upstreamModel,
			reqStream,
			hasPreviousResponseID,
		)
		maxAttempts := openAIWSReconnectRetryLimit + 1
		wsAttempts := 0
		var wsResult *OpenAIForwardResult
		var wsErr error
		wsLastFailureReason := ""
		wsPrevResponseRecoveryTried := false
		wsInvalidEncryptedContentRecoveryTried := false
		recoverPrevResponseNotFound := func(attempt int) bool {
			if wsPrevResponseRecoveryTried {
				return false
			}
			previousResponseID := openAIWSPayloadString(wsReqBody, "previous_response_id")
			if previousResponseID == "" {
				logOpenAIWSModeInfo(
					"reconnect_prev_response_recovery_skip account_id=%d attempt=%d reason=missing_previous_response_id previous_response_id_present=false",
					account.ID,
					attempt,
				)
				return false
			}
			if HasFunctionCallOutput(wsReqBody) {
				logOpenAIWSModeInfo(
					"reconnect_prev_response_recovery_skip account_id=%d attempt=%d reason=has_function_call_output previous_response_id_present=true",
					account.ID,
					attempt,
				)
				return false
			}
			delete(wsReqBody, "previous_response_id")
			wsPrevResponseRecoveryTried = true
			logOpenAIWSModeInfo(
				"reconnect_prev_response_recovery account_id=%d attempt=%d action=drop_previous_response_id retry=1 previous_response_id=%s previous_response_id_kind=%s",
				account.ID,
				attempt,
				truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen),
				normalizeOpenAIWSLogValue(ClassifyOpenAIPreviousResponseIDKind(previousResponseID)),
			)
			return true
		}
		recoverInvalidEncryptedContent := func(attempt int) bool {
			if wsInvalidEncryptedContentRecoveryTried {
				return false
			}
			removedReasoningItems := trimOpenAIEncryptedReasoningItems(wsReqBody)
			if !removedReasoningItems {
				logOpenAIWSModeInfo(
					"reconnect_invalid_encrypted_content_recovery_skip account_id=%d attempt=%d reason=missing_encrypted_reasoning_items",
					account.ID,
					attempt,
				)
				return false
			}
			previousResponseID := openAIWSPayloadString(wsReqBody, "previous_response_id")
			hasFunctionCallOutput := HasFunctionCallOutput(wsReqBody)
			if previousResponseID != "" && !hasFunctionCallOutput {
				delete(wsReqBody, "previous_response_id")
			}
			wsInvalidEncryptedContentRecoveryTried = true
			logOpenAIWSModeInfo(
				"reconnect_invalid_encrypted_content_recovery account_id=%d attempt=%d action=drop_encrypted_reasoning_items retry=1 previous_response_id_present=%v previous_response_id=%s previous_response_id_kind=%s has_function_call_output=%v dropped_previous_response_id=%v",
				account.ID,
				attempt,
				previousResponseID != "",
				truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen),
				normalizeOpenAIWSLogValue(ClassifyOpenAIPreviousResponseIDKind(previousResponseID)),
				hasFunctionCallOutput,
				previousResponseID != "" && !hasFunctionCallOutput,
			)
			return true
		}
		retryBudget := s.openAIWSRetryTotalBudget()
		retryStartedAt := time.Now()
	wsRetryLoop:
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			wsAttempts = attempt
			wsResult, wsErr = s.forwardOpenAIWSV2(
				ctx,
				c,
				account,
				wsReqBody,
				token,
				wsDecision,
				isCodexCLI,
				reqStream,
				originalModel,
				upstreamModel,
				startTime,
				attempt,
				wsLastFailureReason,
			)
			if wsErr == nil {
				break
			}
			if c != nil && c.Writer != nil && c.Writer.Written() {
				break
			}

			reason, retryable := classifyOpenAIWSReconnectReason(wsErr)
			if reason != "" {
				wsLastFailureReason = reason
			}
			// previous_response_not_found 说明续链锚点不可用：
			// 对非 function_call_output 场景，允许一次“去掉 previous_response_id 后重放”。
			if reason == "previous_response_not_found" && recoverPrevResponseNotFound(attempt) {
				continue
			}
			if reason == "invalid_encrypted_content" && recoverInvalidEncryptedContent(attempt) {
				continue
			}
			if retryable && attempt < maxAttempts {
				backoff := s.openAIWSRetryBackoff(attempt)
				if retryBudget > 0 && time.Since(retryStartedAt)+backoff > retryBudget {
					s.recordOpenAIWSRetryExhausted()
					logOpenAIWSModeInfo(
						"reconnect_budget_exhausted account_id=%d attempts=%d max_retries=%d reason=%s elapsed_ms=%d budget_ms=%d",
						account.ID,
						attempt,
						openAIWSReconnectRetryLimit,
						normalizeOpenAIWSLogValue(reason),
						time.Since(retryStartedAt).Milliseconds(),
						retryBudget.Milliseconds(),
					)
					break
				}
				s.recordOpenAIWSRetryAttempt(backoff)
				logOpenAIWSModeInfo(
					"reconnect_retry account_id=%d retry=%d max_retries=%d reason=%s backoff_ms=%d",
					account.ID,
					attempt,
					openAIWSReconnectRetryLimit,
					normalizeOpenAIWSLogValue(reason),
					backoff.Milliseconds(),
				)
				if backoff > 0 {
					timer := time.NewTimer(backoff)
					select {
					case <-ctx.Done():
						if !timer.Stop() {
							<-timer.C
						}
						wsErr = wrapOpenAIWSFallback("retry_backoff_canceled", ctx.Err())
						break wsRetryLoop
					case <-timer.C:
					}
				}
				continue
			}
			if retryable {
				s.recordOpenAIWSRetryExhausted()
				logOpenAIWSModeInfo(
					"reconnect_exhausted account_id=%d attempts=%d max_retries=%d reason=%s",
					account.ID,
					attempt,
					openAIWSReconnectRetryLimit,
					normalizeOpenAIWSLogValue(reason),
				)
			} else if reason != "" {
				s.recordOpenAIWSNonRetryableFastFallback()
				logOpenAIWSModeInfo(
					"reconnect_stop account_id=%d attempt=%d reason=%s",
					account.ID,
					attempt,
					normalizeOpenAIWSLogValue(reason),
				)
			}
			break
		}
		if wsErr == nil {
			firstTokenMs := int64(0)
			hasFirstTokenMs := wsResult != nil && wsResult.FirstTokenMs != nil
			if hasFirstTokenMs {
				firstTokenMs = int64(*wsResult.FirstTokenMs)
			}
			requestID := ""
			if wsResult != nil {
				requestID = strings.TrimSpace(wsResult.RequestID)
			}
			logOpenAIWSModeDebug(
				"forward_succeeded account_id=%d request_id=%s stream=%v has_first_token_ms=%v first_token_ms=%d ws_attempts=%d",
				account.ID,
				requestID,
				reqStream,
				hasFirstTokenMs,
				firstTokenMs,
				wsAttempts,
			)
			wsResult.UpstreamModel = upstreamModel
			if wsResult.BillingModel == "" {
				wsResult.BillingModel = billingModel
			}
			if wsResult.ImageCount > 0 {
				wsResult.ImageSize = imageSizeTier
				wsResult.ImageInputSize = imageInputSize
				wsResult.BillingModel = imageBillingModel
			}
			return wsResult, nil
		}
		s.writeOpenAIWSFallbackErrorResponse(c, account, wsErr)
		return nil, wsErr
	}

	httpInvalidEncryptedContentRetryTried := false
	httpPromptCacheBoostRetryTried := false
	httpCodexAutoResetRetryTried := false
	for {
		// Build upstream request
		upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
		upstreamReq, err := s.buildUpstreamRequest(upstreamCtx, c, account, body, token, reqStream, promptCacheKey, isCodexCLI)
		releaseUpstreamCtx()
		if err != nil {
			return nil, err
		}

		// Get proxy URL
		proxyURL := ""
		if account.ProxyID != nil && account.Proxy != nil {
			proxyURL = account.Proxy.URL()
		}

		// Send request
		var requestFirstTokenPlaceholder openAIRequestFirstTokenPlaceholderState
		var upstreamElapsed time.Duration
		var resp *http.Response
		doUpstream := func() (*http.Response, error) {
			return s.httpUpstream.DoWithTLS(upstreamReq, proxyURL, account.ID, account.Concurrency, s.resolveTLSProfile(account))
		}
		if reqStream {
			resp, requestFirstTokenPlaceholder, upstreamElapsed, err = s.doOpenAIUpstreamWithFirstTokenTimeoutPlaceholder(
				c,
				account,
				originalModel,
				startTime,
				openAIRequestFirstTokenPlaceholderDialectResponses,
				doUpstream,
			)
		} else {
			upstreamStart := time.Now()
			resp, err = doUpstream()
			upstreamElapsed = time.Since(upstreamStart)
		}
		SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, upstreamElapsed.Milliseconds())
		if err != nil {
			if requestFirstTokenPlaceholder.Sent {
				_ = s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
				s.RecordOpenAIPoolFailureAfterCommittedResponse(ctx, account, http.StatusBadGateway, openAITransportFailoverBody, upstreamModel, err.Error())
				writeOpenAIRequestPlaceholderErrorSSE(c, openAIRequestFirstTokenPlaceholderDialectResponses, originalModel, "upstream_error", "Upstream request failed")
				return &OpenAIForwardResult{
					Usage:         OpenAIUsage{},
					Model:         originalModel,
					UpstreamModel: upstreamModel,
					Stream:        true,
					OpenAIWSMode:  false,
					Duration:      time.Since(startTime),
				}, fmt.Errorf("upstream request failed after first token placeholder: %w", err)
			}
			if failoverErr := s.newOpenAIPoolRequestFailoverError(c, account, upstreamReq, err, false); failoverErr != nil {
				return nil, failoverErr
			}
			return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
		}

		// Handle error response
		if resp.StatusCode >= 400 {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(respBody))

			upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
			upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
			upstreamCode := extractUpstreamErrorCode(respBody)
			if requestFirstTokenPlaceholder.Sent {
				s.RecordOpenAIPromptCacheBoostUnsupportedAfterCommittedResponse(account, resp.StatusCode, upstreamMsg, respBody, promptCacheBoostKeyInjected, promptCacheBoostRetentionInjected)
				if resp.StatusCode == http.StatusTooManyRequests {
					_ = s.tryAutoConsumeOpenAICodexResetCredit(ctx, account, resp.Header, respBody)
				}
				setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, "")
				_ = s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody, upstreamModel)
				s.RecordOpenAIPoolFailureAfterCommittedResponse(ctx, account, resp.StatusCode, respBody, upstreamModel, upstreamMsg)
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamRequestID:  resp.Header.Get("x-request-id"),
					Kind:               "http_error",
					Message:            upstreamMsg,
				})
				writeOpenAIRequestPlaceholderErrorSSE(c, openAIRequestFirstTokenPlaceholderDialectResponses, originalModel, "upstream_error", firstNonEmptyString(upstreamMsg, "Upstream request failed"))
				return &OpenAIForwardResult{
					RequestID:     resp.Header.Get("x-request-id"),
					Usage:         OpenAIUsage{},
					Model:         originalModel,
					UpstreamModel: upstreamModel,
					Stream:        true,
					OpenAIWSMode:  false,
					Duration:      time.Since(startTime),
				}, fmt.Errorf("upstream error after first token placeholder: %d message=%s", resp.StatusCode, upstreamMsg)
			}
			if refreshedAccount, refreshedToken, ok := s.tryRecoverOpenAIOAuth401(ctx, c, account, resp.StatusCode, respBody); ok {
				account = refreshedAccount
				token = refreshedToken
				continue
			}
			if !httpPromptCacheBoostRetryTried && (promptCacheBoostKeyInjected || promptCacheBoostRetentionInjected) && isOpenAIPromptCacheBoostUnsupportedError(resp.StatusCode, upstreamMsg, respBody) {
				keyUnsupported, retentionUnsupported := openAIPromptCacheBoostUnsupportedFields(resp.StatusCode, upstreamMsg, respBody)
				stripKey := promptCacheBoostKeyInjected && keyUnsupported
				stripRetention := promptCacheBoostRetentionInjected && retentionUnsupported
				if retryBody, changed := stripOpenAIPromptCacheBoostFields(body, stripKey, stripRetention); changed {
					body = retryBody
					stripOpenAIPromptCacheBoostFieldsMap(reqBody, stripKey, stripRetention)
					if stripKey {
						promptCacheKey = ""
						promptCacheBoostKeyInjected = false
					}
					if stripRetention {
						promptCacheBoostRetentionInjected = false
					}
					s.temporarilyDisableOpenAIPromptCacheBoost(account, stripKey, stripRetention)
					releaseOpenAIParsedRequestBody(c)
					httpPromptCacheBoostRetryTried = true
					logger.L().Info("openai responses: prompt cache boost unsupported, retrying without unsupported fields",
						zap.Int64("account_id", account.ID),
						zap.Bool("strip_prompt_cache_key", stripKey),
						zap.Bool("strip_prompt_cache_retention", stripRetention),
						zap.Int("upstream_status", resp.StatusCode),
						zap.String("upstream_message", upstreamMsg),
					)
					continue
				}
			}
			if !httpInvalidEncryptedContentRetryTried && resp.StatusCode == http.StatusBadRequest && upstreamCode == "invalid_encrypted_content" {
				if trimOpenAIEncryptedReasoningItems(reqBody) {
					body, err = json.Marshal(reqBody)
					if err != nil {
						return nil, fmt.Errorf("serialize invalid_encrypted_content retry body: %w", err)
					}
					httpInvalidEncryptedContentRetryTried = true
					logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Retrying non-WSv2 request once after invalid_encrypted_content (account: %s)", account.Name)
					continue
				}
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Skip non-WSv2 invalid_encrypted_content retry because encrypted reasoning items are missing (account: %s)", account.Name)
			}
			if !httpCodexAutoResetRetryTried && resp.StatusCode == http.StatusTooManyRequests {
				if s.tryAutoConsumeOpenAICodexResetCredit(ctx, account, resp.Header, respBody) {
					httpCodexAutoResetRetryTried = true
					continue
				}
			}
			if s.shouldFailoverOpenAIAccountResponse(ctx, account, resp.StatusCode, upstreamMsg, respBody) {
				upstreamDetail := ""
				if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
					maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
					if maxBytes <= 0 {
						maxBytes = 2048
					}
					upstreamDetail = truncateString(string(respBody), maxBytes)
				}
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamRequestID:  resp.Header.Get("x-request-id"),
					Kind:               "failover",
					Message:            upstreamMsg,
					Detail:             upstreamDetail,
				})

				s.handleFailoverSideEffects(ctx, resp, account, upstreamModel)
				decision := s.classifyOpenAIPoolFailover(ctx, account, resp.StatusCode, upstreamMsg, respBody)
				return nil, &UpstreamFailoverError{
					StatusCode:             resp.StatusCode,
					ResponseBody:           respBody,
					RetryableOnSameAccount: decision.RetryableOnSameAccount,
					SkipPoolSoftCooldown:   decision.SkipSoftCooldown,
				}
			}
			return s.handleErrorResponse(ctx, resp, c, account, body, billingModel)
		}
		defer func() { _ = resp.Body.Close() }()

		reasoningEffort := extractOpenAIReasoningEffort(reqBody, upstreamModel, billingModel, originalModel)
		serviceTier := extractOpenAIServiceTier(reqBody)
		releaseOpenAIParsedRequestBody(c)

		// Handle normal response
		var usage *OpenAIUsage
		var firstTokenMs *int
		responseID := ""
		imageCount := 0
		var imageOutputSizes []string
		if reqStream {
			streamResult, err := s.handleStreamingResponse(ctx, resp, c, account, startTime, originalModel, upstreamModel, requestFirstTokenPlaceholder.Sent)
			if err != nil {
				return nil, err
			}
			usage = streamResult.usage
			firstTokenMs = streamResult.firstTokenMs
			responseID = strings.TrimSpace(streamResult.responseID)
			imageCount = streamResult.imageCount
			imageOutputSizes = streamResult.imageOutputSizes
		} else {
			nonStreamResult, err := s.handleNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel)
			if err != nil {
				return nil, err
			}
			usage = nonStreamResult.usage
			responseID = strings.TrimSpace(nonStreamResult.responseID)
			imageCount = nonStreamResult.imageCount
			imageOutputSizes = nonStreamResult.imageOutputSizes
		}
		s.bindHTTPResponseAccount(ctx, c, account, responseID)

		// Extract and save Codex usage snapshot from response headers (for real OAuth accounts).
		if account.Type == AccountTypeOAuth && !account.IsShadow() {
			if snapshot := ParseCodexRateLimitHeaders(resp.Header); snapshot != nil {
				s.updateCodexUsageSnapshot(ctx, account.ID, snapshot)
			}
		}

		if usage == nil {
			usage = &OpenAIUsage{}
		}

		forwardResult := &OpenAIForwardResult{
			RequestID:       resp.Header.Get("x-request-id"),
			ResponseID:      responseID,
			Usage:           *usage,
			Model:           originalModel,
			BillingModel:    billingModel,
			UpstreamModel:   upstreamModel,
			ServiceTier:     serviceTier,
			ReasoningEffort: reasoningEffort,
			Stream:          reqStream,
			OpenAIWSMode:    false,
			Duration:        time.Since(startTime),
			FirstTokenMs:    firstTokenMs,
		}
		if imageCount > 0 {
			forwardResult.ImageCount = imageCount
			forwardResult.ImageSize = imageSizeTier
			forwardResult.ImageInputSize = imageInputSize
			forwardResult.ImageOutputSizes = imageOutputSizes
			forwardResult.BillingModel = imageBillingModel
		}
		return forwardResult, nil
	}
}

func (s *OpenAIGatewayService) forwardOpenAIPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	reqModel string,
	reasoningEffort *string,
	reqStream bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	upstreamPassthroughModel := ""
	if isOpenAIResponsesCompactPath(c) {
		compactMappedModel := s.resolveOpenAICompactForwardModel(account, reqModel)
		if compactMappedModel != "" && compactMappedModel != reqModel {
			nextBody, setErr := sjson.SetBytes(body, "model", compactMappedModel)
			if setErr != nil {
				return nil, fmt.Errorf("set compact passthrough model: %w", setErr)
			}
			body = nextBody
			upstreamPassthroughModel = compactMappedModel
		}
	}

	if account != nil && account.Type == AccountTypeOAuth {
		if rejectReason := detectOpenAIPassthroughInstructionsRejectReason(reqModel, body); rejectReason != "" {
			rejectMsg := "OpenAI codex passthrough requires a non-empty instructions field"
			MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
			logOpenAIPassthroughInstructionsRejected(ctx, c, account, reqModel, rejectReason, body)
			c.JSON(http.StatusForbidden, gin.H{
				"error": gin.H{
					"type":    "forbidden_error",
					"message": rejectMsg,
				},
			})
			return nil, fmt.Errorf("openai passthrough rejected before upstream: %s", rejectReason)
		}

		normalizedBody, normalized, err := normalizeOpenAIPassthroughOAuthBody(body, isOpenAIResponsesCompactPath(c))
		if err != nil {
			return nil, err
		}
		if normalized {
			body = normalizedBody
		}
		reqStream = gjson.GetBytes(body, "stream").Bool()
	}

	if account != nil && account.IsOpenAIResponsesPassthroughCompatEnabled() && isOpenAIResponsesRequestPath(c) {
		normalizedInputBody, normalizedInput, err := normalizeOpenAIResponsesStringInputBody(body)
		if err != nil {
			return nil, err
		}
		if normalizedInput {
			body = normalizedInputBody
		}
		normalizedParamsBody, normalizedParams, err := normalizeOpenAIAPIKeyResponsesUnsupportedParamsBody(body)
		if err != nil {
			return nil, err
		}
		if normalizedParams {
			body = normalizedParamsBody
		}
	}
	if account != nil && account.IsOpenAIResponsesArgumentsObjectCompatEnabled() && isOpenAIResponsesRequestPath(c) {
		normalizedArgsBody, normalizedArgs, err := normalizeOpenAIResponsesInputArgumentsBody(body)
		if err != nil {
			return nil, err
		}
		if normalizedArgs {
			body = normalizedArgsBody
		}
	}

	sanitizedBody, sanitized, err := sanitizeEmptyBase64InputImagesInOpenAIBody(body)
	if err != nil {
		return nil, err
	}
	if sanitized {
		body = sanitizedBody
	}

	// Apply OpenAI fast policy to the passthrough body (filter/block by service_tier).
	// 统一使用 upstream 视角的 model：透传路径下 body 已经过 compact 映射 +
	// OAuth normalize，body 中的 model 字段即上游真正会看到的 slug。
	// 这样可以与 chat-completions / messages / native /responses 入口的
	// upstreamModel 保持一致，避免 whitelist 命中差异。当 body 中没有
	// model 字段时退回 reqModel。
	policyModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if policyModel == "" {
		policyModel = reqModel
	}
	updatedBody, policyErr := s.applyOpenAIFastPolicyToBody(ctx, account, policyModel, body)
	if policyErr != nil {
		var blocked *OpenAIFastBlockedError
		if errors.As(policyErr, &blocked) {
			writeOpenAIFastPolicyBlockedResponse(c, blocked)
		}
		return nil, policyErr
	}
	body = updatedBody
	if account.IsOpenAIUpstreamStrongIsolationEnabled() {
		isolatedBody, isolated, err := applyOpenAIUpstreamStrongIsolationBody(body, true)
		if err != nil {
			return nil, fmt.Errorf("apply upstream strong isolation: %w", err)
		}
		if isolated {
			body = isolatedBody
		}
	}

	apiKey := getAPIKeyFromContext(c)
	if IsImageGenerationIntent(openAIResponsesEndpoint, reqModel, body) && !GroupAllowsImageGeneration(apiKeyGroup(apiKey)) {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"type":    "permission_error",
				"message": ImageGenerationPermissionMessage(),
			},
		})
		return nil, errors.New("image generation disabled for group")
	}
	imageBillingModel := ""
	imageSizeTier := ""
	imageInputSize := ""
	if IsImageGenerationIntent(openAIResponsesEndpoint, reqModel, body) {
		var imageCfgErr error
		imageCfg, imageCfgErr := resolveOpenAIResponsesImageBillingConfigDetailedFromBody(body, reqModel)
		if imageCfgErr != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, imageCfgErr.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{
					"type":    "invalid_request_error",
					"message": imageCfgErr.Error(),
					"param":   "size",
				},
			})
			return nil, imageCfgErr
		}
		imageBillingModel = imageCfg.Model
		imageSizeTier = imageCfg.SizeTier
		imageInputSize = imageCfg.InputSize
	}

	logger.LegacyPrintf("service.openai_gateway",
		"[OpenAI 自动透传] 命中自动透传分支: account=%d name=%s type=%s model=%s stream=%v",
		account.ID,
		account.Name,
		account.Type,
		reqModel,
		reqStream,
	)
	if reqStream && c != nil && c.Request != nil {
		if timeoutHeaders := collectOpenAIPassthroughTimeoutHeaders(c.Request.Header); len(timeoutHeaders) > 0 {
			streamWarnLogger := logger.FromContext(ctx).With(
				zap.String("component", "service.openai_gateway"),
				zap.Int64("account_id", account.ID),
				zap.Strings("timeout_headers", timeoutHeaders),
			)
			if s.isOpenAIPassthroughTimeoutHeadersAllowed() {
				streamWarnLogger.Warn("OpenAI passthrough 透传请求包含超时相关请求头，且当前配置为放行，可能导致上游提前断流")
			} else {
				streamWarnLogger.Warn("OpenAI passthrough 检测到超时相关请求头，将按配置过滤以降低断流风险")
			}
		}
	}

	// Get access token
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	if c != nil {
		c.Set("openai_passthrough", true)
	}

	httpCodexAutoResetRetryTried := false
	for {
		upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
		upstreamReq, err := s.buildUpstreamRequestOpenAIPassthrough(upstreamCtx, c, account, body, token)
		releaseUpstreamCtx()
		if err != nil {
			return nil, err
		}

		var requestFirstTokenPlaceholder openAIRequestFirstTokenPlaceholderState
		var upstreamElapsed time.Duration
		var resp *http.Response
		doUpstream := func() (*http.Response, error) {
			return s.httpUpstream.DoWithTLS(upstreamReq, proxyURL, account.ID, account.Concurrency, s.resolveTLSProfile(account))
		}
		if reqStream {
			resp, requestFirstTokenPlaceholder, upstreamElapsed, err = s.doOpenAIUpstreamWithFirstTokenTimeoutPlaceholder(
				c,
				account,
				reqModel,
				startTime,
				openAIRequestFirstTokenPlaceholderDialectResponses,
				doUpstream,
			)
		} else {
			upstreamStart := time.Now()
			resp, err = doUpstream()
			upstreamElapsed = time.Since(upstreamStart)
		}
		SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, upstreamElapsed.Milliseconds())
		if err != nil {
			if requestFirstTokenPlaceholder.Sent {
				_ = s.handleOpenAIUpstreamTransportError(ctx, c, account, err, true)
				s.RecordOpenAIPoolFailureAfterCommittedResponse(ctx, account, http.StatusBadGateway, openAITransportFailoverBody, reqModel, err.Error())
				writeOpenAIRequestPlaceholderErrorSSE(c, openAIRequestFirstTokenPlaceholderDialectResponses, reqModel, "upstream_error", "Upstream request failed")
				return &OpenAIForwardResult{
					Usage:         OpenAIUsage{},
					Model:         reqModel,
					UpstreamModel: upstreamPassthroughModel,
					Stream:        true,
					OpenAIWSMode:  false,
					Duration:      time.Since(startTime),
				}, fmt.Errorf("passthrough upstream request failed after first token placeholder: %w", err)
			}
			if failoverErr := s.newOpenAIPoolRequestFailoverError(c, account, upstreamReq, err, true); failoverErr != nil {
				return nil, failoverErr
			}
			return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, true)
		}

		if resp.StatusCode >= 400 {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(respBody))
			upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
			upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)

			if requestFirstTokenPlaceholder.Sent {
				if resp.StatusCode == http.StatusTooManyRequests {
					_ = s.tryAutoConsumeOpenAICodexResetCredit(ctx, account, resp.Header, respBody)
				}
				setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, "")
				_ = s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody, reqModel)
				s.RecordOpenAIPoolFailureAfterCommittedResponse(ctx, account, resp.StatusCode, respBody, reqModel, upstreamMsg)
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamRequestID:  resp.Header.Get("x-request-id"),
					Passthrough:        true,
					Kind:               "http_error",
					Message:            upstreamMsg,
				})
				writeOpenAIRequestPlaceholderErrorSSE(c, openAIRequestFirstTokenPlaceholderDialectResponses, reqModel, "upstream_error", firstNonEmptyString(upstreamMsg, "Upstream request failed"))
				return &OpenAIForwardResult{
					RequestID:     resp.Header.Get("x-request-id"),
					Usage:         OpenAIUsage{},
					Model:         reqModel,
					UpstreamModel: upstreamPassthroughModel,
					Stream:        true,
					OpenAIWSMode:  false,
					Duration:      time.Since(startTime),
				}, fmt.Errorf("passthrough upstream error after first token placeholder: %d message=%s", resp.StatusCode, upstreamMsg)
			}

			if shouldFailoverOpenAIPassthroughResponse(resp.StatusCode) {
				if !httpCodexAutoResetRetryTried && resp.StatusCode == http.StatusTooManyRequests {
					if s.tryAutoConsumeOpenAICodexResetCredit(ctx, account, resp.Header, respBody) {
						httpCodexAutoResetRetryTried = true
						continue
					}
				}
				return nil, s.handleFailoverErrorResponsePassthrough(ctx, resp, c, account, body)
			}
			return nil, s.handleErrorResponsePassthrough(ctx, resp, c, account, body)
		}

		defer func() { _ = resp.Body.Close() }()

		serviceTier := extractOpenAIServiceTierFromBody(body)

		var usage *OpenAIUsage
		var firstTokenMs *int
		responseID := ""
		imageCount := 0
		var imageOutputSizes []string
		if reqStream {
			result, err := s.handleStreamingResponsePassthrough(ctx, resp, c, account, startTime, reqModel, upstreamPassthroughModel, requestFirstTokenPlaceholder.Sent)
			if err != nil {
				return nil, err
			}
			usage = result.usage
			firstTokenMs = result.firstTokenMs
			responseID = strings.TrimSpace(result.responseID)
			imageCount = result.imageCount
			imageOutputSizes = result.imageOutputSizes
		} else {
			result, err := s.handleNonStreamingResponsePassthrough(ctx, resp, c, account, reqModel, upstreamPassthroughModel)
			if err != nil {
				return nil, err
			}
			usage = result.usage
			responseID = strings.TrimSpace(result.responseID)
			imageCount = result.imageCount
			imageOutputSizes = result.imageOutputSizes
		}
		s.bindHTTPResponseAccount(ctx, c, account, responseID)

		if !account.IsShadow() {
			if snapshot := ParseCodexRateLimitHeaders(resp.Header); snapshot != nil {
				s.updateCodexUsageSnapshot(ctx, account.ID, snapshot)
			}
		}

		if usage == nil {
			usage = &OpenAIUsage{}
		}

		forwardResult := &OpenAIForwardResult{
			RequestID:       resp.Header.Get("x-request-id"),
			ResponseID:      responseID,
			Usage:           *usage,
			Model:           reqModel,
			UpstreamModel:   upstreamPassthroughModel,
			ServiceTier:     serviceTier,
			ReasoningEffort: reasoningEffort,
			Stream:          reqStream,
			OpenAIWSMode:    false,
			Duration:        time.Since(startTime),
			FirstTokenMs:    firstTokenMs,
		}
		if imageCount > 0 {
			forwardResult.ImageCount = imageCount
			forwardResult.ImageSize = imageSizeTier
			forwardResult.ImageInputSize = imageInputSize
			forwardResult.ImageOutputSizes = imageOutputSizes
			forwardResult.BillingModel = imageBillingModel
		}
		return forwardResult, nil
	}
}

func logOpenAIPassthroughInstructionsRejected(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	reqModel string,
	rejectReason string,
	body []byte,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	accountID := int64(0)
	accountName := ""
	accountType := ""
	if account != nil {
		accountID = account.ID
		accountName = strings.TrimSpace(account.Name)
		accountType = strings.TrimSpace(string(account.Type))
	}
	fields := []zap.Field{
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", accountID),
		zap.String("account_name", accountName),
		zap.String("account_type", accountType),
		zap.String("request_model", strings.TrimSpace(reqModel)),
		zap.String("reject_reason", strings.TrimSpace(rejectReason)),
	}
	fields = appendCodexCLIOnlyRejectedRequestFields(fields, c, body)
	logger.FromContext(ctx).With(fields...).Warn("OpenAI passthrough 本地拦截：Codex 请求缺少有效 instructions")
}

func (s *OpenAIGatewayService) buildUpstreamRequestOpenAIPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
) (*http.Request, error) {
	targetURL := openaiPlatformAPIURL
	switch account.Type {
	case AccountTypeOAuth:
		targetURL = chatgptCodexURL
	case AccountTypeAPIKey:
		baseURL := account.GetOpenAIBaseURL()
		if baseURL != "" {
			validatedURL, err := s.validateUpstreamBaseURL(baseURL)
			if err != nil {
				return nil, err
			}
			targetURL = buildOpenAIResponsesURL(validatedURL)
		}
	}
	targetURL = appendOpenAIResponsesRequestPathSuffix(targetURL, openAIResponsesRequestPathSuffix(c))

	ctx = WithOpsLatencyRecorder(ctx, func(key string, elapsed time.Duration) {
		SetOpsLatencyMsOnce(c, key, elapsed.Milliseconds())
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	// 透传客户端请求头（安全白名单）。
	allowTimeoutHeaders := s.isOpenAIPassthroughTimeoutHeadersAllowed()
	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			lower := strings.ToLower(strings.TrimSpace(key))
			if !isOpenAIPassthroughAllowedRequestHeader(lower, allowTimeoutHeaders) {
				continue
			}
			for _, v := range values {
				req.Header.Add(key, v)
			}
		}
	}

	// 覆盖入站鉴权残留，并注入上游认证
	req.Header.Del("authorization")
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-api-key")
	req.Header.Set("authorization", "Bearer "+token)

	// OAuth 透传到 ChatGPT internal API 时补齐必要头。
	if account.Type == AccountTypeOAuth {
		credentialAccount := account
		if account.IsShadow() {
			resolved, err := resolveCredentialAccount(ctx, s.accountRepo, account)
			if err != nil {
				return nil, fmt.Errorf("resolve chatgpt account headers: %w", err)
			}
			credentialAccount = resolved
		}
		promptCacheKey := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
		req.Host = "chatgpt.com"
		if chatgptAccountID := credentialAccount.GetChatGPTAccountID(); chatgptAccountID != "" {
			req.Header.Set("chatgpt-account-id", chatgptAccountID)
		}
		apiKeyID := getAPIKeyIDFromContext(c)
		// 先保存客户端原始值，再做 compact 补充，避免后续统一隔离时读到已处理的值。
		clientSessionID := strings.TrimSpace(req.Header.Get("session_id"))
		clientConversationID := strings.TrimSpace(req.Header.Get("conversation_id"))
		if isOpenAIResponsesCompactPath(c) {
			req.Header.Set("accept", "application/json")
			if req.Header.Get("version") == "" {
				req.Header.Set("version", codexCLIVersion)
			}
			if clientSessionID == "" {
				clientSessionID = resolveOpenAICompactSessionID(c)
			}
		} else if req.Header.Get("accept") == "" {
			req.Header.Set("accept", "text/event-stream")
		}
		if req.Header.Get("OpenAI-Beta") == "" {
			req.Header.Set("OpenAI-Beta", "responses=experimental")
		}
		if req.Header.Get("originator") == "" {
			req.Header.Set("originator", "codex_cli_rs")
		}
		// 用隔离后的 session 标识符覆盖客户端透传值，防止跨用户会话碰撞。
		if clientSessionID == "" {
			clientSessionID = promptCacheKey
		}
		if clientConversationID == "" {
			clientConversationID = promptCacheKey
		}
		if clientSessionID != "" {
			req.Header.Set("session_id", isolateOpenAISessionID(apiKeyID, clientSessionID))
		}
		if clientConversationID != "" {
			req.Header.Set("conversation_id", isolateOpenAISessionID(apiKeyID, clientConversationID))
		}
	}

	// 透传模式也支持账户自定义 User-Agent 与 ForceCodexCLI 兜底。
	customUA := account.GetOpenAIUserAgent()
	if customUA != "" {
		req.Header.Set("user-agent", customUA)
	}
	if s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		req.Header.Set("user-agent", codexCLIUserAgent)
	}
	// 浏览器型 UA 兜底：仅 OAuth（ChatGPT 内部接口）账号生效，若最终 user-agent 仍为浏览器
	// （Chrome/Firefox/Safari/Edge 等），替换为后台配置的 Codex UA，避免 Cloudflare 触发 JS 质询。
	s.overrideBrowserUserAgent(ctx, account, req)

	// 终态收口：originator 必须与最终 User-Agent 首段配套且为官方身份，否则上游可能拒绝。
	if account.Type == AccountTypeOAuth {
		enforceCodexIdentityHeaders(req.Header)
	}
	if account.IsOpenAIUpstreamStrongIsolationEnabled() {
		applyOpenAIUpstreamStrongIsolationHeaders(req)
	}

	// 账号级请求头覆写（仅 openai api_key 账号启用时生效；OAuth 路径 no-op）
	account.ApplyHeaderOverrides(req.Header)
	if isOpenAIResponsesCompactPath(c) {
		req.Header.Set("accept", "application/json")
	}

	if req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}

	return req, nil
}

func shouldFailoverOpenAIPassthroughResponse(statusCode int) bool {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, 529:
		return true
	default:
		return statusCode >= 500
	}
}

func (s *OpenAIGatewayService) handleFailoverErrorResponsePassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	requestBody []byte,
) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)
	logOpenAIInstructionsRequiredDebug(ctx, c, account, resp.StatusCode, upstreamMsg, requestBody, body)
	reqModel, _, _ := extractOpenAIRequestMetaFromBody(requestBody)
	_ = s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, reqModel)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:             account.Platform,
		AccountID:            account.ID,
		AccountName:          account.Name,
		UpstreamStatusCode:   resp.StatusCode,
		UpstreamRequestID:    resp.Header.Get("x-request-id"),
		Passthrough:          true,
		Kind:                 "failover",
		Message:              upstreamMsg,
		Detail:               upstreamDetail,
		UpstreamResponseBody: upstreamDetail,
	})
	decision := s.classifyOpenAIPoolFailover(ctx, account, resp.StatusCode, upstreamMsg, body)
	return &UpstreamFailoverError{
		StatusCode:             resp.StatusCode,
		ResponseBody:           body,
		ResponseHeaders:        resp.Header.Clone(),
		RetryableOnSameAccount: decision.RetryableOnSameAccount,
		SkipPoolSoftCooldown:   decision.SkipSoftCooldown,
	}
}

func (s *OpenAIGatewayService) handleErrorResponsePassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	requestBody []byte,
) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)
	logOpenAIInstructionsRequiredDebug(ctx, c, account, resp.StatusCode, upstreamMsg, requestBody, body)
	// 透传模式保留原始上游错误响应，但运行态账号状态仍需更新，
	// 避免粘性路由继续复用刚被限流的账号。
	reqModel, _, _ := extractOpenAIRequestMetaFromBody(requestBody)
	_ = s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, reqModel)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:             account.Platform,
		AccountID:            account.ID,
		AccountName:          account.Name,
		UpstreamStatusCode:   resp.StatusCode,
		UpstreamRequestID:    resp.Header.Get("x-request-id"),
		Passthrough:          true,
		Kind:                 "http_error",
		Message:              upstreamMsg,
		Detail:               upstreamDetail,
		UpstreamResponseBody: upstreamDetail,
	})

	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	MarkResponseCommitted(c)
	if !writeOpenAICompactSSEBridge(c, resp.StatusCode, body) {
		c.Data(resp.StatusCode, contentType, body)
	}

	if upstreamMsg == "" {
		return fmt.Errorf("upstream error: %d", resp.StatusCode)
	}
	return fmt.Errorf("upstream error: %d message=%s", resp.StatusCode, upstreamMsg)
}

func isOpenAIPassthroughAllowedRequestHeader(lowerKey string, allowTimeoutHeaders bool) bool {
	if lowerKey == "" {
		return false
	}
	if isOpenAIPassthroughTimeoutHeader(lowerKey) {
		return allowTimeoutHeaders
	}
	return openaiPassthroughAllowedHeaders[lowerKey]
}

func isOpenAIPassthroughTimeoutHeader(lowerKey string) bool {
	switch lowerKey {
	case "x-stainless-timeout", "x-stainless-read-timeout", "x-stainless-connect-timeout", "x-request-timeout", "request-timeout", "grpc-timeout":
		return true
	default:
		return false
	}
}

func (s *OpenAIGatewayService) isOpenAIPassthroughTimeoutHeadersAllowed() bool {
	return s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIPassthroughAllowTimeoutHeaders
}

func collectOpenAIPassthroughTimeoutHeaders(h http.Header) []string {
	if h == nil {
		return nil
	}
	var matched []string
	for key, values := range h {
		lowerKey := strings.ToLower(strings.TrimSpace(key))
		if isOpenAIPassthroughTimeoutHeader(lowerKey) {
			entry := lowerKey
			if len(values) > 0 {
				entry = fmt.Sprintf("%s=%s", lowerKey, strings.Join(values, "|"))
			}
			matched = append(matched, entry)
		}
	}
	sort.Strings(matched)
	return matched
}

type openaiStreamingResultPassthrough struct {
	usage            *OpenAIUsage
	firstTokenMs     *int
	responseID       string
	imageCount       int
	imageOutputSizes []string
}

type openaiNonStreamingResultPassthrough struct {
	*OpenAIUsage
	usage            *OpenAIUsage
	responseID       string
	imageCount       int
	imageOutputSizes []string
}

func openAIStreamClientOutputStarted(c *gin.Context, localStarted bool) bool {
	if localStarted {
		return true
	}
	return c != nil && c.Writer != nil && c.Writer.Written()
}

func openAIStreamEventIsPreamble(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.created", "response.in_progress":
		return true
	default:
		return false
	}
}

func openAIStreamDataStartsClientOutput(data, eventType string) bool {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return false
	}
	if strings.TrimSpace(eventType) == "response.failed" {
		return false
	}
	return !openAIStreamEventIsPreamble(eventType)
}

func openAIStreamFailedEventSemanticStatus(payload []byte, message string) int {
	if isOpenAIContextWindowError(message, payload) {
		return http.StatusBadRequest
	}

	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.code").String()))
	if code == "" {
		code = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.code").String()))
	}
	errType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.type").String()))
	if errType == "" {
		errType = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.type").String()))
	}
	combined := strings.TrimSpace(errType + " " + code + " " + strings.ToLower(strings.TrimSpace(message)))
	switch {
	case strings.Contains(errType, "invalid_request"):
		return http.StatusBadRequest
	case strings.Contains(combined, "rate_limit"):
		return http.StatusTooManyRequests
	case strings.Contains(combined, "authentication") || strings.Contains(combined, "unauthorized") || strings.Contains(combined, "invalid_api_key"):
		return http.StatusUnauthorized
	case strings.Contains(combined, "permission") || strings.Contains(combined, "forbidden") || strings.Contains(combined, "access denied"):
		return http.StatusForbidden
	case code == "server_is_overloaded" || code == "slow_down":
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

func openAIStreamFailedEventPassthroughBody(payload []byte, failedMessage string) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	if gjson.GetBytes(payload, "error").Exists() {
		return payload
	}
	responseError := gjson.GetBytes(payload, "response.error")
	if !responseError.Exists() {
		if strings.TrimSpace(failedMessage) == "" {
			return payload
		}
		body, err := json.Marshal(gin.H{
			"error": gin.H{
				"message": failedMessage,
			},
		})
		if err != nil {
			return payload
		}
		return body
	}

	errorPayload := gin.H{}
	if errType := strings.TrimSpace(gjson.Get(responseError.Raw, "type").String()); errType != "" {
		errorPayload["type"] = errType
	}
	if code := strings.TrimSpace(gjson.Get(responseError.Raw, "code").String()); code != "" {
		errorPayload["code"] = code
	}
	if param := strings.TrimSpace(gjson.Get(responseError.Raw, "param").String()); param != "" {
		errorPayload["param"] = param
	}
	message := strings.TrimSpace(gjson.Get(responseError.Raw, "message").String())
	if message == "" {
		message = strings.TrimSpace(failedMessage)
	}
	if message != "" {
		errorPayload["message"] = message
	}
	if len(errorPayload) == 0 {
		return payload
	}
	body, err := json.Marshal(gin.H{"error": errorPayload})
	if err != nil {
		return payload
	}
	return body
}

func applyOpenAIStreamFailedErrorPassthroughRule(
	c *gin.Context,
	platform string,
	payload []byte,
	failedMessage string,
) (status int, errType string, errMsg string, matched bool) {
	ruleBody := openAIStreamFailedEventPassthroughBody(payload, failedMessage)
	upstreamStatus := openAIStreamFailedEventSemanticStatus(payload, failedMessage)
	return applyErrorPassthroughRule(
		c,
		platform,
		upstreamStatus,
		ruleBody,
		http.StatusBadGateway,
		"upstream_error",
		"Upstream request failed",
	)
}

func openAIStreamDataStartsRealOutput(data, eventType string) bool {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" || trimmed == "[DONE]" {
		return false
	}
	switch strings.TrimSpace(eventType) {
	case "response.output_text.delta",
		"response.function_call_arguments.delta",
		"response.custom_tool_call_input.delta",
		"response.reasoning_summary_text.delta",
		"response.reasoning_text.delta":
		return strings.TrimSpace(gjson.Get(trimmed, "delta").String()) != ""
	case "response.output_item.added":
		itemType := strings.TrimSpace(gjson.Get(trimmed, "item.type").String())
		return itemType == "function_call" || itemType == "custom_tool_call"
	default:
		return false
	}
}

func openAIStreamDataStartsClientOutputWithPreambleFlush(data, eventType string, flushPreamble bool) bool {
	if openAIStreamDataStartsClientOutput(data, eventType) {
		return true
	}
	return flushPreamble && strings.TrimSpace(data) != "" && openAIStreamEventIsPreamble(eventType)
}

func (s *OpenAIGatewayService) openAIStreamPreambleFlushEnabled(account *Account, requestedModel string) bool {
	if account == nil || !account.IsOpenAIStreamPreambleFlushEnabled() {
		return false
	}
	if isOpenAIImageGenerationModel(requestedModel) {
		return false
	}
	return true
}

func (s *OpenAIGatewayService) openAIStreamSSECommentPreflushEnabled(account *Account, requestedModel string) bool {
	if account == nil || !account.IsOpenAISSECommentPreflushEnabled() {
		return false
	}
	if isOpenAIImageGenerationModel(requestedModel) {
		return false
	}
	return true
}

func (s *OpenAIGatewayService) openAIStreamSafeTokenPlaceholderEnabled(account *Account, requestedModel string) bool {
	if account == nil || !account.IsOpenAISafeTokenPlaceholderEnabled() {
		return false
	}
	if isOpenAIImageGenerationModel(requestedModel) {
		return false
	}
	return true
}

func (s *OpenAIGatewayService) openAIStreamFirstTokenTimeoutPlaceholder(account *Account, requestedModel string) time.Duration {
	ms := s.openAIStreamFirstTokenTimeoutPlaceholderMs(account, requestedModel)
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func (s *OpenAIGatewayService) openAIStreamFirstTokenTimeoutPlaceholderMs(account *Account, requestedModel string) int {
	if account == nil {
		return 0
	}
	if account.IsImagePoolMode() {
		return 0
	}
	if isOpenAIImageGenerationModel(requestedModel) {
		return 0
	}
	ms := account.GetOpenAIFirstTokenTimeoutPlaceholderMs()
	if ms <= 0 {
		return 0
	}
	if account.IsOpenAIFirstTokenTimeoutPlaceholderGuardEnabled() {
		guardMaxMS := account.GetOpenAIFirstTokenTimeoutPlaceholderGuardMaxMs()
		if !s.openaiFirstTokenTimeoutPlaceholderGuard.allow(account.ID, requestedModel, guardMaxMS, time.Now()) {
			return 0
		}
	}
	return ms
}

func (s *OpenAIGatewayService) recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account *Account, requestedModel string, realFirstTokenMS int) {
	if s == nil || account == nil || realFirstTokenMS <= 0 || !account.IsOpenAIFirstTokenTimeoutPlaceholderGuardEnabled() {
		return
	}
	if account.IsImagePoolMode() || isOpenAIImageGenerationModel(requestedModel) {
		return
	}
	s.openaiFirstTokenTimeoutPlaceholderGuard.record(
		account.ID,
		requestedModel,
		realFirstTokenMS,
		account.GetOpenAIFirstTokenTimeoutPlaceholderGuardMaxMs(),
		time.Now(),
	)
}

func (s *OpenAIGatewayService) RecordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account *Account, requestedModel string, realFirstTokenMS int) {
	s.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, requestedModel, realFirstTokenMS)
}

func openAIStreamFirstTokenTimeoutTimer(startTime time.Time, timeout time.Duration) (*time.Timer, <-chan time.Time) {
	if timeout <= 0 {
		return nil, nil
	}
	remaining := timeout - time.Since(startTime)
	if remaining < 0 {
		remaining = 0
	}
	timer := time.NewTimer(remaining)
	return timer, timer.C
}

func openAIResponsesSafeTokenPlaceholderFrame(responseID string) string {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		responseID = "resp_placeholder"
	}
	return `data: {"type":"response.output_text.delta","delta":"","response_id":` +
		strconv.Quote(responseID) +
		`,"item_id":"msg_placeholder","output_index":0,"content_index":0}` + "\n\n"
}

type openAIRequestFirstTokenPlaceholderDialect string

const (
	openAIRequestFirstTokenPlaceholderDialectResponses       openAIRequestFirstTokenPlaceholderDialect = "responses"
	openAIRequestFirstTokenPlaceholderDialectChatCompletions openAIRequestFirstTokenPlaceholderDialect = "chat_completions"
)

type openAIRequestFirstTokenPlaceholderState struct {
	Sent        bool
	ChatID      string
	ChatCreated int64
}

type openAIUpstreamRequestResult struct {
	resp    *http.Response
	err     error
	elapsed time.Duration
}

func (s *OpenAIGatewayService) doOpenAIUpstreamWithFirstTokenTimeoutPlaceholder(
	c *gin.Context,
	account *Account,
	requestedModel string,
	startTime time.Time,
	dialect openAIRequestFirstTokenPlaceholderDialect,
	do func() (*http.Response, error),
) (*http.Response, openAIRequestFirstTokenPlaceholderState, time.Duration, error) {
	if do == nil {
		return nil, openAIRequestFirstTokenPlaceholderState{}, 0, errors.New("missing upstream request function")
	}
	timeout := s.openAIStreamFirstTokenTimeoutPlaceholder(account, requestedModel)
	if timeout <= 0 {
		upstreamStart := time.Now()
		resp, err := do()
		return resp, openAIRequestFirstTokenPlaceholderState{}, time.Since(upstreamStart), err
	}

	upstreamStart := time.Now()
	resultCh := make(chan openAIUpstreamRequestResult, 1)
	go func() {
		resp, err := do()
		resultCh <- openAIUpstreamRequestResult{resp: resp, err: err, elapsed: time.Since(upstreamStart)}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	state := openAIRequestFirstTokenPlaceholderState{}
	timerCh := timer.C
	for {
		select {
		case result := <-resultCh:
			return result.resp, state, result.elapsed, result.err
		case <-timerCh:
			timerCh = nil
			state = writeOpenAIRequestFirstTokenTimeoutPlaceholder(c, startTime, requestedModel, dialect)
		}
	}
}

func writeOpenAIRequestFirstTokenTimeoutPlaceholder(
	c *gin.Context,
	startTime time.Time,
	model string,
	dialect openAIRequestFirstTokenPlaceholderDialect,
) openAIRequestFirstTokenPlaceholderState {
	if c == nil || c.Writer == nil {
		return openAIRequestFirstTokenPlaceholderState{}
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return openAIRequestFirstTokenPlaceholderState{}
	}
	if !c.Writer.Written() {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(http.StatusOK)
	}

	state := openAIRequestFirstTokenPlaceholderState{Sent: true}
	var frame string
	switch dialect {
	case openAIRequestFirstTokenPlaceholderDialectChatCompletions:
		chatState := apicompat.NewResponsesEventToChatState()
		chatState.Model = model
		state.ChatID = chatState.ID
		state.ChatCreated = chatState.Created
		empty := ""
		roleChunk := apicompat.ChatCompletionsChunk{
			ID:      chatState.ID,
			Object:  "chat.completion.chunk",
			Created: chatState.Created,
			Model:   chatState.Model,
			Choices: []apicompat.ChatChunkChoice{{
				Index:        0,
				Delta:        apicompat.ChatDelta{Role: "assistant"},
				FinishReason: nil,
			}},
		}
		contentChunk := apicompat.ChatCompletionsChunk{
			ID:      chatState.ID,
			Object:  "chat.completion.chunk",
			Created: chatState.Created,
			Model:   chatState.Model,
			Choices: []apicompat.ChatChunkChoice{{
				Index:        0,
				Delta:        apicompat.ChatDelta{Content: &empty},
				FinishReason: nil,
			}},
		}
		roleSSE, roleErr := apicompat.ChatChunkToSSE(roleChunk)
		contentSSE, contentErr := apicompat.ChatChunkToSSE(contentChunk)
		if roleErr != nil || contentErr != nil {
			return openAIRequestFirstTokenPlaceholderState{}
		}
		frame = roleSSE + contentSSE
	default:
		frame = openAIResponsesSafeTokenPlaceholderFrame("")
	}

	if _, err := fmt.Fprint(c.Writer, frame); err != nil {
		return openAIRequestFirstTokenPlaceholderState{}
	}
	MarkResponseCommitted(c)
	flusher.Flush()
	SetOpsLatencyMsOnce(c, OpsFirstClientFlushMsKey, time.Since(startTime).Milliseconds())
	return state
}

func writeOpenAIRequestPlaceholderErrorSSE(
	c *gin.Context,
	dialect openAIRequestFirstTokenPlaceholderDialect,
	model string,
	errType string,
	message string,
) {
	if c == nil || c.Writer == nil {
		return
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return
	}
	errType = strings.TrimSpace(errType)
	if errType == "" {
		errType = "upstream_error"
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "Upstream request failed"
	}

	switch dialect {
	case openAIRequestFirstTokenPlaceholderDialectChatCompletions:
		_, _ = fmt.Fprint(c.Writer, "event: error\ndata: "+`{"error":{"type":`+strconv.Quote(errType)+`,"message":`+strconv.Quote(message)+`}}`+"\n\n")
	default:
		payload, err := json.Marshal(map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id":     "resp_placeholder_failed",
				"object": "response",
				"model":  strings.TrimSpace(model),
				"status": "failed",
				"output": []any{},
				"error": map[string]any{
					"code":    errType,
					"message": message,
				},
			},
		})
		if err == nil {
			_, _ = fmt.Fprintf(c.Writer, "event: response.failed\ndata: %s\n\n", payload)
		}
	}
	MarkResponseCommitted(c)
	flusher.Flush()
}

// RecordOpenAIPoolFailureAfterCommittedResponse records pool-mode cooldown side
// effects when a stream response has already been committed and transparent
// failover is no longer possible.
func (s *OpenAIGatewayService) RecordOpenAIPoolFailureAfterCommittedResponse(
	ctx context.Context,
	account *Account,
	statusCode int,
	responseBody []byte,
	requestedModel string,
	message string,
) {
	if s == nil || account == nil || !account.IsOpenAI() || !account.IsPoolMode() {
		return
	}
	message = sanitizeUpstreamErrorMessage(strings.TrimSpace(message))
	if message == "" {
		message = sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(responseBody))
	}
	if message == "" {
		message = "Upstream request failed"
	}
	decision := s.classifyOpenAIPoolFailover(ctx, account, statusCode, message, responseBody)
	if !decision.Failover || decision.SkipSoftCooldown {
		return
	}
	if !s.shouldStartOpenAIPoolSoftCooldown(account) {
		return
	}
	probeModel := strings.TrimSpace(requestedModel)
	probeKind := openAIPoolProbeKindForModel(probeModel)
	if account.IsImagePoolMode() {
		probeKind = "images"
	}
	s.MarkOpenAIPoolAccountSoftCooldownWithContext(ctx, account, statusCode, responseBody, openAIPoolSoftCooldownContext{
		ProbeCapability: decision.ProbeCapability,
		ProbeModel:      probeModel,
		ProbeKind:       probeKind,
		CooldownSource:  "upstream_failure",
		StatusCode:      statusCode,
		Reason:          message,
	})
}

func openAIStreamFailedEventShouldFailover(payload []byte, message string) bool {
	if isOpenAIContextWindowError(message, payload) {
		return false
	}
	if isOpenAITransientProcessingError(http.StatusBadRequest, message, payload) {
		return true
	}
	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.code").String()))
	if code == "" {
		code = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.code").String()))
	}
	errType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.type").String()))
	if errType == "" {
		errType = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.type").String()))
	}
	combined := strings.ToLower(strings.TrimSpace(message + " " + code + " " + errType))
	if combined == "" {
		return true
	}
	nonRetryableMarkers := []string{
		"invalid_request",
		"content_policy",
		"policy",
		"safety",
		"high-risk cyber",
		"not allowed",
		"violat",
	}
	for _, marker := range nonRetryableMarkers {
		if strings.Contains(combined, marker) {
			return false
		}
	}
	return true
}

func (s *OpenAIGatewayService) recordOpenAIStreamUpstreamError(
	c *gin.Context,
	account *Account,
	passthrough bool,
	upstreamRequestID string,
	kind string,
	payload []byte,
	message string,
) string {
	message = sanitizeUpstreamErrorMessage(strings.TrimSpace(message))
	if message == "" {
		message = "OpenAI upstream response failed"
	}
	upstreamStatus := openAIStreamFailedEventSemanticStatus(payload, message)
	detail := ""
	if len(payload) > 0 && s != nil && s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		detail = truncateString(string(payload), maxBytes)
	}
	if c != nil {
		setOpsUpstreamError(c, upstreamStatus, message, detail)
		event := OpsUpstreamErrorEvent{
			Platform:           PlatformOpenAI,
			UpstreamStatusCode: upstreamStatus,
			UpstreamRequestID:  strings.TrimSpace(upstreamRequestID),
			Passthrough:        passthrough,
			Kind:               kind,
			Message:            message,
			Detail:             detail,
		}
		if account != nil {
			event.Platform = account.Platform
			event.AccountID = account.ID
			event.AccountName = account.Name
		}
		appendOpsUpstreamError(c, event)
	}
	return message
}

func (s *OpenAIGatewayService) newOpenAIStreamFailoverError(
	c *gin.Context,
	account *Account,
	passthrough bool,
	upstreamRequestID string,
	payload []byte,
	message string,
) *UpstreamFailoverError {
	message = sanitizeUpstreamErrorMessage(strings.TrimSpace(message))
	if message == "" {
		message = "OpenAI stream disconnected before completion"
	}
	detail := ""
	if len(payload) > 0 && s != nil && s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		detail = truncateString(string(payload), maxBytes)
	}
	if c != nil {
		setOpsUpstreamError(c, http.StatusBadGateway, message, detail)
		event := OpsUpstreamErrorEvent{
			Platform:           PlatformOpenAI,
			UpstreamStatusCode: http.StatusBadGateway,
			UpstreamRequestID:  strings.TrimSpace(upstreamRequestID),
			Passthrough:        passthrough,
			Kind:               "failover",
			Message:            message,
			Detail:             detail,
		}
		if account != nil {
			event.Platform = account.Platform
			event.AccountID = account.ID
			event.AccountName = account.Name
		}
		appendOpsUpstreamError(c, event)
	}
	body, _ := json.Marshal(gin.H{
		"error": gin.H{
			"type":    "upstream_error",
			"message": message,
		},
	})
	decision := s.classifyOpenAIPoolFailover(c.Request.Context(), account, http.StatusBadGateway, message, body)
	return &UpstreamFailoverError{
		StatusCode:             http.StatusBadGateway,
		ResponseBody:           body,
		Message:                message,
		RetryableOnSameAccount: decision.RetryableOnSameAccount,
		SkipPoolSoftCooldown:   decision.SkipSoftCooldown,
	}
}

func (s *OpenAIGatewayService) handleStreamingResponsePassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	startTime time.Time,
	originalModel string,
	mappedModel string,
	requestFirstTokenPlaceholderSentOpt ...bool,
) (*openaiStreamingResultPassthrough, error) {
	requestFirstTokenPlaceholderSent := len(requestFirstTokenPlaceholderSentOpt) > 0 && requestFirstTokenPlaceholderSentOpt[0]
	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	// SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	if v := resp.Header.Get("x-request-id"); v != "" {
		c.Header("x-request-id", v)
	}

	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}
	accountSSECommentPreflush := s.openAIStreamSSECommentPreflushEnabled(account, originalModel)
	accountSSECommentPreflushed := requestFirstTokenPlaceholderSent
	if accountSSECommentPreflush && !requestFirstTokenPlaceholderSent {
		if _, err := w.Write([]byte(":\n\n")); err == nil {
			flusher.Flush()
			accountSSECommentPreflushed = true
			SetOpsLatencyMsOnce(c, OpsFirstClientFlushMsKey, time.Since(startTime).Milliseconds())
		}
	}
	passthroughLowLatencyPolicy := s.openAIStreamLowLatencyPolicy(account, originalModel)
	if !accountSSECommentPreflushed && passthroughLowLatencyPolicy.Enabled && passthroughLowLatencyPolicy.Barrier <= 0 && passthroughLowLatencyPolicy.AllowBootstrapComment {
		if _, err := w.Write([]byte(":\n\n")); err == nil {
			flusher.Flush()
			SetOpsLatencyMsOnce(c, OpsFirstClientFlushMsKey, time.Since(startTime).Milliseconds())
		}
	}

	usage := &OpenAIUsage{}
	imageCounter := newOpenAIImageOutputCounter()
	var firstTokenMs *int
	responseID := ""
	clientDisconnected := false
	sawDone := false
	sawTerminalEvent := false
	sawFailedEvent := false
	sawCyberPolicyEvent := false
	failedMessage := ""
	clientOutputStarted := accountSSECommentPreflushed || requestFirstTokenPlaceholderSent
	upstreamRequestID := strings.TrimSpace(resp.Header.Get("x-request-id"))
	flushPreamble := s.openAIStreamPreambleFlushEnabled(account, originalModel)
	safeTokenPlaceholder := s.openAIStreamSafeTokenPlaceholderEnabled(account, originalModel)
	firstTokenTimeoutPlaceholder := s.openAIStreamFirstTokenTimeoutPlaceholder(account, originalModel)
	safeTokenPlaceholderSent := requestFirstTokenPlaceholderSent
	safeTokenPlaceholderPending := false
	firstTokenTimeoutPlaceholderSent := requestFirstTokenPlaceholderSent
	firstTokenTimeoutPlaceholderPending := false
	firstTokenTimeoutGuardSampleRecorded := false
	pendingLines := make([]string, 0, 8)
	pendingLinesAtEventBoundary := func() bool {
		return len(pendingLines) == 0 || pendingLines[len(pendingLines)-1] == ""
	}
	writePendingLines := func() bool {
		for _, pending := range pendingLines {
			if _, err := fmt.Fprintln(w, pending); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI passthrough] Client disconnected during streaming, continue draining upstream for usage: account=%d", account.ID)
				return false
			}
		}
		pendingLines = pendingLines[:0]
		return true
	}
	writePassthroughBootstrapComment := func() {
		if clientDisconnected || openAIStreamClientOutputStarted(c, clientOutputStarted) || !pendingLinesAtEventBoundary() {
			return
		}
		if len(pendingLines) > 0 && !writePendingLines() {
			return
		}
		if _, err := fmt.Fprint(w, ":\n\n"); err != nil {
			clientDisconnected = true
			return
		}
		clientOutputStarted = true
		flusher.Flush()
		SetOpsLatencyMsOnce(c, OpsFirstClientFlushMsKey, time.Since(startTime).Milliseconds())
	}
	writeSafeTokenPlaceholder := func() {
		if !safeTokenPlaceholder || safeTokenPlaceholderSent || clientDisconnected || !pendingLinesAtEventBoundary() {
			return
		}
		if len(pendingLines) > 0 && !writePendingLines() {
			return
		}
		if _, err := fmt.Fprint(w, openAIResponsesSafeTokenPlaceholderFrame(responseID)); err != nil {
			clientDisconnected = true
			return
		}
		safeTokenPlaceholderSent = true
		clientOutputStarted = true
		if firstTokenMs == nil {
			ms := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &ms
		}
		flusher.Flush()
		SetOpsLatencyMsOnce(c, OpsFirstClientFlushMsKey, time.Since(startTime).Milliseconds())
	}
	writeFirstTokenTimeoutPlaceholder := func() bool {
		if firstTokenTimeoutPlaceholder <= 0 || firstTokenTimeoutPlaceholderSent || clientDisconnected || firstTokenMs != nil || !pendingLinesAtEventBoundary() {
			return false
		}
		if len(pendingLines) > 0 && !writePendingLines() {
			return true
		}
		if _, err := fmt.Fprint(w, openAIResponsesSafeTokenPlaceholderFrame(responseID)); err != nil {
			clientDisconnected = true
			return true
		}
		firstTokenTimeoutPlaceholderSent = true
		safeTokenPlaceholderSent = true
		clientOutputStarted = true
		flusher.Flush()
		SetOpsLatencyMsOnce(c, OpsFirstClientFlushMsKey, time.Since(startTime).Milliseconds())
		return true
	}
	drainFirstTokenTimeoutPlaceholderPending := func() {
		if !firstTokenTimeoutPlaceholderPending {
			return
		}
		firstTokenTimeoutPlaceholderPending = false
		if !writeFirstTokenTimeoutPlaceholder() && !clientDisconnected && firstTokenMs == nil && !firstTokenTimeoutPlaceholderSent {
			firstTokenTimeoutPlaceholderPending = true
		}
	}
	recordFirstTokenTimeoutGuardSample := func() {
		if firstTokenTimeoutGuardSampleRecorded {
			return
		}
		firstTokenTimeoutGuardSampleRecorded = true
		s.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, originalModel, int(time.Since(startTime).Milliseconds()))
	}

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanBuf := getSSEScannerBuf64K()
	scanner.Buffer(scanBuf[:0], maxLineSize)
	defer putSSEScannerBuf64K(scanBuf)

	needModelReplace := strings.TrimSpace(originalModel) != "" && strings.TrimSpace(mappedModel) != "" && strings.TrimSpace(originalModel) != strings.TrimSpace(mappedModel)
	resultWithUsage := func() *openaiStreamingResultPassthrough {
		return &openaiStreamingResultPassthrough{
			usage:            usage,
			firstTokenMs:     firstTokenMs,
			responseID:       responseID,
			imageCount:       imageCounter.Count(),
			imageOutputSizes: imageCounter.Sizes(),
		}
	}

	processPassthroughLine := func(line string) (bool, error) {
		lineStartsClientOutput := false
		forceFlushFailedEvent := false
		if data, ok := extractOpenAISSEDataLine(line); ok {
			dataBytes := []byte(data)
			trimmedData := strings.TrimSpace(data)
			if needModelReplace && strings.Contains(data, mappedModel) {
				line = s.replaceModelInSSELine(line, mappedModel, originalModel)
				if replacedData, replaced := extractOpenAISSEDataLine(line); replaced {
					dataBytes = []byte(replacedData)
					trimmedData = strings.TrimSpace(replacedData)
				}
			}
			eventType := strings.TrimSpace(gjson.Get(trimmedData, "type").String())
			if eventType == "response.failed" {
				failedMessage = extractOpenAISSEErrorMessage(dataBytes)
				s.parseSSEUsageBytes(dataBytes, usage)
				cyberPolicyMatched := false
				if decision := s.handleOpenAICyberPolicyEvent(c, account, true, upstreamRequestID, dataBytes, nil); decision.Matched {
					failedMessage = firstNonEmptyString(decision.Message, failedMessage)
					sawCyberPolicyEvent = true
					cyberPolicyMatched = true
				}
				if !cyberPolicyMatched && !openAIStreamClientOutputStarted(c, clientOutputStarted) {
					if status, errType, errMsg, matched := applyOpenAIStreamFailedErrorPassthroughRule(c, account.Platform, dataBytes, failedMessage); matched {
						s.recordOpenAIStreamUpstreamError(c, account, true, upstreamRequestID, "http_error", dataBytes, failedMessage)
						MarkResponseCommitted(c)
						c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
						c.JSON(status, gin.H{
							"error": gin.H{
								"type":    errType,
								"message": errMsg,
							},
						})
						return false, fmt.Errorf("upstream response failed: passthrough rule matched message=%s", errMsg)
					}
					if openAIStreamFailedEventShouldFailover(dataBytes, failedMessage) {
						return false, s.newOpenAIStreamFailoverError(c, account, true, upstreamRequestID, dataBytes, failedMessage)
					}
				}
				forceFlushFailedEvent = true
				sawFailedEvent = true
			}
			if sanitizedData, sanitized := sanitizeOpenAIResponseFailedEventForClient(dataBytes, eventType, openAIStreamClientOutputStarted(c, clientOutputStarted)); sanitized {
				dataBytes = sanitizedData
				trimmedData = string(sanitizedData)
				line = "data: " + trimmedData
			}
			if trimmedData == "[DONE]" {
				sawDone = true
			}
			if openAIStreamEventIsTerminal(trimmedData) {
				sawTerminalEvent = true
			}
			if responseID == "" {
				responseID = extractOpenAIResponseIDFromJSONBytes(dataBytes)
			}
			imageCounter.AddSSEData(dataBytes)
			safeTokenPlaceholderPending = safeTokenPlaceholderPending || eventType == "response.created"
			lineStartsRealOutput := openAIStreamDataStartsRealOutput(trimmedData, eventType)
			lineStartsFirstToken := forceFlushFailedEvent || openAIStreamDataStartsClientOutput(trimmedData, eventType)
			lineStartsClientOutput = lineStartsFirstToken || openAIStreamDataStartsClientOutputWithPreambleFlush(trimmedData, eventType, flushPreamble)
			if lineStartsRealOutput && trimmedData != "[DONE]" {
				recordFirstTokenTimeoutGuardSample()
			}
			if lineStartsFirstToken && trimmedData != "[DONE]" {
				if firstTokenMs == nil {
					ms := int(time.Since(startTime).Milliseconds())
					firstTokenMs = &ms
				}
			}
			s.parseSSEUsageBytes(dataBytes, usage)
		}

		if !clientDisconnected {
			if !clientOutputStarted && !lineStartsClientOutput {
				pendingLines = append(pendingLines, line)
				if line == "" && safeTokenPlaceholderPending {
					safeTokenPlaceholderPending = false
					writeSafeTokenPlaceholder()
				}
				if line == "" {
					drainFirstTokenTimeoutPlaceholderPending()
				}
				return false, nil
			}
			if !clientOutputStarted && len(pendingLines) > 0 {
				if !writePendingLines() {
					return false, nil
				}
			}
			if _, err := fmt.Fprintln(w, line); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI passthrough] Client disconnected during streaming, continue draining upstream for usage: account=%d", account.ID)
			} else {
				clientOutputStarted = true
				flusher.Flush()
				SetOpsLatencyMsOnce(c, OpsFirstClientFlushMsKey, time.Since(startTime).Milliseconds())
			}
			if line == "" && safeTokenPlaceholderPending {
				safeTokenPlaceholderPending = false
				writeSafeTokenPlaceholder()
			}
			if line == "" {
				drainFirstTokenTimeoutPlaceholderPending()
			}
		}
		return false, nil
	}

	if (passthroughLowLatencyPolicy.Enabled && passthroughLowLatencyPolicy.Barrier > 0 && passthroughLowLatencyPolicy.AllowBootstrapComment) || firstTokenTimeoutPlaceholder > 0 {
		type passthroughScanEvent struct {
			line string
			err  error
		}
		events := make(chan passthroughScanEvent, 16)
		done := make(chan struct{})
		go func() {
			defer close(events)
			for scanner.Scan() {
				select {
				case events <- passthroughScanEvent{line: scanner.Text()}:
				case <-done:
					return
				}
			}
			if err := scanner.Err(); err != nil {
				select {
				case events <- passthroughScanEvent{err: err}:
				case <-done:
				}
			}
		}()
		defer close(done)

		bootstrapPending := false
		var bootstrapCh <-chan time.Time
		var bootstrapTimer *time.Timer
		if passthroughLowLatencyPolicy.Enabled && passthroughLowLatencyPolicy.Barrier > 0 && passthroughLowLatencyPolicy.AllowBootstrapComment {
			bootstrapTimer = time.NewTimer(passthroughLowLatencyPolicy.Barrier)
			defer bootstrapTimer.Stop()
			bootstrapCh = bootstrapTimer.C
		}
		firstTokenTimeoutTimer, firstTokenTimeoutCh := openAIStreamFirstTokenTimeoutTimer(startTime, firstTokenTimeoutPlaceholder)
		if firstTokenTimeoutTimer != nil {
			defer firstTokenTimeoutTimer.Stop()
		}
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					goto passthroughScanDone
				}
				if ev.err != nil {
					goto passthroughScanDone
				}
				if _, err := processPassthroughLine(ev.line); err != nil {
					return resultWithUsage(), err
				}
				if bootstrapPending {
					writePassthroughBootstrapComment()
					if openAIStreamClientOutputStarted(c, clientOutputStarted) || clientDisconnected {
						bootstrapPending = false
					}
				}
				drainFirstTokenTimeoutPlaceholderPending()
			case <-bootstrapCh:
				bootstrapCh = nil
				writePassthroughBootstrapComment()
				if !clientDisconnected && !openAIStreamClientOutputStarted(c, clientOutputStarted) {
					bootstrapPending = true
				}
			case <-firstTokenTimeoutCh:
				firstTokenTimeoutCh = nil
				if !writeFirstTokenTimeoutPlaceholder() && !clientDisconnected && firstTokenMs == nil && !firstTokenTimeoutPlaceholderSent {
					firstTokenTimeoutPlaceholderPending = true
				}
			}
		}
	}

	for scanner.Scan() {
		if _, err := processPassthroughLine(scanner.Text()); err != nil {
			return resultWithUsage(), err
		}
	}
passthroughScanDone:
	if err := scanner.Err(); err != nil {
		if sawTerminalEvent && !sawFailedEvent {
			return resultWithUsage(), nil
		}
		if sawFailedEvent && !sawCyberPolicyEvent {
			return resultWithUsage(), fmt.Errorf("upstream response failed: %s", failedMessage)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return resultWithUsage(), fmt.Errorf("stream usage incomplete: %w", err)
		}
		if errors.Is(err, bufio.ErrTooLong) {
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI passthrough] SSE line too long: account=%d max_size=%d error=%v", account.ID, maxLineSize, err)
			return resultWithUsage(), err
		}
		if !openAIStreamClientOutputStarted(c, clientOutputStarted) {
			msg := "OpenAI stream disconnected before completion"
			if errText := strings.TrimSpace(err.Error()); errText != "" {
				msg += ": " + errText
			}
			return resultWithUsage(),
				s.newOpenAIStreamFailoverError(c, account, true, upstreamRequestID, nil, msg)
		}
		if clientDisconnected {
			return resultWithUsage(), fmt.Errorf("stream usage incomplete after disconnect: %w", err)
		}
		logger.LegacyPrintf("service.openai_gateway",
			"[OpenAI passthrough] 流读取异常中断: account=%d request_id=%s err=%v",
			account.ID,
			upstreamRequestID,
			err,
		)
		return resultWithUsage(), fmt.Errorf("stream read error: %w", err)
	}
	if sawFailedEvent && !sawCyberPolicyEvent {
		return resultWithUsage(), fmt.Errorf("upstream response failed: %s", failedMessage)
	}
	if !clientDisconnected && !sawDone && !sawTerminalEvent && ctx.Err() == nil {
		logger.FromContext(ctx).With(
			zap.String("component", "service.openai_gateway"),
			zap.Int64("account_id", account.ID),
			zap.String("upstream_request_id", upstreamRequestID),
		).Info("OpenAI passthrough 上游流在未收到 [DONE] 时结束，疑似断流")
		if !openAIStreamClientOutputStarted(c, clientOutputStarted) {
			return resultWithUsage(),
				s.newOpenAIStreamFailoverError(c, account, true, upstreamRequestID, nil, "OpenAI stream ended before a terminal event")
		}
		return resultWithUsage(), errors.New("stream usage incomplete: missing terminal event")
	}

	return resultWithUsage(), nil
}

func (s *OpenAIGatewayService) handleNonStreamingResponsePassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	originalModel string,
	mappedModel string,
) (*openaiNonStreamingResultPassthrough, error) {
	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	if failoverErr := s.newOpenAIPoolEmbeddedFailoverError(ctx, c, account, resp, body, mappedModel, true); failoverErr != nil {
		return nil, failoverErr
	}

	// Detect SSE responses from upstream and convert to JSON.
	// Some upstreams (e.g. other sub2api instances) may return SSE even when
	// stream=false was requested. Without this conversion the client would
	// receive raw SSE text or a terminal event with empty output.
	if isEventStreamResponse(resp.Header) {
		return s.handlePassthroughSSEToJSON(resp, c, body, originalModel, mappedModel)
	}

	usage := &OpenAIUsage{}
	usageParsed := false
	if len(body) > 0 {
		if parsedUsage, ok := extractOpenAIUsageFromJSONBytes(body); ok {
			*usage = parsedUsage
			usageParsed = true
		}
	}
	if !usageParsed {
		// 兜底：尝试从 SSE 文本中解析 usage
		usage = s.parseSSEUsageFromBody(string(body))
	}

	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	if originalModel != "" && mappedModel != "" && originalModel != mappedModel {
		body = s.replaceModelInResponseBody(body, mappedModel, originalModel)
	}
	if !writeOpenAICompactSSEBridge(c, resp.StatusCode, body) {
		c.Data(resp.StatusCode, contentType, body)
	}
	return &openaiNonStreamingResultPassthrough{
		OpenAIUsage:      usage,
		usage:            usage,
		responseID:       extractOpenAIResponseIDFromJSONBytes(body),
		imageCount:       countOpenAIResponseImageOutputsFromJSONBytes(body),
		imageOutputSizes: collectOpenAIResponseImageOutputSizesFromJSONBytes(body),
	}, nil
}

// handlePassthroughSSEToJSON converts an SSE response body into a JSON
// response for the passthrough path. It mirrors handleSSEToJSON while
// preserving passthrough payloads, except compact-only model remapping may
// rewrite model fields back to the original requested model.
func (s *OpenAIGatewayService) handlePassthroughSSEToJSON(resp *http.Response, c *gin.Context, body []byte, originalModel string, mappedModel string) (*openaiNonStreamingResultPassthrough, error) {
	bodyText := string(body)
	finalResponse, ok := extractCodexFinalResponse(bodyText)

	usage := &OpenAIUsage{}
	if ok {
		if parsedUsage, parsed := extractOpenAIUsageFromJSONBytes(finalResponse); parsed {
			*usage = parsedUsage
		}
		// When the terminal event has an empty output array, reconstruct
		// output from accumulated delta events so the client gets full content.
		if len(gjson.GetBytes(finalResponse, "output").Array()) == 0 {
			if outputJSON, reconstructed := reconstructResponseOutputFromSSE(bodyText); reconstructed {
				if patched, err := sjson.SetRawBytes(finalResponse, "output", outputJSON); err == nil {
					finalResponse = patched
				}
			}
		}
		finalResponse = supplementCompactionItemFromSSE(c, finalResponse, bodyText)
		body = finalResponse
		if originalModel != "" && mappedModel != "" && originalModel != mappedModel {
			body = s.replaceModelInResponseBody(body, mappedModel, originalModel)
		}
		// Correct tool calls in final response
		body = s.correctToolCallsInResponseBody(body)
	} else {
		terminalType, terminalPayload, terminalOK := extractOpenAISSETerminalEvent(bodyText)
		if terminalOK && terminalType == "response.failed" {
			msg := extractOpenAISSEErrorMessage(terminalPayload)
			if msg == "" {
				msg = "Upstream compact response failed"
			}
			return nil, s.writeOpenAINonStreamingProtocolError(resp, c, msg)
		}
		usage = s.parseSSEUsageFromBody(bodyText)
		if originalModel != "" && mappedModel != "" && originalModel != mappedModel {
			bodyText = s.replaceModelInSSEBody(bodyText, mappedModel, originalModel)
		}
		body = []byte(bodyText)
	}

	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	contentType := "application/json; charset=utf-8"
	if !ok {
		contentType = resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "text/event-stream"
		}
	}
	if !writeOpenAICompactSSEBridge(c, resp.StatusCode, body) {
		c.Data(resp.StatusCode, contentType, body)
	}

	return &openaiNonStreamingResultPassthrough{
		OpenAIUsage:      usage,
		usage:            usage,
		responseID:       extractOpenAIResponseIDFromJSONBytes(body),
		imageCount:       countOpenAIImageOutputsFromSSEBody(bodyText),
		imageOutputSizes: collectOpenAIImageOutputSizesFromSSEBody(bodyText),
	}, nil
}

func writeOpenAIPassthroughResponseHeaders(dst http.Header, src http.Header, filter *responseheaders.CompiledHeaderFilter) {
	if dst == nil || src == nil {
		return
	}
	if filter != nil {
		responseheaders.WriteFilteredHeaders(dst, src, filter)
	} else {
		// 兜底：尽量保留最基础的 content-type
		if v := strings.TrimSpace(src.Get("Content-Type")); v != "" {
			dst.Set("Content-Type", v)
		}
	}
	// 透传模式强制放行 x-codex-* 响应头（若上游返回）。
	// 注意：真实 http.Response.Header 的 key 一般会被 canonicalize；但为了兼容测试/自建响应，
	// 这里用 EqualFold 做一次大小写不敏感的查找。
	getCaseInsensitiveValues := func(h http.Header, want string) []string {
		if h == nil {
			return nil
		}
		for k, vals := range h {
			if strings.EqualFold(k, want) {
				return vals
			}
		}
		return nil
	}

	for _, rawKey := range []string{
		"x-codex-primary-used-percent",
		"x-codex-primary-reset-after-seconds",
		"x-codex-primary-window-minutes",
		"x-codex-secondary-used-percent",
		"x-codex-secondary-reset-after-seconds",
		"x-codex-secondary-window-minutes",
		"x-codex-primary-over-secondary-limit-percent",
	} {
		vals := getCaseInsensitiveValues(src, rawKey)
		if len(vals) == 0 {
			continue
		}
		key := http.CanonicalHeaderKey(rawKey)
		dst.Del(key)
		for _, v := range vals {
			dst.Add(key, v)
		}
	}
}

func (s *OpenAIGatewayService) buildUpstreamRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token string, isStream bool, promptCacheKey string, isCodexCLI bool) (*http.Request, error) {
	// Determine target URL based on account type
	var targetURL string
	switch account.Type {
	case AccountTypeOAuth:
		// OAuth accounts use ChatGPT internal API
		targetURL = chatgptCodexURL
	case AccountTypeAPIKey:
		// API Key accounts use Platform API or custom base URL
		baseURL := account.GetOpenAIBaseURL()
		if baseURL == "" {
			targetURL = openaiPlatformAPIURL
		} else {
			validatedURL, err := s.validateUpstreamBaseURL(baseURL)
			if err != nil {
				return nil, err
			}
			targetURL = buildOpenAIResponsesURL(validatedURL)
		}
	default:
		targetURL = openaiPlatformAPIURL
	}
	targetURL = appendOpenAIResponsesRequestPathSuffix(targetURL, openAIResponsesRequestPathSuffix(c))

	ctx = WithOpsLatencyRecorder(ctx, func(key string, elapsed time.Duration) {
		SetOpsLatencyMsOnce(c, key, elapsed.Milliseconds())
	})
	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	// Set authentication header
	req.Header.Set("authorization", "Bearer "+token)

	// Set headers specific to OAuth accounts (ChatGPT internal API)
	if account.Type == AccountTypeOAuth {
		credentialAccount := account
		if account.IsShadow() {
			resolved, err := resolveCredentialAccount(ctx, s.accountRepo, account)
			if err != nil {
				return nil, fmt.Errorf("resolve chatgpt account headers: %w", err)
			}
			credentialAccount = resolved
		}
		// Required: set Host for ChatGPT API (must use req.Host, not Header.Set)
		req.Host = "chatgpt.com"
		// Required: set chatgpt-account-id header
		chatgptAccountID := credentialAccount.GetChatGPTAccountID()
		if chatgptAccountID != "" {
			req.Header.Set("chatgpt-account-id", chatgptAccountID)
		}
	}

	// Whitelist passthrough headers
	for key, values := range c.Request.Header {
		lowerKey := strings.ToLower(key)
		if openaiAllowedHeaders[lowerKey] {
			for _, v := range values {
				req.Header.Add(key, v)
			}
		}
	}
	if account.Type == AccountTypeOAuth {
		compatMessagesBridge := isOpenAICompatMessagesBridgeContext(c) || isOpenAICompatMessagesBridgeBody(body)
		// 清除客户端透传的 session 头，后续用隔离后的值重新设置，防止跨用户会话碰撞。
		clientConversationID := strings.TrimSpace(req.Header.Get("conversation_id"))
		req.Header.Del("conversation_id")
		req.Header.Del("session_id")

		if compatMessagesBridge {
			req.Header.Del("OpenAI-Beta")
			req.Header.Del("originator")
		} else {
			req.Header.Set("OpenAI-Beta", "responses=experimental")
			req.Header.Set("originator", resolveOpenAIUpstreamOriginator(c, isCodexCLI))
		}
		apiKeyID := getAPIKeyIDFromContext(c)
		if isOpenAIResponsesCompactPath(c) {
			req.Header.Set("accept", "application/json")
			if req.Header.Get("version") == "" {
				req.Header.Set("version", codexCLIVersion)
			}
			compactSession := resolveOpenAICompactSessionID(c)
			req.Header.Set("session_id", isolateOpenAISessionID(apiKeyID, compactSession))
		} else {
			req.Header.Set("accept", "text/event-stream")
		}
		if promptCacheKey != "" {
			isolated := isolateOpenAISessionID(apiKeyID, promptCacheKey)
			req.Header.Set("session_id", isolated)
			if !compatMessagesBridge || clientConversationID != "" {
				req.Header.Set("conversation_id", isolated)
			}
		}
	}

	// Apply custom User-Agent if configured
	customUA := account.GetOpenAIUserAgent()
	if customUA != "" {
		req.Header.Set("user-agent", customUA)
	}

	// 若开启 ForceCodexCLI，则强制将上游 User-Agent 伪装为 Codex CLI。
	// 用于网关未透传/改写 User-Agent 时，仍能命中 Codex 侧识别逻辑。
	if s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		req.Header.Set("user-agent", codexCLIUserAgent)
	}

	// 浏览器型 UA 兜底：仅 OAuth（ChatGPT 内部接口）账号生效，若最终 user-agent 仍为浏览器
	// （Chrome/Firefox/Safari/Edge 等），替换为后台配置的 Codex UA，避免 Cloudflare 触发 JS 质询。
	s.overrideBrowserUserAgent(ctx, account, req)

	// 终态收口：originator 必须与最终 User-Agent 首段配套且为官方身份，否则上游可能拒绝。
	if account.Type == AccountTypeOAuth {
		enforceCodexIdentityHeaders(req.Header)
	}
	if account.IsOpenAIUpstreamStrongIsolationEnabled() {
		applyOpenAIUpstreamStrongIsolationHeaders(req)
	}

	// 账号级请求头覆写（仅 openai api_key 账号启用时生效；OAuth 路径 no-op）
	account.ApplyHeaderOverrides(req.Header)
	if isOpenAIResponsesCompactPath(c) {
		req.Header.Set("accept", "application/json")
	}

	// Ensure required headers exist
	if req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}

	return req, nil
}

// overrideBrowserUserAgent 检查请求的最终 user-agent，若为浏览器 UA 则替换为后台配置的 Codex UA。
// 用于规避 Cloudflare 对浏览器型 UA 在 ChatGPT 内部接口上的访问质询。
// 影响范围严格限定：仅 OAuth（Codex/ChatGPT 内部接口）账号生效；API Key 等其他账号原样透传。
// 仅在识别为浏览器（Mozilla/...）时改写，其他 CLI/工具 UA 不动。
func (s *OpenAIGatewayService) overrideBrowserUserAgent(ctx context.Context, account *Account, req *http.Request) {
	if req == nil || account == nil {
		return
	}
	if account.Type != AccountTypeOAuth {
		return
	}
	currentUA := req.Header.Get("user-agent")
	if !openai.IsBrowserUserAgent(currentUA) {
		return
	}
	codexUA := DefaultOpenAICodexUserAgent
	if s != nil && s.settingService != nil {
		if v := strings.TrimSpace(s.settingService.GetOpenAICodexUserAgent(ctx)); v != "" {
			codexUA = v
		}
	}
	req.Header.Set("user-agent", codexUA)
}

func (s *OpenAIGatewayService) overrideBrowserUserAgentHeader(ctx context.Context, account *Account, headers http.Header) {
	if headers == nil || account == nil {
		return
	}
	if account.Type != AccountTypeOAuth {
		return
	}
	currentUA := headers.Get("user-agent")
	if !openai.IsBrowserUserAgent(currentUA) {
		return
	}
	codexUA := DefaultOpenAICodexUserAgent
	if s != nil && s.settingService != nil {
		if v := strings.TrimSpace(s.settingService.GetOpenAICodexUserAgent(ctx)); v != "" {
			codexUA = v
		}
	}
	headers.Set("user-agent", codexUA)
}

func (s *OpenAIGatewayService) handleErrorResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	requestBody []byte,
	requestedModel ...string,
) (*OpenAIForwardResult, error) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)
	logOpenAIInstructionsRequiredDebug(ctx, c, account, resp.StatusCode, upstreamMsg, requestBody, body)
	if decision := s.handleOpenAICyberPolicyEvent(c, account, false, resp.Header.Get("x-request-id"), body, requestBody); decision.Matched {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		statusCode := resp.StatusCode
		if statusCode < 400 {
			statusCode = http.StatusForbidden
		}
		if len(body) > 0 && gjson.ValidBytes(body) {
			contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
			if contentType == "" {
				contentType = "application/json; charset=utf-8"
			}
			MarkResponseCommitted(c)
			c.Data(statusCode, contentType, body)
		} else {
			MarkResponseCommitted(c)
			c.JSON(statusCode, gin.H{
				"error": gin.H{
					"type":    firstNonEmptyString(decision.ErrorType, "invalid_request_error"),
					"code":    "cyber_policy",
					"message": firstNonEmptyString(decision.Message, "upstream cyber_policy blocked this request"),
				},
			})
		}
		return nil, fmt.Errorf("upstream cyber_policy blocked request")
	}

	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		logger.LegacyPrintf("service.openai_gateway",
			"OpenAI upstream error %d (account=%d platform=%s type=%s): %s",
			resp.StatusCode,
			account.ID,
			account.Platform,
			account.Type,
			truncateForLog(body, s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes),
		)
	}

	if status, errType, errMsg, matched := applyErrorPassthroughRule(
		c,
		PlatformOpenAI,
		resp.StatusCode,
		body,
		http.StatusBadGateway,
		"upstream_error",
		"Upstream request failed",
	); matched {
		MarkResponseCommitted(c)
		c.JSON(status, gin.H{
			"error": gin.H{
				"type":    errType,
				"message": errMsg,
			},
		})
		if upstreamMsg == "" {
			upstreamMsg = errMsg
		}
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (passthrough rule matched)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (passthrough rule matched) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Check custom error codes
	if !account.ShouldHandleErrorCode(resp.StatusCode) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "http_error",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		MarkResponseCommitted(c)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"type":    "upstream_error",
				"message": "Upstream gateway error",
			},
		})
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (not in custom error codes)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (not in custom error codes) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Handle upstream error (mark account status)
	var reqModel string
	if len(requestedModel) > 0 {
		reqModel = strings.TrimSpace(requestedModel[0])
	}
	if reqModel == "" {
		reqModel, _, _ = extractOpenAIRequestMetaFromBody(requestBody)
	}
	shouldDisable := s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, reqModel)
	kind := "http_error"
	if shouldDisable {
		kind = "failover"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               kind,
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})
	if shouldDisable {
		decision := s.classifyOpenAIPoolFailover(ctx, account, resp.StatusCode, upstreamMsg, body)
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           body,
			RetryableOnSameAccount: decision.RetryableOnSameAccount,
			SkipPoolSoftCooldown:   decision.SkipSoftCooldown,
		}
	}

	MarkResponseCommitted(c)

	// Return appropriate error response
	var errType, errMsg string
	var statusCode int

	switch resp.StatusCode {
	case 401:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream authentication failed, please contact administrator"
	case 402:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream payment required: insufficient balance or billing issue"
	case 403:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream access forbidden, please contact administrator"
	case 429:
		statusCode = http.StatusTooManyRequests
		errType = "rate_limit_error"
		errMsg = "Upstream rate limit exceeded, please retry later"
	default:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream request failed"
	}

	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": errMsg,
		},
	})

	if upstreamMsg == "" {
		return nil, fmt.Errorf("upstream error: %d", resp.StatusCode)
	}
	return nil, fmt.Errorf("upstream error: %d message=%s", resp.StatusCode, upstreamMsg)
}

// compatErrorWriter is the signature for format-specific error writers used by
// the compat paths (Chat Completions and Anthropic Messages).
type compatErrorWriter func(c *gin.Context, statusCode int, errType, message string)

// handleCompatErrorResponse is the shared non-failover error handler for the
// Chat Completions and Anthropic Messages compat paths. It mirrors the logic of
// handleErrorResponse (passthrough rules, ShouldHandleErrorCode, rate-limit
// tracking, secondary failover) but delegates the final error write to the
// format-specific writer function.
func (s *OpenAIGatewayService) handleCompatErrorResponse(
	resp *http.Response,
	c *gin.Context,
	account *Account,
	writeError compatErrorWriter,
	requestedModel ...string,
) (*OpenAIForwardResult, error) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	if upstreamMsg == "" {
		upstreamMsg = fmt.Sprintf("Upstream error: %d", resp.StatusCode)
	}
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)

	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)
	if account != nil && account.IsOpenAI() && account.IsPoolMode() &&
		s.IsOpenAIPoolDownstreamModelLimitProtectionEnabled(c.Request.Context()) &&
		isOpenAIPoolModelRoutingError(resp.StatusCode, upstreamMsg, body) {
		modelForCooldown := ""
		if len(requestedModel) > 0 {
			modelForCooldown = requestedModel[0]
		}
		decision := s.classifyOpenAIPoolFailover(c.Request.Context(), account, resp.StatusCode, upstreamMsg, body)
		s.handleOpenAIAccountUpstreamError(
			c.Request.Context(), account, resp.StatusCode, resp.Header, body, modelForCooldown,
		)
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "failover",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           body,
			Message:                upstreamMsg,
			ProbeModel:             strings.TrimSpace(modelForCooldown),
			ProbeKind:              openAIPoolProbeKindForModel(modelForCooldown),
			RetryableOnSameAccount: decision.RetryableOnSameAccount,
			SkipPoolSoftCooldown:   decision.SkipSoftCooldown,
		}
	}
	if decision := s.handleOpenAICyberPolicyEvent(c, account, false, resp.Header.Get("x-request-id"), body, nil); decision.Matched {
		MarkResponseCommitted(c)
		statusCode := resp.StatusCode
		if statusCode < 400 {
			statusCode = http.StatusForbidden
		}
		writeError(c, statusCode, firstNonEmptyString(decision.ErrorType, "invalid_request_error"), firstNonEmptyString(decision.Message, "upstream cyber_policy blocked this request"))
		return nil, fmt.Errorf("upstream cyber_policy blocked request")
	}

	// Apply error passthrough rules
	if status, errType, errMsg, matched := applyErrorPassthroughRule(
		c, account.Platform, resp.StatusCode, body,
		http.StatusBadGateway, "api_error", "Upstream request failed",
	); matched {
		MarkResponseCommitted(c)
		writeError(c, status, errType, errMsg)
		if upstreamMsg == "" {
			upstreamMsg = errMsg
		}
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (passthrough rule matched)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (passthrough rule matched) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Check custom error codes — if the account does not handle this status,
	// return a generic error without exposing upstream details.
	if !account.ShouldHandleErrorCode(resp.StatusCode) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "http_error",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		MarkResponseCommitted(c)
		writeError(c, http.StatusInternalServerError, "api_error", "Upstream gateway error")
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (not in custom error codes)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (not in custom error codes) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Track rate limits and decide whether to trigger secondary failover.
	var modelForCooldown string
	if len(requestedModel) > 0 {
		modelForCooldown = requestedModel[0]
	}
	shouldDisable := s.handleOpenAIAccountUpstreamError(
		c.Request.Context(), account, resp.StatusCode, resp.Header, body, modelForCooldown,
	)
	kind := "http_error"
	if shouldDisable {
		kind = "failover"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               kind,
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})
	if shouldDisable {
		decision := s.classifyOpenAIPoolFailover(c.Request.Context(), account, resp.StatusCode, upstreamMsg, body)
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           body,
			RetryableOnSameAccount: decision.RetryableOnSameAccount,
			SkipPoolSoftCooldown:   decision.SkipSoftCooldown,
		}
	}

	MarkResponseCommitted(c)

	// Map status code to error type and write response
	errType := "api_error"
	switch {
	case resp.StatusCode == 400:
		errType = "invalid_request_error"
	case resp.StatusCode == 404:
		errType = "not_found_error"
	case resp.StatusCode == 429:
		errType = "rate_limit_error"
	case resp.StatusCode >= 500:
		errType = "api_error"
	}

	writeError(c, resp.StatusCode, errType, upstreamMsg)
	return nil, fmt.Errorf("upstream error: %d %s", resp.StatusCode, upstreamMsg)
}

// openaiStreamingResult streaming response result
type openaiStreamingResult struct {
	usage            *OpenAIUsage
	firstTokenMs     *int
	responseID       string
	imageCount       int
	imageOutputSizes []string
}

type openaiNonStreamingResult struct {
	*OpenAIUsage
	usage            *OpenAIUsage
	responseID       string
	imageCount       int
	imageOutputSizes []string
}

func (s *OpenAIGatewayService) handleStreamingResponse(ctx context.Context, resp *http.Response, c *gin.Context, account *Account, startTime time.Time, originalModel, mappedModel string, requestFirstTokenPlaceholderSentOpt ...bool) (*openaiStreamingResult, error) {
	requestFirstTokenPlaceholderSent := len(requestFirstTokenPlaceholderSentOpt) > 0 && requestFirstTokenPlaceholderSentOpt[0]
	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}

	// Set SSE response headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	// Pass through other headers
	if v := resp.Header.Get("x-request-id"); v != "" {
		c.Header("x-request-id", v)
	}

	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}
	accountSSECommentPreflush := s.openAIStreamSSECommentPreflushEnabled(account, originalModel)
	accountSSECommentPreflushed := requestFirstTokenPlaceholderSent
	if accountSSECommentPreflush && !requestFirstTokenPlaceholderSent {
		if _, err := w.Write([]byte(":\n\n")); err == nil {
			flusher.Flush()
			accountSSECommentPreflushed = true
			SetOpsLatencyMsOnce(c, OpsFirstClientFlushMsKey, time.Since(startTime).Milliseconds())
		}
	}
	lowLatencyPolicy := s.openAIStreamLowLatencyPolicy(account, originalModel)
	if !accountSSECommentPreflushed && lowLatencyPolicy.Enabled && lowLatencyPolicy.Barrier <= 0 && lowLatencyPolicy.AllowBootstrapComment {
		if _, err := w.Write([]byte(":\n\n")); err == nil {
			flusher.Flush()
			SetOpsLatencyMsOnce(c, OpsFirstClientFlushMsKey, time.Since(startTime).Milliseconds())
		}
	}
	bufferedWriter := bufio.NewWriterSize(w, 4*1024)
	flushBuffered := func() error {
		if err := bufferedWriter.Flush(); err != nil {
			return err
		}
		flusher.Flush()
		SetOpsLatencyMsOnce(c, OpsFirstClientFlushMsKey, time.Since(startTime).Milliseconds())
		return nil
	}

	usage := &OpenAIUsage{}
	imageCounter := newOpenAIImageOutputCounter()
	var firstTokenMs *int
	responseID := ""
	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanBuf := getSSEScannerBuf64K()
	scanner.Buffer(scanBuf[:0], maxLineSize)

	streamInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	// 仅监控上游数据间隔超时，不被下游写入阻塞影响
	var intervalTicker *time.Ticker
	if streamInterval > 0 {
		intervalTicker = time.NewTicker(streamInterval)
		defer intervalTicker.Stop()
	}
	var intervalCh <-chan time.Time
	if intervalTicker != nil {
		intervalCh = intervalTicker.C
	}

	keepaliveInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}
	// 下游 keepalive 仅用于防止代理空闲断开
	var keepaliveTicker *time.Ticker
	if keepaliveInterval > 0 {
		keepaliveTicker = time.NewTicker(keepaliveInterval)
		defer keepaliveTicker.Stop()
	}
	var keepaliveCh <-chan time.Time
	if keepaliveTicker != nil {
		keepaliveCh = keepaliveTicker.C
	}
	// Track downstream writes separately from upstream reads: pre-output failover
	// can buffer response.created / response.in_progress, so keepalive must be
	// based on downstream idle time.
	lastDownstreamWriteAt := time.Now()

	// 仅发送一次错误事件，避免多次写入导致协议混乱。
	// 注意：OpenAI `/v1/responses` streaming 事件必须符合 OpenAI Responses schema；
	// 否则下游 SDK（例如 OpenCode）会因为类型校验失败而报错。
	errorEventSent := false
	clientDisconnected := false // 客户端断开后继续 drain 上游以收集 usage
	sawTerminalEvent := false
	sawFailedEvent := false
	sawCyberPolicyEvent := false
	failedMessage := ""
	clientOutputStarted := accountSSECommentPreflushed || requestFirstTokenPlaceholderSent
	upstreamRequestID := strings.TrimSpace(resp.Header.Get("x-request-id"))
	flushPreamble := s.openAIStreamPreambleFlushEnabled(account, originalModel)
	safeTokenPlaceholder := s.openAIStreamSafeTokenPlaceholderEnabled(account, originalModel)
	firstTokenTimeoutPlaceholder := s.openAIStreamFirstTokenTimeoutPlaceholder(account, originalModel)
	safeTokenPlaceholderSent := requestFirstTokenPlaceholderSent
	safeTokenPlaceholderPending := false
	firstTokenTimeoutPlaceholderSent := requestFirstTokenPlaceholderSent
	firstTokenTimeoutPlaceholderPending := false
	firstTokenTimeoutGuardSampleRecorded := false
	var streamFailoverErr error
	atSSEEventBoundary := true
	sendErrorEvent := func(reason string) {
		if errorEventSent || clientDisconnected {
			return
		}
		errorEventSent = true
		payload := `{"type":"error","sequence_number":0,"error":{"type":"upstream_error","message":` + strconv.Quote(reason) + `,"code":` + strconv.Quote(reason) + `}}`
		if err := flushBuffered(); err != nil {
			clientDisconnected = true
			return
		}
		if _, err := bufferedWriter.WriteString("data: " + payload + "\n\n"); err != nil {
			clientDisconnected = true
			return
		}
		if err := flushBuffered(); err != nil {
			clientDisconnected = true
			return
		}
		clientOutputStarted = true
		lastDownstreamWriteAt = time.Now()
	}
	writeSafeTokenPlaceholder := func() {
		if !safeTokenPlaceholder || safeTokenPlaceholderSent || clientDisconnected || !atSSEEventBoundary {
			return
		}
		if _, err := bufferedWriter.WriteString(openAIResponsesSafeTokenPlaceholderFrame(responseID)); err != nil {
			clientDisconnected = true
			return
		}
		if err := flushBuffered(); err != nil {
			clientDisconnected = true
			return
		}
		safeTokenPlaceholderSent = true
		clientOutputStarted = true
		lastDownstreamWriteAt = time.Now()
		if firstTokenMs == nil {
			ms := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &ms
		}
	}
	writeFirstTokenTimeoutPlaceholder := func() bool {
		if firstTokenTimeoutPlaceholder <= 0 || firstTokenTimeoutPlaceholderSent || clientDisconnected || firstTokenMs != nil || streamFailoverErr != nil || !atSSEEventBoundary {
			return false
		}
		if _, err := bufferedWriter.WriteString(openAIResponsesSafeTokenPlaceholderFrame(responseID)); err != nil {
			clientDisconnected = true
			return true
		}
		if err := flushBuffered(); err != nil {
			clientDisconnected = true
			return true
		}
		firstTokenTimeoutPlaceholderSent = true
		safeTokenPlaceholderSent = true
		clientOutputStarted = true
		lastDownstreamWriteAt = time.Now()
		return true
	}
	drainFirstTokenTimeoutPlaceholderPending := func() {
		if !firstTokenTimeoutPlaceholderPending {
			return
		}
		firstTokenTimeoutPlaceholderPending = false
		if !writeFirstTokenTimeoutPlaceholder() && !clientDisconnected && firstTokenMs == nil && !firstTokenTimeoutPlaceholderSent && streamFailoverErr == nil {
			firstTokenTimeoutPlaceholderPending = true
		}
	}
	recordFirstTokenTimeoutGuardSample := func() {
		if firstTokenTimeoutGuardSampleRecorded {
			return
		}
		firstTokenTimeoutGuardSampleRecorded = true
		s.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, originalModel, int(time.Since(startTime).Milliseconds()))
	}

	needModelReplace := originalModel != mappedModel
	resultWithUsage := func() *openaiStreamingResult {
		return &openaiStreamingResult{
			usage:            usage,
			firstTokenMs:     firstTokenMs,
			responseID:       responseID,
			imageCount:       imageCounter.Count(),
			imageOutputSizes: imageCounter.Sizes(),
		}
	}
	finalizeStream := func() (*openaiStreamingResult, error) {
		if !sawTerminalEvent {
			if !openAIStreamClientOutputStarted(c, clientOutputStarted) {
				return resultWithUsage(), s.newOpenAIStreamFailoverError(
					c,
					account,
					false,
					upstreamRequestID,
					nil,
					"OpenAI stream ended before a terminal event",
				)
			}
			return resultWithUsage(), fmt.Errorf("stream usage incomplete: missing terminal event")
		}
		if sawFailedEvent && !sawCyberPolicyEvent {
			return resultWithUsage(), fmt.Errorf("upstream response failed: %s", failedMessage)
		}
		if !clientDisconnected {
			hadBufferedData := bufferedWriter.Buffered() > 0
			if err := flushBuffered(); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "Client disconnected during final flush, returning collected usage")
			} else if hadBufferedData {
				clientOutputStarted = true
				lastDownstreamWriteAt = time.Now()
			}
		}
		return resultWithUsage(), nil
	}
	handleScanErr := func(scanErr error) (*openaiStreamingResult, error, bool) {
		if scanErr == nil {
			return nil, nil, false
		}
		if sawTerminalEvent && !sawFailedEvent {
			logger.LegacyPrintf("service.openai_gateway", "Upstream scan ended after terminal event: %v", scanErr)
			return resultWithUsage(), nil, true
		}
		if sawFailedEvent && !sawCyberPolicyEvent {
			return resultWithUsage(), fmt.Errorf("upstream response failed: %s", failedMessage), true
		}
		// 客户端断开/取消请求时，上游读取往往会返回 context canceled。
		// /v1/responses 的 SSE 事件必须符合 OpenAI 协议；这里不注入自定义 error event，避免下游 SDK 解析失败。
		if errors.Is(scanErr, context.Canceled) || errors.Is(scanErr, context.DeadlineExceeded) {
			return resultWithUsage(), fmt.Errorf("stream usage incomplete: %w", scanErr), true
		}
		if errors.Is(scanErr, bufio.ErrTooLong) {
			logger.LegacyPrintf("service.openai_gateway", "SSE line too long: account=%d max_size=%d error=%v", account.ID, maxLineSize, scanErr)
			sendErrorEvent("response_too_large")
			return resultWithUsage(), scanErr, true
		}
		if !openAIStreamClientOutputStarted(c, clientOutputStarted) {
			msg := "OpenAI stream disconnected before completion"
			if errText := strings.TrimSpace(scanErr.Error()); errText != "" {
				msg += ": " + errText
			}
			return resultWithUsage(), s.newOpenAIStreamFailoverError(c, account, false, upstreamRequestID, nil, msg), true
		}
		// 客户端已断开时，上游出错仅影响体验，不影响计费；返回已收集 usage
		if clientDisconnected {
			return resultWithUsage(), fmt.Errorf("stream usage incomplete after disconnect: %w", scanErr), true
		}
		sendErrorEvent("stream_read_error")
		return resultWithUsage(), fmt.Errorf("stream read error: %w", scanErr), true
	}
	processSSELine := func(line string, queueDrained bool) {
		if streamFailoverErr != nil {
			return
		}
		defer func() {
			atSSEEventBoundary = line == ""
			if atSSEEventBoundary {
				drainFirstTokenTimeoutPlaceholderPending()
			}
		}()
		// Extract data from SSE line (supports both "data: " and "data:" formats)
		if data, ok := extractOpenAISSEDataLine(line); ok {

			// Replace model in response if needed.
			// Fast path: most events do not contain model field values.
			if needModelReplace && mappedModel != "" && strings.Contains(data, mappedModel) {
				line = s.replaceModelInSSELine(line, mappedModel, originalModel)
			}

			dataBytes := []byte(data)
			if openAIStreamEventIsTerminal(data) {
				sawTerminalEvent = true
			}
			eventType := strings.TrimSpace(gjson.GetBytes(dataBytes, "type").String())
			forceFlushFailedEvent := false
			if eventType == "response.failed" {
				failedMessage = extractOpenAISSEErrorMessage(dataBytes)
				s.parseSSEUsageBytes(dataBytes, usage)
				cyberPolicyMatched := false
				if decision := s.handleOpenAICyberPolicyEvent(c, account, false, upstreamRequestID, dataBytes, nil); decision.Matched {
					failedMessage = firstNonEmptyString(decision.Message, failedMessage)
					sawCyberPolicyEvent = true
					cyberPolicyMatched = true
				}
				if !cyberPolicyMatched && !openAIStreamClientOutputStarted(c, clientOutputStarted) {
					if status, errType, errMsg, matched := applyOpenAIStreamFailedErrorPassthroughRule(c, account.Platform, dataBytes, failedMessage); matched {
						sawFailedEvent = true
						s.recordOpenAIStreamUpstreamError(c, account, false, upstreamRequestID, "http_error", dataBytes, failedMessage)
						MarkResponseCommitted(c)
						c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
						c.JSON(status, gin.H{
							"error": gin.H{
								"type":    errType,
								"message": errMsg,
							},
						})
						streamFailoverErr = fmt.Errorf("upstream response failed: passthrough rule matched message=%s", errMsg)
						return
					}
					if openAIStreamFailedEventShouldFailover(dataBytes, failedMessage) {
						sawFailedEvent = true
						streamFailoverErr = s.newOpenAIStreamFailoverError(c, account, false, upstreamRequestID, dataBytes, failedMessage)
						return
					}
				}
				forceFlushFailedEvent = true
				sawFailedEvent = true
			}
			if sanitizedData, sanitized := sanitizeOpenAIResponseFailedEventForClient(dataBytes, eventType, openAIStreamClientOutputStarted(c, clientOutputStarted)); sanitized {
				dataBytes = sanitizedData
				data = string(sanitizedData)
				line = "data: " + data
			}
			if responseID == "" {
				responseID = extractOpenAIResponseIDFromJSONBytes(dataBytes)
			}
			imageCounter.AddSSEData(dataBytes)
			safeTokenPlaceholderPending = safeTokenPlaceholderPending || eventType == "response.created"

			// Correct Codex tool calls if needed (apply_patch -> edit, etc.)
			if correctedData, corrected := s.toolCorrector.CorrectToolCallsInSSEBytes(dataBytes); corrected {
				dataBytes = correctedData
				data = string(correctedData)
				line = "data: " + data
				eventType = strings.TrimSpace(gjson.GetBytes(dataBytes, "type").String())
			}
			startsFirstToken := forceFlushFailedEvent || openAIStreamDataStartsClientOutput(data, eventType)
			startsRealOutput := openAIStreamDataStartsRealOutput(data, eventType)
			startsClientOutput := startsFirstToken || openAIStreamDataStartsClientOutputWithPreambleFlush(data, eventType, flushPreamble)

			// 写入客户端（客户端断开后继续 drain 上游）
			if !clientDisconnected {
				shouldFlush := clientOutputStarted || startsClientOutput
				if firstTokenMs == nil && startsFirstToken {
					// 保证首个 token 事件尽快出站，避免影响 TTFT。
					shouldFlush = true
				}
				if _, err := bufferedWriter.WriteString(line); err != nil {
					clientDisconnected = true
					logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming, continuing to drain upstream for billing")
				} else if _, err := bufferedWriter.WriteString("\n"); err != nil {
					clientDisconnected = true
					logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming, continuing to drain upstream for billing")
				} else if shouldFlush {
					if err := flushBuffered(); err != nil {
						clientDisconnected = true
						logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming flush, continuing to drain upstream for billing")
					} else {
						clientOutputStarted = true
						lastDownstreamWriteAt = time.Now()
					}
				}
			}

			// Record first token time
			if startsRealOutput {
				recordFirstTokenTimeoutGuardSample()
			}
			if startsFirstToken {
				if firstTokenMs == nil {
					ms := int(time.Since(startTime).Milliseconds())
					firstTokenMs = &ms
				}
			}
			s.parseSSEUsageBytes(dataBytes, usage)
			return
		}

		// Forward non-data lines as-is
		if !clientDisconnected {
			if _, err := bufferedWriter.WriteString(line); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming, continuing to drain upstream for billing")
			} else if _, err := bufferedWriter.WriteString("\n"); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming, continuing to drain upstream for billing")
			} else if clientOutputStarted && (line == "" || queueDrained) {
				if err := flushBuffered(); err != nil {
					clientDisconnected = true
					logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming flush, continuing to drain upstream for billing")
				} else {
					clientOutputStarted = true
					lastDownstreamWriteAt = time.Now()
				}
			}
			if line == "" && safeTokenPlaceholderPending {
				safeTokenPlaceholderPending = false
				atSSEEventBoundary = true
				writeSafeTokenPlaceholder()
			}
		}
	}

	// 无超时/无 keepalive 的常见路径走同步扫描，减少 goroutine 与 channel 开销。
	if streamInterval <= 0 && keepaliveInterval <= 0 && firstTokenTimeoutPlaceholder <= 0 {
		if lowLatencyPolicy.Enabled && lowLatencyPolicy.Barrier > 0 && lowLatencyPolicy.AllowBootstrapComment {
			streamInterval = lowLatencyPolicy.Barrier
		} else {
			defer putSSEScannerBuf64K(scanBuf)
			for scanner.Scan() {
				processSSELine(scanner.Text(), true)
				if streamFailoverErr != nil {
					return resultWithUsage(), streamFailoverErr
				}
			}
			if result, err, done := handleScanErr(scanner.Err()); done {
				return result, err
			}
			return finalizeStream()
		}
	}

	type scanEvent struct {
		line string
		err  error
	}
	// 独立 goroutine 读取上游，避免读取阻塞影响 keepalive/超时处理
	events := make(chan scanEvent, 16)
	done := make(chan struct{})
	var bootstrapCh <-chan time.Time
	var bootstrapTimer *time.Timer
	if lowLatencyPolicy.Enabled && lowLatencyPolicy.Barrier > 0 && lowLatencyPolicy.AllowBootstrapComment {
		bootstrapTimer = time.NewTimer(lowLatencyPolicy.Barrier)
		bootstrapCh = bootstrapTimer.C
		defer bootstrapTimer.Stop()
	}
	firstTokenTimeoutTimer, firstTokenTimeoutCh := openAIStreamFirstTokenTimeoutTimer(startTime, firstTokenTimeoutPlaceholder)
	if firstTokenTimeoutTimer != nil {
		defer firstTokenTimeoutTimer.Stop()
	}
	writeBootstrapComment := func() {
		if clientDisconnected || openAIStreamClientOutputStarted(c, clientOutputStarted) || streamFailoverErr != nil || !atSSEEventBoundary {
			return
		}
		if _, err := bufferedWriter.WriteString(":\n\n"); err != nil {
			clientDisconnected = true
			logger.LegacyPrintf("service.openai_gateway", "Client disconnected during smart low-latency bootstrap, continuing to drain upstream for billing")
			return
		}
		if err := flushBuffered(); err != nil {
			clientDisconnected = true
			logger.LegacyPrintf("service.openai_gateway", "Client disconnected during smart low-latency bootstrap flush, continuing to drain upstream for billing")
			return
		}
		clientOutputStarted = true
		lastDownstreamWriteAt = time.Now()
	}
	sendEvent := func(ev scanEvent) bool {
		select {
		case events <- ev:
			return true
		case <-done:
			return false
		}
	}
	var lastReadAt int64
	atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
	go func(scanBuf *sseScannerBuf64K) {
		defer putSSEScannerBuf64K(scanBuf)
		defer close(events)
		for scanner.Scan() {
			atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
			if !sendEvent(scanEvent{line: scanner.Text()}) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = sendEvent(scanEvent{err: err})
		}
	}(scanBuf)
	defer close(done)

	bootstrapPending := false
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return finalizeStream()
			}
			if result, err, done := handleScanErr(ev.err); done {
				return result, err
			}
			processSSELine(ev.line, len(events) == 0)
			if streamFailoverErr != nil {
				return resultWithUsage(), streamFailoverErr
			}
			if bootstrapPending {
				writeBootstrapComment()
				if clientDisconnected || openAIStreamClientOutputStarted(c, clientOutputStarted) {
					bootstrapPending = false
				}
			}

		case <-bootstrapCh:
			bootstrapCh = nil
			writeBootstrapComment()
			if !clientDisconnected && !openAIStreamClientOutputStarted(c, clientOutputStarted) && streamFailoverErr == nil {
				bootstrapPending = true
			}

		case <-firstTokenTimeoutCh:
			firstTokenTimeoutCh = nil
			if !writeFirstTokenTimeoutPlaceholder() && !clientDisconnected && firstTokenMs == nil && !firstTokenTimeoutPlaceholderSent && streamFailoverErr == nil {
				firstTokenTimeoutPlaceholderPending = true
			}

		case <-intervalCh:
			lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
			if time.Since(lastRead) < streamInterval {
				continue
			}
			if clientDisconnected {
				return resultWithUsage(), fmt.Errorf("stream usage incomplete after timeout")
			}
			logger.LegacyPrintf("service.openai_gateway", "Stream data interval timeout: account=%d model=%s interval=%s", account.ID, originalModel, streamInterval)
			// 处理流超时，可能标记账户为临时不可调度或错误状态
			if s.rateLimitService != nil {
				s.rateLimitService.HandleStreamTimeout(ctx, account, originalModel)
			}
			sendErrorEvent("stream_timeout")
			return resultWithUsage(), fmt.Errorf("stream data interval timeout")

		case <-keepaliveCh:
			if clientDisconnected {
				continue
			}
			if time.Since(lastDownstreamWriteAt) < keepaliveInterval {
				continue
			}
			if _, err := bufferedWriter.WriteString(":\n\n"); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming, continuing to drain upstream for billing")
				continue
			}
			if err := flushBuffered(); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "Client disconnected during keepalive flush, continuing to drain upstream for billing")
			} else {
				lastDownstreamWriteAt = time.Now()
			}
		}
	}

}

// extractOpenAISSEDataLine 低开销提取 SSE `data:` 行内容。
// 兼容 `data: xxx` 与 `data:xxx` 两种格式。
func (s *OpenAIGatewayService) lowLatencyStreamHeadersEnabled() bool {
	return s.streamLowLatencyMode() != config.StreamLowLatencyModeOff
}

func (s *OpenAIGatewayService) streamLowLatencyMode() string {
	if s == nil || s.cfg == nil {
		return config.StreamLowLatencyModeOff
	}
	return s.cfg.StreamLowLatencyMode()
}

func (s *OpenAIGatewayService) aggressiveLowLatencyStreamHeadersEnabled() bool {
	return s.streamLowLatencyMode() == config.StreamLowLatencyModeAggressive
}

type openAIStreamLowLatencyPolicy struct {
	Enabled               bool
	Barrier               time.Duration
	AllowBootstrapComment bool
}

func (s *OpenAIGatewayService) openAIStreamLowLatencyPolicy(account *Account, requestedModel string) openAIStreamLowLatencyPolicy {
	mode := s.streamLowLatencyMode()
	switch mode {
	case config.StreamLowLatencyModeAggressive:
		return openAIStreamLowLatencyPolicy{Enabled: true, AllowBootstrapComment: true}
	case config.StreamLowLatencyModeSmart:
		return openAIStreamLowLatencyPolicy{
			Enabled:               true,
			Barrier:               200 * time.Millisecond,
			AllowBootstrapComment: s.openAIStreamSmartLowLatencyEligible(account, requestedModel),
		}
	default:
		return openAIStreamLowLatencyPolicy{}
	}
}

func (s *OpenAIGatewayService) openAIStreamSmartLowLatencyEligible(account *Account, requestedModel string) bool {
	if s == nil || account == nil {
		return false
	}
	if strings.TrimSpace(requestedModel) != "" && isOpenAIImageGenerationModel(requestedModel) {
		return false
	}
	if s.isOpenAIPoolAccountSoftCooling(account) || s.isOpenAIAccountRuntimeBlocked(account) {
		return false
	}
	stats := s.getOpenAIAccountRuntimeStats()
	if stats == nil {
		return true
	}
	transport := s.getOpenAIWSProtocolResolver().Resolve(account).Transport
	errorRate, ttft, hasTTFT := stats.snapshotForRoute(account.ID, requestedModel, transport)
	if errorRate > 0.05 {
		return false
	}
	return !hasTTFT || ttft <= 2500
}

func (s *OpenAIGatewayService) streamingBootstrapRetries() int {
	if s == nil || s.cfg == nil || s.cfg.Gateway.StreamingBootstrapRetries <= 0 {
		return 0
	}
	return s.cfg.Gateway.StreamingBootstrapRetries
}

func (s *OpenAIGatewayService) StreamingBootstrapRetries() int {
	return s.streamingBootstrapRetries()
}

func extractOpenAISSEDataLine(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	start := len("data:")
	for start < len(line) {
		if line[start] != ' ' && line[start] != '	' {
			break
		}
		start++
	}
	return line[start:], true
}

func extractOpenAISSEEventLine(line string) (string, bool) {
	if !strings.HasPrefix(line, "event:") {
		return "", false
	}
	start := len("event:")
	for start < len(line) {
		if line[start] != ' ' && line[start] != '	' {
			break
		}
		start++
	}
	return strings.TrimSpace(line[start:]), true
}

type openAICompatSSEFrame struct {
	EventType string
	Data      string
}

type openAICompatSSEFrameParser struct {
	eventType string
	dataLines []string
}

func (p *openAICompatSSEFrameParser) AddLine(line string) (openAICompatSSEFrame, bool) {
	if line == "" {
		return p.dispatch()
	}
	if strings.HasPrefix(line, ":") {
		return openAICompatSSEFrame{}, false
	}
	if eventType, ok := extractOpenAISSEEventLine(line); ok {
		p.eventType = eventType
		return openAICompatSSEFrame{}, false
	}
	if data, ok := extractOpenAISSEDataLine(line); ok {
		p.dataLines = append(p.dataLines, data)
	}
	return openAICompatSSEFrame{}, false
}

func (p *openAICompatSSEFrameParser) Finish() (openAICompatSSEFrame, bool) {
	return p.dispatch()
}

func (p *openAICompatSSEFrameParser) dispatch() (openAICompatSSEFrame, bool) {
	frame := openAICompatSSEFrame{
		EventType: p.eventType,
		Data:      strings.Join(p.dataLines, "\n"),
	}
	p.eventType = ""
	p.dataLines = nil
	return frame, frame.Data != ""
}

func openAICompatPayloadWithEventType(payload, eventType string) string {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" || strings.TrimSpace(payload) == "" || strings.TrimSpace(payload) == "[DONE]" {
		return payload
	}
	if gjson.Get(payload, "type").Exists() {
		return payload
	}
	patched, err := sjson.Set(payload, "type", eventType)
	if err != nil {
		return payload
	}
	return patched
}

func (s *OpenAIGatewayService) replaceModelInSSELine(line, fromModel, toModel string) string {
	data, ok := extractOpenAISSEDataLine(line)
	if !ok {
		return line
	}
	if data == "" || data == "[DONE]" {
		return line
	}

	// 使用 gjson 精确检查 model 字段，避免全量 JSON 反序列化
	if m := gjson.Get(data, "model"); m.Exists() && m.Str == fromModel {
		newData, err := sjson.Set(data, "model", toModel)
		if err != nil {
			return line
		}
		return "data: " + newData
	}

	// 检查嵌套的 response.model 字段
	if m := gjson.Get(data, "response.model"); m.Exists() && m.Str == fromModel {
		newData, err := sjson.Set(data, "response.model", toModel)
		if err != nil {
			return line
		}
		return "data: " + newData
	}

	return line
}

// correctToolCallsInResponseBody 修正响应体中的工具调用
func (s *OpenAIGatewayService) correctToolCallsInResponseBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}

	corrected, changed := s.toolCorrector.CorrectToolCallsInSSEBytes(body)
	if changed {
		return corrected
	}
	return body
}

func (s *OpenAIGatewayService) parseSSEUsage(data string, usage *OpenAIUsage) {
	s.parseSSEUsageBytes([]byte(data), usage)
}

func (s *OpenAIGatewayService) parseSSEUsageBytes(data []byte, usage *OpenAIUsage) {
	if usage == nil || len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	// 选择性解析：仅在数据中包含终止事件标识时才进入字段提取。
	if len(data) < 72 {
		return
	}
	eventType := gjson.GetBytes(data, "type").String()
	if eventType != "response.completed" && eventType != "response.done" && eventType != "response.failed" &&
		eventType != "response.incomplete" && eventType != "response.cancelled" && eventType != "response.canceled" {
		return
	}

	if parsedUsage, ok := extractOpenAIUsageFromJSONBytes(data); ok {
		*usage = parsedUsage
	}
}

func extractOpenAIUsageFromJSONBytes(body []byte) (OpenAIUsage, bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return OpenAIUsage{}, false
	}
	if usage, ok := openAIUsageFromGJSON(gjson.GetBytes(body, "usage")); ok {
		return usage, true
	}
	return openAIUsageFromGJSON(gjson.GetBytes(body, "response.usage"))
}

func isOpenAISuccessJSONResponse(resp *http.Response, body []byte) bool {
	if resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if contentType != "" && !strings.Contains(contentType, "json") && !strings.Contains(contentType, "event-stream") {
		return false
	}
	return gjson.ValidBytes(body)
}

func extractOpenAIResponseIDFromJSONBytes(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	if id := strings.TrimSpace(gjson.GetBytes(body, "id").String()); id != "" {
		return id
	}
	return strings.TrimSpace(gjson.GetBytes(body, "response.id").String())
}

func (s *OpenAIGatewayService) bindHTTPResponseAccount(ctx context.Context, c *gin.Context, account *Account, responseID string) {
	if s == nil || account == nil || account.ID <= 0 {
		return
	}
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return
	}
	store := s.getOpenAIWSStateStore()
	if store == nil {
		return
	}
	groupID := getOpenAIGroupIDFromContext(c)
	ttl := s.openAIWSResponseStickyTTL()
	logOpenAIWSBindResponseAccountWarn(groupID, account.ID, responseID, store.BindResponseAccount(ctx, groupID, responseID, account.ID, ttl))
}

func openAIUsageFromGJSON(value gjson.Result) (OpenAIUsage, bool) {
	if !value.Exists() || !value.IsObject() {
		return OpenAIUsage{}, false
	}
	inputTokens := value.Get("input_tokens").Int()
	if inputTokens == 0 {
		inputTokens = value.Get("prompt_tokens").Int()
	}
	outputTokens := value.Get("output_tokens").Int()
	if outputTokens == 0 {
		outputTokens = value.Get("completion_tokens").Int()
	}
	cacheReadTokens := openAICacheReadTokensFromUsage(value)
	cacheCreationTokens := openAICacheCreationTokensFromUsage(value)
	imageOutputTokens := value.Get("output_tokens_details.image_tokens").Int()
	if imageOutputTokens == 0 {
		imageOutputTokens = value.Get("completion_tokens_details.image_tokens").Int()
	}
	return OpenAIUsage{
		InputTokens:              int(inputTokens),
		OutputTokens:             int(outputTokens),
		CacheCreationInputTokens: cacheCreationTokens,
		CacheReadInputTokens:     cacheReadTokens,
		ImageOutputTokens:        int(imageOutputTokens),
	}, true
}

func openAICacheReadTokensFromUsage(value gjson.Result) int {
	for _, nested := range []gjson.Result{
		value.Get("input_tokens_details.cached_tokens"),
		value.Get("prompt_tokens_details.cached_tokens"),
	} {
		if nested.Exists() {
			return max(int(nested.Int()), 0)
		}
	}
	return firstPositiveGJSONInt(
		value.Get("cache_read_input_tokens"),
		value.Get("cache_read_tokens"),
		value.Get("cached_tokens"),
	)
}

func openAICacheCreationTokensFromUsage(value gjson.Result) int {
	for _, nested := range []gjson.Result{
		value.Get("input_tokens_details.cache_write_tokens"),
		value.Get("prompt_tokens_details.cache_write_tokens"),
		value.Get("input_tokens_details.cache_creation_tokens"),
		value.Get("prompt_tokens_details.cache_creation_tokens"),
	} {
		if nested.Exists() {
			return max(int(nested.Int()), 0)
		}
	}
	return firstPositiveGJSONInt(
		value.Get("cache_write_tokens"),
		value.Get("cache_creation_input_tokens"),
		value.Get("cache_write_input_tokens"),
		value.Get("cache_creation_tokens"),
	)
}

func (s *OpenAIGatewayService) handleNonStreamingResponse(ctx context.Context, resp *http.Response, c *gin.Context, account *Account, originalModel, mappedModel string) (*openaiNonStreamingResult, error) {
	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	if failoverErr := s.newOpenAIPoolEmbeddedFailoverError(ctx, c, account, resp, body, mappedModel, false); failoverErr != nil {
		return nil, failoverErr
	}

	// Detect SSE responses for ALL account types via Content-Type header.
	// Some OpenAI-compatible upstreams (including other sub2api instances)
	// may return SSE even when stream=false was requested.
	if isEventStreamResponse(resp.Header) {
		return s.handleSSEToJSON(resp, c, account, body, originalModel, mappedModel)
	}
	// bodyLooksLikeSSE is a line-level heuristic: real SSE framing requires
	// "data:"/"event:" field names at the very start of a physical line. A
	// plain substring scan would also match ordinary JSON responses whose text
	// merely echoes the literal text "data:" or "event:".
	bodyLooksLikeSSE := bodyHasSSEFraming(body)

	// For OAuth accounts, also fall back to a body-content heuristic because
	// the upstream may omit the Content-Type header while still sending SSE.
	// This heuristic is NOT applied to API-key accounts to avoid false
	// positives on JSON responses that coincidentally contain "data:" or
	// "event:" in their text content.
	if account.Type == AccountTypeOAuth && bodyLooksLikeSSE {
		return s.handleSSEToJSON(resp, c, account, body, originalModel, mappedModel)
	}

	usageValue, usageOK := extractOpenAIUsageFromJSONBytes(body)
	if !usageOK {
		if bodyLooksLikeSSE {
			return s.handleSSEToJSON(resp, c, account, body, originalModel, mappedModel)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && !isOpenAISuccessJSONResponse(resp, body) {
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           body,
				RetryableOnSameAccount: true,
			}
		}
		return nil, fmt.Errorf("parse response: invalid json response")
	}
	usage := &usageValue

	// Replace model in response if needed
	if originalModel != mappedModel {
		body = s.replaceModelInResponseBody(body, mappedModel, originalModel)
	}

	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	contentType := "application/json"
	if s.cfg != nil && !s.cfg.Security.ResponseHeaders.Enabled {
		if upstreamType := resp.Header.Get("Content-Type"); upstreamType != "" {
			contentType = upstreamType
		}
	}

	if !writeOpenAICompactSSEBridge(c, resp.StatusCode, body) {
		c.Data(resp.StatusCode, contentType, body)
	}

	return &openaiNonStreamingResult{
		OpenAIUsage:      usage,
		usage:            usage,
		responseID:       extractOpenAIResponseIDFromJSONBytes(body),
		imageCount:       countOpenAIResponseImageOutputsFromJSONBytes(body),
		imageOutputSizes: collectOpenAIResponseImageOutputSizesFromJSONBytes(body),
	}, nil
}

func isEventStreamResponse(header http.Header) bool {
	contentType := strings.ToLower(header.Get("Content-Type"))
	return strings.Contains(contentType, "text/event-stream")
}

func bodyHasSSEFraming(body []byte) bool {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte("data:")) || bytes.HasPrefix(line, []byte("event:")) {
			return true
		}
	}
	return false
}

func (s *OpenAIGatewayService) handleSSEToJSON(resp *http.Response, c *gin.Context, account *Account, body []byte, originalModel, mappedModel string) (*openaiNonStreamingResult, error) {
	bodyText := string(body)
	finalResponse, ok := extractCodexFinalResponse(bodyText)

	usage := &OpenAIUsage{}
	if ok {
		if parsedUsage, parsed := extractOpenAIUsageFromJSONBytes(finalResponse); parsed {
			*usage = parsedUsage
		}
		// When the terminal event has an empty output array, reconstruct
		// output from accumulated delta events so the client gets full content.
		// gjson Array() returns empty slice for null, missing, or empty arrays.
		if len(gjson.GetBytes(finalResponse, "output").Array()) == 0 {
			if outputJSON, reconstructed := reconstructResponseOutputFromSSE(bodyText); reconstructed {
				if patched, err := sjson.SetRawBytes(finalResponse, "output", outputJSON); err == nil {
					finalResponse = patched
				}
			}
		}
		finalResponse = supplementCompactionItemFromSSE(c, finalResponse, bodyText)
		body = finalResponse
		if originalModel != mappedModel {
			body = s.replaceModelInResponseBody(body, mappedModel, originalModel)
		}
		// Correct tool calls in final response
		body = s.correctToolCallsInResponseBody(body)
	} else {
		terminalType, terminalPayload, terminalOK := extractOpenAISSETerminalEvent(bodyText)
		if terminalOK && terminalType == "response.failed" {
			msg := extractOpenAISSEErrorMessage(terminalPayload)
			if msg == "" {
				msg = "Upstream compact response failed"
			}
			upstreamRequestID := ""
			if resp != nil {
				upstreamRequestID = resp.Header.Get("x-request-id")
			}
			if decision := s.handleOpenAICyberPolicyEvent(c, account, false, upstreamRequestID, terminalPayload, nil); decision.Matched {
				responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
				contentType := resp.Header.Get("Content-Type")
				if contentType == "" {
					contentType = "text/event-stream"
				}
				MarkResponseCommitted(c)
				c.Data(resp.StatusCode, contentType, body)
				return &openaiNonStreamingResult{
					OpenAIUsage:      usage,
					usage:            usage,
					responseID:       extractOpenAIResponseIDFromJSONBytes(terminalPayload),
					imageCount:       countOpenAIImageOutputsFromSSEBody(bodyText),
					imageOutputSizes: collectOpenAIImageOutputSizesFromSSEBody(bodyText),
				}, nil
			}
			return nil, s.writeOpenAINonStreamingProtocolError(resp, c, msg)
		}
		usage = s.parseSSEUsageFromBody(bodyText)
		if originalModel != mappedModel {
			bodyText = s.replaceModelInSSEBody(bodyText, mappedModel, originalModel)
		}
		body = []byte(bodyText)
	}

	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	contentType := "application/json; charset=utf-8"
	if !ok {
		contentType = resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "text/event-stream"
		}
	}
	if !writeOpenAICompactSSEBridge(c, resp.StatusCode, body) {
		c.Data(resp.StatusCode, contentType, body)
	}

	return &openaiNonStreamingResult{
		OpenAIUsage:      usage,
		usage:            usage,
		responseID:       extractOpenAIResponseIDFromJSONBytes(body),
		imageCount:       countOpenAIImageOutputsFromSSEBody(bodyText),
		imageOutputSizes: collectOpenAIImageOutputSizesFromSSEBody(bodyText),
	}, nil
}

func extractOpenAISSETerminalEvent(body string) (string, []byte, bool) {
	var terminalType string
	var terminalPayload []byte
	forEachOpenAISSEDataPayload(body, func(data []byte) {
		if terminalPayload != nil {
			return
		}
		eventType := strings.TrimSpace(gjson.GetBytes(data, "type").String())
		switch eventType {
		case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
			terminalType = eventType
			terminalPayload = append([]byte(nil), data...)
		}
	})
	if terminalPayload != nil {
		return terminalType, terminalPayload, true
	}
	return "", nil, false
}

func extractOpenAISSEErrorMessage(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	for _, path := range []string{"response.error.message", "error.message", "message"} {
		if msg := strings.TrimSpace(gjson.GetBytes(payload, path).String()); msg != "" {
			return sanitizeUpstreamErrorMessage(msg)
		}
	}
	return sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(payload)))
}

func sanitizeOpenAIResponseFailedEventForClient(payload []byte, eventType string, clientOutputStarted bool) ([]byte, bool) {
	if eventType != "response.failed" || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload, false
	}
	updated := payload
	if clientOutputStarted && isOpenAIContextWindowError(extractOpenAISSEErrorMessage(payload), payload) {
		errorPath := ""
		switch {
		case gjson.GetBytes(updated, "response.error").Exists():
			errorPath = "response.error"
		case gjson.GetBytes(updated, "error").Exists():
			errorPath = "error"
		}
		if errorPath != "" {
			next, err := sjson.SetBytes(updated, errorPath+".type", "invalid_request_error")
			if err != nil {
				return payload, false
			}
			updated = next
			next, err = sjson.SetBytes(updated, errorPath+".code", "context_length_exceeded")
			if err != nil {
				return payload, false
			}
			updated = next
		}
	}
	if !gjson.GetBytes(updated, "response").Exists() {
		return updated, !bytes.Equal(updated, payload)
	}
	for _, path := range []string{
		"response.instructions",
		"response.output",
		"response.usage",
		"response.metadata",
		"response.reasoning",
		"response.tools",
		"response.tool_choice",
		"response.parallel_tool_calls",
		"response.text",
		"response.truncation",
		"response.max_output_tokens",
		"response.incomplete_details",
	} {
		next, err := sjson.DeleteBytes(updated, path)
		if err != nil {
			return payload, false
		}
		updated = next
	}
	return updated, !bytes.Equal(updated, payload)
}

func (s *OpenAIGatewayService) writeOpenAINonStreamingProtocolError(resp *http.Response, c *gin.Context, message string) error {
	message = sanitizeUpstreamErrorMessage(strings.TrimSpace(message))
	if message == "" {
		message = "Upstream returned an invalid non-streaming response"
	}
	setOpsUpstreamError(c, http.StatusBadGateway, message, "")
	if openAICompactClientWantsStream(c) && StopOpenAICompactSSEKeepaliveCommitted(c) {
		writeOpenAICompactSSEFailureMessage(c, http.StatusBadGateway, "upstream_error", message)
		return fmt.Errorf("non-streaming openai protocol error: %s", message)
	}
	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.JSON(http.StatusBadGateway, gin.H{
		"error": gin.H{
			"type":    "upstream_error",
			"message": message,
		},
	})
	return fmt.Errorf("non-streaming openai protocol error: %s", message)
}

func extractCodexFinalResponse(body string) ([]byte, bool) {
	var finalResponse []byte
	forEachOpenAISSEDataPayload(body, func(data []byte) {
		if finalResponse != nil {
			return
		}
		eventType := gjson.GetBytes(data, "type").String()
		if eventType == "response.done" || eventType == "response.completed" {
			if response := gjson.GetBytes(data, "response"); response.Exists() && response.Type == gjson.JSON && response.Raw != "" {
				finalResponse = []byte(response.Raw)
			}
		}
	})
	if finalResponse != nil {
		return finalResponse, true
	}
	return nil, false
}

func responsesStreamEventMayContributeToOutput(eventType string) bool {
	switch eventType {
	case "response.output_text.delta",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.custom_tool_call_input.delta",
		"response.reasoning_summary_text.delta",
		"response.reasoning_text.delta":
		return true
	default:
		return false
	}
}

func collectRawResponsesOutputItemsFromSSE(bodyText string) ([]byte, bool) {
	var items []json.RawMessage
	seen := make(map[string]struct{})
	hasCompactionItem := false
	appendItem := func(item gjson.Result) {
		if !item.Exists() || !item.IsObject() {
			return
		}
		key := strings.TrimSpace(item.Get("id").String())
		if key == "" {
			key = item.Raw
		}
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		if isResponsesCompactionItemType(item.Get("type").String()) {
			hasCompactionItem = true
		}
		items = append(items, json.RawMessage(item.Raw))
	}
	forEachOpenAISSEDataPayload(bodyText, func(data []byte) {
		if strings.TrimSpace(gjson.GetBytes(data, "type").String()) != "response.output_item.done" {
			return
		}
		appendItem(gjson.GetBytes(data, "item"))
	})
	if !hasCompactionItem {
		forEachOpenAISSEDataPayload(bodyText, func(data []byte) {
			if strings.TrimSpace(gjson.GetBytes(data, "type").String()) != "response.output_item.added" {
				return
			}
			item := gjson.GetBytes(data, "item")
			if !isResponsesCompactionItemType(item.Get("type").String()) {
				return
			}
			appendItem(item)
		})
	}
	if len(items) == 0 {
		return nil, false
	}
	outputJSON, err := json.Marshal(items)
	if err != nil {
		return nil, false
	}
	return outputJSON, true
}

func isResponsesCompactionItemType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "compaction", "compaction_summary":
		return true
	default:
		return false
	}
}

func supplementCompactionItemFromSSE(c *gin.Context, finalResponse []byte, bodyText string) []byte {
	if !isOpenAIResponsesCompactPath(c) {
		return finalResponse
	}
	if len(gjson.GetBytes(finalResponse, "output").Array()) == 0 {
		return finalResponse
	}
	if responsesOutputHasCompactionItem(finalResponse) {
		return finalResponse
	}
	item, found := findRawCompactionItemFromSSE(bodyText)
	if !found {
		return finalResponse
	}
	patched, err := sjson.SetRawBytes(finalResponse, "output.-1", item)
	if err != nil {
		return finalResponse
	}
	return patched
}

func responsesOutputHasCompactionItem(response []byte) bool {
	for _, item := range gjson.GetBytes(response, "output").Array() {
		if isResponsesCompactionItemType(item.Get("type").String()) {
			return true
		}
	}
	return false
}

func findRawCompactionItemFromSSE(bodyText string) (json.RawMessage, bool) {
	var found json.RawMessage
	pick := func(eventType string) {
		forEachOpenAISSEDataPayload(bodyText, func(data []byte) {
			if found != nil {
				return
			}
			if strings.TrimSpace(gjson.GetBytes(data, "type").String()) != eventType {
				return
			}
			item := gjson.GetBytes(data, "item")
			if !item.IsObject() || !isResponsesCompactionItemType(item.Get("type").String()) {
				return
			}
			found = json.RawMessage(item.Raw)
		})
	}
	pick("response.output_item.done")
	if found == nil {
		pick("response.output_item.added")
	}
	return found, found != nil
}

// reconstructResponseOutputFromSSE scans raw SSE body text and returns a
// JSON-encoded output array. Raw output_item.done items are authoritative and
// preserve compact-specific fields that the accumulator does not understand.
func reconstructResponseOutputFromSSE(bodyText string) ([]byte, bool) {
	if outputJSON, ok := collectRawResponsesOutputItemsFromSSE(bodyText); ok {
		return outputJSON, true
	}
	acc := apicompat.NewBufferedResponseAccumulator()
	imageOutputs := make([]json.RawMessage, 0, 1)
	seenImages := make(map[string]struct{})
	forEachOpenAISSEDataPayload(bodyText, func(data []byte) {
		if imageOutput, ok := extractImageGenerationOutputFromSSEData(data, seenImages); ok {
			imageOutputs = append(imageOutputs, imageOutput)
		}
		eventType := strings.TrimSpace(gjson.GetBytes(data, "type").String())
		if responsesStreamEventMayContributeToOutput(eventType) {
			var event apicompat.ResponsesStreamEvent
			if err := json.Unmarshal(data, &event); err == nil {
				acc.ProcessEvent(&event)
			}
		}
	})
	return buildResponsesOutputJSON(acc, imageOutputs)
}

func buildResponsesOutputJSON(acc *apicompat.BufferedResponseAccumulator, imageOutputs []json.RawMessage) ([]byte, bool) {
	if (acc == nil || !acc.HasContent()) && len(imageOutputs) == 0 {
		return nil, false
	}

	var output []json.RawMessage
	if acc != nil && acc.HasContent() {
		outputJSON, err := json.Marshal(acc.BuildOutput())
		if err == nil {
			_ = json.Unmarshal(outputJSON, &output)
		}
	}
	output = append(output, imageOutputs...)
	if len(output) == 0 {
		return nil, false
	}

	outputJSON, err := json.Marshal(output)
	if err != nil {
		return nil, false
	}
	return outputJSON, true
}

func extractImageGenerationOutputFromSSEData(data []byte, seen map[string]struct{}) (json.RawMessage, bool) {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return nil, false
	}
	if gjson.GetBytes(data, "type").String() != "response.output_item.done" {
		return nil, false
	}
	item := gjson.GetBytes(data, "item")
	if !item.Exists() || !item.IsObject() || item.Get("type").String() != "image_generation_call" {
		return nil, false
	}
	if strings.TrimSpace(item.Get("result").String()) == "" {
		return nil, false
	}
	key := strings.TrimSpace(item.Get("id").String())
	if key == "" {
		key = strings.TrimSpace(item.Get("output_format").String()) + "|" + strings.TrimSpace(item.Get("result").String())
	}
	if key != "" && seen != nil {
		if _, exists := seen[key]; exists {
			return nil, false
		}
		seen[key] = struct{}{}
	}
	return json.RawMessage(item.Raw), true
}

func (s *OpenAIGatewayService) parseSSEUsageFromBody(body string) *OpenAIUsage {
	usage := &OpenAIUsage{}
	forEachOpenAISSEDataPayload(body, func(data []byte) {
		s.parseSSEUsageBytes(data, usage)
	})
	return usage
}

func (s *OpenAIGatewayService) replaceModelInSSEBody(body, fromModel, toModel string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if _, ok := extractOpenAISSEDataLine(line); !ok {
			continue
		}
		lines[i] = s.replaceModelInSSELine(line, fromModel, toModel)
	}
	return strings.Join(lines, "\n")
}

func (s *OpenAIGatewayService) validateUpstreamBaseURL(raw string) (string, error) {
	if s.cfg != nil && !s.cfg.Security.URLAllowlist.Enabled {
		normalized, err := urlvalidator.ValidateURLFormat(raw, s.cfg.Security.URLAllowlist.AllowInsecureHTTP)
		if err != nil {
			return "", fmt.Errorf("invalid base_url: %w", err)
		}
		return normalized, nil
	}
	normalized, err := urlvalidator.ValidateHTTPSURL(raw, urlvalidator.ValidationOptions{
		AllowedHosts:     s.cfg.Security.URLAllowlist.UpstreamHosts,
		RequireAllowlist: true,
		AllowPrivate:     s.cfg.Security.URLAllowlist.AllowPrivateHosts,
	})
	if err != nil {
		return "", fmt.Errorf("invalid base_url: %w", err)
	}
	return normalized, nil
}

// buildOpenAIResponsesURL 组装 OpenAI Responses 端点。
// - base 以 /v1 结尾：追加 /responses
// - base 以其他版本段结尾（如 /v4）：追加 /responses
// - base 已是 /responses：原样返回
// - 其他情况：追加 /v1/responses
func buildOpenAIResponsesURL(base string) string {
	return buildOpenAIEndpointURL(base, "/v1/responses")
}

func trimOpenAIEncryptedReasoningItems(reqBody map[string]any) bool {
	if len(reqBody) == 0 {
		return false
	}

	inputValue, has := reqBody["input"]
	if !has {
		return false
	}

	switch input := inputValue.(type) {
	case []any:
		filtered := input[:0]
		changed := false
		for _, item := range input {
			nextItem, itemChanged, keep := sanitizeEncryptedReasoningInputItem(item)
			if itemChanged {
				changed = true
			}
			if !keep {
				continue
			}
			filtered = append(filtered, nextItem)
		}
		if !changed {
			return false
		}
		if len(filtered) == 0 {
			delete(reqBody, "input")
			return true
		}
		reqBody["input"] = filtered
		return true
	case []map[string]any:
		filtered := input[:0]
		changed := false
		for _, item := range input {
			nextItem, itemChanged, keep := sanitizeEncryptedReasoningInputItem(item)
			if itemChanged {
				changed = true
			}
			if !keep {
				continue
			}
			nextMap, ok := nextItem.(map[string]any)
			if !ok {
				filtered = append(filtered, item)
				continue
			}
			filtered = append(filtered, nextMap)
		}
		if !changed {
			return false
		}
		if len(filtered) == 0 {
			delete(reqBody, "input")
			return true
		}
		reqBody["input"] = filtered
		return true
	case map[string]any:
		nextItem, changed, keep := sanitizeEncryptedReasoningInputItem(input)
		if !changed {
			return false
		}
		if !keep {
			delete(reqBody, "input")
			return true
		}
		nextMap, ok := nextItem.(map[string]any)
		if !ok {
			return false
		}
		reqBody["input"] = nextMap
		return true
	default:
		return false
	}
}

func sanitizeEncryptedReasoningInputItem(item any) (next any, changed bool, keep bool) {
	inputItem, ok := item.(map[string]any)
	if !ok {
		return item, false, true
	}

	itemType, _ := inputItem["type"].(string)
	if strings.TrimSpace(itemType) != "reasoning" {
		return item, false, true
	}

	_, hasEncryptedContent := inputItem["encrypted_content"]
	if !hasEncryptedContent {
		return item, false, true
	}

	delete(inputItem, "encrypted_content")
	if len(inputItem) == 1 {
		return nil, true, false
	}
	return inputItem, true, true
}

func IsOpenAIResponsesCompactPathForTest(c *gin.Context) bool {
	return isOpenAIResponsesCompactPath(c)
}

func OpenAICompactSessionSeedKeyForTest() string {
	return openAICompactSessionSeedKey
}

func NormalizeOpenAICompactRequestBodyForTest(body []byte) ([]byte, bool, error) {
	return normalizeOpenAICompactRequestBody(body)
}

func isOpenAIResponsesCompactPath(c *gin.Context) bool {
	suffix := strings.TrimSpace(openAIResponsesRequestPathSuffix(c))
	return suffix == "/compact" || strings.HasPrefix(suffix, "/compact/")
}

func isOpenAIResponsesRequestPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	normalizedPath := strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/")
	if normalizedPath == "" {
		return false
	}
	idx := strings.LastIndex(normalizedPath, "/responses")
	if idx < 0 {
		return false
	}
	suffix := normalizedPath[idx+len("/responses"):]
	return suffix == "" || strings.HasPrefix(suffix, "/")
}

func normalizeOpenAICompactRequestBody(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}

	normalized := []byte(`{}`)
	// Keep the current Codex /compact schema while still dropping request-scoped
	// fields such as prompt_cache_key, store, and stream.
	for _, field := range []string{
		"model",
		"input",
		"instructions",
		"tools",
		"parallel_tool_calls",
		"reasoning",
		"text",
		"previous_response_id",
	} {
		value := gjson.GetBytes(body, field)
		if !value.Exists() {
			continue
		}
		next, err := sjson.SetRawBytes(normalized, field, []byte(value.Raw))
		if err != nil {
			return body, false, fmt.Errorf("normalize compact body %s: %w", field, err)
		}
		normalized = next
	}

	if bytes.Equal(bytes.TrimSpace(body), bytes.TrimSpace(normalized)) {
		return body, false, nil
	}
	return normalized, true, nil
}

func normalizeOpenAICodexCompactReasoningEffortMap(reqBody map[string]any, effectiveModel string) bool {
	if reqBody == nil || !isOpenAIGPT56Model(effectiveModel) {
		return false
	}
	if reasoning, ok := reqBody["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && strings.EqualFold(strings.TrimSpace(effort), "max") {
			reasoning["effort"] = "xhigh"
			return true
		}
	}
	if effort, ok := reqBody["reasoning_effort"].(string); ok && strings.EqualFold(strings.TrimSpace(effort), "max") {
		reqBody["reasoning_effort"] = "xhigh"
		return true
	}
	return false
}

func resolveOpenAICompactSessionID(c *gin.Context) string {
	if c != nil {
		if sessionID := strings.TrimSpace(c.GetHeader("session_id")); sessionID != "" {
			return sessionID
		}
		if conversationID := strings.TrimSpace(c.GetHeader("conversation_id")); conversationID != "" {
			return conversationID
		}
		if seed, ok := c.Get(openAICompactSessionSeedKey); ok {
			if seedStr, ok := seed.(string); ok && strings.TrimSpace(seedStr) != "" {
				return strings.TrimSpace(seedStr)
			}
		}
	}
	return uuid.NewString()
}

func openAIResponsesRequestPathSuffix(c *gin.Context) string {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return ""
	}
	normalizedPath := strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/")
	if normalizedPath == "" {
		return ""
	}
	idx := strings.LastIndex(normalizedPath, "/responses")
	if idx < 0 {
		return ""
	}
	suffix := normalizedPath[idx+len("/responses"):]
	if suffix == "" || suffix == "/" {
		return ""
	}
	if !strings.HasPrefix(suffix, "/") {
		return ""
	}
	return suffix
}

func appendOpenAIResponsesRequestPathSuffix(baseURL, suffix string) string {
	trimmedBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	trimmedSuffix := strings.TrimSpace(suffix)
	if trimmedBase == "" || trimmedSuffix == "" {
		return trimmedBase
	}
	return trimmedBase + trimmedSuffix
}

func (s *OpenAIGatewayService) replaceModelInResponseBody(body []byte, fromModel, toModel string) []byte {
	// 使用 gjson/sjson 精确替换 model 字段，避免全量 JSON 反序列化
	if m := gjson.GetBytes(body, "model"); m.Exists() && m.Str == fromModel {
		newBody, err := sjson.SetBytes(body, "model", toModel)
		if err != nil {
			return body
		}
		return newBody
	}
	return body
}

// OpenAIRecordUsageInput input for recording usage
type OpenAIRecordUsageInput struct {
	Result                  *OpenAIForwardResult
	APIKey                  *APIKey
	User                    *User
	Account                 *Account
	Subscription            *UserSubscription
	QuotaPlatform           string
	InboundEndpoint         string
	UpstreamEndpoint        string
	UserAgent               string // 请求的 User-Agent
	IPAddress               string // 请求的客户端 IP 地址
	RequestPayloadHash      string
	PromptCacheAffinityHash string
	PromptCacheGroupID      *int64
	APIKeyService           APIKeyQuotaUpdater
	// CyberBlocked 为 true 时把该用量行标记为 cyber（request_type=cyber），计费逻辑不变。
	CyberBlocked bool
	ChannelUsageFields
}

type CyberPolicyUsageInput struct {
	APIKey             *APIKey
	Account            *Account
	Subscription       *UserSubscription
	RequestID          string
	Model              string
	Stream             bool
	InputTokens        int
	OutputTokens       int
	InboundEndpoint    string
	UpstreamEndpoint   string
	UserAgent          string
	IPAddress          string
	RequestPayloadHash string
	APIKeyService      APIKeyQuotaUpdater
	ChannelUsageFields
}

func (s *OpenAIGatewayService) RecordCyberPolicyUsageLog(ctx context.Context, in CyberPolicyUsageInput) {
	if s == nil || in.APIKey == nil || in.APIKey.User == nil || in.Account == nil || strings.TrimSpace(in.Model) == "" {
		return
	}
	result := &OpenAIForwardResult{
		RequestID: in.RequestID,
		Model:     in.Model,
		Stream:    in.Stream,
		Usage: OpenAIUsage{
			InputTokens:  in.InputTokens,
			OutputTokens: in.OutputTokens,
		},
	}
	if err := s.RecordUsage(ctx, &OpenAIRecordUsageInput{
		Result:             result,
		APIKey:             in.APIKey,
		User:               in.APIKey.User,
		Account:            in.Account,
		Subscription:       in.Subscription,
		InboundEndpoint:    in.InboundEndpoint,
		UpstreamEndpoint:   in.UpstreamEndpoint,
		UserAgent:          in.UserAgent,
		IPAddress:          in.IPAddress,
		RequestPayloadHash: in.RequestPayloadHash,
		APIKeyService:      in.APIKeyService,
		ChannelUsageFields: in.ChannelUsageFields,
		CyberBlocked:       true,
	}); err != nil {
		logger.LegacyPrintf("service.openai_gateway", "cyber usage record failed: request_id=%s err=%v", in.RequestID, err)
	}
}

// RecordUsage records usage and deducts balance
func (s *OpenAIGatewayService) RecordUsage(ctx context.Context, input *OpenAIRecordUsageInput) error {
	if input == nil {
		return errors.New("openai usage input is nil")
	}
	result := input.Result
	if result == nil {
		return errors.New("openai usage result is nil")
	}
	if s.rateLimitService != nil && input.Account != nil && input.Account.Platform == PlatformOpenAI {
		s.rateLimitService.ResetOpenAI403Counter(ctx, input.Account.ID)
	}

	apiKey := input.APIKey
	user := input.User
	account := input.Account
	subscription := input.Subscription
	ApplyOpenAIImageBillingResolution(result)

	// OpenAI input_tokens 是总输入，包含缓存读取和缓存写入明细。
	// 将三类 token 拆成互斥桶，避免缓存写入同时按普通输入和 cache_write 重复计费。
	actualInputTokens := result.Usage.InputTokens - result.Usage.CacheReadInputTokens - result.Usage.CacheCreationInputTokens
	if actualInputTokens < 0 {
		actualInputTokens = 0
	}
	s.logOpenAIPromptCacheHitRate(ctx, account, result.Usage)
	s.recordOpenAIPromptCacheWarmResult(ctx, input)

	// Calculate cost
	tokens := UsageTokens{
		InputTokens:         actualInputTokens,
		OutputTokens:        result.Usage.OutputTokens,
		CacheCreationTokens: result.Usage.CacheCreationInputTokens,
		CacheReadTokens:     result.Usage.CacheReadInputTokens,
		ImageOutputTokens:   result.Usage.ImageOutputTokens,
	}

	// Get rate multiplier
	multiplier := 1.0
	if s.cfg != nil {
		multiplier = s.cfg.Default.RateMultiplier
	}
	if apiKey.GroupID != nil && apiKey.Group != nil {
		resolver := s.userGroupRateResolver
		if resolver == nil {
			resolver = newUserGroupRateResolver(nil, nil, resolveUserGroupRateCacheTTL(s.cfg), nil, "service.openai_gateway")
		}
		multiplier = resolver.Resolve(ctx, user.ID, *apiKey.GroupID, apiKey.Group.RateMultiplier)
	}
	multiplier, imageMultiplier, videoMultiplier := computePeakAwareMultipliers(apiKey, multiplier, timezone.Now())

	var cost *CostBreakdown
	var err error
	billingModel := forwardResultBillingModel(result.Model, result.UpstreamModel)
	if result.BillingModel != "" {
		billingModel = strings.TrimSpace(result.BillingModel)
	}
	if input.BillingModelSource == BillingModelSourceChannelMapped && input.ChannelMappedModel != "" && input.ChannelMappedModel != input.OriginalModel {
		billingModel = input.ChannelMappedModel
	}
	if input.BillingModelSource == BillingModelSourceRequested && input.OriginalModel != "" {
		billingModel = input.OriginalModel
	}
	billingModels := usageBillingModelCandidates(
		billingModel,
		result.BillingModel,
		input.ChannelMappedModel,
		input.OriginalModel,
		result.UpstreamModel,
		result.Model,
	)
	serviceTier := ""
	if result.ServiceTier != nil {
		serviceTier = strings.TrimSpace(*result.ServiceTier)
	}
	cost, err = s.calculateOpenAIRecordUsageCost(ctx, result, apiKey, billingModels, multiplier, imageMultiplier, videoMultiplier, tokens, serviceTier)
	if err != nil {
		if !isUsagePricingUnavailableError(err) {
			return err
		}
		logger.L().With(
			zap.String("component", "service.openai_gateway"),
			zap.Strings("billing_models", billingModels),
			zap.String("requested_model", input.OriginalModel),
			zap.String("mapped_model", input.ChannelMappedModel),
			zap.String("upstream_model", result.UpstreamModel),
			zap.Int64("api_key_id", apiKey.ID),
			zap.Int64("account_id", account.ID),
		).Warn("openai_usage.pricing_missing_record_zero_cost", zap.Error(err))
		cost = &CostBreakdown{BillingMode: string(BillingModeToken)}
	}

	// Determine billing type
	isSubscriptionBilling := subscription != nil && apiKey.Group != nil && apiKey.Group.IsSubscriptionType()
	billingType := BillingTypeBalance
	if isSubscriptionBilling {
		billingType = BillingTypeSubscription
	}

	// Create usage log
	durationMs := int(result.Duration.Milliseconds())
	accountRateMultiplier := account.BillingRateMultiplier()
	requestID := resolveUsageBillingRequestID(ctx, result.RequestID)

	// 确定 RequestedModel（渠道映射前的原始模型）
	requestedModel := result.Model
	if input.OriginalModel != "" {
		requestedModel = input.OriginalModel
	}

	usageLog := &UsageLog{
		UserID:               user.ID,
		APIKeyID:             apiKey.ID,
		AccountID:            account.ID,
		RequestID:            requestID,
		Model:                result.Model,
		RequestedModel:       requestedModel,
		UpstreamModel:        optionalNonEqualStringPtr(result.UpstreamModel, result.Model),
		ServiceTier:          result.ServiceTier,
		ReasoningEffort:      result.ReasoningEffort,
		InboundEndpoint:      optionalTrimmedStringPtr(input.InboundEndpoint),
		UpstreamEndpoint:     optionalTrimmedStringPtr(input.UpstreamEndpoint),
		InputTokens:          actualInputTokens,
		OutputTokens:         result.Usage.OutputTokens,
		CacheCreationTokens:  result.Usage.CacheCreationInputTokens,
		CacheReadTokens:      result.Usage.CacheReadInputTokens,
		ImageOutputTokens:    result.Usage.ImageOutputTokens,
		ImageCount:           result.ImageCount,
		ImageSize:            optionalTrimmedStringPtr(result.ImageSize),
		ImageInputSize:       optionalTrimmedStringPtr(result.ImageInputSize),
		ImageOutputSize:      optionalTrimmedStringPtr(result.ImageOutputSize),
		ImageSizeSource:      optionalTrimmedStringPtr(result.ImageSizeSource),
		ImageSizeBreakdown:   result.ImageSizeBreakdown,
		VideoCount:           result.VideoCount,
		VideoResolution:      optionalTrimmedStringPtr(result.VideoResolution),
		VideoDurationSeconds: optionalPositiveIntPtr(result.VideoDurationSeconds),
	}
	if cost != nil {
		usageLog.InputCost = cost.InputCost
		usageLog.OutputCost = cost.OutputCost
		usageLog.ImageOutputCost = cost.ImageOutputCost
		usageLog.CacheCreationCost = cost.CacheCreationCost
		usageLog.CacheReadCost = cost.CacheReadCost
		usageLog.TotalCost = cost.TotalCost
		usageLog.ActualCost = cost.ActualCost
	}
	if result.VideoCount > 0 {
		usageLog.RateMultiplier = videoMultiplier
	} else if result.ImageCount > 0 {
		usageLog.RateMultiplier = imageMultiplier
	} else {
		usageLog.RateMultiplier = multiplier
	}
	usageLog.AccountRateMultiplier = &accountRateMultiplier
	usageLog.BillingType = billingType
	usageLog.Stream = result.Stream
	if input.CyberBlocked {
		usageLog.RequestType = RequestTypeCyberBlocked
	}
	usageLog.OpenAIWSMode = result.OpenAIWSMode
	usageLog.DurationMs = &durationMs
	usageLog.FirstTokenMs = result.FirstTokenMs
	usageLog.SlotWaitMs = usageLogLatencyIntFromContext(ctx, ctxkey.SlotWaitMs)
	usageLog.UpstreamHeaderMs = result.UpstreamHeaderMs
	if usageLog.UpstreamHeaderMs == nil {
		usageLog.UpstreamHeaderMs = usageLogLatencyIntFromContext(ctx, ctxkey.UpstreamHeaderMs)
	}
	usageLog.UpstreamFirstByteMs = result.UpstreamFirstByteMs
	if usageLog.UpstreamFirstByteMs == nil {
		usageLog.UpstreamFirstByteMs = usageLogLatencyIntFromContext(ctx, ctxkey.UpstreamFirstByteMs)
	}
	usageLog.FirstClientFlushMs = result.FirstClientFlushMs
	if usageLog.FirstClientFlushMs == nil {
		usageLog.FirstClientFlushMs = usageLogLatencyIntFromContext(ctx, ctxkey.FirstClientFlushMs)
	}
	usageLog.EdgePrepareMs = result.EdgePrepareMs
	if usageLog.EdgePrepareMs == nil {
		usageLog.EdgePrepareMs = usageLogLatencyIntFromContext(ctx, ctxkey.EdgePrepareMs)
	}
	usageLog.EdgeQueueWaitMs = result.EdgeQueueWaitMs
	if usageLog.EdgeQueueWaitMs == nil {
		usageLog.EdgeQueueWaitMs = usageLogLatencyIntFromContext(ctx, ctxkey.EdgeQueueWaitMs)
	}
	usageLog.EdgeRelayStartMs = result.EdgeRelayStartMs
	if usageLog.EdgeRelayStartMs == nil {
		usageLog.EdgeRelayStartMs = usageLogLatencyIntFromContext(ctx, ctxkey.EdgeRelayStartMs)
	}
	usageLog.EdgeFallbackReason = result.EdgeFallbackReason
	if usageLog.EdgeFallbackReason == nil {
		usageLog.EdgeFallbackReason = usageLogStringFromContext(ctx, ctxkey.EdgeFallbackReason)
	}
	usageLog.EdgeRetryCount = result.EdgeRetryCount
	if usageLog.EdgeRetryCount == nil {
		usageLog.EdgeRetryCount = usageLogLatencyIntFromContext(ctx, ctxkey.EdgeRetryCount)
	}
	usageLog.CreatedAt = time.Now()
	// 设置渠道信息
	usageLog.ChannelID = optionalInt64Ptr(input.ChannelID)
	usageLog.ModelMappingChain = optionalTrimmedStringPtr(input.ModelMappingChain)
	// 设置计费模式
	if cost != nil && cost.BillingMode != "" {
		billingMode := cost.BillingMode
		usageLog.BillingMode = &billingMode
	} else if result.VideoCount > 0 {
		billingMode := string(BillingModeVideo)
		usageLog.BillingMode = &billingMode
	} else if result.ImageCount > 0 {
		billingMode := string(BillingModeImage)
		usageLog.BillingMode = &billingMode
	} else {
		billingMode := string(BillingModeToken)
		usageLog.BillingMode = &billingMode
	}
	// 添加 UserAgent
	if input.UserAgent != "" {
		usageLog.UserAgent = &input.UserAgent
	}

	// 添加 IPAddress
	if input.IPAddress != "" {
		usageLog.IPAddress = &input.IPAddress
	}

	if apiKey.GroupID != nil {
		usageLog.GroupID = apiKey.GroupID
	}
	if subscription != nil {
		usageLog.SubscriptionID = &subscription.ID
	}

	// 计算账号统计定价费用（使用最终上游模型匹配自定义规则）
	if apiKey.GroupID != nil {
		applyAccountStatsCost(ctx, usageLog, s.channelService, s.billingService,
			account.ID, *apiKey.GroupID, result.UpstreamModel, result.Model,
			tokens, cost.TotalCost,
		)
	}

	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		writeUsageLogBestEffort(ctx, s.usageLogRepo, usageLog, "service.openai_gateway")
		logger.LegacyPrintf("service.openai_gateway", "[SIMPLE MODE] Usage recorded (not billed): user=%d, tokens=%d", usageLog.UserID, usageLog.TotalTokens())
		s.deferredService.ScheduleLastUsedUpdate(account.ID)
		return nil
	}

	quotaPlatform := strings.TrimSpace(input.QuotaPlatform)
	if quotaPlatform == "" {
		quotaPlatform = PlatformFromAPIKey(apiKey)
	}

	billingErr := func() error {
		_, err := applyUsageBilling(ctx, requestID, usageLog, &postUsageBillingParams{
			Cost:                  cost,
			User:                  user,
			APIKey:                apiKey,
			Account:               account,
			Subscription:          subscription,
			RequestPayloadHash:    resolveUsageBillingPayloadFingerprint(ctx, input.RequestPayloadHash),
			IsSubscriptionBill:    isSubscriptionBilling,
			AccountRateMultiplier: accountRateMultiplier,
			APIKeyService:         input.APIKeyService,
			Platform:              quotaPlatform,
		}, s.billingDeps(), s.usageBillingRepo)
		return err
	}()

	if billingErr != nil {
		return billingErr
	}
	writeUsageLogBestEffort(ctx, s.usageLogRepo, usageLog, "service.openai_gateway")

	return nil
}

func (s *OpenAIGatewayService) logOpenAIPromptCacheHitRate(ctx context.Context, account *Account, usage OpenAIUsage) {
	if account == nil || account.Platform != PlatformOpenAI || usage.InputTokens <= 0 {
		return
	}
	if !defaultOpenAIPromptCacheHitRateLogThrottle.Allow(account.ID, time.Now()) {
		return
	}
	cacheRead := usage.CacheReadInputTokens
	if cacheRead < 0 {
		cacheRead = 0
	}
	if cacheRead > usage.InputTokens {
		cacheRead = usage.InputTokens
	}
	hitRate := float64(cacheRead) / float64(usage.InputTokens)
	logger.FromContext(ctx).Info("openai.prompt_cache.hit_rate",
		zap.Int64("account_id", account.ID),
		zap.Int("input_tokens", usage.InputTokens),
		zap.Int("cache_read_tokens", cacheRead),
		zap.Int("cache_creation_tokens", usage.CacheCreationInputTokens),
		zap.Float64("cache_hit_rate", hitRate),
		zap.Bool("prompt_cache_boost_enabled", account.IsOpenAIPromptCacheBoostEnabled()),
	)
}

func (s *OpenAIGatewayService) calculateOpenAIRecordUsageCost(
	ctx context.Context,
	result *OpenAIForwardResult,
	apiKey *APIKey,
	billingModels []string,
	multiplier float64,
	imageMultiplier float64,
	videoMultiplier float64,
	tokens UsageTokens,
	serviceTier string,
) (*CostBreakdown, error) {
	billingModel := firstUsageBillingModel(billingModels)
	if isGrokVideoUsageResult(result, billingModels) {
		if resolved := s.resolveOpenAIChannelPricing(ctx, billingModel, apiKey); resolved == nil || resolved.Mode != BillingModeToken {
			return s.calculateOpenAIVideoCost(ctx, billingModel, apiKey, result, videoMultiplier), nil
		}
	}
	if result != nil && result.ImageCount > 0 {
		if resolved := s.resolveOpenAIChannelPricing(ctx, billingModel, apiKey); resolved == nil || resolved.Mode != BillingModeToken {
			return s.calculateOpenAIImageCost(ctx, billingModel, apiKey, result, imageMultiplier), nil
		}
	}
	if len(billingModels) == 0 || billingModel == "" {
		return nil, errors.New("openai usage billing model is empty")
	}
	var lastErr error
	for _, candidate := range billingModels {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		cost, err := s.calculateOpenAIRecordUsageTokenCost(ctx, apiKey, candidate, multiplier, tokens, serviceTier)
		if err == nil {
			return cost, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no non-empty billing model candidates")
	}
	return nil, fmt.Errorf("calculate OpenAI usage cost failed for billing models %s: %w", strings.Join(billingModels, ","), lastErr)
}

func isUsagePricingUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrModelPricingUnavailable) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no pricing available") || strings.Contains(msg, "pricing not found")
}

func usageLogLatencyIntFromContext(ctx context.Context, key ctxkey.Key) *int {
	if ctx == nil {
		return nil
	}
	value, ok := ctx.Value(key).(int64)
	if !ok || value < 0 || value > int64(^uint(0)>>1) {
		return nil
	}
	out := int(value)
	return &out
}

func usageLogStringFromContext(ctx context.Context, key ctxkey.Key) *string {
	if ctx == nil {
		return nil
	}
	value, _ := ctx.Value(key).(string)
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func (s *OpenAIGatewayService) calculateOpenAIRecordUsageTokenCost(
	ctx context.Context,
	apiKey *APIKey,
	billingModel string,
	multiplier float64,
	tokens UsageTokens,
	serviceTier string,
) (*CostBreakdown, error) {
	if s.resolver != nil && apiKey.Group != nil {
		gid := apiKey.Group.ID
		return s.billingService.CalculateCostUnified(CostInput{
			Ctx:            ctx,
			Model:          billingModel,
			GroupID:        &gid,
			Tokens:         tokens,
			RequestCount:   1,
			RateMultiplier: multiplier,
			ServiceTier:    serviceTier,
			Resolver:       s.resolver,
		})
	}
	return s.billingService.CalculateCostWithServiceTier(billingModel, tokens, multiplier, serviceTier)
}

func (s *OpenAIGatewayService) calculateOpenAIImageCost(
	ctx context.Context,
	billingModel string,
	apiKey *APIKey,
	result *OpenAIForwardResult,
	multiplier float64,
) *CostBreakdown {
	sizeTier := NormalizeImageBillingTierOrDefault(result.ImageSize)
	if resolved := s.resolveOpenAIChannelPricing(ctx, billingModel, apiKey); resolved != nil &&
		(resolved.Mode == BillingModePerRequest || resolved.Mode == BillingModeImage) {
		gid := apiKey.Group.ID
		cost, err := s.billingService.CalculateCostUnified(CostInput{
			Ctx:            ctx,
			Model:          billingModel,
			GroupID:        &gid,
			RequestCount:   result.ImageCount,
			SizeTier:       sizeTier,
			RateMultiplier: multiplier,
			Resolver:       s.resolver,
			Resolved:       resolved,
		})
		if err == nil {
			return cost
		}
		logger.LegacyPrintf("service.openai_gateway", "Calculate image channel cost failed: %v", err)
	}

	var groupConfig *ImagePriceConfig
	if apiKey != nil && apiKey.Group != nil {
		groupConfig = &ImagePriceConfig{
			Price1K: apiKey.Group.ImagePrice1K,
			Price2K: apiKey.Group.ImagePrice2K,
			Price4K: apiKey.Group.ImagePrice4K,
		}
	}
	return s.billingService.CalculateImageCost(billingModel, sizeTier, result.ImageCount, groupConfig, multiplier)
}

func isGrokVideoBillingModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "grok-imagine-video")
}

func isGrokVideoUsageResult(result *OpenAIForwardResult, billingModels []string) bool {
	if result == nil || result.VideoCount <= 0 {
		return false
	}
	candidates := make([]string, 0, len(billingModels)+3)
	candidates = append(candidates, billingModels...)
	candidates = append(candidates, result.BillingModel, result.Model, result.UpstreamModel)
	for _, candidate := range candidates {
		if isGrokVideoBillingModel(candidate) {
			return true
		}
	}
	return false
}

func (s *OpenAIGatewayService) calculateOpenAIVideoCost(
	ctx context.Context,
	billingModel string,
	apiKey *APIKey,
	result *OpenAIForwardResult,
	multiplier float64,
) *CostBreakdown {
	if result == nil {
		return &CostBreakdown{}
	}
	resolution := NormalizeVideoBillingResolutionOrDefault(result.VideoResolution)
	durationSeconds := NormalizeVideoBillingDurationSecondsOrDefault(result.VideoDurationSeconds)
	videoCount := result.VideoCount
	if videoCount <= 0 {
		videoCount = 1
	}

	if resolved := s.resolveOpenAIChannelPricing(ctx, billingModel, apiKey); resolved != nil &&
		(resolved.Mode == BillingModePerRequest || resolved.Mode == BillingModeImage) {
		gid := apiKey.Group.ID
		cost, err := s.billingService.CalculateCostUnified(CostInput{
			Ctx:            ctx,
			Model:          billingModel,
			GroupID:        &gid,
			RequestCount:   videoCount,
			SizeTier:       resolution,
			RateMultiplier: multiplier,
			Resolver:       s.resolver,
			Resolved:       resolved,
		})
		if err == nil {
			scaleCostBreakdown(cost, float64(durationSeconds))
			cost.BillingMode = string(BillingModeVideo)
			return cost
		}
		logger.LegacyPrintf("service.openai_gateway", "Calculate video channel cost failed: %v", err)
	}

	return s.billingService.CalculateVideoCost(
		billingModel,
		resolution,
		videoCount,
		durationSeconds,
		videoPriceConfigFromAPIKey(apiKey),
		multiplier,
	)
}

func scaleCostBreakdown(cost *CostBreakdown, factor float64) {
	if cost == nil || factor == 1 {
		return
	}
	cost.InputCost *= factor
	cost.OutputCost *= factor
	cost.ImageOutputCost *= factor
	cost.CacheCreationCost *= factor
	cost.CacheReadCost *= factor
	cost.TotalCost *= factor
	cost.ActualCost *= factor
}

func (s *OpenAIGatewayService) resolveOpenAIChannelPricing(ctx context.Context, billingModel string, apiKey *APIKey) *ResolvedPricing {
	if s.resolver == nil || apiKey == nil || apiKey.Group == nil {
		return nil
	}
	gid := apiKey.Group.ID
	resolved := s.resolver.Resolve(ctx, PricingInput{Model: billingModel, GroupID: &gid})
	if resolved.Source == PricingSourceChannel {
		return resolved
	}
	return nil
}

// ParseCodexRateLimitHeaders extracts Codex usage limits from response headers.
// Exported for use in ratelimit_service when handling OpenAI 429 responses.
func ParseCodexRateLimitHeaders(headers http.Header) *OpenAICodexUsageSnapshot {
	snapshot := &OpenAICodexUsageSnapshot{}
	hasData := false

	// Helper to parse float64 from header
	parseFloat := func(key string) *float64 {
		if v := headers.Get(key); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return &f
			}
		}
		return nil
	}

	// Helper to parse int from header
	parseInt := func(key string) *int {
		if v := headers.Get(key); v != "" {
			if i, err := strconv.Atoi(v); err == nil {
				return &i
			}
		}
		return nil
	}

	// Primary (weekly) limits
	if v := parseFloat("x-codex-primary-used-percent"); v != nil {
		snapshot.PrimaryUsedPercent = v
		hasData = true
	}
	if v := parseInt("x-codex-primary-reset-after-seconds"); v != nil {
		snapshot.PrimaryResetAfterSeconds = v
		hasData = true
	}
	if v := parseInt("x-codex-primary-window-minutes"); v != nil {
		snapshot.PrimaryWindowMinutes = v
		hasData = true
	}

	// Secondary (5h) limits
	if v := parseFloat("x-codex-secondary-used-percent"); v != nil {
		snapshot.SecondaryUsedPercent = v
		hasData = true
	}
	if v := parseInt("x-codex-secondary-reset-after-seconds"); v != nil {
		snapshot.SecondaryResetAfterSeconds = v
		hasData = true
	}
	if v := parseInt("x-codex-secondary-window-minutes"); v != nil {
		snapshot.SecondaryWindowMinutes = v
		hasData = true
	}

	// Overflow ratio
	if v := parseFloat("x-codex-primary-over-secondary-limit-percent"); v != nil {
		snapshot.PrimaryOverSecondaryPercent = v
		hasData = true
	}

	if !hasData {
		return nil
	}

	snapshot.UpdatedAt = time.Now().Format(time.RFC3339)
	return snapshot
}

func codexSnapshotBaseTime(snapshot *OpenAICodexUsageSnapshot, fallback time.Time) time.Time {
	if snapshot == nil {
		return fallback
	}
	if snapshot.UpdatedAt == "" {
		return fallback
	}
	base, err := time.Parse(time.RFC3339, snapshot.UpdatedAt)
	if err != nil {
		return fallback
	}
	return base
}

func codexResetAtRFC3339(base time.Time, resetAfterSeconds *int) *string {
	if resetAfterSeconds == nil {
		return nil
	}
	sec := *resetAfterSeconds
	if sec < 0 {
		sec = 0
	}
	resetAt := base.Add(time.Duration(sec) * time.Second).Format(time.RFC3339)
	return &resetAt
}

func buildCodexUsageExtraUpdates(snapshot *OpenAICodexUsageSnapshot, fallbackNow time.Time) map[string]any {
	if snapshot == nil {
		return nil
	}

	baseTime := codexSnapshotBaseTime(snapshot, fallbackNow)
	updates := make(map[string]any)

	// 保存原始 primary/secondary 字段，便于排查问题
	if snapshot.PrimaryUsedPercent != nil {
		updates["codex_primary_used_percent"] = *snapshot.PrimaryUsedPercent
	}
	if snapshot.PrimaryResetAfterSeconds != nil {
		updates["codex_primary_reset_after_seconds"] = *snapshot.PrimaryResetAfterSeconds
	}
	if snapshot.PrimaryWindowMinutes != nil {
		updates["codex_primary_window_minutes"] = *snapshot.PrimaryWindowMinutes
	}
	if snapshot.SecondaryUsedPercent != nil {
		updates["codex_secondary_used_percent"] = *snapshot.SecondaryUsedPercent
	}
	if snapshot.SecondaryResetAfterSeconds != nil {
		updates["codex_secondary_reset_after_seconds"] = *snapshot.SecondaryResetAfterSeconds
	}
	if snapshot.SecondaryWindowMinutes != nil {
		updates["codex_secondary_window_minutes"] = *snapshot.SecondaryWindowMinutes
	}
	if snapshot.PrimaryOverSecondaryPercent != nil {
		updates["codex_primary_over_secondary_percent"] = *snapshot.PrimaryOverSecondaryPercent
	}
	updates["codex_usage_updated_at"] = baseTime.Format(time.RFC3339)

	// 归一化到 5h/7d 规范字段
	if normalized := snapshot.Normalize(); normalized != nil {
		if normalized.Used5hPercent != nil {
			updates["codex_5h_used_percent"] = *normalized.Used5hPercent
		}
		if normalized.Reset5hSeconds != nil {
			updates["codex_5h_reset_after_seconds"] = *normalized.Reset5hSeconds
		}
		if normalized.Window5hMinutes != nil {
			updates["codex_5h_window_minutes"] = *normalized.Window5hMinutes
		}
		if normalized.Used7dPercent != nil {
			updates["codex_7d_used_percent"] = *normalized.Used7dPercent
		}
		if normalized.Reset7dSeconds != nil {
			updates["codex_7d_reset_after_seconds"] = *normalized.Reset7dSeconds
		}
		if normalized.Window7dMinutes != nil {
			updates["codex_7d_window_minutes"] = *normalized.Window7dMinutes
		}
		if reset5hAt := codexResetAtRFC3339(baseTime, normalized.Reset5hSeconds); reset5hAt != nil {
			updates["codex_5h_reset_at"] = *reset5hAt
		}
		if reset7dAt := codexResetAtRFC3339(baseTime, normalized.Reset7dSeconds); reset7dAt != nil {
			updates["codex_7d_reset_at"] = *reset7dAt
		}
	}

	return updates
}

// updateCodexUsageSnapshot saves the Codex usage snapshot to account's Extra field
func (s *OpenAIGatewayService) updateCodexUsageSnapshot(ctx context.Context, accountID int64, snapshot *OpenAICodexUsageSnapshot) {
	if snapshot == nil {
		return
	}
	if s == nil || s.accountRepo == nil {
		return
	}

	now := time.Now()
	updates := buildCodexUsageExtraUpdates(snapshot, now)
	if len(updates) == 0 {
		return
	}
	if !s.getCodexSnapshotThrottle().Allow(accountID, now) {
		return
	}

	go func() {
		updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.accountRepo.UpdateExtra(updateCtx, accountID, updates)
	}()
}

func (s *OpenAIGatewayService) UpdateCodexUsageSnapshotFromHeaders(ctx context.Context, accountID int64, headers http.Header) {
	if accountID <= 0 || headers == nil {
		return
	}
	if snapshot := ParseCodexRateLimitHeaders(headers); snapshot != nil {
		s.updateCodexUsageSnapshot(ctx, accountID, snapshot)
	}
}

func getOpenAIReasoningEffortFromReqBody(reqBody map[string]any, requestedModel string) (value string, present bool) {
	if reqBody == nil {
		return "", false
	}

	// Primary: reasoning.effort
	if reasoning, ok := reqBody["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			return normalizeOpenAIReasoningEffortForModel(effort, requestedModel), true
		}
	}

	// Fallback: some clients may use a flat field.
	if effort, ok := reqBody["reasoning_effort"].(string); ok {
		return normalizeOpenAIReasoningEffortForModel(effort, requestedModel), true
	}

	return "", false
}

func deriveOpenAIReasoningEffortFromModel(model string) string {
	if strings.TrimSpace(model) == "" {
		return ""
	}

	modelID := strings.TrimSpace(model)
	if strings.Contains(modelID, "/") {
		parts := strings.Split(modelID, "/")
		modelID = parts[len(parts)-1]
	}

	parts := strings.FieldsFunc(strings.ToLower(modelID), func(r rune) bool {
		switch r {
		case '-', '_', ' ':
			return true
		default:
			return false
		}
	})
	if len(parts) == 0 {
		return ""
	}

	return normalizeOpenAIReasoningEffortForModel(parts[len(parts)-1], model)
}

func deriveOpenAIReasoningEffortFromModelCandidates(modelCandidates []string) string {
	for _, model := range modelCandidates {
		if value := deriveOpenAIReasoningEffortFromModel(model); value != "" {
			return value
		}
	}
	return ""
}

func extractOpenAIRequestMetaFromBody(body []byte) (model string, stream bool, promptCacheKey string) {
	if len(body) == 0 {
		return "", false, ""
	}

	model = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	stream = gjson.GetBytes(body, "stream").Bool()
	promptCacheKey = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	return model, stream, promptCacheKey
}

// normalizeOpenAIPassthroughOAuthBody 将透传 OAuth 请求体收敛为旧链路关键行为：
// 1) 删除 ChatGPT internal API 不支持的顶层 Responses 参数
// 2) store=false 3) 非 compact 保持 stream=true；compact 强制 stream=false
func normalizeOpenAIPassthroughOAuthBody(body []byte, compact bool) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}

	normalized := body
	changed := false

	for _, field := range openAIChatGPTInternalUnsupportedFields {
		if value := gjson.GetBytes(normalized, field); !value.Exists() {
			continue
		}
		next, err := sjson.DeleteBytes(normalized, field)
		if err != nil {
			return body, false, fmt.Errorf("normalize passthrough body delete %s: %w", field, err)
		}
		normalized = next
		changed = true
	}

	if compact {
		if store := gjson.GetBytes(normalized, "store"); store.Exists() {
			next, err := sjson.DeleteBytes(normalized, "store")
			if err != nil {
				return body, false, fmt.Errorf("normalize passthrough body delete store: %w", err)
			}
			normalized = next
			changed = true
		}
		if stream := gjson.GetBytes(normalized, "stream"); stream.Exists() {
			next, err := sjson.DeleteBytes(normalized, "stream")
			if err != nil {
				return body, false, fmt.Errorf("normalize passthrough body delete stream: %w", err)
			}
			normalized = next
			changed = true
		}
	} else {
		if store := gjson.GetBytes(normalized, "store"); !store.Exists() || store.Type != gjson.False {
			next, err := sjson.SetBytes(normalized, "store", false)
			if err != nil {
				return body, false, fmt.Errorf("normalize passthrough body store=false: %w", err)
			}
			normalized = next
			changed = true
		}
		if stream := gjson.GetBytes(normalized, "stream"); !stream.Exists() || stream.Type != gjson.True {
			next, err := sjson.SetBytes(normalized, "stream", true)
			if err != nil {
				return body, false, fmt.Errorf("normalize passthrough body stream=true: %w", err)
			}
			normalized = next
			changed = true
		}
	}

	return normalized, changed, nil
}

func detectOpenAIPassthroughInstructionsRejectReason(reqModel string, body []byte) string {
	model := strings.ToLower(strings.TrimSpace(reqModel))
	if !strings.Contains(model, "codex") {
		return ""
	}

	instructions := gjson.GetBytes(body, "instructions")
	if !instructions.Exists() {
		return "instructions_missing"
	}
	if instructions.Type != gjson.String {
		return "instructions_not_string"
	}
	if strings.TrimSpace(instructions.String()) == "" {
		return "instructions_empty"
	}
	return ""
}

func extractOpenAIReasoningEffortFromBody(body []byte, modelCandidates ...string) *string {
	reasoningEffort := strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String())
	if reasoningEffort == "" {
		reasoningEffort = strings.TrimSpace(gjson.GetBytes(body, "reasoning_effort").String())
	}
	if reasoningEffort != "" {
		normalized := normalizeOpenAIReasoningEffortForModel(reasoningEffort, firstNonEmpty(modelCandidates...))
		if normalized == "" {
			return nil
		}
		return &normalized
	}

	value := deriveOpenAIReasoningEffortFromModelCandidates(modelCandidates)
	if value == "" {
		return nil
	}
	return &value
}

func extractOpenAIServiceTier(reqBody map[string]any) *string {
	if reqBody == nil {
		return nil
	}
	raw, ok := reqBody["service_tier"].(string)
	if !ok {
		return nil
	}
	return normalizeOpenAIServiceTier(raw)
}

func extractOpenAIServiceTierFromBody(body []byte) *string {
	if len(body) == 0 {
		return nil
	}
	return normalizeOpenAIServiceTier(gjson.GetBytes(body, "service_tier").String())
}

func normalizeOpenAIServiceTier(raw string) *string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return nil
	}
	if value == "fast" {
		value = "priority"
	}
	// 放过 OpenAI 官方文档定义的所有合法 tier 值：priority/flex/auto/default/scale。
	// 对 Codex 客户端零影响（Codex 只发 priority 或 flex，见 codex-rs/core/src/client.rs），
	// 但能让直连 OpenAI SDK 的用户透传 auto/default/scale 以便抓包/调试。
	// 真未知值仍返回 nil，由 normalizeResponsesBodyServiceTier 从 body 中删除。
	switch value {
	case "priority", "flex", "auto", "default", "scale":
		return &value
	default:
		return nil
	}
}

// OpenAIFastBlockedError indicates a request was rejected by the OpenAI fast
// policy (action=block). Mirrors BetaBlockedError on the Claude side.
type OpenAIFastBlockedError struct {
	Message string
}

func (e *OpenAIFastBlockedError) Error() string { return e.Message }

// evaluateOpenAIFastPolicy returns the action and error message that should be
// applied for a request with the given account/model/service_tier. When the
// policy service is unavailable or no rule matches, it returns
// (BetaPolicyActionPass, "") so callers can short-circuit safely.
//
// Matching rules:
//   - Scope filters by account type (all / oauth / apikey / bedrock)
//   - UserIDs, when present, filters by the trusted Sub2API user that owns the API key
//   - ServiceTier must be empty (= any), "all", or equal the normalized tier
//   - ModelWhitelist narrows the rule to specific models; FallbackAction
//     handles the non-matching case (default: pass)
//   - User-specific rules take precedence over global rules; each group keeps configured order
//
// 与 Claude BetaPolicy 的差异（保留首条匹配 short-circuit）：
//   - BetaPolicy 处理的是 anthropic-beta header 中的 token 集合，不同
//     规则可能针对不同 token，filter 需要累加成 set；block 则 first-match。
//   - OpenAI fast policy 操作的是单个字段 service_tier：filter 即删字段，
//     没有可累加的对象。一次请求只携带一个 service_tier，规则的 tier
//     维度天然互斥；同一 (scope, tier) 下若多条规则的 model whitelist
//     发生重叠，admin 可通过规则顺序明确意图。因此采用 first-match 而
//     非 BetaPolicy 那样的"block 覆盖 filter 覆盖 pass"语义。
func (s *OpenAIGatewayService) evaluateOpenAIFastPolicy(ctx context.Context, account *Account, model, serviceTier string) (action, errMsg string) {
	if s == nil || s.settingService == nil {
		return BetaPolicyActionPass, ""
	}
	tier := strings.ToLower(strings.TrimSpace(serviceTier))
	if tier == "" {
		return BetaPolicyActionPass, ""
	}
	settings := openAIFastPolicySettingsFromContext(ctx)
	if settings == nil {
		fetched, err := s.settingService.GetOpenAIFastPolicySettings(ctx)
		if err != nil || fetched == nil {
			return BetaPolicyActionPass, ""
		}
		settings = fetched
	}
	return evaluateOpenAIFastPolicyWithSettings(settings, openAIFastPolicyUserID(ctx), account, model, tier)
}

// evaluateOpenAIFastPolicyWithSettings is the pure-function core extracted so
// long-lived sessions (e.g. WS) can prefetch settings once and avoid hitting
// the settingService on every frame. See WSSession entry and
// openAIFastPolicySettingsFromContext for the caching glue.
func evaluateOpenAIFastPolicyWithSettings(settings *OpenAIFastPolicySettings, userID int64, account *Account, model, tier string) (action, errMsg string) {
	if settings == nil {
		return BetaPolicyActionPass, ""
	}
	isOAuth := account != nil && account.IsOAuth()
	isBedrock := account != nil && account.IsBedrock()
	for _, userScoped := range []bool{true, false} {
		for _, rule := range settings.Rules {
			if (len(rule.UserIDs) > 0) != userScoped || !openAIFastPolicyUserMatches(rule.UserIDs, userID) {
				continue
			}
			if !betaPolicyScopeMatches(rule.Scope, isOAuth, isBedrock) {
				continue
			}
			ruleTier := strings.ToLower(strings.TrimSpace(rule.ServiceTier))
			if ruleTier != "" && ruleTier != OpenAIFastTierAny && ruleTier != tier {
				continue
			}
			eff := BetaPolicyRule{
				Action:               rule.Action,
				ErrorMessage:         rule.ErrorMessage,
				ModelWhitelist:       rule.ModelWhitelist,
				FallbackAction:       rule.FallbackAction,
				FallbackErrorMessage: rule.FallbackErrorMessage,
			}
			return resolveRuleAction(eff, model)
		}
	}
	return BetaPolicyActionPass, ""
}

func openAIFastPolicyUserID(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	userID, _ := ctx.Value(ctxkey.UserID).(int64)
	if userID <= 0 {
		return 0
	}
	return userID
}

func openAIFastPolicyUserMatches(ruleUserIDs []int64, userID int64) bool {
	if len(ruleUserIDs) == 0 {
		return true
	}
	for _, ruleUserID := range ruleUserIDs {
		if ruleUserID == userID {
			return true
		}
	}
	return false
}

// openAIFastPolicyCtxKey 是 context 中预取的 OpenAIFastPolicySettings 缓存
// 键，仅用于 WebSocket 长会话内多帧复用同一份策略快照，避免每帧 DB 命中。
//
// Trade-off：策略变更不会影响当前 WS session（只影响新 session）。这是
// 有意为之 —— 对长会话来说，"策略一致性"比"立刻生效"更重要，且 Claude
// BetaPolicy 的 gin.Context 缓存也是同样取舍。需要 hot-reload 时管理员
// 可以通过踢断 session 强制刷新。
type openAIFastPolicyCtxKeyType struct{}

var openAIFastPolicyCtxKey = openAIFastPolicyCtxKeyType{}

// withOpenAIFastPolicyContext 将一份 settings 快照绑定到 context，供该 ctx
// 衍生 goroutine 中的 evaluateOpenAIFastPolicy 复用。
func withOpenAIFastPolicyContext(ctx context.Context, settings *OpenAIFastPolicySettings) context.Context {
	if ctx == nil || settings == nil {
		return ctx
	}
	return context.WithValue(ctx, openAIFastPolicyCtxKey, settings)
}

func openAIFastPolicySettingsFromContext(ctx context.Context) *OpenAIFastPolicySettings {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(openAIFastPolicyCtxKey).(*OpenAIFastPolicySettings); ok {
		return v
	}
	return nil
}

// applyOpenAIFastPolicyToBody applies the OpenAI fast policy to a raw request
// body. When action=filter it removes the service_tier field; when
// action=block it returns (body, *OpenAIFastBlockedError). On pass it
// normalizes the service_tier value (e.g. client alias "fast" → "priority"),
// rewriting the body so the upstream receives a slug it recognizes.
//
// Rationale for normalize-on-pass: chat-completions / messages 入口在调用本
// 函数之前已经通过 normalizeResponsesBodyServiceTier 把 service_tier 归一化
// 到了上游可识别值；passthrough（OpenAI 自动透传） / native /responses 等
// 入口没有这一前置步骤，pass 路径下若不在此处归一化，"fast" 就会被原样
// 透传到 OpenAI 上游导致 400/拒绝。把归一化收敛到本函数，所有入口行为一致。
func (s *OpenAIGatewayService) applyOpenAIFastPolicyToBody(ctx context.Context, account *Account, model string, body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	rawTier := gjson.GetBytes(body, "service_tier").String()
	if rawTier == "" {
		return body, nil
	}
	normTier := normalizedOpenAIServiceTierValue(rawTier)
	if normTier == "" {
		return body, nil
	}
	action, errMsg := s.evaluateOpenAIFastPolicy(ctx, account, model, normTier)
	switch action {
	case BetaPolicyActionBlock:
		msg := errMsg
		if msg == "" {
			msg = fmt.Sprintf("openai service_tier=%s is not allowed for model %s", normTier, model)
		}
		return body, &OpenAIFastBlockedError{Message: msg}
	case BetaPolicyActionFilter:
		trimmed, err := sjson.DeleteBytes(body, "service_tier")
		if err != nil {
			return body, fmt.Errorf("strip service_tier from body: %w", err)
		}
		return trimmed, nil
	case OpenAIFastPolicyActionForcePriority:
		updated, err := sjson.SetBytes(body, "service_tier", OpenAIFastTierPriority)
		if err != nil {
			return body, fmt.Errorf("force service_tier priority on body: %w", err)
		}
		return updated, nil
	default:
		// pass：把别名（如 "fast"）写回为规范值（"priority"）。
		if normTier == rawTier {
			return body, nil
		}
		updated, err := sjson.SetBytes(body, "service_tier", normTier)
		if err != nil {
			return body, fmt.Errorf("normalize service_tier on pass: %w", err)
		}
		return updated, nil
	}
}

// writeOpenAIFastPolicyBlockedResponse writes a 403 JSON response for a
// request blocked by the OpenAI fast policy.
func writeOpenAIFastPolicyBlockedResponse(c *gin.Context, err *OpenAIFastBlockedError) {
	if c == nil || err == nil {
		return
	}
	MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
	c.JSON(http.StatusForbidden, gin.H{
		"error": gin.H{
			"type":    "permission_error",
			"message": err.Message,
		},
	})
}

// applyOpenAIFastPolicyToWSResponseCreate evaluates the OpenAI fast policy
// against a single client→upstream WebSocket frame whose top-level
// "type"=="response.create". It mirrors the HTTP-side
// applyOpenAIFastPolicyToBody contract but operates on a Realtime/Responses
// WS payload:
//
//   - pass: keeps service_tier, normalizing aliases such as "fast" to "priority"
//   - filter: returns a copy with top-level service_tier removed
//   - force_priority: keeps service_tier and rewrites it to "priority"
//   - block: returns (frame, *OpenAIFastBlockedError)
//
// Only frames whose "type" field strictly equals "response.create" are
// inspected/mutated. Any other frame type — including the empty string —
// passes through untouched. The OpenAI Realtime client-event spec requires
// "type" to be set, so an empty type is treated as a malformed frame we do
// not police; the upstream is the source of truth for rejecting it.
//
// service_tier lives at the top level of response.create — same as the
// Responses HTTP body shape (see openai_gateway_chat_completions.go:304 +
// extractOpenAIServiceTierFromBody at line 5593, and the test fixture at
// openai_ws_forwarder_ingress_session_test.go:402). We therefore only need
// to inspect / strip the top-level field; there is no nested form in the
// schema today.
//
// The caller is responsible for choosing the upstream model passed in —
// this helper does not re-derive it.
func (s *OpenAIGatewayService) applyOpenAIFastPolicyToWSResponseCreate(
	ctx context.Context,
	account *Account,
	model string,
	frame []byte,
) ([]byte, *OpenAIFastBlockedError, error) {
	if len(frame) == 0 {
		return frame, nil, nil
	}
	if !gjson.ValidBytes(frame) {
		return frame, nil, nil
	}
	frameType := strings.TrimSpace(gjson.GetBytes(frame, "type").String())
	// Strict match: only response.create is policy-checked. Empty / other
	// types pass through untouched so we never accidentally strip fields
	// from response.cancel, conversation.item.create, or any future
	// client-event the spec adds. The Realtime spec requires "type" on
	// every client event, so an empty type is malformed input — let the
	// upstream reject it rather than guessing at our layer.
	if frameType != "response.create" {
		return frame, nil, nil
	}
	rawTier := gjson.GetBytes(frame, "service_tier").String()
	if rawTier == "" {
		return frame, nil, nil
	}
	normTier := normalizedOpenAIServiceTierValue(rawTier)
	if normTier == "" {
		return frame, nil, nil
	}
	action, errMsg := s.evaluateOpenAIFastPolicy(ctx, account, model, normTier)
	switch action {
	case BetaPolicyActionBlock:
		msg := errMsg
		if msg == "" {
			msg = fmt.Sprintf("openai service_tier=%s is not allowed for model %s", normTier, model)
		}
		return frame, &OpenAIFastBlockedError{Message: msg}, nil
	case BetaPolicyActionFilter:
		trimmed, err := sjson.DeleteBytes(frame, "service_tier")
		if err != nil {
			return frame, nil, fmt.Errorf("strip service_tier from ws frame: %w", err)
		}
		return trimmed, nil, nil
	case OpenAIFastPolicyActionForcePriority:
		updated, err := sjson.SetBytes(frame, "service_tier", OpenAIFastTierPriority)
		if err != nil {
			return frame, nil, fmt.Errorf("force service_tier priority in ws frame: %w", err)
		}
		return updated, nil, nil
	default:
		if normTier == rawTier {
			return frame, nil, nil
		}
		updated, err := sjson.SetBytes(frame, "service_tier", normTier)
		if err != nil {
			return frame, nil, fmt.Errorf("normalize service_tier in ws frame: %w", err)
		}
		return updated, nil, nil
	}
}

// newOpenAIFastPolicyWSEventID returns a Realtime-style event_id for a
// server-emitted error event. Matches the loose "evt_<rand>" convention used
// by upstream Realtime servers; the exact value is not load-bearing and is
// only required for client-side log correlation. We reuse the existing
// google/uuid dependency rather than pulling a new one.
func newOpenAIFastPolicyWSEventID() string {
	id, err := uuid.NewRandom()
	if err != nil {
		// Extremely unlikely; fall back to a fixed prefix so the field is
		// still non-empty and the schema stays self-consistent.
		return "evt_openai_fast_policy"
	}
	// Strip dashes so it visually matches "evt_<hex>" rather than UUID v4
	// canonical form, mirroring what real Realtime traces look like.
	return "evt_" + strings.ReplaceAll(id.String(), "-", "")
}

// buildOpenAIFastPolicyBlockedWSEvent renders an OpenAI Realtime/Responses
// style "error" event payload for a request blocked by the OpenAI fast
// policy. The shape mirrors Realtime error events as observed in upstream
// traces and per the spec's server "error" event:
//
//	{
//	  "event_id": "evt_<random>",
//	  "type": "error",
//	  "error": {
//	    "type": "invalid_request_error",
//	    "code": "policy_violation",
//	    "message": "..."
//	  }
//	}
//
// event_id lets clients correlate the rejection in their logs; "code" gives
// programmatic clients a stable identifier (HTTP-side equivalent is the
// 403 permission_error JSON body).
func buildOpenAIFastPolicyBlockedWSEvent(err *OpenAIFastBlockedError) []byte {
	if err == nil {
		return nil
	}
	eventID := newOpenAIFastPolicyWSEventID()
	payload, mErr := json.Marshal(map[string]any{
		"event_id": eventID,
		"type":     "error",
		"error": map[string]any{
			"type":    "invalid_request_error",
			"code":    "policy_violation",
			"message": err.Message,
		},
	})
	if mErr != nil {
		// Fallback to a minimal hand-rolled payload; Marshal of the literal
		// shape above should never fail in practice.
		return []byte(`{"event_id":"` + eventID + `","type":"error","error":{"type":"invalid_request_error","code":"policy_violation","message":"openai fast policy blocked this request"}}`)
	}
	return payload
}

func sanitizeEmptyBase64InputImagesInOpenAIBody(body []byte) ([]byte, bool, error) {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"image_url"`)) || !bytes.Contains(body, []byte(`base64,`)) {
		return body, false, nil
	}

	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return body, false, fmt.Errorf("sanitize request body: %w", err)
	}
	if !sanitizeEmptyBase64InputImagesInOpenAIRequestBodyMap(reqBody) {
		return body, false, nil
	}
	normalized, err := json.Marshal(reqBody)
	if err != nil {
		return body, false, fmt.Errorf("serialize sanitized request body: %w", err)
	}
	return normalized, true, nil
}

func sanitizeEmptyBase64InputImagesInOpenAIRequestBodyMap(reqBody map[string]any) bool {
	if reqBody == nil {
		return false
	}
	input, ok := reqBody["input"]
	if !ok {
		return false
	}
	normalizedInput, changed := sanitizeEmptyBase64InputImagesInOpenAIInput(input)
	if !changed {
		return false
	}
	reqBody["input"] = normalizedInput
	return true
}

func sanitizeEmptyBase64InputImagesInOpenAIInput(input any) (any, bool) {
	items, ok := input.([]any)
	if !ok {
		return input, false
	}

	normalizedItems := make([]any, 0, len(items))
	changed := false
	for _, item := range items {
		itemMap, ok := item.(map[string]any)
		if !ok {
			normalizedItems = append(normalizedItems, item)
			continue
		}
		if shouldDropEmptyBase64InputImagePart(itemMap) {
			changed = true
			continue
		}
		content, ok := itemMap["content"]
		if !ok {
			normalizedItems = append(normalizedItems, itemMap)
			continue
		}
		parts, ok := content.([]any)
		if !ok {
			normalizedItems = append(normalizedItems, itemMap)
			continue
		}

		normalizedParts := make([]any, 0, len(parts))
		itemChanged := false
		for _, part := range parts {
			if shouldDropEmptyBase64InputImagePart(part) {
				changed = true
				itemChanged = true
				continue
			}
			normalizedParts = append(normalizedParts, part)
		}
		if itemChanged {
			if len(normalizedParts) == 0 {
				continue
			}
			itemMap["content"] = normalizedParts
		}
		normalizedItems = append(normalizedItems, itemMap)
	}
	if !changed {
		return input, false
	}
	return normalizedItems, true
}

func shouldDropEmptyBase64InputImagePart(part any) bool {
	partMap, ok := part.(map[string]any)
	if !ok {
		return false
	}
	typeValue, _ := partMap["type"].(string)
	if strings.TrimSpace(typeValue) != "input_image" {
		return false
	}
	imageURL, _ := partMap["image_url"].(string)
	return isEmptyBase64DataURI(imageURL)
}

func isEmptyBase64DataURI(raw string) bool {
	if !strings.HasPrefix(raw, "data:") {
		return false
	}
	rest := strings.TrimPrefix(raw, "data:")
	semicolonIdx := strings.Index(rest, ";")
	if semicolonIdx < 0 {
		return false
	}
	rest = rest[semicolonIdx+1:]
	if !strings.HasPrefix(rest, "base64,") {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(rest, "base64,")) == ""
}

func getOpenAIRequestBodyMap(c *gin.Context, body []byte) (map[string]any, error) {
	if c != nil {
		if cached, ok := c.Get(OpenAIParsedRequestBodyKey); ok {
			if reqBody, ok := cached.(map[string]any); ok && reqBody != nil {
				return reqBody, nil
			}
		}
	}

	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	if c != nil {
		c.Set(OpenAIParsedRequestBodyKey, reqBody)
	}
	return reqBody, nil
}

func releaseOpenAIParsedRequestBody(c *gin.Context) {
	if c == nil {
		return
	}
	delete(c.Keys, OpenAIParsedRequestBodyKey)
}

func extractOpenAIReasoningEffort(reqBody map[string]any, modelCandidates ...string) *string {
	if value, present := getOpenAIReasoningEffortFromReqBody(reqBody, firstNonEmpty(modelCandidates...)); present {
		if value == "" {
			return nil
		}
		return &value
	}

	value := deriveOpenAIReasoningEffortFromModelCandidates(modelCandidates)
	if value == "" {
		return nil
	}
	return &value
}

func normalizeOpenAIReasoningEffort(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return ""
	}

	// Normalize separators for "x-high"/"x_high" variants.
	value = strings.NewReplacer("-", "", "_", "", " ", "").Replace(value)

	switch value {
	case "none", "minimal":
		return ""
	case "low", "medium", "high":
		return value
	case "xhigh", "extrahigh", "max":
		return "xhigh"
	default:
		// Only store known effort levels for now to keep UI consistent.
		return ""
	}
}

func normalizeOpenAIReasoningEffortForModel(raw, model string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "max") && isOpenAIGPT56Model(model) {
		return "max"
	}
	return normalizeOpenAIReasoningEffort(raw)
}
