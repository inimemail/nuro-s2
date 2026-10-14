package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGeminiCacheViewOwnerExpiryRevisionAndCapacity(t *testing.T) {
	v := NewGeminiCacheResourceView(1)
	owner := GeminiCacheOwner{1, 2, 3}
	id := newGeminiCacheID()
	b := GeminiCacheBinding{ID: id, Owner: owner, AccountID: 4, RequestedModel: "gemini-3.6-flash", UpstreamName: "cachedContents/private", State: "active", ExpiresAt: time.Now().Add(time.Hour), Revision: 1}
	require.NoError(t, v.Apply(&GeminiCacheViewBatch{Revision: 1, Bindings: []GeminiCacheBinding{b}, Complete: true}, true))
	v.deadline.Store(time.Since(v.started).Nanoseconds() + int64(time.Second))
	_, err := v.Lookup(owner, id, b.RequestedModel)
	require.NoError(t, err)
	_, err = v.Lookup(GeminiCacheOwner{5, 2, 3}, id, b.RequestedModel)
	require.ErrorIs(t, err, ErrGeminiCacheNotFound)
	second := b
	second.ID = newGeminiCacheID()
	second.Revision = 2
	require.ErrorIs(t, v.Apply(&GeminiCacheViewBatch{Revision: 2, Bindings: []GeminiCacheBinding{b, second}, Complete: true}, true), ErrGeminiCacheUnavailable)
	require.Equal(t, 1, v.count, "failed bootstrap must not corrupt published count")
	b.State = "pending"
	b.Revision = 3
	require.NoError(t, v.Apply(&GeminiCacheViewBatch{Revision: 3, Bindings: []GeminiCacheBinding{b}, Complete: true}, false))
	v.deadline.Store(time.Since(v.started).Nanoseconds() + int64(time.Second))
	_, err = v.Lookup(owner, id, b.RequestedModel)
	require.ErrorIs(t, err, ErrGeminiCacheNotFound)
	require.Error(t, v.Apply(&GeminiCacheViewBatch{Revision: 1, Complete: true}, true), "late bootstrap cannot resurrect permission")
	b.State = "active"
	b.Revision = 4
	b.ExpiresAt = time.Now().Add(-time.Second)
	require.NoError(t, v.Apply(&GeminiCacheViewBatch{Revision: 4, Bindings: []GeminiCacheBinding{b}, Complete: true}, false))
	require.Zero(t, v.count)
	v.Invalidate()
	_, err = v.Lookup(owner, id, b.RequestedModel)
	require.ErrorIs(t, err, ErrGeminiCacheUnavailable)
}

func TestGeminiCacheParsingAndDecimalQuote(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	request, err := ParseGeminiCacheCreate([]byte(`{"model":"models/gemini-3.6-flash","contents":[{"role":"user","parts":[{"text":"test"}]}],"ttl":"60s"}`), now, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, now.Add(time.Minute), request.ExpiresAt)
	for _, body := range []string{
		`{"model":"gemini-3.6-flash","model":"gemini-2.5-flash","contents":[]}`,
		`{"model":"gemini-3.6-flash","contents":null}`,
		`{"model":"gemini-3.6-flash","contents":[{"parts":[{"fileData":{"fileUri":"secret"}}]}]}`,
		`{"model":"gemini-3.6-flash","contents":[{"parts":[{"text":"x"}]}],"ttl":"1e9999s"}`,
	} {
		_, err := ParseGeminiCacheCreate([]byte(body), now, 24*time.Hour)
		require.Error(t, err, body)
	}
	pricing := GeminiCachePricingContext{InputPerToken: "0.00000000001", StoragePerTokenHour: "0.0000005", UserMultiplier: "1", AccountMultiplier: "0"}
	quote, err := QuoteGeminiCache(pricing, 100000, now, now.Add(time.Hour), true)
	require.NoError(t, err)
	require.Equal(t, "0.05000100", quote.Amount, "unit price must not be rounded up before multiplying")
	require.Equal(t, "0.00000000", quote.AccountAmount, "manual zero cost remains valid")
	quote, err = QuoteGeminiCache(pricing, 100000, now, now, false)
	require.NoError(t, err)
	require.Equal(t, "0.00000000", quote.Amount)
}

func TestGeminiCacheReferenceKeepsPlainRequestsAndRewritesOnlyReference(t *testing.T) {
	plain := []byte(`{"contents":[{"parts":[{"text":"nuro_text"}]}],"cachedContent":"cachedContents/existing-provider-name"}`)
	_, _, local, err := FindGeminiCacheReference(plain, "generateContent")
	require.NoError(t, err)
	require.False(t, local)
	id := newGeminiCacheID()
	body := []byte(`{"generateContentRequest":{"cachedContent":"cachedContents/` + id + `","contents":[{"parts":[{"text":"next"}]}]},"unknown":123}`)
	found, path, local, err := FindGeminiCacheReference(body, "countTokens")
	require.NoError(t, err)
	require.True(t, local)
	require.Equal(t, id, found)
	result, err := RewriteGeminiCacheReference(body, path, "cachedContents/upstream")
	require.NoError(t, err)
	require.Equal(t, strings.Replace(string(body), "cachedContents/"+id, "cachedContents/upstream", 1), string(result))
	require.True(t, isGeminiCacheBoundRequest(WithGeminiCacheBoundRequest(context.Background())))
	require.False(t, isGeminiCacheBoundRequest(context.Background()))
	require.True(t, errors.Is(ErrGeminiCacheNotFound, ErrGeminiCacheNotFound))
}

