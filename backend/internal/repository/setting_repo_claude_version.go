package repository

import (
	"context"
	"database/sql"
	"errors"
	"github.com/Wei-Shaw/sub2api/ent/setting"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"time"
)

func (r *settingRepository) CompareAndSwapClaudeSyncedVersion(ctx context.Context, previous, next string) (bool, error) {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck
	client := tx.Client()
	key := service.SettingKeyClaudeCLIClientVersionSynced
	if err := client.Setting.Create().SetKey(key).SetValue("").SetUpdatedAt(time.Now()).OnConflictColumns(setting.FieldKey).DoNothing().Exec(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	current, err := client.Setting.Query().Where(setting.KeyEQ(key)).ForUpdate().Only(ctx)
	if err != nil {
		return false, err
	}
	if current.Value != previous {
		return false, nil
	}
	enabled, err := client.Setting.Query().Where(setting.KeyEQ(service.SettingKeyClaudeCLIVersionAutoSyncEnabled), setting.ValueEQ("true")).Exist(ctx)
	if err != nil || !enabled {
		return false, err
	}
	if _, err := client.Setting.UpdateOneID(current.ID).SetValue(next).SetUpdatedAt(time.Now()).Save(ctx); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
