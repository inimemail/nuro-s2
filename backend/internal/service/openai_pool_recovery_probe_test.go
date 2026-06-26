package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type openAIPoolProbeHTTPUpstreamRecorder struct {
	path       string
	body       string
	statusCode int
	err        error
}

func (r *openAIPoolProbeHTTPUpstreamRecorder) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	return r.DoWithTLS(req, proxyURL, accountID, accountConcurrency, nil)
}

func (r *openAIPoolProbeHTTPUpstreamRecorder) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	if req.URL != nil {
		r.path = req.URL.Path
	}
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		r.body = string(body)
	}
	if r.err != nil {
		return nil, r.err
	}
	statusCode := r.statusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	return &http.Response{
		StatusCode: statusCode,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}, nil
}

func openAIPoolRecoveryProbeTestSettingService(t *testing.T, regularEnabled, imageEnabled bool) *SettingService {
	return openAIPoolRecoveryProbeTestSettingServiceWithValues(t, regularEnabled, imageEnabled, nil)
}

func openAIPoolRecoveryProbeTestSettingServiceWithValues(t *testing.T, regularEnabled, imageEnabled bool, values map[string]string) *SettingService {
	t.Helper()
	gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{
		fingerprintUnification:       true,
		rewriteMessageCacheControl:   false,
		openAIPoolRecoveryProbe:      regularEnabled,
		openAIImagePoolRecoveryProbe: imageEnabled,
		expiresAt:                    time.Now().Add(time.Minute).UnixNano(),
	})
	t.Cleanup(func() {
		gatewayForwardingCache = atomic.Value{}
	})
	return NewSettingService(openAIPoolProbeSettingRepoStub{values: values}, &config.Config{})
}

type openAIPoolProbeSettingRepoStub struct {
	values map[string]string
}

func (r openAIPoolProbeSettingRepoStub) Get(_ context.Context, key string) (*Setting, error) {
	if value, ok := r.values[key]; ok {
		return &Setting{Key: key, Value: value}, nil
	}
	return nil, ErrSettingNotFound
}

func (r openAIPoolProbeSettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", ErrSettingNotFound
}

func (r openAIPoolProbeSettingRepoStub) Set(_ context.Context, _ string, _ string) error {
	panic("unused")
}

func (r openAIPoolProbeSettingRepoStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string)
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			result[key] = value
		}
	}
	return result, nil
}

func (r openAIPoolProbeSettingRepoStub) SetMultiple(_ context.Context, _ map[string]string) error {
	panic("unused")
}

func (r openAIPoolProbeSettingRepoStub) GetAll(_ context.Context) (map[string]string, error) {
	result := make(map[string]string, len(r.values))
	for key, value := range r.values {
		result[key] = value
	}
	return result, nil
}

func (r openAIPoolProbeSettingRepoStub) Delete(_ context.Context, _ string) error {
	panic("unused")
}

func TestOpenAIPoolRecoveryProbe_ImageCapabilityUsesImagesEndpoint(t *testing.T) {
	upstream := &openAIPoolProbeHTTPUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:       201,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
			"api_key":   "sk-test",
			"base_url":  "https://upstream.example",
		},
	}
	svc.openaiPoolSoftCooldownContext.Store(account.ID, openAIPoolSoftCooldownContext{
		ProbeCapability: OpenAIImagesCapabilityNative,
	})

	result := svc.probeOpenAIPoolAccountRecovery(context.Background(), account, "gpt-5.4")

	require.True(t, result.success)
	require.Equal(t, "images", result.endpoint)
	require.Equal(t, "/v1/images/generations", upstream.path)
	require.Contains(t, upstream.body, `"model":"gpt-image-2"`)
	require.Contains(t, upstream.body, `"size":"1024x1024"`)
	require.NotContains(t, upstream.body, `"messages"`)
}

