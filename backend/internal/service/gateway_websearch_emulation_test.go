//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/websearch"
	"github.com/stretchr/testify/require"
)

// --- isOnlyWebSearchToolInBody ---

func TestIsOnlyWebSearchToolInBody_WebSearchType(t *testing.T) {
	require.True(t, isOnlyWebSearchToolInBody([]byte(`{"tools":[{"type":"web_search"}]}`)))
}

func TestIsOnlyWebSearchToolInBody_WebSearch2025Type(t *testing.T) {
	require.True(t, isOnlyWebSearchToolInBody([]byte(`{"tools":[{"type":"web_search_20250305"}]}`)))
}

func TestIsOnlyWebSearchToolInBody_GoogleSearchType(t *testing.T) {
	require.True(t, isOnlyWebSearchToolInBody([]byte(`{"tools":[{"type":"google_search"}]}`)))
}

func TestIsOnlyWebSearchToolInBody_NameWebSearch(t *testing.T) {
	require.True(t, isOnlyWebSearchToolInBody([]byte(`{"tools":[{"name":"web_search"}]}`)))
}

func TestIsOnlyWebSearchToolInBody_NameWebSearch2025(t *testing.T) {
	require.True(t, isOnlyWebSearchToolInBody([]byte(`{"tools":[{"name":"web_search_20250305"}]}`)))
}

func TestIsOnlyWebSearchToolInBody_NameGoogleSearch(t *testing.T) {
	require.True(t, isOnlyWebSearchToolInBody([]byte(`{"tools":[{"name":"google_search"}]}`)))
}

func TestIsOnlyWebSearchToolInBody_MultipleTools(t *testing.T) {
	require.False(t, isOnlyWebSearchToolInBody(
		[]byte(`{"tools":[{"type":"web_search"},{"type":"text_editor"}]}`)))
}

func TestIsOnlyWebSearchToolInBody_NoTools(t *testing.T) {
	require.False(t, isOnlyWebSearchToolInBody([]byte(`{"model":"claude-3"}`)))
}

func TestIsOnlyWebSearchToolInBody_EmptyToolsArray(t *testing.T) {
	require.False(t, isOnlyWebSearchToolInBody([]byte(`{"tools":[]}`)))
}

func TestIsOnlyWebSearchToolInBody_NonWebSearchTool(t *testing.T) {
	require.False(t, isOnlyWebSearchToolInBody([]byte(`{"tools":[{"type":"text_editor"}]}`)))
}

func TestIsOnlyWebSearchToolInBody_ToolsNotArray(t *testing.T) {
	require.False(t, isOnlyWebSearchToolInBody([]byte(`{"tools":"web_search"}`)))
}

// --- extractSearchQueryFromBody ---

func TestExtractSearchQueryFromBody_StringContent(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"what is golang"}]}`
	require.Equal(t, "what is golang", extractSearchQueryFromBody([]byte(body)))
}

func TestExtractSearchQueryFromBody_ArrayContent(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"search this"}]}]}`
	require.Equal(t, "search this", extractSearchQueryFromBody([]byte(body)))
}

func TestExtractSearchQueryFromBody_MultipleMessages(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}]}`
	require.Equal(t, "second", extractSearchQueryFromBody([]byte(body)))
}

func TestExtractSearchQueryFromBody_LastMessageNotUser(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":"a"}]}`
	require.Equal(t, "", extractSearchQueryFromBody([]byte(body)))
}

func TestExtractSearchQueryFromBody_EmptyMessages(t *testing.T) {
	require.Equal(t, "", extractSearchQueryFromBody([]byte(`{"messages":[]}`)))
}

func TestExtractSearchQueryFromBody_NoMessages(t *testing.T) {
	require.Equal(t, "", extractSearchQueryFromBody([]byte(`{"model":"claude-3"}`)))
}

func TestExtractSearchQueryFromBody_ArrayContentSkipsEmptyText(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"image"},{"type":"text","text":""},{"type":"text","text":"real query"}]}]}`
	require.Equal(t, "real query", extractSearchQueryFromBody([]byte(body)))
}

func TestExtractSearchQueryFromBody_ArrayContentNoTextBlock(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`
	require.Equal(t, "", extractSearchQueryFromBody([]byte(body)))
}

