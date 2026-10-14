package service

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGeminiCacheAccountCapabilityContract(t *testing.T) {
	a := &Account{Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://provider.example", "gemini_explicit_cache_enabled": true, "gemini_explicit_cache_max_ttl_seconds": 3600.0}}
	require.NoError(t, normalizeGeminiCacheCredentials(a))
	require.True(t, IsExplicitCacheAccount(a))
	require.Equal(t, time.Hour, GeminiCacheAccountMaxTTL(a))
	for _, ttl := range []float64{59, 86401, 60.5, math.NaN(), math.Inf(1)} {
		a.Credentials["gemini_explicit_cache_max_ttl_seconds"] = ttl
		require.Error(t, normalizeGeminiCacheCredentials(a))
		require.False(t, IsExplicitCacheAccount(a))
	}
	a.Credentials["base_url"] = "https://generativelanguage.googleapis.com"
	require.NoError(t, normalizeGeminiCacheCredentials(a))
	require.NotContains(t, a.Credentials, "gemini_explicit_cache_enabled")
	require.True(t, IsExplicitCacheAccount(a))
	a.Type = AccountTypeOAuth
	require.False(t, IsExplicitCacheAccount(a))
}

func TestGeminiCacheQuoteSeparatesCostAndUserPrice(t *testing.T) {
	now := time.Now()
	p := GeminiCachePricingContext{Version: 2, InputPerToken: "0.000002", StoragePerTokenHour: "0", UserMultiplier: "3", AccountMultiplier: "2", AccountInputPerToken: "0.000001", AccountStoragePerTokenHour: "0.000001"}
	q, e := QuoteGeminiCache(p, 100000, now, now.Add(time.Hour), true)
	require.NoError(t, e)
	require.Equal(t, "0.60000000", q.Amount)
	require.Equal(t, "0.40000000", q.AccountAmount)
	p.UserMultiplier = "0"
	q, e = QuoteGeminiCache(p, 100000, now, now.Add(time.Hour), true)
	require.NoError(t, e)
	require.Equal(t, "0.00000000", q.Amount)
	require.Equal(t, "0.40000000", q.AccountAmount)
	p.AccountStoragePerTokenHour = "NaN"
	_, e = QuoteGeminiCache(p, 100000, now, now.Add(time.Hour), true)
	require.Error(t, e)
}

func TestGeminiCacheStorageProviderAndDeadline(t *testing.T) {
	b := NewBillingService(nil, nil)
	api := &Account{Platform: PlatformGemini, Type: AccountTypeAPIKey}
	vertex := &Account{Platform: PlatformGemini, Type: AccountTypeServiceAccount}
	rate, source, e := b.geminiCacheStoragePriceForAccount("gemini-3.6-flash", gemini36LaunchOfferEnd.Add(-time.Second), api)
	require.NoError(t, e)
	require.Equal(t, .5e-6, rate)
	require.Equal(t, "official_gemini", source)
	rate, _, e = b.geminiCacheStoragePriceForAccount("gemini-3.6-flash", gemini36LaunchOfferEnd, api)
	require.NoError(t, e)
	require.Equal(t, 1e-6, rate)
	rate, source, e = b.geminiCacheStoragePriceForAccount("gemini-3.6-flash", gemini36LaunchOfferEnd.Add(-time.Second), vertex)
	require.NoError(t, e)
	require.Equal(t, 1e-6, rate)
	require.Equal(t, "official_vertex", source)
	api.Credentials = map[string]any{"base_url": "https://third.example"}
	_, _, e = b.geminiCacheStoragePriceForAccount("gemini-3.6-flash", time.Now(), api)
	require.Error(t, e)
	_, _, e = b.geminiCacheStoragePriceForAccount("other-model", time.Now(), nil)
	require.Error(t, e)
}

func TestGeminiCacheLocalReferenceRejectsDuplicateFields(t *testing.T) {
	id := newGeminiCacheID()
	for _, body := range []string{`{"cachedContent":"cachedContents/` + id + `","cachedContent":"cachedContents/provider"}`, `{"cachedContent":"cachedContents/` + id + `","contents":[{"parts":[{"text":"a","text":"b"}]}]}`} {
		_, _, local, e := FindGeminiCacheReference([]byte(body), "generateContent")
		require.True(t, local)
		require.Error(t, e)
	}
	for _, body := range []string{`{"cachedContent":null}`, `{"cachedContent":123}`, `{"cachedContent":"cachedContents/provider"}`} {
		_, _, local, e := FindGeminiCacheReference([]byte(body), "generateContent")
		require.False(t, local)
		require.NoError(t, e)
	}
}

type cacheHTTPContractStub struct {
	HTTPUpstream
	run func(*http.Request) (*http.Response, error)
}

func (s cacheHTTPContractStub) DoWithTLS(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.run(r)
}
func TestGeminiCacheProviderPayloadAndUnknownResult(t *testing.T) {
	a := &Account{Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-secret"}}
	expiry := time.Now().UTC().Add(time.Hour)
	request, e := ParseGeminiCacheCreate([]byte(`{"model":"gemini-2.5-flash","contents":[{"parts":[{"text":"test"}]}],"ttl":"3600s"}`), time.Now(), 24*time.Hour)
	require.NoError(t, e)
	s := &GeminiMessagesCompatService{cfg: &config.Config{}}
	s.httpUpstream = cacheHTTPContractStub{run: func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "test-secret", r.Header.Get("x-goog-api-key"))
		require.Empty(t, r.Header.Get("Authorization"))
		var body map[string]json.RawMessage
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		if strings.HasSuffix(r.URL.Path, ":countTokens") {
			require.Contains(t, body, "generateContentRequest")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"totalTokens":1024}`))}, nil
		}
		require.NotContains(t, body, "ttl")
		require.Contains(t, body, "expireTime")
		require.Equal(t, `"nuro-op-test"`, string(body["displayName"]))
		data, _ := json.Marshal(map[string]any{"name": "cachedContents/test", "model": "models/gemini-2.5-flash", "expireTime": expiry, "usageMetadata": map[string]int{"totalTokenCount": 1024}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	}}
	tokens, e := s.CountCacheTokens(t.Context(), a, "gemini-2.5-flash", request.Body)
	require.NoError(t, e)
	require.Equal(t, int64(1024), tokens)
	result, e := s.CreateCacheContent(t.Context(), a, "gemini-2.5-flash", "test", request)
	require.NoError(t, e)
	require.Equal(t, int64(1024), result.Tokens)
	s.httpUpstream = cacheHTTPContractStub{run: func(*http.Request) (*http.Response, error) { return nil, errors.New("connection lost") }}
	_, e = s.CreateCacheContent(context.Background(), a, "gemini-2.5-flash", "test", request)
	var failure *GeminiCacheProviderError
	require.ErrorAs(t, e, &failure)
	require.True(t, failure.Uncertain)
	s.httpUpstream = cacheHTTPContractStub{run: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"name":"cachedContents/malformed"}`))}, nil
	}}
	_, e = s.CreateCacheContent(t.Context(), a, "gemini-2.5-flash", "test", request)
	require.ErrorAs(t, e, &failure)
	require.True(t, failure.Uncertain)
}

