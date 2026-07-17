package repository

import (
	"context"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

type unexpectedSchedulerCache struct {
	service.SchedulerCache
}

func TestUpdateUpstreamBillingProbeEnabledRefreshesBindingGuards(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newAccountRepositoryWithSQL(nil, db, nil)
	mock.ExpectExec(`(?s)UPDATE accounts.*#- '\{upstream_billing_probe,next_probe_at\}'.*upstream_billing_guard_enabled = FALSE`).
		WithArgs(sqlmock.AnyArg(), int64(7), service.PlatformOpenAI, service.AccountTypeAPIKey, false).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)INSERT INTO scheduler_outbox`).
		WithArgs(service.SchedulerOutboxEventAccountChanged, sqlmock.AnyArg(), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

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

func TestUpdateUpstreamBillingGuardGroupLimitsReplacesOnlyBindingPolicies(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newAccountRepositoryWithSQL(nil, db, nil)
	mock.ExpectQuery(`(?s)WITH requested.*UPDATE account_groups.*CASE group_id WHEN \$2::bigint THEN \$3::double precision WHEN \$4::bigint THEN \$5::double precision ELSE NULL::double precision END.*SELECT group_id, FALSE AS is_missing FROM updated`).
		WithArgs(int64(7), int64(10), 1.5, int64(20), 3.0, pq.Array([]int64{10, 20})).
		WillReturnRows(sqlmock.NewRows([]string{"group_id", "is_missing"}).AddRow(10, false).AddRow(20, false))
	mock.ExpectExec(`(?s)INSERT INTO scheduler_outbox`).
		WithArgs(service.SchedulerOutboxEventAccountChanged, sqlmock.AnyArg(), nil, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := repo.UpdateUpstreamBillingGuardGroupLimits(context.Background(), 7, map[int64]float64{20: 3, 10: 1.5})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateUpstreamBillingGuardGroupLimitsRejectsInvalidValuesBeforeSQL(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newAccountRepositoryWithSQL(nil, db, nil)

	err := repo.UpdateUpstreamBillingGuardGroupLimits(context.Background(), 7, map[int64]float64{10: math.Inf(1)})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateUpstreamBillingGuardGroupLimitsDoesNotRefreshSchedulerWhenUnchanged(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newAccountRepositoryWithSQL(nil, db, &unexpectedSchedulerCache{})
	mock.ExpectQuery(`(?s)WITH requested.*UPDATE account_groups.*IS DISTINCT FROM.*SELECT group_id, FALSE AS is_missing FROM updated`).
		WithArgs(int64(7), int64(10), 1.5, pq.Array([]int64{10})).
		WillReturnRows(sqlmock.NewRows([]string{"group_id", "is_missing"}))

	err := repo.UpdateUpstreamBillingGuardGroupLimits(context.Background(), 7, map[int64]float64{10: 1.5})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateUpstreamBillingGuardGroupLimitsReturnsConflictWhenBindingChanged(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newAccountRepositoryWithSQL(nil, db, &unexpectedSchedulerCache{})
	mock.ExpectQuery(`(?s)WITH requested.*UPDATE account_groups.*SELECT group_id, TRUE AS is_missing FROM missing`).
		WithArgs(int64(7), int64(10), 1.5, pq.Array([]int64{10})).
		WillReturnRows(sqlmock.NewRows([]string{"group_id", "is_missing"}).AddRow(10, true))

	err := repo.UpdateUpstreamBillingGuardGroupLimits(context.Background(), 7, map[int64]float64{10: 1.5})
	require.Error(t, err)
	statusCode, status := infraerrors.ToHTTP(err)
	require.Equal(t, http.StatusConflict, statusCode)
	require.Equal(t, "UPSTREAM_BILLING_GUARD_GROUP_BINDING_CHANGED", status.Reason)
	require.NotContains(t, status.Message, "account 7")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateUpstreamBillingGuardGroupLimitsClearsAllPolicies(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newAccountRepositoryWithSQL(nil, db, nil)
	mock.ExpectQuery(`(?s)WITH requested.*SET upstream_billing_guard_max_multiplier = NULL::double precision.*IS DISTINCT FROM \(NULL::double precision\)`).
		WithArgs(int64(7), pq.Array([]int64{})).
		WillReturnRows(sqlmock.NewRows([]string{"group_id", "is_missing"}).AddRow(10, false).AddRow(20, false))
	mock.ExpectExec(`(?s)INSERT INTO scheduler_outbox`).
		WithArgs(service.SchedulerOutboxEventAccountChanged, sqlmock.AnyArg(), nil, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := repo.UpdateUpstreamBillingGuardGroupLimits(context.Background(), 7, map[int64]float64{})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateAccountExtraPreservesDatabaseProbeSnapshot(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectExec(`(?s)UPDATE accounts.*jsonb_build_object\(\$2::text, extra -> \(\$2::text\)\)`).
		WithArgs(`{"feature":true,"upstream_billing_probe_enabled":true}`, service.UpstreamBillingProbeExtraKey, int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	extra := map[string]any{
		"feature": true,
		service.UpstreamBillingProbeEnabledExtraKey: true,
		service.UpstreamBillingProbeExtraKey:        map[string]any{"status": "stale"},
	}

	err := updateAccountExtraPreservingUpstreamBillingProbeSnapshot(context.Background(), db, 7, extra)
	require.NoError(t, err)
	require.Contains(t, extra, service.UpstreamBillingProbeExtraKey, "normalization must not mutate the caller's map")
	require.NoError(t, mock.ExpectationsWereMet())
}
