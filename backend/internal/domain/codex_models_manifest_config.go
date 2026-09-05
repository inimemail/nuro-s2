package domain

// GroupCodexModelsManifestConfig controls the optional fixed-account source
// used by an OpenAI group's Codex /models endpoint.
type GroupCodexModelsManifestConfig struct {
	Enabled             bool    `json:"enabled"`
	AccountIDs          []int64 `json:"account_ids,omitempty"`
	FallbackToScheduler bool    `json:"fallback_to_scheduler,omitempty"`
}