func TestGeminiCacheVertexEndpointUsesModelAndResourceRegion(t *testing.T) {
	account := &Account{Platform: PlatformGemini, Type: AccountTypeServiceAccount, Credentials: map[string]any{"project_id": "test-project", "location": "us-central1", "vertex_model_locations": map[string]any{"gemini-2.5-flash": "europe-west1"}}}
	endpoint, err := buildGeminiCacheVertexEndpoint(account, "/publishers/google/models/gemini-2.5-flash:countTokens", []byte(`{"model":"models/gemini-2.5-flash"}`))
	require.NoError(t, err)
	require.Equal(t, "https://europe-west1-aiplatform.googleapis.com/v1/projects/test-project/locations/europe-west1/publishers/google/models/gemini-2.5-flash:countTokens", endpoint)
	endpoint, err = buildGeminiCacheVertexEndpoint(account, "/cachedContents", []byte(`{"model":"projects/test-project/locations/europe-west1/publishers/google/models/gemini-2.5-flash"}`))
	require.NoError(t, err)
	require.Contains(t, endpoint, "europe-west1-aiplatform.googleapis.com/v1/projects/test-project/locations/europe-west1/cachedContents")
	endpoint, err = buildGeminiCacheVertexEndpoint(account, "/projects/test-project/locations/europe-west1/cachedContents/test?updateMask=expire_time", []byte(`{"expireTime":"2026-10-15T00:00:00Z"}`))
	require.NoError(t, err)
	require.Contains(t, endpoint, "europe-west1-aiplatform.googleapis.com/v1/projects/test-project/locations/europe-west1/cachedContents/test?updateMask=expire_time")
	for _, path := range []string{"/projects/other/locations/europe-west1/cachedContents/test", "/projects/test-project/locations/europe-west1/publishers/google/models/gemini"} {
		_, err = buildGeminiCacheVertexEndpoint(account, path, nil)
		require.Error(t, err)
	}
}

