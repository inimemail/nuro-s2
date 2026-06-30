-- Record the concrete keyword matched by keyword-based content moderation blocks.

ALTER TABLE content_moderation_logs
  ADD COLUMN IF NOT EXISTS matched_keyword VARCHAR(255) NOT NULL DEFAULT '';
