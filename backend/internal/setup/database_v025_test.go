package setup

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestDatabaseConnectionTargetFirstV025(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	mock.ExpectClose()
	calls := 0
	err = testDatabaseConnection(&DatabaseConfig{DBName: "tenant"}, func(_ *DatabaseConfig, name string) (*sql.DB, error) {
		calls++
		require.Equal(t, "tenant", name)
		return db, nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDatabaseConnectionNoBootstrapOnFailureV025(t *testing.T) {
	for _, failure := range []error{errors.New("network unavailable"), &pq.Error{Code: "28P01"}, &pq.Error{Code: "53300"}} {
		calls := 0
		err := testDatabaseConnection(&DatabaseConfig{DBName: "tenant"}, func(_ *DatabaseConfig, _ string) (*sql.DB, error) {
			calls++
			return nil, failure
		})
		require.ErrorIs(t, err, failure)
		require.Equal(t, 1, calls)
	}
}

func TestDatabaseConnectionCreatesOnlyMissingV025(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(fmt.Sprint(exists), func(t *testing.T) {
			bootstrap, bm, err := sqlmock.New()
			require.NoError(t, err)
			target, tm, err := sqlmock.New()
			require.NoError(t, err)
			name := `tenant"quoted`
			bm.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)")).WithArgs(name).
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(exists))
			if !exists {
				bm.ExpectExec(regexp.QuoteMeta(`CREATE DATABASE "tenant""quoted"`)).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			bm.ExpectClose()
			tm.ExpectClose()
			var calls []string
			err = testDatabaseConnection(&DatabaseConfig{DBName: name}, func(_ *DatabaseConfig, dbName string) (*sql.DB, error) {
				calls = append(calls, dbName)
				switch len(calls) {
				case 1:
					return nil, fmt.Errorf("wrapped: %w", &pq.Error{Code: "3D000"})
				case 2:
					return bootstrap, nil
				default:
					return target, nil
				}
			})
			require.NoError(t, err)
			require.Equal(t, []string{name, "postgres", name}, calls)
			require.NoError(t, bm.ExpectationsWereMet())
			require.NoError(t, tm.ExpectationsWereMet())
		})
	}
}

func TestBuildPostgresDSNEscapesValuesV025(t *testing.T) {
	dsn := buildPostgresDSN(&DatabaseConfig{Host: "localhost", Port: 5432, User: "u", Password: `a' b\c`, SSLMode: "verify-full"}, "tenant db")
	require.Contains(t, dsn, `password='a\' b\\c'`)
	require.Contains(t, dsn, `dbname='tenant db'`)
	require.Contains(t, dsn, `sslmode='verify-full'`)
}
