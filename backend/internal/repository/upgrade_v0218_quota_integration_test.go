//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUpgradeVertexQuotaSettlementIsPlatformScopedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUsageBillingRepository(client, integrationDB)
	for _, platform := range []string{service.PlatformGemini, service.PlatformAnthropic} {
		t.Run(platform, func(t *testing.T) {
			user := mustCreateUser(t, client, &service.User{Email: uuid.NewString() + "@example.com", PasswordHash: "hash"})
			key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-upgrade-" + uuid.NewString(), Name: "quota test"})
			account := mustCreateAccount(t, client, &service.Account{Name: "vertex-quota-" + uuid.NewString(),
				Platform: platform, Type: service.AccountTypeServiceAccount, Extra: map[string]any{"quota_limit": 10.0}})
			cmd := &service.UsageBillingCommand{RequestID: uuid.NewString(), APIKeyID: key.ID, UserID: user.ID,
				AccountID: account.ID, AccountType: account.Type, AccountPlatform: platform, AccountQuotaCost: 2.5}
			first, err := repo.Apply(ctx, cmd)
			require.NoError(t, err)
			require.True(t, first.Applied)
			second, err := repo.Apply(ctx, cmd)
			require.NoError(t, err)
			require.False(t, second.Applied)
			var used float64
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COALESCE((extra->>'quota_used')::numeric, 0) FROM accounts WHERE id=$1`, account.ID).Scan(&used))
			if platform == service.PlatformGemini {
				require.Equal(t, 2.5, used)
			} else {
				require.Zero(t, used)
			}
		})
	}
}
