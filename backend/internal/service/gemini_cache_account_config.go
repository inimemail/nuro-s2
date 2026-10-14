package service

import (
	"errors"
	"math"
)

// Only third-party Gemini API-key text accounts may declare this capability.
// Official API/Vertex support is detected separately from the endpoint/auth mode.
func normalizeGeminiCacheCredentials(account *Account) error {
	if account == nil || account.Credentials == nil {
		return nil
	}
	const enabledKey = "gemini_explicit_cache_enabled"
	const ttlKey = "gemini_explicit_cache_max_ttl_seconds"
	if account.Platform != PlatformGemini || account.Type != AccountTypeAPIKey || account.IsPoolMode() || isOfficialGeminiAPIBaseURL(account.GetCredential("base_url")) {
		delete(account.Credentials, enabledKey)
		delete(account.Credentials, ttlKey)
		return nil
	}
	raw, exists := account.Credentials[enabledKey]
	if !exists {
		delete(account.Credentials, ttlKey)
		return nil
	}
	enabled, ok := raw.(bool)
	if !ok {
		return errors.New("explicit cache capability must be a boolean")
	}
	if !enabled {
		delete(account.Credentials, ttlKey)
		return nil
	}
	ttl, ok := resolveAccountExtraNumber(account.Credentials, ttlKey)
	if !ok || math.IsNaN(ttl) || math.IsInf(ttl, 0) || ttl != math.Trunc(ttl) || ttl < 60 || ttl > 86400 {
		return errors.New("explicit cache TTL must be an integer between 60 and 86400 seconds")
	}
	account.Credentials[ttlKey] = int64(ttl)
	return nil
}
