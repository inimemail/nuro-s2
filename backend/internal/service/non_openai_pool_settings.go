package service

import (
	"encoding/json"
)

// NonOpenAIPoolPlatformSettings controls the pool runtime for one non-OpenAI
// platform. OpenAI deliberately keeps its existing independent settings and
// runtime implementation.
type NonOpenAIPoolPlatformSettings struct {
	RecoveryProbeEnabled   bool `json:"recovery_probe_enabled"`
	SoftCooldownMaxSeconds int  `json:"soft_cooldown_max_seconds"`
	ProbeTimeoutSeconds    int  `json:"probe_timeout_seconds"`
}

// NonOpenAIPoolSettings is persisted as one JSON setting so adding a platform
// does not require another API field or a database migration.
type NonOpenAIPoolSettings struct {
	Enabled                    bool                                     `json:"enabled"`
	DefaultCooldownSeconds     int                                      `json:"default_cooldown_seconds"`
	AuthCooldownSeconds        int                                      `json:"auth_cooldown_seconds"`
	ServerErrorCooldownSeconds int                                      `json:"server_error_cooldown_seconds"`
	TransportCooldownSeconds   int                                      `json:"transport_cooldown_seconds"`
	MaxCooldownSeconds         int                                      `json:"max_cooldown_seconds"`
	ProbeTimeoutSeconds        int                                      `json:"probe_timeout_seconds"`
	ProbeMaxBackoffSeconds     int                                      `json:"probe_max_backoff_seconds"`
	Platforms                  map[string]NonOpenAIPoolPlatformSettings `json:"platforms"`
}

func DefaultNonOpenAIPoolSettings() NonOpenAIPoolSettings {
	return NonOpenAIPoolSettings{
		Enabled: true, DefaultCooldownSeconds: 5, AuthCooldownSeconds: 30,
		ServerErrorCooldownSeconds: 5, TransportCooldownSeconds: 5,
		MaxCooldownSeconds: 30, ProbeTimeoutSeconds: 5, ProbeMaxBackoffSeconds: 60,
		Platforms: map[string]NonOpenAIPoolPlatformSettings{
			PlatformGemini:      {RecoveryProbeEnabled: true, SoftCooldownMaxSeconds: 30, ProbeTimeoutSeconds: 5},
			PlatformAntigravity: {RecoveryProbeEnabled: true, SoftCooldownMaxSeconds: 30, ProbeTimeoutSeconds: 5},
			PlatformGrok:        {RecoveryProbeEnabled: true, SoftCooldownMaxSeconds: 30, ProbeTimeoutSeconds: 5},
			PlatformKimi:        {RecoveryProbeEnabled: true, SoftCooldownMaxSeconds: 30, ProbeTimeoutSeconds: 5},
			PlatformZhipu:       {RecoveryProbeEnabled: true, SoftCooldownMaxSeconds: 30, ProbeTimeoutSeconds: 5},
			PlatformDeepSeek:    {RecoveryProbeEnabled: true, SoftCooldownMaxSeconds: 30, ProbeTimeoutSeconds: 5},
		},
	}
}

func mustMarshalDefaultNonOpenAIPoolSettings() string {
	value, err := json.Marshal(DefaultNonOpenAIPoolSettings())
	if err != nil {
		panic(err)
	}
	return string(value)
}

func normalizeNonOpenAIPoolSettings(value NonOpenAIPoolSettings) NonOpenAIPoolSettings {
	defaults := DefaultNonOpenAIPoolSettings()
	clamp := func(v, fallback, max int) int {
		if v <= 0 {
			v = fallback
		}
		if v > max {
			v = max
		}
		return v
	}
	value.DefaultCooldownSeconds = clamp(value.DefaultCooldownSeconds, defaults.DefaultCooldownSeconds, 600)
	value.AuthCooldownSeconds = clamp(value.AuthCooldownSeconds, defaults.AuthCooldownSeconds, 3600)
	value.ServerErrorCooldownSeconds = clamp(value.ServerErrorCooldownSeconds, defaults.ServerErrorCooldownSeconds, 600)
	value.TransportCooldownSeconds = clamp(value.TransportCooldownSeconds, defaults.TransportCooldownSeconds, 600)
	value.MaxCooldownSeconds = clamp(value.MaxCooldownSeconds, defaults.MaxCooldownSeconds, 3600)
	value.ProbeTimeoutSeconds = clamp(value.ProbeTimeoutSeconds, defaults.ProbeTimeoutSeconds, 60)
	value.ProbeMaxBackoffSeconds = clamp(value.ProbeMaxBackoffSeconds, defaults.ProbeMaxBackoffSeconds, 3600)
	if value.Platforms == nil {
		value.Platforms = map[string]NonOpenAIPoolPlatformSettings{}
	}
	for platform, fallback := range defaults.Platforms {
		item, exists := value.Platforms[platform]
		if !exists {
			item = fallback
		}
		item.SoftCooldownMaxSeconds = clamp(item.SoftCooldownMaxSeconds, fallback.SoftCooldownMaxSeconds, 3600)
		item.ProbeTimeoutSeconds = clamp(item.ProbeTimeoutSeconds, value.ProbeTimeoutSeconds, 60)
		value.Platforms[platform] = item
	}
	return value
}

func cloneNonOpenAIPoolSettings(value NonOpenAIPoolSettings) NonOpenAIPoolSettings {
	cloned := value
	cloned.Platforms = make(map[string]NonOpenAIPoolPlatformSettings, len(value.Platforms))
	for platform, settings := range value.Platforms {
		cloned.Platforms[platform] = settings
	}
	return cloned
}
