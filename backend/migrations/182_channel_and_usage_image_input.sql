ALTER TABLE channel_model_pricing
    ADD COLUMN IF NOT EXISTS image_input_price DECIMAL(20, 10);

ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS image_input_tokens INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS image_input_cost DECIMAL(20, 10) NOT NULL DEFAULT 0;
