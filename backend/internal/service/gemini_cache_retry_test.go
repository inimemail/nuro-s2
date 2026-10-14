package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type cacheRecordedOperationRepo struct {
	GeminiCachedContentRepository
	op       *GeminiCacheOperation
	resource *GeminiCachedContent
}

type cacheRecoveryBoundaryRepo struct {
	GeminiCachedContentRepository
	resource  *GeminiCachedContent
	confirmed bool
	code      string
}

func (r *cacheRecoveryBoundaryRepo) GetForOwner(context.Context, GeminiCacheOwner, string) (*GeminiCachedContent, error) {
	return r.resource, nil
}
func (r *cacheRecoveryBoundaryRepo) Confirm(context.Context, *GeminiCacheOperation, string, int64, time.Time) error {
	r.confirmed = true
	return errors.New("stop after confirmation boundary")
}
func (r *cacheRecoveryBoundaryRepo) Retry(_ context.Context, _ *GeminiCacheOperation, code string, _ time.Time, _ bool) error {
	r.code = code
	return nil
}

type cacheRecoveryAccountRepo struct {
	AccountRepository
	account *Account
}

func (r cacheRecoveryAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

type cacheRecoveryBoundaryProvider struct {
	GeminiCacheProvider
	getErr    error
	getResult *GeminiCacheProviderResult
	find      func(*Account) (*GeminiCacheProviderResult, error)
	deletes   int
}

func (p *cacheRecoveryBoundaryProvider) GetCacheContent(context.Context, *Account, string) (*GeminiCacheProviderResult, error) {
	return p.getResult, p.getErr
}
func (p *cacheRecoveryBoundaryProvider) DeleteCacheContent(context.Context, *Account, string) error {
	p.deletes++
	return nil
}
func (p *cacheRecoveryBoundaryProvider) FindCacheOperation(_ context.Context, a *Account, _ string) (*GeminiCacheProviderResult, error) {
	return p.find(a)
}

func TestGeminiCacheRecoveryDoesNotAcceptUnconfirmed404(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		repo := &cacheRecoveryBoundaryRepo{resource: &GeminiCachedContent{UpstreamName: "cachedContents/original", TokenCount: 1000}}
		provider := &cacheRecoveryBoundaryProvider{getErr: &GeminiCacheProviderError{Status: 404, ConfirmedNotFound: confirmed}}
		account := &Account{Platform: PlatformGemini, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
		svc := &GeminiCachedContentService{repo: repo, provider: provider, accounts: cacheRecoveryAccountRepo{account: account}}
		svc.recoverOperation(t.Context(), &GeminiCacheOperation{Kind: "delete", State: "send_started", TargetExpiry: time.Now()})
		require.Equal(t, confirmed, repo.confirmed)
		require.Zero(t, provider.deletes)
		if !confirmed {
			require.Equal(t, "mutation_result_unknown", repo.code)
		}
	}
}

func TestGeminiCacheRecoveryUsesPersistedCreateRegion(t *testing.T) {
	account := &Account{Platform: PlatformGemini, Type: AccountTypeServiceAccount, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"project_id": "test-project", "location": "us-central1", "vertex_model_locations": map[string]any{"gemini-2.5-flash": "us-east1"}}}
	repo := &cacheRecoveryBoundaryRepo{resource: &GeminiCachedContent{UpstreamModel: "gemini-2.5-flash", Pricing: GeminiCachePricingContext{ProviderProject: "test-project", ProviderLocation: "europe-west1"}}}
	provider := &cacheRecoveryBoundaryProvider{find: func(a *Account) (*GeminiCacheProviderResult, error) {
		require.Equal(t, "europe-west1", a.VertexLocation("gemini-2.5-flash"))
		return &GeminiCacheProviderResult{Name: "projects/test-project/locations/europe-west1/cachedContents/original", Model: "projects/test-project/locations/europe-west1/publishers/google/models/gemini-2.5-flash", Tokens: 1000, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	svc := &GeminiCachedContentService{repo: repo, provider: provider, accounts: cacheRecoveryAccountRepo{account: account}}
	svc.recoverOperation(t.Context(), &GeminiCacheOperation{Kind: "create", State: "send_started"})
	require.True(t, repo.confirmed)
	require.Equal(t, "us-east1", account.VertexLocation("gemini-2.5-flash"), "shared account remains unchanged")
}

func (r cacheRecordedOperationRepo) FindOperation(context.Context, GeminiCacheOwner, string, string) (*GeminiCacheOperation, error) {
	return r.op, nil
}
func (r cacheRecordedOperationRepo) GetForOwner(context.Context, GeminiCacheOwner, string) (*GeminiCachedContent, error) {
	return r.resource, nil
}

type cacheRetryPublisher struct{}

func (cacheRetryPublisher) PublishCacheChargeAdmission(context.Context, *GeminiCacheOperation) error {
	panic("recorded operations cannot publish another hold")
}

func TestGeminiCacheRecordedCreateSurvivesExpiryAndMaintenance(t *testing.T) {
	id := int64(3)
	key := &APIKey{ID: 2, UserID: 1, GroupID: &id, User: &User{ID: 1}, Group: &Group{ID: 3, Platform: PlatformGemini}}
	owner := GeminiCacheOwner{1, 2, 3}
	body := []byte(`{"model":"gemini-2.5-flash","expireTime":"2000-01-01T00:00:00Z","contents":[{"parts":[{"text":"original"}]}]}`)
	op := &GeminiCacheOperation{ID: newGeminiCacheID(), ResourceID: newGeminiCacheID(), Owner: owner, Kind: "create", State: "settled", Fingerprint: GeminiCacheFingerprint(owner, "create", "", body)}
	resource := &GeminiCachedContent{ID: op.ResourceID, Owner: owner}
	cfg := &config.Config{}
	cfg.Gateway.GeminiExplicitCacheMaintenance = true
	svc := &GeminiCachedContentService{repo: cacheRecordedOperationRepo{op: op, resource: resource}, publisher: cacheRetryPublisher{}, cfg: cfg, enabled: true, view: NewGeminiCacheResourceView(1)}
	svc.running.Store(true)
	svc.view.deadline.Store(time.Since(svc.view.started).Nanoseconds() + int64(time.Minute))
	got, recorded, err := svc.Create(t.Context(), key, nil, body, "original-id")
	require.NoError(t, err)
	require.Same(t, resource, got)
	require.Same(t, op, recorded)
	svc.enabled = false
	svc.serveExisting.Store(true)
	_, _, err = svc.Create(t.Context(), key, nil, body, "original-id")
	require.NoError(t, err, "disabled creation still reports a recorded operation")
	_, _, err = svc.Create(t.Context(), key, nil, []byte(`{"different":true}`), "original-id")
	require.ErrorIs(t, err, ErrGeminiCacheConflict)
}
