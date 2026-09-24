package repository

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestOpenCodeUsageCASFencesConfigurationAndProxy(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := newAccountRepositoryWithSQL(nil, db, nil)
	a := &service.Account{ID: 1, Platform: service.PlatformOpenCodeGo, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test"}, Extra: map[string]any{}, UpdatedAt: time.Now()}
	mock.ExpectExec(`(?s)UPDATE accounts SET extra=.*credentials=\$5::jsonb.*updated_at=\$7.*auto_refresh.*EXISTS.*proxies.*updated_at=\$10`).WithArgs(sqlmock.AnyArg(), int64(1), a.Platform, a.Type, `{"api_key":"test"}`, nil, a.UpdatedAt, "configured", "false", nil).WillReturnResult(sqlmock.NewResult(0, 0))
	saved, err := repo.SaveOpenCodeGoUsageSnapshot(context.Background(), a, "internal-scope", &service.OpenCodeGoUsageSnapshot{LastAttemptAt: time.Now().Unix()})
	require.NoError(t, err)
	require.False(t, saved)
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestOpenCodeUsageDueFiltersBeforeLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := newAccountRepositoryWithSQL(nil, db, nil)
	mock.ExpectQuery(`(?s)WITH eligible AS.*last_used_at > to_timestamp\(attempted\).*LEAST.*LIMIT \$4`).WithArgs(sqlmock.AnyArg(), float64(60), float64(900), 20).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	accounts, err := repo.ListDueOpenCodeGoUsageAccounts(context.Background(), time.Now(), time.Minute, 15*time.Minute, 100)
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.NoError(t, mock.ExpectationsWereMet())
}
