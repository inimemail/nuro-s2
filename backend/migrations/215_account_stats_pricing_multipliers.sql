ALTER TABLE channel_account_stats_model_pricing
    ADD COLUMN IF NOT EXISTS fast_multiplier NUMERIC(12,6),
    ADD COLUMN IF NOT EXISTS flex_multiplier NUMERIC(12,6);

ALTER TABLE channel_account_stats_pricing_intervals
    ADD COLUMN IF NOT EXISTS input_multiplier NUMERIC(12,6),
    ADD COLUMN IF NOT EXISTS output_multiplier NUMERIC(12,6),
    ADD COLUMN IF NOT EXISTS cache_write_multiplier NUMERIC(12,6),
    ADD COLUMN IF NOT EXISTS cache_read_multiplier NUMERIC(12,6);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'cas_model_pricing_fast_multiplier_positive') THEN
        ALTER TABLE channel_account_stats_model_pricing
            ADD CONSTRAINT cas_model_pricing_fast_multiplier_positive
            CHECK (fast_multiplier IS NULL OR fast_multiplier > 0);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'cas_model_pricing_flex_multiplier_positive') THEN
        ALTER TABLE channel_account_stats_model_pricing
            ADD CONSTRAINT cas_model_pricing_flex_multiplier_positive
            CHECK (flex_multiplier IS NULL OR flex_multiplier > 0);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'cas_pricing_intervals_multipliers_positive') THEN
        ALTER TABLE channel_account_stats_pricing_intervals
            ADD CONSTRAINT cas_pricing_intervals_multipliers_positive CHECK (
                (input_multiplier IS NULL OR input_multiplier > 0) AND
                (output_multiplier IS NULL OR output_multiplier > 0) AND
                (cache_write_multiplier IS NULL OR cache_write_multiplier > 0) AND
                (cache_read_multiplier IS NULL OR cache_read_multiplier > 0));
    END IF;
END $$;
