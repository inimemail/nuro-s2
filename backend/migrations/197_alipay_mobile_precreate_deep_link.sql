-- Preserve the legacy mobile Alipay WAP flow unless explicitly enabled.
INSERT INTO settings (key, value, updated_at)
VALUES ('ALIPAY_MOBILE_PRECREATE_DEEP_LINK', 'false', NOW())
ON CONFLICT (key) DO NOTHING;
