UPDATE groups
SET account_scheduling_strategy = 'strict_priority'
WHERE account_scheduling_strategy IS NULL
   OR account_scheduling_strategy NOT IN ('strict_priority', 'health_first', 'health_cost_balanced');

COMMENT ON COLUMN groups.account_scheduling_strategy IS
    '账号调度策略：strict_priority 保持原有优先级调度，health_first 健康领先，health_cost_balanced 健康成本均衡';
