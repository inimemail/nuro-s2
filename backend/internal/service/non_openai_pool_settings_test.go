package service

import (
	"encoding/json"
	"testing"
)

func TestNormalizeNonOpenAIPoolSettingsMigratesLegacyPlatformToImageBucket(t *testing.T) {
	legacy := NonOpenAIPoolSettings{
		Enabled: true,
		Platforms: map[string]NonOpenAIPoolPlatformSettings{
			PlatformGrok: {RecoveryProbeEnabled: true, SoftCooldownMaxSeconds: 17, ProbeTimeoutSeconds: 7},
		},
	}
	normalized := normalizeNonOpenAIPoolSettings(legacy)
	grok := normalized.Platforms[PlatformGrok]
	if !grok.Image.RecoveryProbeEnabled {
		t.Fatal("legacy settings should keep image/media recovery enabled")
	}
	if grok.Image.SoftCooldownMaxSeconds != 30 || grok.Image.ProbeTimeoutSeconds != 5 {
		t.Fatalf("image defaults = %+v, want max=30 timeout=5", grok.Image)
	}
}

func TestNormalizeNonOpenAIPoolSettingsPreservesExplicitImageBucket(t *testing.T) {
	settings := DefaultNonOpenAIPoolSettings()
	grok := settings.Platforms[PlatformGrok]
	grok.Image = NonOpenAIPoolBucketSettings{RecoveryProbeEnabled: false, SoftCooldownMaxSeconds: 19, ProbeTimeoutSeconds: 8}
	settings.Platforms[PlatformGrok] = grok

	normalized := normalizeNonOpenAIPoolSettings(settings).Platforms[PlatformGrok].Image
	if normalized.RecoveryProbeEnabled {
		t.Fatal("explicit disabled image recovery should be preserved")
	}
	if normalized.SoftCooldownMaxSeconds != 19 || normalized.ProbeTimeoutSeconds != 8 {
		t.Fatalf("explicit image settings were not preserved: %+v", normalized)
	}
}

func TestNormalizeNonOpenAIPoolSettingsPreservesExplicitDisabledZeroImageJSON(t *testing.T) {
	var settings NonOpenAIPoolSettings
	err := json.Unmarshal([]byte(`{
		"enabled": true,
		"platforms": {
			"grok": {
				"recovery_probe_enabled": true,
				"soft_cooldown_max_seconds": 17,
				"probe_timeout_seconds": 7,
				"image": {"recovery_probe_enabled": false}
			}
		}
	}`), &settings)
	if err != nil {
		t.Fatalf("decode settings: %v", err)
	}

	image := normalizeNonOpenAIPoolSettings(settings).Platforms[PlatformGrok].Image
	if image.RecoveryProbeEnabled {
		t.Fatal("explicit disabled image recovery was mistaken for a legacy missing image object")
	}
	if image.SoftCooldownMaxSeconds != 30 || image.ProbeTimeoutSeconds != 5 {
		t.Fatalf("explicit partial image settings should receive numeric defaults: %+v", image)
	}
}

func TestNormalizeNonOpenAIPoolSettingsMigratesMissingImageJSON(t *testing.T) {
	var settings NonOpenAIPoolSettings
	err := json.Unmarshal([]byte(`{
		"enabled": true,
		"platforms": {
			"grok": {
				"recovery_probe_enabled": true,
				"soft_cooldown_max_seconds": 17,
				"probe_timeout_seconds": 7
			}
		}
	}`), &settings)
	if err != nil {
		t.Fatalf("decode settings: %v", err)
	}

	image := normalizeNonOpenAIPoolSettings(settings).Platforms[PlatformGrok].Image
	if !image.RecoveryProbeEnabled {
		t.Fatal("legacy JSON without image settings should enable image recovery by default")
	}
}
