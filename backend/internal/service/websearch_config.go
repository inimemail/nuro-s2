package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/websearch"
)

// WebSearchEmulationConfig holds the global web search emulation configuration.
type WebSearchEmulationConfig struct {
	Enabled   bool                      `json:"enabled"`
	Providers []WebSearchProviderConfig `json:"providers"`
}

// WebSearchProviderConfig describes a single search provider.
type WebSearchProviderConfig struct {
	Type             string `json:"type"`                    // Brave, Tavily or Exa
	APIKey           string `json:"api_key,omitempty"`       // secret — omitted in API responses
	APIKeyConfigured bool   `json:"api_key_configured"`      // read-only mask
	QuotaLimit       *int64 `json:"quota_limit"`             // nil = unlimited, >0 = limited
	SubscribedAt     *int64 `json:"subscribed_at,omitempty"` // subscription start (unix seconds); quota resets monthly
	QuotaUsed        int64  `json:"quota_used,omitempty"`    // read-only: current usage from Redis
	ProxyID          *int64 `json:"proxy_id"`                // optional proxy association
	ExpiresAt        *int64 `json:"expires_at,omitempty"`    // optional expiration timestamp
}

// --- Validation ---

const maxWebSearchProviders = 10

// Older nodes route unrecognized providers through Brave. Keep Exa credentials
// in a separate setting that those nodes do not read.
const settingKeyWebSearchExa = "web_search_emulation_exa"

type webSearchExaSetting struct {
	Provider WebSearchProviderConfig `json:"provider"`
	Position int                     `json:"position"`
}

var validProviderTypes = map[string]bool{
	websearch.ProviderTypeBrave:  true,
	websearch.ProviderTypeTavily: true,
	websearch.ProviderTypeExa:    true,
}

func validateWebSearchConfig(cfg *WebSearchEmulationConfig) error {
	if cfg == nil {
		return nil
	}
	if len(cfg.Providers) > maxWebSearchProviders {
		return fmt.Errorf("too many providers (max %d)", maxWebSearchProviders)
	}
	seen := make(map[string]bool, len(cfg.Providers))
	for i, p := range cfg.Providers {
		if !validProviderTypes[p.Type] {
			return fmt.Errorf("provider[%d]: invalid type %q", i, p.Type)
		}
		if p.QuotaLimit != nil && *p.QuotaLimit < 0 {
			return fmt.Errorf("provider[%d]: quota_limit must be > 0 or null", i)
		}
		if seen[p.Type] {
			return fmt.Errorf("provider[%d]: duplicate type %q", i, p.Type)
		}
		seen[p.Type] = true
	}
	return nil
}

// --- In-process cache (same pattern as gateway forwarding settings) ---

type cachedWebSearchEmulationConfig struct {
	config    *WebSearchEmulationConfig
	err       error
	proxyURLs map[int64]string
	expiresAt int64 // unix nano
}

var webSearchEmulationCache atomic.Pointer[cachedWebSearchEmulationConfig]

// Serialize reads on cache misses, writes and manager publication.
var webSearchConfigMu sync.Mutex

const (
	webSearchEmulationCacheTTL  = 60 * time.Second
	webSearchEmulationErrorTTL  = 5 * time.Second
	webSearchEmulationDBTimeout = 5 * time.Second
)

// GetWebSearchEmulationConfig returns the in-process snapshot when it is fresh.
func (s *SettingService) GetWebSearchEmulationConfig(ctx context.Context) (*WebSearchEmulationConfig, error) {
	if cached := webSearchEmulationCache.Load(); cached != nil && time.Now().UnixNano() < cached.expiresAt {
		return cached.config, cached.err
	}
	webSearchConfigMu.Lock()
	defer webSearchConfigMu.Unlock()
	return s.getWebSearchConfigLocked(ctx)
}

