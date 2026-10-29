package service

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type grokCatalogReviewRepository struct {
	AccountRepository
	current   *Account
	conflicts int
	writes    int
}

func (r *grokCatalogReviewRepository) GetByID(ctx context.Context, _ int64) (*Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.current, nil
}

func (r *grokCatalogReviewRepository) CompareAndSwapGrokCatalog(_ context.Context, _ int64, _ any, _ time.Time, _ GrokCatalogObservation) (bool, error) {
	r.writes++
	return r.writes > r.conflicts, nil
}

func TestGrokCatalogPersistenceRejectsEditedConfigurationAndBoundsConflicts(t *testing.T) {
	now := time.Now().UTC()
	source := &Account{ID: 17, Platform: PlatformGrok, Type: AccountTypeAPIKey, UpdatedAt: now}
	t.Run("same endpoint credential rotation", func(t *testing.T) {
		current := *source
		current.UpdatedAt = now.Add(time.Second)
		require.NotEqual(t, GrokCatalogScope(source), GrokCatalogScope(&current))
		repo := &grokCatalogReviewRepository{current: &current}
		s := &AccountTestService{accountRepo: repo}
		require.Error(t, s.persistGrokCatalog(context.Background(), source, now, []string{"old-model"}, nil))
		require.Zero(t, repo.writes)
	})
	for _, conflicts := range []int{0, 1, 3} {
		t.Run("bounded CAS conflicts "+strconv.Itoa(conflicts), func(t *testing.T) {
			repo := &grokCatalogReviewRepository{current: source, conflicts: conflicts}
			s := &AccountTestService{accountRepo: repo}
			err := s.persistGrokCatalog(context.Background(), source, now, []string{"grok-model"}, nil)
			if conflicts == 3 {
				require.Error(t, err)
				require.Equal(t, 3, repo.writes)
			} else {
				require.NoError(t, err)
				require.Equal(t, conflicts+1, repo.writes)
			}
		})
	}
	t.Run("cancelled request", func(t *testing.T) {
		repo := &grokCatalogReviewRepository{current: source}
		s := &AccountTestService{accountRepo: repo}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.ErrorIs(t, s.persistGrokCatalog(ctx, source, now, nil, errors.New("fixture fetch failed")), context.Canceled)
		require.Zero(t, repo.writes)
	})
}

func TestGrokCatalogMergePreservesNewerAndSuccessfulEvidence(t *testing.T) {
	now := time.Now().UTC()
	old := GrokCatalogObservation{Scope: "same", AttemptedAt: now, SucceededAt: &now, Status: "success", Models: []string{"grok-b", "grok-a"}}
	late := GrokCatalogObservation{Scope: "same", AttemptedAt: now.Add(-time.Minute), Status: "success", Models: []string{"late"}}
	require.Equal(t, old, mergeGrokCatalogObservation(old, late))
	failed := GrokCatalogObservation{Scope: "same", AttemptedAt: now.Add(time.Minute), Status: "failed"}
	merged := mergeGrokCatalogObservation(old, failed)
	require.Equal(t, "failed", merged.Status)
	require.ElementsMatch(t, old.Models, merged.Models)
	require.Equal(t, old.SucceededAt, merged.SucceededAt)
	failed.Scope = "changed"
	merged = mergeGrokCatalogObservation(old, failed)
	require.Empty(t, merged.Models, "another source must not inherit a successful catalog")
	require.Nil(t, merged.SucceededAt)
}

func TestGrokCatalogDiagnosticsAreSourceAndTimeBounded(t *testing.T) {
	now := time.Now().UTC()
	account := &Account{ID: 17, Platform: PlatformGrok, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://example.test/v1", "model_mapping": map[string]any{
			"listed": "grok-listed", "absent": "grok-absent", "alias": "custom", "image": "grok-image",
		}},
		Extra: map[string]any{},
	}
	snapshot := GrokCatalogObservation{Scope: GrokCatalogScope(account), AttemptedAt: now.Add(-time.Minute),
		SucceededAt: &now, Status: "success", Models: []string{"grok-listed"}}
	account.Extra[GrokModelCatalogExtraKey] = snapshot
	result := BuildGrokCatalogDiagnostics(account, now)
	require.Equal(t, "success", result.Status)
	require.Equal(t, []GrokModelDiagnostic{
		{Model: "absent", Status: "not_listed"}, {Model: "alias", Status: "unverifiable"},
		{Model: "image", Status: "out_of_scope"}, {Model: "listed", Status: "listed"},
	}, result.Models)
	require.Equal(t, "stale", BuildGrokCatalogDiagnostics(account, now.Add(25*time.Hour)).Status)
	account.Credentials["base_url"] = "https://changed.example.test/v1"
	result = BuildGrokCatalogDiagnostics(account, now)
	require.Equal(t, "source_changed", result.Status)
	require.Zero(t, result.CatalogCount)
	for _, model := range result.Models {
		require.NotEqual(t, "listed", model.Status)
		require.NotEqual(t, "not_listed", model.Status)
	}
	snapshot.Scope = GrokCatalogScope(account)
	snapshot.SucceededAt = nil
	snapshot.Status = "failed"
	account.Extra[GrokModelCatalogExtraKey] = snapshot
	require.Equal(t, "failed", BuildGrokCatalogDiagnostics(account, now).Status)
	require.Equal(t, "unknown", BuildGrokCatalogDiagnostics(nil, now).Status)
}