// --- buildSearchResultBlocks ---

func TestBuildSearchResultBlocks_WithResults(t *testing.T) {
	results := []websearch.SearchResult{
		{URL: "https://a.com", Title: "A", Snippet: "snippet a", PageAge: "2 days"},
		{URL: "https://b.com", Title: "B", Snippet: "snippet b"},
	}
	blocks := buildSearchResultBlocks(results)
	require.Len(t, blocks, 2)
	require.Equal(t, "web_search_result", blocks[0]["type"])
	require.Equal(t, "https://a.com", blocks[0]["url"])
	require.Equal(t, "snippet a", blocks[0]["page_content"])
	require.Equal(t, "2 days", blocks[0]["page_age"])
	// Second result has no PageAge
	require.Equal(t, "https://b.com", blocks[1]["url"])
	_, hasPageAge := blocks[1]["page_age"]
	require.False(t, hasPageAge)
}

func TestBuildSearchResultBlocks_Empty(t *testing.T) {
	blocks := buildSearchResultBlocks(nil)
	require.Empty(t, blocks)
}

func TestBuildSearchResultBlocks_SnippetEmpty(t *testing.T) {
	blocks := buildSearchResultBlocks([]websearch.SearchResult{{URL: "https://x.com", Title: "X", Snippet: ""}})
	_, hasContent := blocks[0]["page_content"]
	require.False(t, hasContent)
}

// --- buildTextSummary ---

func TestBuildTextSummary_WithResults(t *testing.T) {
	results := []websearch.SearchResult{
		{URL: "https://a.com", Title: "A", Snippet: "desc a"},
	}
	summary := buildTextSummary("test query", results)
	require.Contains(t, summary, "test query")
	require.Contains(t, summary, "1. **A**")
	require.Contains(t, summary, "https://a.com")
}

func TestBuildTextSummary_NoResults(t *testing.T) {
	summary := buildTextSummary("test", nil)
	require.Contains(t, summary, "No search results found for: test")
}

// --- shouldEmulateWebSearch ---

// webSearchToolBody is a valid request body with exactly one web_search tool.
var webSearchToolBody = []byte(`{"tools":[{"type":"web_search"}],"messages":[{"role":"user","content":"test"}]}`)

// nonWebSearchToolBody is a request body without web_search tool.
var nonWebSearchToolBody = []byte(`{"tools":[{"type":"text_editor"}],"messages":[{"role":"user","content":"test"}]}`)

// newAnthropicAPIKeyAccount creates a test Account with the given web search emulation mode.
func newAnthropicAPIKeyAccount(mode string) *Account {
	return &Account{
		ID:       1,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{featureKeyWebSearchEmulation: mode},
	}
}

// setGlobalWebSearchConfig stores a config in the global cache used by SettingService.IsWebSearchEmulationEnabled.
func setGlobalWebSearchConfig(cfg *WebSearchEmulationConfig) {
	webSearchEmulationCache.Store(&cachedWebSearchEmulationConfig{
		config:    cfg,
		expiresAt: time.Now().Add(10 * time.Minute).UnixNano(),
	})
}

// clearGlobalWebSearchConfig resets the global cache to force re-read.
func clearGlobalWebSearchConfig() {
	webSearchEmulationCache.Store((*cachedWebSearchEmulationConfig)(nil))
}

// newSettingServiceForWebSearchTest creates a SettingService with a mock repo pre-loaded with config.
func newSettingServiceForWebSearchTest(enabled bool) *SettingService {
	repo := newMockSettingRepo()
	cfg := &WebSearchEmulationConfig{
		Enabled:   enabled,
		Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "sk-test"}},
	}
	data, _ := json.Marshal(cfg)
	repo.data[SettingKeyWebSearchEmulationConfig] = string(data)
	return NewSettingService(repo, &config.Config{})
}

// newChannelServiceWithCache creates a ChannelService with a pre-built cache containing the channel.
func newChannelServiceWithCache(groupID int64, ch *Channel) *ChannelService {
	svc := &ChannelService{}
	cache := &channelCache{
		channelByGroupID: map[int64]*Channel{groupID: ch},
		byID:             map[int64]*Channel{ch.ID: ch},
		groupPlatform:    map[int64]string{},
		loadedAt:         time.Now(),
	}
	svc.cache.Store(cache)
	return svc
}