func (s *SettingService) getWebSearchConfigLocked(ctx context.Context) (*WebSearchEmulationConfig, error) {
	previous := webSearchEmulationCache.Load()
	if previous != nil && time.Now().UnixNano() < previous.expiresAt {
		return previous.config, previous.err
	}
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webSearchEmulationDBTimeout)
	defer cancel()

	cfg, err := s.getWebSearchEmulationConfigRaw(dbCtx)
	if errors.Is(err, ErrSettingNotFound) {
		cfg, err = &WebSearchEmulationConfig{}, nil
	}
	if err != nil {
		cfg = &WebSearchEmulationConfig{}
	}
	s.publishWebSearchConfig(dbCtx, previous, cfg, err)
	return cfg, err
}

func parseWebSearchConfigJSON(raw string) *WebSearchEmulationConfig {
	cfg := &WebSearchEmulationConfig{}
	if raw == "" {
		return cfg
	}
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		slog.Warn("websearch: failed to parse config JSON", "error", err)
		return &WebSearchEmulationConfig{}
	}
	return cfg
}

// SaveWebSearchEmulationConfig validates and persists the configuration.
// Empty API keys in the input are preserved from the existing config.
func (s *SettingService) SaveWebSearchEmulationConfig(ctx context.Context, cfg *WebSearchEmulationConfig) error {
	if cfg == nil {
		return infraerrors.BadRequest("INVALID_WEB_SEARCH_CONFIG", "missing web search configuration")
	}
	if err := validateWebSearchConfig(cfg); err != nil {
		return infraerrors.BadRequest("INVALID_WEB_SEARCH_CONFIG", err.Error())
	}
	webSearchConfigMu.Lock()
	defer webSearchConfigMu.Unlock()

	// Keep the published snapshot independent of the caller's request data.
	copyConfig := *cfg
	copyConfig.Providers = slices.Clone(cfg.Providers)
	cfg = &copyConfig
	if err := s.mergeExistingAPIKeys(ctx, cfg); err != nil {
		return fmt.Errorf("websearch: load existing API keys: %w", err)
	}

	// After merge, validate all enabled providers have API keys
	if cfg.Enabled {
		for _, p := range cfg.Providers {
			if p.APIKey == "" {
				return infraerrors.BadRequest("MISSING_API_KEY",
					fmt.Sprintf("provider %s has no API key configured", p.Type))
			}
		}
	}

	settings, err := marshalWebSearchSettings(cfg)
	if err != nil {
		return fmt.Errorf("websearch: marshal config: %w", err)
	}
	if err := s.settingRepo.SetMultiple(ctx, settings); err != nil {
		return fmt.Errorf("websearch: save config: %w", err)
	}

	reloadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webSearchEmulationDBTimeout)
	defer cancel()
	s.publishWebSearchConfig(reloadCtx, webSearchEmulationCache.Load(), cfg, nil)

	return nil
}

func marshalWebSearchSettings(cfg *WebSearchEmulationConfig) (map[string]string, error) {
	legacy := *cfg
	exa := ""
	for index, provider := range cfg.Providers {
		if provider.Type != websearch.ProviderTypeExa {
			continue
		}
		body, err := json.Marshal(webSearchExaSetting{Provider: provider, Position: index})
		if err != nil {
			return nil, err
		}
		exa = string(body)
		legacy.Providers = slices.Delete(slices.Clone(cfg.Providers), index, index+1)
		break
	}
	body, err := json.Marshal(legacy)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		SettingKeyWebSearchEmulationConfig: string(body),
		settingKeyWebSearchExa:             exa,
	}, nil
}

func webSearchConfigFromSettings(settings map[string]string) (*WebSearchEmulationConfig, error) {
	cfg := parseWebSearchConfigJSON(settings[SettingKeyWebSearchEmulationConfig])
	raw := settings[settingKeyWebSearchExa]
	if raw == "" {
		return cfg, nil
	}
	var exa webSearchExaSetting
	if err := json.Unmarshal([]byte(raw), &exa); err != nil {
		return nil, fmt.Errorf("websearch: decode Exa configuration: %w", err)
	}
	if exa.Provider.Type != websearch.ProviderTypeExa {
		return nil, errors.New("websearch: invalid Exa configuration")
	}
	cfg.Providers = slices.DeleteFunc(cfg.Providers, func(provider WebSearchProviderConfig) bool {
		return provider.Type == websearch.ProviderTypeExa
	})
	position := min(max(exa.Position, 0), len(cfg.Providers))
	cfg.Providers = slices.Insert(cfg.Providers, position, exa.Provider)
	return cfg, nil
}