func TestOpenAIPoolRecoveryProbe_ImagePoolKindUsesImagesEndpoint(t *testing.T) {
	upstream := &openAIPoolProbeHTTPUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:       203,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":       true,
			"image_pool_mode": true,
			"api_key":         "sk-test",
			"base_url":        "https://upstream.example",
		},
	}
	svc.openaiPoolSoftCooldownContext.Store(account.ID, openAIPoolSoftCooldownContext{
		ProbeKind:  "images",
		ProbeModel: "image-alias",
	})

	result := svc.probeOpenAIPoolAccountRecovery(context.Background(), account, "gpt-5.4")

	require.True(t, result.success)
	require.Equal(t, "images", result.endpoint)
	require.Equal(t, "/v1/images/generations", upstream.path)
	require.Contains(t, upstream.body, `"model":"image-alias"`)
	require.Contains(t, upstream.body, `"prompt":"small test image"`)
	require.NotContains(t, upstream.body, `"messages"`)
}

func TestOpenAIPoolRecoveryProbe_DisabledRegularPoolClearsExpiredCooldown(t *testing.T) {
	upstream := &openAIPoolProbeHTTPUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		httpUpstream:   upstream,
		settingService: openAIPoolRecoveryProbeTestSettingService(t, false, true),
	}
	account := &Account{
		ID:       206,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
			"api_key":   "sk-test",
			"base_url":  "https://upstream.example",
		},
	}
	svc.openaiPoolSoftCooldownUntil.Store(account.ID, time.Now().Add(-time.Second))

	svc.maybeStartOpenAIPoolRecoveryProbe(context.Background(), account, "gpt-5.4")

	_, cooling := svc.openAIPoolAccountSoftCooldownUntil(account)
	require.False(t, cooling)
	require.Empty(t, upstream.path)
}

func TestOpenAIPoolRecoveryProbe_DisabledImagePoolClearsExpiredCooldown(t *testing.T) {
	upstream := &openAIPoolProbeHTTPUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		httpUpstream:   upstream,
		settingService: openAIPoolRecoveryProbeTestSettingService(t, true, false),
	}
	account := &Account{
		ID:       207,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":       true,
			"image_pool_mode": true,
			"api_key":         "sk-test",
			"base_url":        "https://upstream.example",
		},
	}
	svc.openaiPoolSoftCooldownUntil.Store(account.ID, time.Now().Add(-time.Second))

	svc.maybeStartOpenAIPoolRecoveryProbe(context.Background(), account, "gpt-5.4")

	_, cooling := svc.openAIPoolAccountSoftCooldownUntil(account)
	require.False(t, cooling)
	require.Empty(t, upstream.path)
}

func TestOpenAIPoolRecoveryProbe_RegularPoolAlwaysUsesDefaultTestModel(t *testing.T) {
	upstream := &openAIPoolProbeHTTPUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		cfg:            &config.Config{},
		httpUpstream:   upstream,
		settingService: openAIPoolRecoveryProbeTestSettingService(t, true, true),
	}
	account := &Account{
		ID:       205,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
			"api_key":   "sk-test",
			"base_url":  "https://upstream.example",
		},
	}
	svc.openaiPoolSoftCooldownContext.Store(account.ID, openAIPoolSoftCooldownContext{
		StatusCode: http.StatusBadGateway,
		Reason:     "temporary upstream failure",
		ProbeKind:  "openai",
		ProbeModel: "bad-probe-model",
	})

	result := svc.probeOpenAIPoolAccountRecovery(context.Background(), account, "user-typed-wrong-model")

	require.True(t, result.success)
	require.Equal(t, "responses", result.endpoint)
	require.Contains(t, upstream.body, `"model":"gpt-5.5"`)
	require.NotContains(t, upstream.body, `"model":"user-typed-wrong-model"`)
	require.NotContains(t, upstream.body, `"model":"bad-probe-model"`)
	require.NotContains(t, upstream.body, `"model":"gpt-4o"`)
}

