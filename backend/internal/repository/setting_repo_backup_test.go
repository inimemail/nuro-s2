package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

func TestBackupMetadataTransactionCommitAndRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "checkpoint failure rolls back record"}[fail], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			repo := &settingRepository{client: ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))}
			mock.ExpectBegin()
			mock.ExpectQuery(`INSERT INTO "settings"`).WillReturnRows(sqlmock.NewRows([]string{"id"}))
			mock.ExpectQuery(`SELECT .* FROM "settings" WHERE "settings"."key" = \$1 LIMIT 2 FOR UPDATE`).WithArgs("backup_records").WillReturnRows(sqlmock.NewRows([]string{"id", "key", "value", "updated_at"}).AddRow(1, "backup_records", "[]", time.Now()))
			mock.ExpectQuery(`INSERT INTO "settings"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
			if fail {
				mock.ExpectQuery(`INSERT INTO "settings"`).WillReturnError(errors.New("checkpoint write failed"))
				mock.ExpectRollback()
			} else {
				mock.ExpectQuery(`INSERT INTO "settings"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
				mock.ExpectCommit()
			}
			err = repo.WithBackupRecordLock(context.Background(), func(ctx context.Context) error {
				if err := repo.Set(ctx, "backup_records", `[{"id":"completed"}]`); err != nil {
					return err
				}
				return repo.Set(ctx, "backup_monthly_archive_checkpoint", `"2026-09-01"`)
			})
			if fail {
				require.ErrorContains(t, err, "checkpoint write failed")
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