// mergeExistingAPIKeys preserves API keys from the current config when incoming value is empty.
func (s *SettingService) mergeExistingAPIKeys(ctx context.Context, cfg *WebSearchEmulationConfig) error {
	needsExisting := false
	for i := range cfg.Providers {
		cfg.Providers[i].APIKey = strings.TrimSpace(cfg.Providers[i].APIKey)
		needsExisting = needsExisting || cfg.Providers[i].APIKey == ""
	}
	if !needsExisting {
		return nil
	}
	existing, err := s.getWebSearchEmulationConfigRaw(ctx)
	if errors.Is(err, ErrSettingNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	existingByType := make(map[string]string, len(existing.Providers))
	for _, p := range existing.Providers {
		if p.APIKey != "" {
			existingByType[p.Type] = p.APIKey
		}
	}
	for i := range cfg.Providers {
		if cfg.Providers[i].APIKey == "" {
			if key, ok := existingByType[cfg.Providers[i].Type]; ok {
				cfg.Providers[i].APIKey = key
			}
		}
	}
	return nil
}

func (s *SettingService) getWebSearchEmulationConfigRaw(ctx context.Context) (*WebSearchEmulationConfig, error) {
	settings, err := s.settingRepo.GetMultiple(ctx, []string{SettingKeyWebSearchEmulationConfig, settingKeyWebSearchExa})
	if err != nil {
		return nil, err
	}
	if len(settings) == 0 {
		return nil, ErrSettingNotFound
	}
	return webSearchConfigFromSettings(settings)
}

// IsWebSearchEmulationEnabled is a quick check for whether the global switch is on.
func (s *SettingService) IsWebSearchEmulationEnabled(ctx context.Context) bool {
	cfg, err := s.GetWebSearchEmulationConfig(ctx)
	if err != nil {
		return false
	}
	return cfg.Enabled && len(cfg.Providers) > 0
}

// SetWebSearchManagerBuilder injects a callback that creates and wires a websearch.Manager.
// The infra layer (main/wire) provides this builder, keeping redis out of the service layer.
// Triggers initial build.
func (s *SettingService) SetWebSearchManagerBuilder(ctx context.Context, builder WebSearchManagerBuilder) {
	webSearchConfigMu.Lock()
	defer webSearchConfigMu.Unlock()
	s.webSearchManagerBuilder = builder
	// Force a read so the new builder receives the current settings once.
	webSearchEmulationCache.Store(nil)
	_, _ = s.getWebSearchConfigLocked(ctx)
}

// publishWebSearchConfig runs under webSearchConfigMu. Proxy rows can change
// independently of the settings, so refresh them before comparing snapshots.
func (s *SettingService) publishWebSearchConfig(ctx context.Context, previous *cachedWebSearchEmulationConfig, cfg *WebSearchEmulationConfig, configErr error) {
	snapshot := &cachedWebSearchEmulationConfig{config: cfg, err: configErr}
	var proxyErr error
	if s.webSearchManagerBuilder != nil {
		snapshot.proxyURLs, proxyErr = s.resolveProviderProxyURLs(ctx, cfg)
		if previous == nil || !reflect.DeepEqual(previous.config, cfg) ||
			!maps.Equal(previous.proxyURLs, snapshot.proxyURLs) {
			s.webSearchManagerBuilder(cfg, snapshot.proxyURLs)
		}
	}
	ttl := webSearchEmulationCacheTTL
	if configErr != nil || proxyErr != nil {
		ttl = webSearchEmulationErrorTTL
	}
	snapshot.expiresAt = time.Now().Add(ttl).UnixNano()
	webSearchEmulationCache.Store(snapshot)
}

// resolveProviderProxyURLs collects proxy IDs from providers and resolves them to URLs.
func (s *SettingService) resolveProviderProxyURLs(ctx context.Context, cfg *WebSearchEmulationConfig) (map[int64]string, error) {
	if cfg == nil || !cfg.Enabled || s.proxyRepo == nil {
		return nil, nil
	}
	var ids []int64
	for _, p := range cfg.Providers {
		if p.ProxyID != nil && *p.ProxyID > 0 {
			ids = append(ids, *p.ProxyID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	proxies, err := s.proxyRepo.ListByIDs(ctx, ids)
	if err != nil {
		slog.Warn("websearch: failed to resolve proxy URLs", "error", err)
		return nil, err
	}
	result := make(map[int64]string, len(proxies))
	now := time.Now()
	for _, px := range proxies {
		if px.IsActive() && !px.IsExpired(now) {
			result[px.ID] = px.URL()
		}
	}
	return result, nil
}

// WebSearchTestResult holds the result of a search test.
type WebSearchTestResult struct {
	Provider string                   `json:"provider"`
	Results  []websearch.SearchResult `json:"results"`
	Query    string                   `json:"query"`
}

// TestWebSearch executes a test search using the currently configured Manager.
// Uses Manager.TestSearch which bypasses quota tracking.
const testSearchTimeout = 15 * time.Second

func TestWebSearch(ctx context.Context, query string) (*WebSearchTestResult, error) {
	mgr := getWebSearchManager()
	if mgr == nil {
		return nil, fmt.Errorf("web search: manager not initialized, save config first")
	}
	testCtx, cancel := context.WithTimeout(ctx, testSearchTimeout)
	defer cancel()
	resp, providerName, err := mgr.TestSearch(testCtx, websearch.SearchRequest{
		Query:      query,
		MaxResults: webSearchDefaultMaxResults,
	})
	if err != nil {
		return nil, err
	}
	return &WebSearchTestResult{
		Provider: providerName,
		Results:  resp.Results,
		Query:    resp.Query,
	}, nil
}

// PopulateWebSearchUsage returns a copy with quota usage populated from Redis (api_key kept as-is).
func PopulateWebSearchUsage(ctx context.Context, cfg *WebSearchEmulationConfig) *WebSearchEmulationConfig {
	if cfg == nil {
		return nil
	}
	out := *cfg
	out.Providers = make([]WebSearchProviderConfig, len(cfg.Providers))

	mgr := getWebSearchManager()

	for i, p := range cfg.Providers {
		out.Providers[i] = p
		out.Providers[i].APIKeyConfigured = p.APIKey != ""

		if mgr != nil {
			used, _ := mgr.GetUsage(ctx, p.Type)
			out.Providers[i].QuotaUsed = used
		}
	}
	return &out
}

// ResetWebSearchUsage deletes the Redis quota key for the given provider type.
func ResetWebSearchUsage(ctx context.Context, providerType string) error {
	mgr := getWebSearchManager()
	if mgr == nil {
		return fmt.Errorf("web search manager not initialized")
	}
	return mgr.ResetUsage(ctx, providerType)
}

// SanitizeWebSearchConfig returns a copy with api_key fields masked and quota usage populated.
func SanitizeWebSearchConfig(ctx context.Context, cfg *WebSearchEmulationConfig) *WebSearchEmulationConfig {
	if cfg == nil {
		return nil
	}
	out := *cfg
	out.Providers = make([]WebSearchProviderConfig, len(cfg.Providers))

	// Load usage from the global Manager (reads from Redis)
	mgr := getWebSearchManager()

	for i, p := range cfg.Providers {
		out.Providers[i] = p
		out.Providers[i].APIKeyConfigured = p.APIKey != ""
		out.Providers[i].APIKey = "" // never return the secret

		// Populate quota usage from Redis
		if mgr != nil {
			used, _ := mgr.GetUsage(ctx, p.Type)
			out.Providers[i].QuotaUsed = used
		}
	}
	return &out
}