func TestOpenAIPoolRecoveryProbe_RegularPoolUsesConfiguredProbeModel(t *testing.T) {
	upstream := &openAIPoolProbeHTTPUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIPoolRecoveryProbeModel: "gpt-5-probe",
		}),
	}
	account := &Account{
		ID:       208,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
			"api_key":   "sk-test",
			"base_url":  "https://upstream.example",
		},
	}
	svc.openaiPoolSoftCooldownContext.Store(account.ID, openAIPoolSoftCooldownContext{
		StatusCode: http.StatusBadGateway,
		ProbeKind:  "openai",
		ProbeModel: "bad-probe-model",
	})

	result := svc.probeOpenAIPoolAccountRecovery(context.Background(), account, "user-typed-wrong-model")

	require.True(t, result.success)
	require.Equal(t, "responses", result.endpoint)
	require.Contains(t, upstream.body, `"model":"gpt-5-probe"`)
	require.NotContains(t, upstream.body, `"model":"user-typed-wrong-model"`)
	require.NotContains(t, upstream.body, `"model":"bad-probe-model"`)
}

func TestOpenAIPoolRecoveryProbe_ImagePoolUsesConfiguredDefaultProbeModel(t *testing.T) {
	upstream := &openAIPoolProbeHTTPUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIImagePoolRecoveryProbeModel: "gpt-image-probe",
		}),
	}
	account := &Account{
		ID:       209,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
			"api_key":   "sk-test",
			"base_url":  "https://upstream.example",
		},
	}
	svc.openaiPoolSoftCooldownContext.Store(account.ID, openAIPoolSoftCooldownContext{
		ProbeCapability: OpenAIImagesCapabilityNative,
	})

	result := svc.probeOpenAIPoolAccountRecovery(context.Background(), account, "gpt-5.4")

	require.True(t, result.success)
	require.Equal(t, "images", result.endpoint)
	require.Contains(t, upstream.body, `"model":"gpt-image-probe"`)
	require.Contains(t, upstream.body, `"size":"1024x1024"`)
	require.NotContains(t, upstream.body, `"model":"gpt-image-2"`)
}

func TestOpenAIPoolRecoveryProbeTimeout_ImagesUseLongTimeout(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       204,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}

	require.Equal(t, openAIPoolRecoveryProbeTimeout, svc.openAIPoolRecoveryProbeTimeout(context.Background(), account, "gpt-5.4"))

	svc.openaiPoolSoftCooldownContext.Store(account.ID, openAIPoolSoftCooldownContext{
		ProbeKind:  "images",
		ProbeModel: "gpt-image-2",
	})
	require.Equal(t, openAIPoolRecoveryProbeImageTimeout, svc.openAIPoolRecoveryProbeTimeout(context.Background(), account, "gpt-5.4"))
}

func TestOpenAIPoolRecoveryProbeTimeout_UsesConfiguredPoolValues(t *testing.T) {
	svc := &OpenAIGatewayService{
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIPoolProbeTimeoutSeconds:      "7",
			SettingKeyOpenAIImagePoolProbeTimeoutSeconds: "123",
		}),
	}
	account := &Account{
		ID:       210,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}

	require.Equal(t, 7*time.Second, svc.openAIPoolRecoveryProbeTimeout(context.Background(), account, "gpt-5.4"))

	svc.openaiPoolSoftCooldownContext.Store(account.ID, openAIPoolSoftCooldownContext{
		ProbeKind: "images",
	})
	require.Equal(t, 123*time.Second, svc.openAIPoolRecoveryProbeTimeout(context.Background(), account, "gpt-5.4"))
}