func TestShouldEmulateWebSearch_NilManager(t *testing.T) {
	SetWebSearchManager(nil)
	defer SetWebSearchManager(nil)

	settingSvc := newSettingServiceForWebSearchTest(true)
	setGlobalWebSearchConfig(&WebSearchEmulationConfig{
		Enabled:   true,
		Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "k"}},
	})
	defer clearGlobalWebSearchConfig()

	svc := &GatewayService{settingService: settingSvc}
	account := newAnthropicAPIKeyAccount(WebSearchModeEnabled)
	require.False(t, svc.shouldEmulateWebSearch(context.Background(), account, nil, webSearchToolBody))
}

func TestShouldEmulateWebSearch_NotOnlyWebSearchTool(t *testing.T) {
	mgr := websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "k"}}, nil)
	SetWebSearchManager(mgr)
	defer SetWebSearchManager(nil)

	settingSvc := newSettingServiceForWebSearchTest(true)
	setGlobalWebSearchConfig(&WebSearchEmulationConfig{
		Enabled:   true,
		Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "k"}},
	})
	defer clearGlobalWebSearchConfig()

	svc := &GatewayService{settingService: settingSvc}
	account := newAnthropicAPIKeyAccount(WebSearchModeEnabled)
	require.False(t, svc.shouldEmulateWebSearch(context.Background(), account, nil, nonWebSearchToolBody))
}

func TestShouldEmulateWebSearch_GlobalDisabled(t *testing.T) {
	mgr := websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "k"}}, nil)
	SetWebSearchManager(mgr)
	defer SetWebSearchManager(nil)

	// Global config disabled
	setGlobalWebSearchConfig(&WebSearchEmulationConfig{
		Enabled:   false,
		Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "k"}},
	})
	defer clearGlobalWebSearchConfig()

	settingSvc := newSettingServiceForWebSearchTest(false)
	svc := &GatewayService{settingService: settingSvc}
	account := newAnthropicAPIKeyAccount(WebSearchModeEnabled)
	require.False(t, svc.shouldEmulateWebSearch(context.Background(), account, nil, webSearchToolBody))
}

func TestShouldEmulateWebSearch_AccountDisabled(t *testing.T) {
	mgr := websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "k"}}, nil)
	SetWebSearchManager(mgr)
	defer SetWebSearchManager(nil)

	setGlobalWebSearchConfig(&WebSearchEmulationConfig{
		Enabled:   true,
		Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "k"}},
	})
	defer clearGlobalWebSearchConfig()

	settingSvc := newSettingServiceForWebSearchTest(true)
	svc := &GatewayService{settingService: settingSvc}
	account := newAnthropicAPIKeyAccount(WebSearchModeDisabled)
	require.False(t, svc.shouldEmulateWebSearch(context.Background(), account, nil, webSearchToolBody))
}

func TestShouldEmulateWebSearch_AccountEnabled(t *testing.T) {
	mgr := websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "k"}}, nil)
	SetWebSearchManager(mgr)
	defer SetWebSearchManager(nil)

	setGlobalWebSearchConfig(&WebSearchEmulationConfig{
		Enabled:   true,
		Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "k"}},
	})
	defer clearGlobalWebSearchConfig()

	settingSvc := newSettingServiceForWebSearchTest(true)
	svc := &GatewayService{settingService: settingSvc}
	account := newAnthropicAPIKeyAccount(WebSearchModeEnabled)
	require.True(t, svc.shouldEmulateWebSearch(context.Background(), account, nil, webSearchToolBody))
}

