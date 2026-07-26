-- Exact expression used by the bounded registration alias probes. The
-- non-transactional migration runner executes this CONCURRENTLY statement.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_users_email_dot_stripped
    ON users ((REPLACE(LOWER(TRIM(email)), '.', '')) text_pattern_ops)
    WHERE deleted_at IS NULL;
