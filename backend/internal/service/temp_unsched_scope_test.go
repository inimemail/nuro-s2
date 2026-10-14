package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type tempModelScopeTestRepo struct {
	AccountRepository
	models       []string
	accountCalls int
	failure      error
}

func (r *tempModelScopeTestRepo) SetModelRateLimit(_ context.Context, _ int64, model string, _ time.Time, _ ...string) error {
	r.models = append(r.models, model)
	return r.failure
}
func (r *tempModelScopeTestRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	r.accountCalls++
	return nil
}

func TestTempUnschedModelScopeNeverWidensOnMissingModelOrWriteFailure(t *testing.T) {
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"temp_unschedulable_enabled": true,
		"temp_unschedulable_rules":   []any{map[string]any{"error_code": 429, "keywords": []any{"busy"}, "duration_minutes": 30, "scope": "model"}},
	}}
	for _, item := range []struct {
		model   string
		failure error
	}{{"", nil}, {"mapped-model", nil}, {"mapped-model", errors.New("database unavailable")}} {
		repo := &tempModelScopeTestRepo{failure: item.failure}
		s := &RateLimitService{accountRepo: repo}
		require.True(t, s.tryTempUnschedulable(context.Background(), account, 429, []byte("busy"), item.model))
		require.Zero(t, repo.accountCalls)
		if item.model == "" {
			require.Empty(t, repo.models)
		} else {
			require.Equal(t, []string{item.model}, repo.models)
		}
	}
	rule := account.Credentials["temp_unschedulable_rules"].([]any)[0].(map[string]any)
	delete(rule, "scope")
	require.Equal(t, "account", matchedTempUnschedulableScope(account, 429, []byte("busy")), "legacy rules keep account scope")
	rule["scope"] = "model"
	rule["error_code"] = 401
	account.Type = AccountTypeOAuth
	require.Empty(t, matchedTempUnschedulableScope(account, 401, []byte("busy")), "OAuth auth errors must use credential recovery")
}
