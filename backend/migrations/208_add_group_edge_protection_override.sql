-- NULL preserves the global Edge protection policy. FALSE disables only the
-- Edge upstream protection module for requests using this group.
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS edge_protection_enabled BOOLEAN NULL;
