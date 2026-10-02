-- TypeSafe System One is a native API-key-only platform.
-- Platform values are stored as text, but user platform quotas retain a
-- database CHECK constraint from earlier migrations. Expand only that
-- constraint; TypeSafe is intentionally excluded from channel-monitor and
-- composite-route provider lists.
DO $$
BEGIN
    IF to_regclass('user_platform_quotas') IS NOT NULL THEN
        ALTER TABLE user_platform_quotas DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;
        ALTER TABLE user_platform_quotas ADD CONSTRAINT user_platform_quotas_platform_check
            CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go', 'typesafe'));
    END IF;
END $$;
