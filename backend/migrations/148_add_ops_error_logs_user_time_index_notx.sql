CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ops_error_logs_user_time
  ON ops_error_logs (user_id, created_at DESC)
  WHERE user_id IS NOT NULL;
