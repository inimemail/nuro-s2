package service

import (
	"encoding/json"
	"strings"
)

// NonOpenAIPoolPlatformSettings controls the pool runtime for one non-OpenAI
// platform. OpenAI deliberately keeps its existing independent settings and
// runtime implementation.
type NonOpenAIPoolPlatformSettings struct {
	RecoveryProbeEnabled   bool                        `json:"recovery_probe_enabled"`
	RecoveryProbeModel     string                      `json:"recovery_probe_model"`
	SoftCooldownMaxSeconds int                         `json:"soft_cooldown_max_seconds"`
	ProbeTimeoutSeconds    int                         `json:"probe_timeout_seconds"`
	Image                  NonOpenAIPoolBucketSettings `json:"image"`
	imagePresent           bool
}

func (s *NonOpenAIPoolPlatformSettings) UnmarshalJSON(data []byte) error {
	type platformSettingsJSON struct {
		RecoveryProbeEnabled   bool                         `json:"recovery_probe_enabled"`
		RecoveryProbeModel     string                       `json:"recovery_probe_model"`
		SoftCooldownMaxSeconds int                          `json:"soft_cooldown_max_seconds"`
		ProbeTimeoutSeconds    int                          `json:"probe_timeout_seconds"`
		Image                  *NonOpenAIPoolBucketSettings `json:"image"`
	}
	var decoded platformSettingsJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	s.RecoveryProbeEnabled = decoded.RecoveryProbeEnabled
	s.RecoveryProbeModel = decoded.RecoveryProbeModel
	s.SoftCooldownMaxSeconds = decoded.SoftCooldownMaxSeconds
	s.ProbeTimeoutSeconds = decoded.ProbeTimeoutSeconds
	s.Image = NonOpenAIPoolBucketSettings{}
	s.imagePresent = decoded.Image != nil
	if decoded.Image != nil {
		s.Image = *decoded.Image
	}
	return nil
}

// NonOpenAIPoolBucketSettings contains the settings that differ between a
// platform's normal model traffic and its image/media traffic.
type NonOpenAIPoolBucketSettings struct {
	RecoveryProbeEnabled   bool   `json:"recovery_probe_enabled"`
	RecoveryProbeModel     string `json:"recovery_probe_model"`
	SoftCooldownMaxSeconds int    `json:"soft_cooldown_max_seconds"`
	ProbeTimeoutSeconds    int    `json:"probe_timeout_seconds"`
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
		Platforms: defaultNonOpenAIPoolPlatforms(),
	}
}

func defaultNonOpenAIPoolPlatforms() map[string]NonOpenAIPoolPlatformSettings {
	platforms := make(map[string]NonOpenAIPoolPlatformSettings, 6)
	probeModels := map[string]string{
		PlatformGemini: "gemini-2.0-flash", PlatformAntigravity: "claude-sonnet-4-5",
		PlatformGrok: "grok-4.5", PlatformKimi: "kimi-k2",
		PlatformZhipu: "glm-4.7", PlatformDeepSeek: "deepseek-chat",
	}
	imageProbeModels := map[string]string{
		PlatformGemini: "gemini-2.5-flash-image", PlatformAntigravity: "gemini-2.5-flash-image",
		PlatformGrok: "grok-imagine-image",
	}
	for _, platform := range []string{PlatformGemini, PlatformAntigravity, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepSeek} {
		platforms[platform] = NonOpenAIPoolPlatformSettings{
			RecoveryProbeEnabled: true, RecoveryProbeModel: probeModels[platform], SoftCooldownMaxSeconds: 30, ProbeTimeoutSeconds: 5,
			Image: NonOpenAIPoolBucketSettings{RecoveryProbeEnabled: true, RecoveryProbeModel: imageProbeModels[platform], SoftCooldownMaxSeconds: 30, ProbeTimeoutSeconds: 360},
		}
	}
	return platforms
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
		item.RecoveryProbeModel = strings.TrimSpace(item.RecoveryProbeModel)
		if item.RecoveryProbeModel == "" {
			item.RecoveryProbeModel = fallback.RecoveryProbeModel
		}
		item.ProbeTimeoutSeconds = clamp(item.ProbeTimeoutSeconds, value.ProbeTimeoutSeconds, 60)
		// Older settings had only one platform bucket. Seed the new image bucket
		// from the legacy values so an upgrade does not disable image recovery.
		if !item.imagePresent && item.Image.SoftCooldownMaxSeconds <= 0 && item.Image.ProbeTimeoutSeconds <= 0 &&
			!item.Image.RecoveryProbeEnabled {
			item.Image = fallback.Image
		}
		item.Image.SoftCooldownMaxSeconds = clamp(item.Image.SoftCooldownMaxSeconds, fallback.Image.SoftCooldownMaxSeconds, 3600)
		item.Image.RecoveryProbeModel = strings.TrimSpace(item.Image.RecoveryProbeModel)
		if item.Image.RecoveryProbeModel == "" {
			item.Image.RecoveryProbeModel = fallback.Image.RecoveryProbeModel
		}
		if item.Image.ProbeTimeoutSeconds <= 0 {
			item.Image.ProbeTimeoutSeconds = fallback.Image.ProbeTimeoutSeconds
		}
		item.Image.ProbeTimeoutSeconds = clamp(item.Image.ProbeTimeoutSeconds, fallback.Image.ProbeTimeoutSeconds, 600)
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
