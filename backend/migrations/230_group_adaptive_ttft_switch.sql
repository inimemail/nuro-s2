ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS adaptive_ttft_switch_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN IF NOT EXISTS adaptive_ttft_switch_threshold_seconds INTEGER NOT NULL DEFAULT 60;

UPDATE groups
SET adaptive_ttft_switch_threshold_seconds = 60
WHERE adaptive_ttft_switch_threshold_seconds < 1
   OR adaptive_ttft_switch_threshold_seconds > 3600;

-- Reuse the existing group scheduling invalidation function, but broaden the
-- trigger so changing only the TTFT policy cannot leave another replica's API
-- key auth snapshot on the previous value.
DROP TRIGGER IF EXISTS trg_groups_account_scheduling_strategy_auth_cache_invalidation ON groups;
CREATE TRIGGER trg_groups_account_scheduling_strategy_auth_cache_invalidation
AFTER UPDATE OF account_scheduling_strategy,
                adaptive_ttft_switch_enabled,
                adaptive_ttft_switch_threshold_seconds ON groups
FOR EACH ROW
WHEN (OLD.account_scheduling_strategy IS DISTINCT FROM NEW.account_scheduling_strategy
   OR OLD.adaptive_ttft_switch_enabled IS DISTINCT FROM NEW.adaptive_ttft_switch_enabled
   OR OLD.adaptive_ttft_switch_threshold_seconds IS DISTINCT FROM NEW.adaptive_ttft_switch_threshold_seconds)
EXECUTE FUNCTION enqueue_group_account_scheduling_strategy_auth_cache_invalidation();

COMMENT ON COLUMN groups.adaptive_ttft_switch_enabled IS
    '自适应健康调度是否允许因首 Token 过慢切换账号；不影响错误、冷却和严格优先级';

COMMENT ON COLUMN groups.adaptive_ttft_switch_threshold_seconds IS
    '自适应健康调度首 Token 慢速切换阈值（秒），有效范围 1-3600，默认 60';
