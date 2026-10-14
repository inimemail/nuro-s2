package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Management writes share the same lock order with group deletion:
// ascending live group rows, then the moderation setting row.
func (r *settingRepository) SetContentModerationConfig(ctx context.Context, value string, groupIDs []int64) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	client := tx.Client()
	if _, err := client.ExecContext(ctx, "SET LOCAL lock_timeout = '2s'"); err != nil {
		return err
	}
	ids := append([]int64(nil), groupIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var previous int64
	for _, id := range ids {
		if id == previous {
			continue
		}
		previous = id
		rows, err := client.QueryContext(ctx, "SELECT id FROM groups WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", id)
		if err != nil {
			return err
		}
		found := rows.Next()
		rowErr := rows.Err()
		_ = rows.Close()
		if rowErr != nil {
			return rowErr
		}
		if !found {
			return fmt.Errorf("content moderation group %d is no longer available", id)
		}
	}
	// UPSERT acquires the same row lock as the deletion-side SELECT FOR UPDATE.
	if _, err := client.ExecContext(ctx, `INSERT INTO settings (key,value,updated_at) VALUES ($1,$2,NOW()) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=NOW()`, service.SettingKeyContentModerationConfig, value); err != nil {
		return err
	}
	return tx.Commit()
}

func removeDeletedModerationGroup(ctx context.Context, exec sqlExecutor, groupID int64) error {
	rows, err := exec.QueryContext(ctx, "SELECT value FROM settings WHERE key=$1 FOR UPDATE", service.SettingKeyContentModerationConfig)
	if err != nil {
		return err
	}
	var value string
	if rows.Next() {
		if err := rows.Scan(&value); err != nil {
			_ = rows.Close()
			return err
		}
	}
	rowErr := rows.Err()
	_ = rows.Close()
	if rowErr != nil {
		return rowErr
	}
	if value == "" {
		return nil
	}
	var config map[string]json.RawMessage
	if json.Unmarshal([]byte(value), &config) != nil || config == nil {
		return nil
	}
	var ids []int64
	if json.Unmarshal(config["group_ids"], &ids) != nil {
		return nil
	}
	kept := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id != groupID {
			kept = append(kept, id)
		}
	}
	if len(kept) == len(ids) {
		return nil
	}
	config["group_ids"], err = json.Marshal(kept)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, "UPDATE settings SET value=$2,updated_at=NOW() WHERE key=$1", service.SettingKeyContentModerationConfig, string(raw))
	return err
}
