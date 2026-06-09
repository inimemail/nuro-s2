package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type openAIPoolProbeHTTPUpstreamRecorder struct {
	path       string
	body       string
	statusCode int
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
	return &SettingService{}
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

func TestOpenAIPoolRecoveryProbe_RoutingErrorSkipsFailedRequestedModel(t *testing.T) {
	upstream := &openAIPoolProbeHTTPUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
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
		Reason:     "unknown provider customer-router for model user-typed-wrong-model",
		ProbeKind:  "openai",
		ProbeModel: "user-typed-wrong-model",
	})

	result := svc.probeOpenAIPoolAccountRecovery(context.Background(), account, "user-typed-wrong-model")

	require.True(t, result.success)
	require.Equal(t, "responses", result.endpoint)
	require.Contains(t, upstream.body, `"model":"gpt-5.5"`)
	require.NotContains(t, upstream.body, `"model":"user-typed-wrong-model"`)
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

	require.Equal(t, openAIPoolRecoveryProbeTimeout, svc.openAIPoolRecoveryProbeTimeout(account, "gpt-5.4"))

	svc.openaiPoolSoftCooldownContext.Store(account.ID, openAIPoolSoftCooldownContext{
		ProbeKind:  "images",
		ProbeModel: "gpt-image-2",
	})
	require.Equal(t, openAIPoolRecoveryProbeImageTimeout, svc.openAIPoolRecoveryProbeTimeout(account, "gpt-5.4"))
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