func TestOpenAIPoolSoftCooldown_UsesConfiguredRegularPoolCap(t *testing.T) {
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{},
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIPoolSoftCooldownMaxSeconds: "3",
		}),
	}
	account := &Account{
		ID:          211,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"error":{"type":"rate_limit_exceeded","message":"rate limited","resets_in_seconds":600}}`)

	svc.MarkOpenAIPoolAccountSoftCooldownWithContext(context.Background(), account, http.StatusTooManyRequests, body, openAIPoolSoftCooldownContext{})

	state := svc.OpenAIPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.False(t, state.Due)
	require.LessOrEqual(t, time.Until(state.Until), 4*time.Second)
	require.Greater(t, time.Until(state.Until), time.Second)
}

func TestOpenAIPoolSoftCooldown_UsesConfiguredImagePoolCap(t *testing.T) {
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{},
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIPoolSoftCooldownMaxSeconds:      "3",
			SettingKeyOpenAIImagePoolSoftCooldownMaxSeconds: "4",
		}),
	}
	account := &Account{
		ID:       212,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":       true,
			"image_pool_mode": true,
		},
	}
	body := []byte(`{"error":{"type":"rate_limit_exceeded","message":"rate limited","resets_in_seconds":600}}`)

	svc.MarkOpenAIPoolAccountSoftCooldownWithContext(context.Background(), account, http.StatusTooManyRequests, body, openAIPoolSoftCooldownContext{})

	state := svc.OpenAIPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.False(t, state.Due)
	require.LessOrEqual(t, time.Until(state.Until), 5*time.Second)
	require.Greater(t, time.Until(state.Until), 2*time.Second)
}

func TestOpenAIPoolRecoveryProbeFailure_ReopensConfiguredRegularPoolCooldown(t *testing.T) {
	upstream := &openAIPoolProbeHTTPUpstreamRecorder{statusCode: http.StatusInternalServerError}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIPoolSoftCooldownMaxSeconds: "3",
		}),
	}
	account := &Account{
		ID:       215,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
			"api_key":   "sk-test",
			"base_url":  "https://upstream.example",
		},
	}
	cooldownUntil := time.Now().Add(-time.Second)
	svc.openaiPoolSoftCooldownUntil.Store(account.ID, cooldownUntil)
	svc.openaiPoolSoftCooldownContext.Store(account.ID, openAIPoolSoftCooldownContext{ProbeKind: "openai"})

	svc.runOpenAIPoolRecoveryProbe(context.Background(), account, "gpt-5.4", cooldownUntil)

	state := svc.OpenAIPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.False(t, state.Due)
	require.Equal(t, "probe_backoff", state.CooldownSource)
	require.LessOrEqual(t, time.Until(state.Until), 4*time.Second)
	require.Greater(t, time.Until(state.Until), 2*time.Second)
}

func TestOpenAIPoolRecoveryProbeFailure_ReopensConfiguredImagePoolCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIPoolSoftCooldownMaxSeconds:      "3",
			SettingKeyOpenAIImagePoolSoftCooldownMaxSeconds: "4",
		}),
	}
	account := &Account{
		ID:       216,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":       true,
			"image_pool_mode": true,
		},
	}

	backoff := svc.nextOpenAIPoolRecoveryProbeBackoff(context.Background(), account, true)

	require.Equal(t, 4*time.Second, backoff)
}

func TestOpenAIPoolSoftCooldown_AccountLevelDisableSkipsCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{},
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIPoolSoftCooldownMaxSeconds: "3",
		}),
	}
	account := &Account{
		ID:       217,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                  true,
			"pool_soft_cooldown_enabled": false,
		},
	}

	svc.MarkOpenAIPoolAccountSoftCooldownWithContext(context.Background(), account, http.StatusForbidden, nil, openAIPoolSoftCooldownContext{})

	state := svc.OpenAIPoolSoftCooldownState(account.ID)
	require.False(t, state.Cooling)
}

func TestOpenAIPoolSoftCooldownStateForAccount_ClampsExistingRegularPoolCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIPoolSoftCooldownMaxSeconds: "3",
		}),
	}
	account := &Account{
		ID:       218,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}
	svc.openaiPoolSoftCooldownUntil.Store(account.ID, time.Now().Add(time.Minute))

	first := svc.OpenAIPoolSoftCooldownStateForAccount(context.Background(), account)
	second := svc.OpenAIPoolSoftCooldownStateForAccount(context.Background(), account)

	require.True(t, first.Cooling)
	require.LessOrEqual(t, time.Until(first.Until), 4*time.Second)
	require.Greater(t, time.Until(first.Until), 2*time.Second)
	require.Equal(t, first.Until, second.Until)
}

func TestOpenAIPoolSoftCooldownStateForAccount_ClampsExistingImagePoolCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIPoolSoftCooldownMaxSeconds:      "3",
			SettingKeyOpenAIImagePoolSoftCooldownMaxSeconds: "4",
		}),
	}
	account := &Account{
		ID:       219,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":       true,
			"image_pool_mode": true,
		},
	}
	svc.openaiPoolSoftCooldownUntil.Store(account.ID, time.Now().Add(time.Minute))

	first := svc.OpenAIPoolSoftCooldownStateForAccount(context.Background(), account)
	second := svc.OpenAIPoolSoftCooldownStateForAccount(context.Background(), account)

	require.True(t, first.Cooling)
	require.LessOrEqual(t, time.Until(first.Until), 5*time.Second)
	require.Greater(t, time.Until(first.Until), 3*time.Second)
	require.Equal(t, first.Until, second.Until)
}

func TestOpenAIPoolSoftCooldownClamp_DoesNotReviveClearedCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIPoolSoftCooldownMaxSeconds: "3",
		}),
	}
	account := &Account{
		ID:       220,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}
	oldUntil := time.Now().Add(time.Minute)
	svc.openaiPoolSoftCooldownUntil.Store(account.ID, oldUntil)
	svc.ClearAccountSchedulingBlock(account.ID)

	got := svc.clampOpenAIPoolSoftCooldownUntil(context.Background(), account, oldUntil)
	_, cooling := svc.openAIPoolAccountSoftCooldownUntil(account)

	require.True(t, got.IsZero())
	require.False(t, cooling)
}

func TestOpenAIPoolSoftCooldownClamp_DoesNotOverwriteShorterCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIPoolSoftCooldownMaxSeconds: "3",
		}),
	}
	account := &Account{
		ID:       221,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}
	oldUntil := time.Now().Add(time.Minute)
	shorterUntil := time.Now().Add(time.Second)
	svc.openaiPoolSoftCooldownUntil.Store(account.ID, oldUntil)
	svc.openaiPoolSoftCooldownUntil.Store(account.ID, shorterUntil)

	got := svc.clampOpenAIPoolSoftCooldownUntil(context.Background(), account, oldUntil)
	stored, ok := svc.openAIPoolAccountSoftCooldownUntilByID(account.ID)

	require.True(t, ok)
	require.Equal(t, shorterUntil, got)
	require.Equal(t, shorterUntil, stored)
}

func TestOpenAIPoolSoftCooldown_ActiveWindowDoesNotExtend(t *testing.T) {
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{},
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIPoolSoftCooldownMaxSeconds: "3",
		}),
	}
	account := &Account{
		ID:          213,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"error":{"type":"rate_limit_exceeded","message":"rate limited","resets_in_seconds":600}}`)

	svc.MarkOpenAIPoolAccountSoftCooldownWithContext(context.Background(), account, http.StatusTooManyRequests, body, openAIPoolSoftCooldownContext{})
	first := svc.OpenAIPoolSoftCooldownState(account.ID)
	require.True(t, first.Cooling)

	svc.MarkOpenAIPoolAccountSoftCooldownWithContext(context.Background(), account, http.StatusUnauthorized, nil, openAIPoolSoftCooldownContext{})

	second := svc.OpenAIPoolSoftCooldownState(account.ID)
	require.True(t, second.Cooling)
	require.Equal(t, first.Until, second.Until)
	require.LessOrEqual(t, time.Until(second.Until), 4*time.Second)
}

