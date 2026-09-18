package repository

import (
	"context"
	"errors"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestDuplicateAccountTransaction(t *testing.T) {
	for _, failure := range []string{"", "binding", "outbox"} {
		t.Run("failure="+failure, func(t *testing.T) {
			db, mock := newSQLMock(t)
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			repo := newAccountRepositoryWithSQL(client, db, nil)
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT .* FROM "groups".*FOR UPDATE`).
				WillReturnRows(sqlmock.NewRows([]string{"id", "platform"}).AddRow(3, "openai"))
			mock.ExpectQuery(`INSERT INTO "accounts"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(100))
			mock.ExpectExec(`UPDATE "accounts"`).WillReturnResult(sqlmock.NewResult(0, 1))
			binding := mock.ExpectExec(`INSERT INTO "account_groups".*"priority".*"upstream_billing_guard_max_multiplier".*"upstream_billing_guard_min_multiplier"`)
			if failure == "binding" {
				binding.WillReturnError(errors.New("binding failed"))
			} else {
				binding.WillReturnResult(sqlmock.NewResult(0, 1))
				outbox := mock.ExpectExec(`INSERT INTO scheduler_outbox`)
				if failure == "outbox" {
					outbox.WillReturnError(errors.New("outbox failed"))
				} else {
					outbox.WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			if failure == "" {
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
			}
			max, min := 1.2, 0.3
			account := &service.Account{Name: "copy", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, UpstreamBillingGuardEnabled: true, UpstreamBillingGuardMaxMultiplier: max}
			err := repo.CreateWithAccountGroups(context.Background(), account, []service.AccountGroup{{GroupID: 3, Priority: 11, UpstreamBillingGuardOverrideMaxMultiplier: &max, UpstreamBillingGuardOverrideMinMultiplier: &min}})
			if failure == "" {
				require.NoError(t, err)
				require.Equal(t, []int64{3}, account.GroupIDs)
				require.Equal(t, int64(100), account.AccountGroups[0].AccountID)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDuplicateAccountTransactionRejectsDeletedGroupBeforeCreate(t *testing.T) {
	db, mock := newSQLMock(t)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM "groups".*FOR UPDATE`).WillReturnRows(sqlmock.NewRows([]string{"id", "platform"}))
	mock.ExpectRollback()
	err := repo.CreateWithAccountGroups(context.Background(), &service.Account{}, []service.AccountGroup{{GroupID: 3}})
	require.ErrorIs(t, err, service.ErrGroupNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}