func TestShouldEmulateWebSearch_DefaultMode_ChannelEnabled(t *testing.T) {
	mgr := websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "k"}}, nil)
	SetWebSearchManager(mgr)
	defer SetWebSearchManager(nil)

	setGlobalWebSearchConfig(&WebSearchEmulationConfig{
		Enabled:   true,
		Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "k"}},
	})
	defer clearGlobalWebSearchConfig()

	settingSvc := newSettingServiceForWebSearchTest(true)
	ch := &Channel{
		ID:     10,
		Status: StatusActive,
		FeaturesConfig: map[string]any{
			featureKeyWebSearchEmulation: map[string]any{PlatformAnthropic: true},
		},
	}
	channelSvc := newChannelServiceWithCache(42, ch)
	svc := &GatewayService{settingService: settingSvc, channelService: channelSvc}

	account := newAnthropicAPIKeyAccount(WebSearchModeDefault)
	groupID := int64(42)
	require.True(t, svc.shouldEmulateWebSearch(context.Background(), account, &groupID, webSearchToolBody))
}

func TestShouldEmulateWebSearch_DefaultMode_ChannelDisabled(t *testing.T) {
	mgr := websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "k"}}, nil)
	SetWebSearchManager(mgr)
	defer SetWebSearchManager(nil)

	setGlobalWebSearchConfig(&WebSearchEmulationConfig{
		Enabled:   true,
		Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "k"}},
	})
	defer clearGlobalWebSearchConfig()

	settingSvc := newSettingServiceForWebSearchTest(true)
	ch := &Channel{
		ID:     10,
		Status: StatusActive,
		FeaturesConfig: map[string]any{
			featureKeyWebSearchEmulation: map[string]any{PlatformAnthropic: false},
		},
	}
	channelSvc := newChannelServiceWithCache(42, ch)
	svc := &GatewayService{settingService: settingSvc, channelService: channelSvc}

	account := newAnthropicAPIKeyAccount(WebSearchModeDefault)
	groupID := int64(42)
	require.False(t, svc.shouldEmulateWebSearch(context.Background(), account, &groupID, webSearchToolBody))
}

func TestShouldEmulateWebSearch_DefaultMode_NilGroupID(t *testing.T) {
	mgr := websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "k"}}, nil)
	SetWebSearchManager(mgr)
	defer SetWebSearchManager(nil)

	setGlobalWebSearchConfig(&WebSearchEmulationConfig{
		Enabled:   true,
		Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "k"}},
	})
	defer clearGlobalWebSearchConfig()

	settingSvc := newSettingServiceForWebSearchTest(true)
	svc := &GatewayService{settingService: settingSvc}
	account := newAnthropicAPIKeyAccount(WebSearchModeDefault)
	// nil groupID + default mode → falls through to channel check → returns false
	require.False(t, svc.shouldEmulateWebSearch(context.Background(), account, nil, webSearchToolBody))
}

func TestShouldEmulateWebSearch_DefaultMode_NilChannelService(t *testing.T) {
	mgr := websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "k"}}, nil)
	SetWebSearchManager(mgr)
	defer SetWebSearchManager(nil)

	setGlobalWebSearchConfig(&WebSearchEmulationConfig{
		Enabled:   true,
		Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "k"}},
	})
	defer clearGlobalWebSearchConfig()

	settingSvc := newSettingServiceForWebSearchTest(true)
	svc := &GatewayService{settingService: settingSvc, channelService: nil}
	account := newAnthropicAPIKeyAccount(WebSearchModeDefault)
	groupID := int64(42)
	// nil channelService + default mode → returns false
	require.False(t, svc.shouldEmulateWebSearch(context.Background(), account, &groupID, webSearchToolBody))
}

func TestShouldEmulateWebSearchFor_OpenAIAPIKeyAccountEnabled(t *testing.T) {
	mgr := websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "k"}}, nil)
	SetWebSearchManager(mgr)
	defer SetWebSearchManager(nil)

	setGlobalWebSearchConfig(&WebSearchEmulationConfig{
		Enabled:   true,
		Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "k"}},
	})
	defer clearGlobalWebSearchConfig()

	settingSvc := newSettingServiceForWebSearchTest(true)
	account := &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{featureKeyWebSearchEmulation: WebSearchModeEnabled},
	}
	groupID := int64(0)
	require.True(t, shouldEmulateWebSearchFor(context.Background(), settingSvc, nil, account, &groupID, webSearchToolBody))
	require.False(t, shouldEmulateWebSearchFor(context.Background(), settingSvc, nil, account, &groupID, nonWebSearchToolBody))
}