func TestOpenAIPoolSoftCooldown_ActiveLongWindowShortensToConfiguredCap(t *testing.T) {
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{},
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIPoolSoftCooldownMaxSeconds: "3",
		}),
	}
	account := &Account{
		ID:          222,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	svc.openaiPoolSoftCooldownUntil.Store(account.ID, time.Now().Add(time.Minute))

	svc.MarkOpenAIPoolAccountSoftCooldownWithContext(context.Background(), account, http.StatusForbidden, nil, openAIPoolSoftCooldownContext{})

	state := svc.OpenAIPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.LessOrEqual(t, time.Until(state.Until), 4*time.Second)
	require.Greater(t, time.Until(state.Until), 2*time.Second)
}

func TestOpenAIPoolSoftCooldown_ExpiredWindowCanStartNewWindow(t *testing.T) {
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{},
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyOpenAIPoolSoftCooldownMaxSeconds: "3",
		}),
	}
	account := &Account{
		ID:          214,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	svc.openaiPoolSoftCooldownUntil.Store(account.ID, time.Now().Add(-time.Second))
	body := []byte(`{"error":{"type":"rate_limit_exceeded","message":"rate limited","resets_in_seconds":600}}`)
	before := time.Now()

	svc.MarkOpenAIPoolAccountSoftCooldownWithContext(context.Background(), account, http.StatusTooManyRequests, body, openAIPoolSoftCooldownContext{})

	state := svc.OpenAIPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.True(t, state.Until.After(before.Add(2*time.Second)))
	require.LessOrEqual(t, time.Until(state.Until), 4*time.Second)
}

