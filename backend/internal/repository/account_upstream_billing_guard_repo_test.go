package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type unexpectedSchedulerCache struct {
	service.SchedulerCache
}

func TestUpdateUpstreamBillingProbeEnabledGuardsConcurrentDisable(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newAccountRepositoryWithSQL(nil, db, &unexpectedSchedulerCache{})
	mock.ExpectExec(`(?s)UPDATE accounts.*#- '\{upstream_billing_probe,next_probe_at\}'.*upstream_billing_guard_enabled = FALSE`).
		WithArgs(sqlmock.AnyArg(), int64(7), service.PlatformOpenAI, service.AccountTypeAPIKey, false).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.UpdateUpstreamBillingProbeEnabled(context.Background(), 7, false)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateExtraRejectsMalformedUpstreamBillingProbeEnabled(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newAccountRepositoryWithSQL(nil, db, &unexpectedSchedulerCache{})

	err := repo.UpdateExtra(context.Background(), 7, map[string]any{
		service.UpstreamBillingProbeEnabledExtraKey: "false",
	})
	require.ErrorIs(t, err, service.ErrInvalidUpstreamBillingProbeEnabled)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBulkUpdateGuardsConcurrentProbeDisable(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newAccountRepositoryWithSQL(nil, db, &unexpectedSchedulerCache{})
	mock.ExpectExec(`(?s)UPDATE accounts.*extra = .*WHERE id = ANY\(\$2\).*NOT \(platform = \$3 AND type = \$4 AND upstream_billing_guard_enabled = TRUE\)`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), service.PlatformOpenAI, service.AccountTypeAPIKey).
		WillReturnResult(sqlmock.NewResult(0, 0))

	_, err := repo.BulkUpdate(context.Background(), []int64{7, 8}, service.AccountBulkUpdate{
		Extra: map[string]any{service.UpstreamBillingProbeEnabledExtraKey: false},
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBulkUpdateRejectsMalformedUpstreamBillingProbeEnabled(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newAccountRepositoryWithSQL(nil, db, &unexpectedSchedulerCache{})

	_, err := repo.BulkUpdate(context.Background(), []int64{7}, service.AccountBulkUpdate{
		Extra: map[string]any{service.UpstreamBillingProbeEnabledExtraKey: "false"},
	})
	require.ErrorIs(t, err, service.ErrInvalidUpstreamBillingProbeEnabled)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateUpstreamBillingProbeSnapshotDoesNotSyncSchedulerWithoutGuardTransition(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newAccountRepositoryWithSQL(nil, db, &unexpectedSchedulerCache{})
	account := &service.Account{
		ID:          7,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{},
		Extra:       map[string]any{},
	}
	snapshot := &service.UpstreamBillingProbeSnapshot{LastAttemptAt: time.Now()}
	mock.ExpectExec(`(?s)UPDATE accounts.*upstream_billing_guard_blocked`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	changed, err := repo.UpdateUpstreamBillingProbeSnapshot(context.Background(), account, snapshot, nil)
	require.NoError(t, err)
	require.False(t, changed)
	require.NoError(t, mock.ExpectationsWereMet())
}
