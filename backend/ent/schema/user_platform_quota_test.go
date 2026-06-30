package schema

import "testing"

func TestUserPlatformQuotaPlatformValidatorAllowsQuotaPlatforms(t *testing.T) {
	fields := UserPlatformQuota{}.Fields()
	if len(fields) < 2 {
		t.Fatalf("expected platform field in schema")
	}

	validators := fields[1].Descriptor().Validators
	for _, platform := range []string{"anthropic", "openai", "gemini", "antigravity", "grok"} {
		for _, validator := range validators {
			fn, ok := validator.(func(string) error)
			if !ok {
				t.Fatalf("unexpected validator type %T", validator)
			}
			if err := fn(platform); err != nil {
				t.Fatalf("platform %q should be accepted: %v", platform, err)
			}
		}
	}
}