func TestAnthropicPoolSoftCooldown_ActiveWindowDoesNotExtend(t *testing.T) {
	svc := &GatewayService{}
	account := &Account{
		ID:          215,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}

	svc.MarkAnthropicPoolAccountSoftCooldown(context.Background(), account, http.StatusForbidden, nil, anthropicPoolSoftCooldownContext{})
	first := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.True(t, first.Cooling)

	svc.MarkAnthropicPoolAccountSoftCooldown(context.Background(), account, http.StatusForbidden, nil, anthropicPoolSoftCooldownContext{})

	second := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.True(t, second.Cooling)
	require.Equal(t, first.Until, second.Until)
	require.LessOrEqual(t, time.Until(second.Until), anthropicPoolSoftCooldownMaxDefault+time.Second)
}

func TestAnthropicPoolSoftCooldown_ActiveLongWindowShortensToConfiguredCap(t *testing.T) {
	svc := &GatewayService{
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyAnthropicPoolSoftCooldownMaxSeconds: "4",
		}),
	}
	account := &Account{
		ID:          311,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	svc.anthropicPoolSoftCooldownUntil.Store(account.ID, time.Now().Add(time.Minute))

	svc.MarkAnthropicPoolAccountSoftCooldown(context.Background(), account, http.StatusForbidden, nil, anthropicPoolSoftCooldownContext{})

	state := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.LessOrEqual(t, time.Until(state.Until), 5*time.Second)
	require.Greater(t, time.Until(state.Until), 3*time.Second)
}

func TestAnthropicPoolRecoveryProbeFailure_ReopensConfiguredCooldown(t *testing.T) {
	svc := &GatewayService{
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyAnthropicPoolSoftCooldownMaxSeconds: "4",
		}),
	}
	account := &Account{
		ID:          303,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}

	backoff := svc.nextAnthropicPoolRecoveryProbeBackoff(context.Background(), account, true)

	require.Equal(t, 4*time.Second, backoff)
}

func TestAnthropicPoolSoftCooldown_AccountLevelDisableSkipsCooldown(t *testing.T) {
	svc := &GatewayService{
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyAnthropicPoolSoftCooldownMaxSeconds: "3",
		}),
	}
	account := &Account{
		ID:       304,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                  true,
			"pool_soft_cooldown_enabled": false,
		},
	}

	svc.MarkAnthropicPoolAccountSoftCooldown(context.Background(), account, http.StatusForbidden, nil, anthropicPoolSoftCooldownContext{})

	state := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.False(t, state.Cooling)
}

