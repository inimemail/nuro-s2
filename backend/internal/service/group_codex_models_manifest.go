package service

// normalizeCodexModelsManifestConfig keeps the feature disabled for non-OpenAI
// groups and removes invalid/duplicate account IDs without changing order.
func normalizeCodexModelsManifestConfig(platform string, cfg GroupCodexModelsManifestConfig) GroupCodexModelsManifestConfig {
	if platform != PlatformOpenAI {
		return GroupCodexModelsManifestConfig{}
	}
	seen := make(map[int64]struct{}, len(cfg.AccountIDs))
	out := GroupCodexModelsManifestConfig{Enabled: cfg.Enabled, FallbackToScheduler: cfg.FallbackToScheduler}
	for _, id := range cfg.AccountIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out.AccountIDs = append(out.AccountIDs, id)
	}
	return out
}
