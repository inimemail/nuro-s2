-- Allow explicit composite routes to target the OpenAI-compatible domestic providers.
DO $$
BEGIN
    IF to_regclass('composite_model_routes') IS NOT NULL THEN
        ALTER TABLE composite_model_routes DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;
        ALTER TABLE composite_model_routes
            ADD CONSTRAINT composite_model_routes_target_platform_check
            CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek'));
    END IF;
END $$;
