package repository

import (
	"context"
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"time"
)

// Patch only the managed catalog key; never replay the rest of Extra. Account
// edits preserve this key atomically and CAS checks the credential mode too.
func (r *accountRepository) CompareAndSwapGrokCatalog(ctx context.Context, id int64, previous any, updatedAt time.Time, next service.GrokCatalogObservation) (bool, error) {
	old, err := json.Marshal(previous)
	if err != nil {
		return false, err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	result, err := sqlExecutorFromContext(ctx, r.sql).ExecContext(ctx, `UPDATE accounts SET extra=COALESCE(extra,'{}'::jsonb)||jsonb_build_object($1::text,$2::jsonb)
 WHERE id=$3 AND deleted_at IS NULL AND platform='grok' AND updated_at=$5 AND (extra->$1::text) IS NOT DISTINCT FROM NULLIF($4::jsonb,'null'::jsonb)`, service.GrokModelCatalogExtraKey, string(data), id, string(old), updatedAt)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