func TestGeminiCacheDeleteRequiresConfirmedNotFound(t *testing.T) {
	account := &Account{Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-secret"}}
	for _, tc := range []struct {
		body string
		gone bool
	}{
		{`<html>proxy route not found</html>`, false},
		{`{"error":{"code":404,"status":"PERMISSION_DENIED","message":"denied"}}`, false},
		{`{"error":{"code":404,"status":"NOT_FOUND","message":"gone","message":"unknown"}}`, false},
		{`{"error":{"code":404,"status":"NOT_FOUND","message":"gone"}}`, true},
	} {
		svc := &GeminiMessagesCompatService{cfg: &config.Config{}}
		svc.httpUpstream = cacheHTTPContractStub{run: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		}}
		err := svc.DeleteCacheContent(t.Context(), account, "cachedContents/test")
		if tc.gone {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
}

func TestGeminiCacheExistingResourceBindingSurvivesRegionChange(t *testing.T) {
	account := &Account{Platform: PlatformGemini, Type: AccountTypeServiceAccount, Credentials: map[string]any{"project_id": "test-project", "location": "us-central1", "vertex_model_locations": map[string]any{"gemini-2.5-flash": "us-east1"}}}
	resource := &GeminiCachedContent{UpstreamName: "projects/test-project/locations/europe-west1/cachedContents/original", UpstreamModel: "gemini-2.5-flash", TokenCount: 1000}
	result := &GeminiCacheProviderResult{Name: resource.UpstreamName, Model: "projects/test-project/locations/europe-west1/publishers/google/models/gemini-2.5-flash", Tokens: 1000}
	require.NoError(t, validateExistingCacheProviderBinding(account, resource, result))
	bound, err := GeminiCacheBoundAccount(account, resource.UpstreamName)
	require.NoError(t, err)
	require.Equal(t, "europe-west1", bound.VertexLocation(resource.UpstreamModel))
	require.Equal(t, "us-east1", account.VertexLocation(resource.UpstreamModel))
	endpoint, err := buildVertexGeminiAccountURL(bound, resource.UpstreamModel, "generateContent", false)
	require.NoError(t, err)
	require.Contains(t, endpoint, "https://europe-west1-aiplatform.googleapis.com/")
	_, err = GeminiCacheBoundAccount(account, "projects/another/locations/europe-west1/cachedContents/original")
	require.ErrorIs(t, err, ErrGeminiCacheConflict)
	result.Name = "projects/test-project/locations/europe-west1/cachedContents/another"
	require.ErrorIs(t, validateExistingCacheProviderBinding(account, resource, result), ErrGeminiCacheConflict)
	result.Name = resource.UpstreamName
	result.Tokens++
	require.ErrorIs(t, validateExistingCacheProviderBinding(account, resource, result), ErrGeminiCacheConflict)
}