func TestAnthropicPoolSoftCooldown_BedrockPoolUsesSoftCooldown(t *testing.T) {
	svc := &GatewayService{
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyAnthropicPoolSoftCooldownMaxSeconds: "4",
		}),
	}
	account := &Account{
		ID:       305,
		Platform: PlatformAnthropic,
		Type:     AccountTypeBedrock,
		Credentials: map[string]any{
			"pool_mode":                          true,
			"pool_soft_cooldown_error_threshold": 1,
			"auth_mode":                          "apikey",
			"api_key":                            "bedrock-test",
		},
	}

	svc.MarkAnthropicPoolAccountSoftCooldown(context.Background(), account, http.StatusForbidden, nil, anthropicPoolSoftCooldownContext{})

	state := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.LessOrEqual(t, time.Until(state.Until), 5*time.Second)
	require.Greater(t, time.Until(state.Until), 3*time.Second)
}

func TestAnthropicPoolRecoveryProbe_BedrockPoolUsesBedrockEndpoint(t *testing.T) {
	upstream := &openAIPoolProbeHTTPUpstreamRecorder{}
	svc := &GatewayService{
		httpUpstream: upstream,
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyAnthropicPoolRecoveryProbeModel: "anthropic.claude-3-5-sonnet-20240620-v1:0",
		}),
	}
	account := &Account{
		ID:       306,
		Platform: PlatformAnthropic,
		Type:     AccountTypeBedrock,
		Credentials: map[string]any{
			"pool_mode":                          true,
			"pool_soft_cooldown_error_threshold": 1,
			"auth_mode":                          "apikey",
			"api_key":                            "bedrock-test",
		},
	}
	cooldownUntil := time.Now().Add(-time.Second)
	svc.anthropicPoolSoftCooldownUntil.Store(account.ID, cooldownUntil)
	svc.anthropicPoolSoftCooldownContext.Store(account.ID, anthropicPoolSoftCooldownContext{ProbeKind: "messages"})

	svc.runAnthropicPoolRecoveryProbe(context.Background(), account, "anthropic.claude-3-5-sonnet-20240620-v1:0", cooldownUntil)

	require.Contains(t, upstream.path, "/model/")
	require.Contains(t, upstream.body, "bedrock-2023-05-31")
	state := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.False(t, state.Cooling)
}

func TestAnthropicPoolSoftCooldown_BedrockTransportErrorSoftCooldowns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &openAIPoolProbeHTTPUpstreamRecorder{err: errors.New("dial tcp upstream refused")}
	svc := &GatewayService{
		httpUpstream: upstream,
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyAnthropicPoolSoftCooldownMaxSeconds: "4",
		}),
	}
	account := &Account{
		ID:       307,
		Platform: PlatformAnthropic,
		Type:     AccountTypeBedrock,
		Credentials: map[string]any{
			"pool_mode":                          true,
			"pool_soft_cooldown_error_threshold": 1,
			"auth_mode":                          "apikey",
			"api_key":                            "bedrock-test",
		},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	resp, err := svc.executeBedrockUpstream(
		context.Background(),
		c,
		account,
		[]byte(`{"messages":[]}`),
		"us.anthropic.claude-sonnet-4-6",
		"us-east-1",
		false,
		nil,
		"bedrock-test",
		"",
	)

	require.Nil(t, resp)
	require.Error(t, err)
	state := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.Equal(t, http.StatusBadGateway, state.StatusCode)
	require.Equal(t, "request_error", state.CooldownSource)
	require.LessOrEqual(t, time.Until(state.Until), 5*time.Second)
	require.Greater(t, time.Until(state.Until), 3*time.Second)
}

