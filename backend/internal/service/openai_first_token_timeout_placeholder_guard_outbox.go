package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"time"
)

const openAIFirstTokenTimeoutPlaceholderGuardOutboxDBLimit = 250 * time.Millisecond

type openAIFirstTokenTimeoutPlaceholderGuardOutbox struct {
	db *sql.DB
}

func (o *openAIFirstTokenTimeoutPlaceholderGuardOutbox) upsert(ctx context.Context, sample openAIFirstTokenTimeoutPlaceholderGuardSample) error {
	if o == nil || o.db == nil {
		return nil
	}
	hash := sha256.Sum256([]byte(sample.key))
	_, err := o.db.ExecContext(ctx, `
		INSERT INTO openai_first_token_guard_outbox (
			guard_key_hash, guard_key, real_token_ms, recorded_at_ns
		) VALUES ($1, $2, $3, $4)
		ON CONFLICT (guard_key_hash) DO UPDATE SET
			guard_key = EXCLUDED.guard_key,
			real_token_ms = EXCLUDED.real_token_ms,
			recorded_at_ns = EXCLUDED.recorded_at_ns,
			updated_at = NOW()
		WHERE openai_first_token_guard_outbox.recorded_at_ns < EXCLUDED.recorded_at_ns
	`, hash[:], sample.key, sample.realTokenMS, sample.recordedAt)
	return err
}

func (o *openAIFirstTokenTimeoutPlaceholderGuardOutbox) list(ctx context.Context, limit int) ([]openAIFirstTokenTimeoutPlaceholderGuardSample, error) {
	if o == nil || o.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = openAIFirstTokenTimeoutPlaceholderGuardRetryBatchSize
	}
	rows, err := o.db.QueryContext(ctx, `
		WITH expired AS (
			DELETE FROM openai_first_token_guard_outbox
			WHERE updated_at < NOW() - ($2::double precision * INTERVAL '1 millisecond')
			RETURNING guard_key_hash
		)
		SELECT guard_key, real_token_ms, recorded_at_ns
		FROM openai_first_token_guard_outbox
		ORDER BY updated_at ASC
		LIMIT $1
	`, limit, openAIFirstTokenTimeoutPlaceholderGuardStateTTL.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	samples := make([]openAIFirstTokenTimeoutPlaceholderGuardSample, 0, limit)
	for rows.Next() {
		var sample openAIFirstTokenTimeoutPlaceholderGuardSample
		if err := rows.Scan(&sample.key, &sample.realTokenMS, &sample.recordedAt); err != nil {
			return nil, err
		}
		samples = append(samples, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return samples, nil
}

func (o *openAIFirstTokenTimeoutPlaceholderGuardOutbox) delete(ctx context.Context, sample openAIFirstTokenTimeoutPlaceholderGuardSample) (bool, error) {
	if o == nil || o.db == nil {
		return true, nil
	}
	hash := sha256.Sum256([]byte(sample.key))
	result, err := o.db.ExecContext(ctx, `
		DELETE FROM openai_first_token_guard_outbox
		WHERE guard_key_hash = $1 AND recorded_at_ns <= $2
	`, hash[:], sample.recordedAt)
	if err != nil {
		return false, err
	}
	deleted, err := result.RowsAffected()
	return deleted > 0, err
}
