ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS adaptive_health_sample_freshness_minutes INTEGER NOT NULL DEFAULT 15;

ALTER TABLE groups
    DROP CONSTRAINT IF EXISTS groups_adaptive_health_sample_freshness_minutes_check;

ALTER TABLE groups
    ADD CONSTRAINT groups_adaptive_health_sample_freshness_minutes_check
    CHECK (adaptive_health_sample_freshness_minutes >= 1 AND adaptive_health_sample_freshness_minutes <= 120);

-- Keep every replica's API-key group snapshot aligned when only the health
-- evidence window changes.
DROP TRIGGER IF EXISTS trg_groups_account_scheduling_strategy_auth_cache_invalidation ON groups;
CREATE TRIGGER trg_groups_account_scheduling_strategy_auth_cache_invalidation
AFTER UPDATE OF account_scheduling_strategy,
                adaptive_ttft_switch_enabled,
                adaptive_ttft_switch_threshold_seconds,
                adaptive_health_sample_freshness_minutes ON groups
FOR EACH ROW
WHEN (OLD.account_scheduling_strategy IS DISTINCT FROM NEW.account_scheduling_strategy
   OR OLD.adaptive_ttft_switch_enabled IS DISTINCT FROM NEW.adaptive_ttft_switch_enabled
   OR OLD.adaptive_ttft_switch_threshold_seconds IS DISTINCT FROM NEW.adaptive_ttft_switch_threshold_seconds
   OR OLD.adaptive_health_sample_freshness_minutes IS DISTINCT FROM NEW.adaptive_health_sample_freshness_minutes)
EXECUTE FUNCTION enqueue_group_account_scheduling_strategy_auth_cache_invalidation();

COMMENT ON COLUMN groups.adaptive_health_sample_freshness_minutes IS
    '自适应健康调度错误率与首 Token 样本有效期（分钟），有效范围 1-120，默认 15';