func TestAnthropicPoolSoftCooldownStateForAccount_ClampsExistingCooldown(t *testing.T) {
	svc := &GatewayService{
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyAnthropicPoolSoftCooldownMaxSeconds: "4",
		}),
	}
	account := &Account{
		ID:       308,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}
	svc.anthropicPoolSoftCooldownUntil.Store(account.ID, time.Now().Add(time.Minute))

	first := svc.AnthropicPoolSoftCooldownStateForAccount(context.Background(), account)
	second := svc.AnthropicPoolSoftCooldownStateForAccount(context.Background(), account)

	require.True(t, first.Cooling)
	require.LessOrEqual(t, time.Until(first.Until), 5*time.Second)
	require.Greater(t, time.Until(first.Until), 3*time.Second)
	require.Equal(t, first.Until, second.Until)
}

func TestAnthropicPoolSoftCooldownClamp_DoesNotReviveClearedCooldown(t *testing.T) {
	svc := &GatewayService{
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyAnthropicPoolSoftCooldownMaxSeconds: "4",
		}),
	}
	account := &Account{
		ID:       309,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}
	oldUntil := time.Now().Add(time.Minute)
	svc.anthropicPoolSoftCooldownUntil.Store(account.ID, oldUntil)
	svc.clearAnthropicPoolSoftCooldown(account.ID)

	got := svc.clampAnthropicPoolSoftCooldownUntil(context.Background(), account, oldUntil)
	_, cooling := svc.anthropicPoolAccountSoftCooldownUntil(account)

	require.True(t, got.IsZero())
	require.False(t, cooling)
}

func TestAnthropicPoolSoftCooldownClamp_DoesNotOverwriteShorterCooldown(t *testing.T) {
	svc := &GatewayService{
		settingService: openAIPoolRecoveryProbeTestSettingServiceWithValues(t, true, true, map[string]string{
			SettingKeyAnthropicPoolSoftCooldownMaxSeconds: "4",
		}),
	}
	account := &Account{
		ID:       310,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}
	oldUntil := time.Now().Add(time.Minute)
	shorterUntil := time.Now().Add(time.Second)
	svc.anthropicPoolSoftCooldownUntil.Store(account.ID, oldUntil)
	svc.anthropicPoolSoftCooldownUntil.Store(account.ID, shorterUntil)

	got := svc.clampAnthropicPoolSoftCooldownUntil(context.Background(), account, oldUntil)
	stored, ok := svc.anthropicPoolAccountSoftCooldownUntilByID(account.ID)

	require.True(t, ok)
	require.Equal(t, shorterUntil, got)
	require.Equal(t, shorterUntil, stored)
}

func TestAnthropicPoolSoftCooldown_ExpiredWindowCanStartNewWindow(t *testing.T) {
	svc := &GatewayService{}
	account := &Account{
		ID:          216,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	svc.anthropicPoolSoftCooldownUntil.Store(account.ID, time.Now().Add(-time.Second))
	before := time.Now()

	svc.MarkAnthropicPoolAccountSoftCooldown(context.Background(), account, http.StatusForbidden, nil, anthropicPoolSoftCooldownContext{})

	state := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.True(t, state.Until.After(before.Add(29*time.Second)))
	require.LessOrEqual(t, time.Until(state.Until), anthropicPoolSoftCooldownMaxDefault+time.Second)
}

func TestOpenAIPoolRecoveryProbe_StaleResultDoesNotRewriteManualClear(t *testing.T) {
	upstream := &openAIPoolProbeHTTPUpstreamRecorder{statusCode: http.StatusInternalServerError}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:       202,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
			"api_key":   "sk-test",
			"base_url":  "https://upstream.example",
		},
	}
	cooldownUntil := time.Now().Add(-time.Second)
	svc.openaiPoolSoftCooldownUntil.Store(account.ID, cooldownUntil)
	svc.ClearAccountSchedulingBlock(account.ID)

	svc.runOpenAIPoolRecoveryProbe(context.Background(), account, "gpt-5.4", cooldownUntil)

	_, cooling := svc.openAIPoolAccountSoftCooldownUntil(account)
	require.False(t, cooling)
}