// Exercise the shared interception gate, including channel inheritance.
func TestShouldEmulateWebSearchFor_AccountChannelModes(t *testing.T) {
	previousManager := getWebSearchManager()
	previousConfig := webSearchEmulationCache.Load()
	SetWebSearchManager(websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "k"}}, nil))
	t.Cleanup(func() {
		SetWebSearchManager(previousManager)
		if previousConfig == nil {
			clearGlobalWebSearchConfig()
		} else {
			webSearchEmulationCache.Store(previousConfig)
		}
	})

	settingSvc := newSettingServiceForWebSearchTest(true)
	groupID := int64(42)
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth, AccountTypeSetupToken} {
			for _, mode := range []string{WebSearchModeDefault, WebSearchModeEnabled, WebSearchModeDisabled} {
				for _, channelEnabled := range []bool{false, true} {
					channelMode := "channel_off"
					if channelEnabled {
						channelMode = "channel_on"
					}
					t.Run(platform+"/"+accountType+"/"+mode+"/"+channelMode, func(t *testing.T) {
						account := &Account{
							Platform: platform, Type: accountType,
							Extra: map[string]any{featureKeyWebSearchEmulation: mode},
						}
						channelSvc := newChannelServiceWithCache(groupID, &Channel{
							ID: 10, Status: StatusActive,
							FeaturesConfig: map[string]any{
								featureKeyWebSearchEmulation: map[string]any{platform: channelEnabled},
							},
						})
						want := channelEnabled
						if accountType == AccountTypeAPIKey {
							switch mode {
							case WebSearchModeEnabled:
								want = true
							case WebSearchModeDisabled:
								want = false
							}
						} else if platform == PlatformOpenAI {
							want = false
						}
						for _, globalEnabled := range []bool{false, true} {
							setGlobalWebSearchConfig(&WebSearchEmulationConfig{
								Enabled:   globalEnabled,
								Providers: []WebSearchProviderConfig{{Type: "brave", APIKey: "k"}},
							})
							require.Equal(t, globalEnabled && want, shouldEmulateWebSearchFor(
								context.Background(), settingSvc, channelSvc, account, &groupID, webSearchToolBody),
								"globalEnabled=%v", globalEnabled)
						}
					})
				}
			}
		}
	}
}

type webSearchSnapshotRepo struct {
	*mockSettingRepo
	beforeReturn func()
	readErr      error
}

func (r *webSearchSnapshotRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	if r.readErr != nil {
		return nil, r.readErr
	}
	settings, err := r.mockSettingRepo.GetMultiple(ctx, keys)
	if r.beforeReturn != nil {
		r.beforeReturn()
	}
	return settings, err
}

func TestWebSearchConfigCacheKeepsReadErrorsAndReloadsAfterRecovery(t *testing.T) {
	previous := webSearchEmulationCache.Load()
	clearGlobalWebSearchConfig()
	t.Cleanup(func() { webSearchEmulationCache.Store(previous) })
	dbErr := errors.New("database unavailable")
	repo := &webSearchSnapshotRepo{mockSettingRepo: newMockSettingRepo(), readErr: dbErr}
	svc := NewSettingService(repo, &config.Config{})
	for range 2 {
		_, err := svc.GetWebSearchEmulationConfig(t.Context())
		require.ErrorIs(t, err, dbErr)
	}

	repo.readErr = nil
	clearGlobalWebSearchConfig()
	cfg, err := svc.GetWebSearchEmulationConfig(t.Context())
	require.NoError(t, err)
	require.False(t, cfg.Enabled)
}

