package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageBillingApplyContinuesWhenAPIKeyWasDeleted(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageBillingRepository{db: db}

	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO usage_billing_dedup").
		WithArgs("request-deleted-key", int64(7), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectQuery("SELECT request_fingerprint FROM usage_billing_dedup_archive").
		WithArgs("request-deleted-key", int64(7)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("UPDATE users").
		WithArgs(1.25, int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(8.75))
	mock.ExpectQuery("UPDATE api_keys").
		WithArgs(0.5, int64(7), service.StatusAPIKeyActive, service.StatusAPIKeyQuotaExhausted).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("UPDATE api_keys SET").
		WithArgs(0.5, int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	result, err := repo.Apply(context.Background(), &service.UsageBillingCommand{
		RequestID:           "request-deleted-key",
		APIKeyID:            7,
		UserID:              9,
		BalanceCost:         1.25,
		APIKeyQuotaCost:     0.5,
		APIKeyRateLimitCost: 0.5,
	})
	require.NoError(t, err)
	require.True(t, result.Applied)
	require.Equal(t, 8.75, *result.NewBalance)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingApplyStillFailsOnAPIKeyCounterDatabaseError(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageBillingRepository{db: db}
	dbErr := errors.New("database connection lost")

	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO usage_billing_dedup").
		WithArgs("request-counter-error", int64(7), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectQuery("SELECT request_fingerprint FROM usage_billing_dedup_archive").
		WithArgs("request-counter-error", int64(7)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("UPDATE api_keys").
		WithArgs(0.5, int64(7), service.StatusAPIKeyActive, service.StatusAPIKeyQuotaExhausted).
		WillReturnError(dbErr)
	mock.ExpectRollback()

	_, err := repo.Apply(context.Background(), &service.UsageBillingCommand{
		RequestID:       "request-counter-error",
		APIKeyID:        7,
		APIKeyQuotaCost: 0.5,
	})
	require.ErrorIs(t, err, dbErr)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingApplyStillFailsOnAPIKeyRateLimitDatabaseError(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageBillingRepository{db: db}
	dbErr := errors.New("database connection lost")

	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO usage_billing_dedup").
		WithArgs("request-rate-error", int64(7), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectQuery("SELECT request_fingerprint FROM usage_billing_dedup_archive").
		WithArgs("request-rate-error", int64(7)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("UPDATE api_keys SET").
		WithArgs(0.5, int64(7)).
		WillReturnError(dbErr)
	mock.ExpectRollback()

	_, err := repo.Apply(context.Background(), &service.UsageBillingCommand{
		RequestID:           "request-rate-error",
		APIKeyID:            7,
		APIKeyRateLimitCost: 0.5,
	})
	require.ErrorIs(t, err, dbErr)
	require.NoError(t, mock.ExpectationsWereMet())
}