func BenchmarkGeminiCacheReferencePlain(b *testing.B) {
	body := []byte(`{"contents":[{"parts":[{"text":"hello"}]}]}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _, _ = FindGeminiCacheReference(body, "generateContent")
	}
}

type cacheViewTimeBoundaryRepo struct {
	GeminiCachedContentRepository
	batch      *GeminiCacheViewBatch
	bootstraps []bool
}

func (r *cacheViewTimeBoundaryRepo) ReadView(_ context.Context, _ int64, _ int, bootstrap bool) (*GeminiCacheViewBatch, error) {
	r.bootstraps = append(r.bootstraps, bootstrap)
	return r.batch, nil
}
func (*cacheViewTimeBoundaryRepo) LeaseView(context.Context, string, int64, time.Duration) error {
	return nil
}

func TestGeminiCacheViewNaturalSubscriptionBoundary(t *testing.T) {
	v := NewGeminiCacheResourceView(10)
	owner := GeminiCacheOwner{1, 2, 3}
	b := GeminiCacheBinding{ID: newGeminiCacheID(), Owner: owner, RequestedModel: "gemini-2.5-flash", UpstreamName: "cachedContents/private", State: "active", ExpiresAt: time.Now().Add(time.Hour)}
	boundary := time.Now().Add(4 * time.Second)
	repo := &cacheViewTimeBoundaryRepo{batch: &GeminiCacheViewBatch{Complete: true, Revision: 1, Bindings: []GeminiCacheBinding{b}, NextAuthorizationChange: &boundary}}
	require.NoError(t, v.Refresh(t.Context(), repo, "test", true))
	require.Equal(t, boundary.Sub(v.started).Nanoseconds(), v.deadline.Load(), "view lease is capped at natural subscription expiry")
	repo.batch = &GeminiCacheViewBatch{Complete: true, Revision: 1}
	require.NoError(t, v.Refresh(t.Context(), repo, "test", false))
	require.Equal(t, boundary.Sub(v.started).Nanoseconds(), v.deadline.Load(), "empty incremental refresh cannot extend authorization")
	// Advance only the local stored boundary so this test needs no sleep.
	v.authorizationDeadline.Store(time.Since(v.started).Nanoseconds() - 1)
	v.deadline.Store(time.Since(v.started).Nanoseconds() - 1)
	_, err := v.Lookup(owner, b.ID, b.RequestedModel)
	require.ErrorIs(t, err, ErrGeminiCacheUnavailable)
	b.State = "unavailable"
	repo.batch = &GeminiCacheViewBatch{Complete: true, Revision: 1, Bindings: []GeminiCacheBinding{b}}
	require.NoError(t, v.Refresh(t.Context(), repo, "test", false))
	require.Equal(t, []bool{true, false, true}, repo.bootstraps, "natural expiry rebuilds even without a revision event")
	_, err = v.Lookup(owner, b.ID, b.RequestedModel)
	require.ErrorIs(t, err, ErrGeminiCachePermission)
	require.Zero(t, v.authorizationDeadline.Load())
}

func TestGeminiCacheViewIncrementalSubscriptionBoundary(t *testing.T) {
	v := NewGeminiCacheResourceView(10)
	repo := &cacheViewTimeBoundaryRepo{batch: &GeminiCacheViewBatch{Complete: true, Revision: 1}}
	require.NoError(t, v.Refresh(t.Context(), repo, "test", true))
	owner := GeminiCacheOwner{1, 2, 3}
	b := GeminiCacheBinding{ID: newGeminiCacheID(), Owner: owner, RequestedModel: "gemini-2.5-flash", UpstreamName: "cachedContents/private", State: "unavailable", ExpiresAt: time.Now().Add(time.Hour), Revision: 2}
	boundary := time.Now().Add(4 * time.Second)
	repo.batch = &GeminiCacheViewBatch{Complete: true, Revision: 2, Bindings: []GeminiCacheBinding{b}, NextAuthorizationChange: &boundary}
	require.NoError(t, v.Refresh(t.Context(), repo, "test", false))
	require.Equal(t, boundary.Sub(v.started).Nanoseconds(), v.deadline.Load(), "new resource must introduce its natural subscription boundary")
	_, err := v.Lookup(owner, b.ID, b.RequestedModel)
	require.ErrorIs(t, err, ErrGeminiCachePermission)
	v.authorizationDeadline.Store(time.Since(v.started).Nanoseconds() - 1)
	b.State = "active"
	repo.batch = &GeminiCacheViewBatch{Complete: true, Revision: 2, Bindings: []GeminiCacheBinding{b}}
	require.NoError(t, v.Refresh(t.Context(), repo, "test", false))
	require.Equal(t, []bool{true, false, true}, repo.bootstraps)
	_, err = v.Lookup(owner, b.ID, b.RequestedModel)
	require.NoError(t, err, "natural start restores permission without a revision event")
}
