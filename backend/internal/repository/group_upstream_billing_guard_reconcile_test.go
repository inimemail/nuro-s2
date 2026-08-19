package repository

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestReconcileUpstreamBillingGuardAccountsPreservesOverridesAndRefreshesDisabledAccounts(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newGroupRepositoryWithSQL(nil, db)

	mock.ExpectQuery(`(?s)WITH affected.*UPDATE accounts.*NOT EXISTS.*g2\.platform = a\.platform.*g2\.platform IN \('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kimi', 'zhipu', 'deepseek'\).*SELECT id FROM disabled`).
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

func TestReconcileRemovedGroupBillingGuardsUsesRemainingSamePlatformPolicies(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(`(?s)UPDATE accounts a.*a\.id = ANY\(\$1\).*a\.platform IN \('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kimi', 'zhipu', 'deepseek'\).*g\.platform = a\.platform.*RETURNING a\.id`).
		WithArgs(pq.Array([]int64{41, 42})).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(41))
	mock.ExpectExec(`(?s)INSERT INTO scheduler_outbox`).
		WithArgs(service.SchedulerOutboxEventAccountGroupsChanged, int64(41), nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := reconcileRemovedGroupBillingGuards(context.Background(), db, []int64{41, 42}, 9)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDeleteAccountGroupsByGroupIDDoesNotDisableGuardsDuringBindingReplacement(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newGroupRepositoryWithSQL(nil, db)
	mock.ExpectExec(`DELETE FROM account_groups WHERE group_id = \$1`).
		WithArgs(int64(9)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`(?s)INSERT INTO scheduler_outbox`).
		WithArgs(service.SchedulerOutboxEventGroupChanged, nil, sqlmock.AnyArg(), nil).
		WillReturnResult(sqlmock.NewResult(1, 1))

	affected, err := repo.DeleteAccountGroupsByGroupID(context.Background(), 9)
	require.NoError(t, err)
	require.Equal(t, int64(2), affected)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGroupAccountAvailableSQLAppliesGuardToEveryProbePlatform(t *testing.T) {
	require.Contains(t, groupAccountAvailableSQL, "a.platform = g.platform")
	require.Contains(t, groupAccountAvailableSQL, "g.platform IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kimi', 'zhipu', 'deepseek')")
	require.False(t, strings.Contains(groupAccountAvailableSQL, "a.platform = 'openai'"))
}
