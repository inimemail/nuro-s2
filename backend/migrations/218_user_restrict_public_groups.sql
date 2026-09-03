ALTER TABLE users
    ADD COLUMN IF NOT EXISTS restrict_public_groups BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN users.restrict_public_groups IS
    'Restrict access to public groups to explicit user_allowed_groups rows';
