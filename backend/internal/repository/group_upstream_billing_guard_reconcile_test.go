package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestReconcileUpstreamBillingGuardAccountsPreservesOverridesAndRefreshesDisabledAccounts(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newGroupRepositoryWithSQL(nil, db)

	mock.ExpectQuery(`(?s)WITH affected.*UPDATE accounts.*NOT EXISTS.*SELECT id FROM disabled`).
		WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(41).AddRow(42))
	for _, accountID := range []int64{41, 42} {
		mock.ExpectExec(`(?s)INSERT INTO scheduler_outbox`).
			WithArgs(service.SchedulerOutboxEventAccountGroupsChanged, accountID, nil, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}

	err := repo.ReconcileUpstreamBillingGuardAccounts(context.Background(), 9)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
