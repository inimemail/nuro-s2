package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/setting"
)

type backupSettingsTxKey struct{}

func (r *settingRepository) settingsClient(ctx context.Context) *ent.Client {
	if client, ok := ctx.Value(backupSettingsTxKey{}).(*ent.Client); ok {
		return client
	}
	return r.client
}

// Backup metadata uses one fixed PostgreSQL row-lock domain, including the
// archive checkpoint. No Redis error can switch a writer into another lock domain.
// All reads and writes in fn use the same transaction and bounded context.
func (r *settingRepository) WithBackupRecordLock(ctx context.Context, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	client := tx.Client()
	if err := client.Setting.Create().SetKey("backup_records").SetValue("[]").SetUpdatedAt(time.Now()).OnConflictColumns(setting.FieldKey).DoNothing().Exec(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := client.Setting.Query().Where(setting.KeyEQ("backup_records")).ForUpdate().Only(ctx); err != nil {
		return err
	}
	if err := fn(context.WithValue(ctx, backupSettingsTxKey{}, client)); err != nil {
		return err
	}
	return tx.Commit()
}
