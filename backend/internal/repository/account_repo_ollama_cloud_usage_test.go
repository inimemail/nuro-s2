package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestOllamaCloudUsageParseRFC3339SQLSupportsPostgres16UTC(t *testing.T) {
	query := ollamaCloudUsageParseRFC3339SQL("fetched_at")
	require.Contains(t, query, "jsonb_path_query_first_tz")
	require.Contains(t, query, "'Z$'")
	require.Contains(t, query, "'+00:00'")
	require.Contains(t, query, "\\.[0-9]{6}")
}

func TestListDueOllamaCloudUsageAccountsFiltersBeforeLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := newAccountRepositoryWithSQL(nil, db, nil)
	mock.ExpectQuery("(?s)WITH eligible AS.*group_activity AS.*WHERE due_class IS NOT NULL.*LIMIT \\$4").
		WithArgs(sqlmock.AnyArg(), float64(60), float64(3600), 20, float64(900)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "group_last_used_at"}))

	accounts, err := repo.ListDueOllamaCloudUsageAccounts(context.Background(), time.Now(), time.Minute, time.Hour, 20)
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOllamaCloudDueQueryNeverScansWholeAccountPool(t *testing.T) {
	query := strings.ToLower(ollamaCloudUsageEligibleSQL)
	require.Contains(t, query, "platform in ('openai', 'anthropic')")
	require.Contains(t, query, "type = 'apikey'")
	require.NotContains(t, query, "scan")
}
