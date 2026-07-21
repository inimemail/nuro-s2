package migrations

import (
	"strings"
	"testing"
)

func TestAuthCacheInvalidationOutboxMigrationCoversCachedFieldsAndCoalesces(t *testing.T) {
	raw, err := FS.ReadFile("194_auth_cache_invalidation_outbox.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	required := []string{
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_auth_cache_invalidation_outbox_cache_key_unique",
		"ON CONFLICT (cache_key) DO UPDATE",
		"OLD.quota_used IS DISTINCT FROM NEW.quota_used",
		"OLD.name IS DISTINCT FROM NEW.name",
		"OLD.rate_limit_7d IS DISTINCT FROM NEW.rate_limit_7d",
		"OLD.balance IS NOT DISTINCT FROM NEW.balance",
		"OLD.concurrency IS NOT DISTINCT FROM NEW.concurrency",
		"OLD.rpm_limit IS NOT DISTINCT FROM NEW.rpm_limit",
		"OLD.peak_rate_multiplier IS NOT DISTINCT FROM NEW.peak_rate_multiplier",
		"OLD.video_price_1080p IS NOT DISTINCT FROM NEW.video_price_1080p",
		"OLD.web_search_price_per_call IS NOT DISTINCT FROM NEW.web_search_price_per_call",
		"OLD.messages_dispatch_model_config IS NOT DISTINCT FROM NEW.messages_dispatch_model_config",
		"CREATE TRIGGER trg_users_auth_cache_invalidation\nBEFORE UPDATE OR DELETE ON users",
		"CREATE TRIGGER trg_groups_auth_cache_invalidation\nBEFORE UPDATE OR DELETE ON groups",
		"CREATE TRIGGER trg_user_group_rpm_auth_cache_invalidation",
		"OLD.rpm_override IS NOT DISTINCT FROM NEW.rpm_override",
	}
	for _, fragment := range required {
		if !strings.Contains(sql, fragment) {
			t.Errorf("migration is missing %q", fragment)
		}
	}
}
