-- Codex alpha/search web search billing override per group.
-- NULL uses the built-in default (0.01 USD/call); zero makes searches free.
ALTER TABLE groups ADD COLUMN IF NOT EXISTS web_search_price_per_call DECIMAL(20,8);
