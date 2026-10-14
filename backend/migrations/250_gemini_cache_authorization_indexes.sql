SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE INDEX idx_gemini_cache_account_live ON gemini_cached_contents(account_id) WHERE state<>'deleted';
CREATE INDEX idx_gemini_cache_group_live ON gemini_cached_contents(group_id) WHERE state<>'deleted';
CREATE TRIGGER gemini_cache_account_delete AFTER DELETE ON accounts FOR EACH ROW EXECUTE FUNCTION nuro_gemini_cache_authorization_event();
CREATE TRIGGER gemini_cache_key_delete AFTER DELETE ON api_keys FOR EACH ROW EXECUTE FUNCTION nuro_gemini_cache_authorization_event();
CREATE TRIGGER gemini_cache_user_delete AFTER DELETE ON users FOR EACH ROW EXECUTE FUNCTION nuro_gemini_cache_authorization_event();
CREATE TRIGGER gemini_cache_group_delete AFTER DELETE ON groups FOR EACH ROW EXECUTE FUNCTION nuro_gemini_cache_authorization_event();
