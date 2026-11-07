//go:build integration

package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestImageTaskPostgresBudgetAdmissionBeforeClaim(t *testing.T) {
	ctx := t.Context()
	client := testEntClient(t)
	suffix := uuid.NewString()
	user := mustCreateUser(t, client, &service.User{Email: suffix + "@example.com"})
	t.Cleanup(func() {
		ctx := context.Background()
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM openai_image_tasks WHERE user_id=$1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE user_id=$1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id=$1", user.ID)
		require.NoError(t, err)
	})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-" + suffix})
	repo := NewOpenAIImageTaskRepository(integrationDB)
	task := &service.OpenAIImageTask{ID: suffix, OwnerID: "api_key:" + suffix, APIKeyID: key.ID, UserID: user.ID, UserConcurrency: 1, Endpoint: "/v1/images/generations", Model: "gpt-image-2.6-flare", RequestBody: []byte(`{"prompt":"` + strings.Repeat("x", 4096) + `"}`), RequestHeaders: map[string][]string{"Content-Type": {"application/json"}}}
	stored, created, err := repo.SubmitWithinLimit(ctx, task, 0)
	require.NoError(t, err)
	require.True(t, created)
	budget := pkghttputil.NewBodyBudget(32)
	scope := budget.Scope()
	claimed, err := repo.ClaimNext(pkghttputil.WithBodyReservation(ctx, scope), "budget-worker", time.Minute)
	require.ErrorIs(t, err, pkghttputil.ErrBodyBudgetExceeded)
	require.Nil(t, claimed)
	scope.Release()
	require.Zero(t, budget.Used())
	var status string
	var attempts int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT status,attempts FROM openai_image_tasks WHERE id=$1", stored.DBID).Scan(&status, &attempts))
	require.Equal(t, service.OpenAIImageTaskStatusQueued, status)
	require.Zero(t, attempts)
	budget = pkghttputil.NewBodyBudget(128 << 10)
	scope = budget.Scope()
	claimed, err = repo.ClaimNext(pkghttputil.WithBodyReservation(ctx, scope), "budget-worker", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, task.ID, claimed.ID)
	require.Equal(t, task.RequestBody, claimed.RequestBody)
	require.GreaterOrEqual(t, budget.Used(), int64(2*len(task.RequestBody)))
	require.NoError(t, repo.MarkSuccess(ctx, claimed.DBID, 200, []byte(`{}`)))
	scope.Release()
	require.Zero(t, budget.Used())
}
