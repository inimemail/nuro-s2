package repository

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestUsageRetentionPartitionRequiresActualExpiredBounds(t *testing.T) {
	cutoff := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)
	require.True(t, usageLogsPartitionFullyExpired("usage_logs_202601", `FOR VALUES FROM ('2026-01-01 00:00:00+00') TO ('2026-02-01 00:00:00+00')`, cutoff))
	require.False(t, usageLogsPartitionFullyExpired("usage_logs_202601", `FOR VALUES FROM ('2026-01-01 00:00:00+00') TO ('2026-03-01 00:00:00+00')`, cutoff))
	require.False(t, usageLogsPartitionFullyExpired("usage_logs_202602", `FOR VALUES FROM ('2026-02-01 00:00:00+00') TO ('2026-03-01 00:00:00+00')`, cutoff))
	require.False(t, usageLogsPartitionFullyExpired("usage_logs_default", "DEFAULT", cutoff))
}
func TestUsageRetentionBoundaryDeletesUseTableIdentity(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &dashboardAggregationRepository{sql: db}
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL lock_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("(?s)SELECT.*pg_partitioned_table").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(`(?s)WITH victims AS.*SELECT tableoid, ctid.*LIMIT \$2.*WHERE \(tableoid, ctid\) IN`).WithArgs(sqlmock.AnyArg(), usageLogsCleanupBatchSize).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, repo.CleanupUsageLogs(context.Background(), time.Now()))
	require.NoError(t, mock.ExpectationsWereMet())
}