func TestWebSearchConfigPublishesLatestSaveAndReloadsPeerUpdates(t *testing.T) {
	previous := webSearchEmulationCache.Load()
	t.Cleanup(func() { webSearchEmulationCache.Store(previous) })
	repo := &webSearchSnapshotRepo{mockSettingRepo: newMockSettingRepo()}
	svc := NewSettingService(repo, &config.Config{})
	old := &WebSearchEmulationConfig{Enabled: true, Providers: []WebSearchProviderConfig{{Type: "exa", APIKey: "old-key"}}}
	settings, err := marshalWebSearchSettings(old)
	require.NoError(t, err)
	require.NoError(t, repo.SetMultiple(t.Context(), settings))
	var managerKey string
	svc.SetWebSearchManagerBuilder(t.Context(), func(cfg *WebSearchEmulationConfig, _ map[int64]string) {
		managerKey = cfg.Providers[0].APIKey
	})
	require.Equal(t, "old-key", managerKey)

	clearGlobalWebSearchConfig()
	readStarted := make(chan struct{})
	finishRead := make(chan struct{})
	repo.beforeReturn = func() {
		close(readStarted)
		<-finishRead
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := svc.GetWebSearchEmulationConfig(t.Context())
		readDone <- err
	}()
	<-readStarted
	edit := &WebSearchEmulationConfig{Enabled: true, Providers: []WebSearchProviderConfig{{Type: "exa", APIKey: "new-key"}}}
	saveDone := make(chan error, 1)
	go func() { saveDone <- svc.SaveWebSearchEmulationConfig(t.Context(), edit) }()
	close(finishRead)
	require.NoError(t, <-readDone)
	require.NoError(t, <-saveDone)
	repo.beforeReturn = nil

	cfg, err := svc.GetWebSearchEmulationConfig(t.Context())
	require.NoError(t, err)
	require.Equal(t, "new-key", cfg.Providers[0].APIKey)
	require.Equal(t, "new-key", managerKey)

	// A different replica writes to the same database. The next cache
	// refresh must replace both settings and the local manager.
	edit.Providers[0].APIKey = "peer-key"
	settings, err = marshalWebSearchSettings(edit)
	require.NoError(t, err)
	require.NoError(t, repo.SetMultiple(t.Context(), settings))
	webSearchEmulationCache.Store(&cachedWebSearchEmulationConfig{config: cfg})
	cfg, err = svc.GetWebSearchEmulationConfig(t.Context())
	require.NoError(t, err)
	require.Equal(t, "peer-key", cfg.Providers[0].APIKey)
	require.Equal(t, "peer-key", managerKey)
}

type webSearchProxySnapshotRepo struct {
	ProxyRepository
	proxy Proxy
	err   error
}

func (r *webSearchProxySnapshotRepo) ListByIDs(_ context.Context, ids []int64) ([]Proxy, error) {
	if len(ids) == 1 && r.proxy.ID == ids[0] {
		return []Proxy{r.proxy}, r.err
	}
	return nil, r.err
}

func TestWebSearchConfigRefreshesProviderProxyChanges(t *testing.T) {
	previous := webSearchEmulationCache.Load()
	t.Cleanup(func() { webSearchEmulationCache.Store(previous) })
	repo := newMockSettingRepo()
	svc := NewSettingService(repo, &config.Config{})
	proxy := &webSearchProxySnapshotRepo{proxy: Proxy{ID: 7, Protocol: "http", Host: "first.example", Port: 8080, Status: StatusActive}}
	svc.proxyRepo = proxy
	proxyID := proxy.proxy.ID
	cfg := &WebSearchEmulationConfig{Enabled: true, Providers: []WebSearchProviderConfig{{Type: "exa", APIKey: "fixture", ProxyID: &proxyID}}}
	settings, err := marshalWebSearchSettings(cfg)
	require.NoError(t, err)
	require.NoError(t, repo.SetMultiple(t.Context(), settings))

	var published []string
	svc.SetWebSearchManagerBuilder(t.Context(), func(_ *WebSearchEmulationConfig, urls map[int64]string) {
		published = append(published, urls[proxyID])
	})
	refresh := func() {
		expired := *webSearchEmulationCache.Load()
		expired.expiresAt = 0
		webSearchEmulationCache.Store(&expired)
		_, err := svc.GetWebSearchEmulationConfig(t.Context())
		require.NoError(t, err)
	}
	refresh()
	require.Equal(t, []string{"http://first.example:8080"}, published)

	proxy.proxy.Host = "second.example"
	refresh()
	proxy.err = errors.New("proxy read unavailable")
	refresh()
	proxy.err = nil
	refresh()
	proxy.proxy.ExpiresAt = new(time.Now().Add(-time.Minute))
	refresh()
	require.Equal(t, []string{"http://first.example:8080", "http://second.example:8080", "", "http://second.example:8080", ""}, published)
}
