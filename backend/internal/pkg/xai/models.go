package xai

import "strings"

// Model describes an xAI model in OpenAI-compatible /models shape.
type Model struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Type        string `json:"type,omitempty"`
	Created     int64  `json:"created,omitempty"`
	OwnedBy     string `json:"owned_by"`
	DisplayName string `json:"display_name,omitempty"`
}

const (
	DefaultTextModel                 = "grok-4.5"
	DefaultImagineImageQualityModel  = "grok-imagine-image-quality"
	DefaultImagineImageFastModel     = "grok-imagine-image"
	DefaultImagineVideoModel         = "grok-imagine-video"
	DefaultImagineVideo15LegacyModel = "grok-imagine-video-1.5"
	DefaultImagineVideo15Model       = "grok-imagine-video-1.5-preview"
)

var defaultModels = []Model{
	{ID: "grok-4.6", Object: "model", OwnedBy: "xai", DisplayName: "Grok 4.6"},
	{ID: "grok-4.5", Object: "model", OwnedBy: "xai", DisplayName: "Grok 4.5"},
	{ID: "grok-4.3", Object: "model", OwnedBy: "xai", DisplayName: "Grok 4.3"},
	{ID: "grok-3-mini", Object: "model", OwnedBy: "xai", DisplayName: "Grok 3 Mini"},
	{ID: "grok-3-mini-fast", Object: "model", OwnedBy: "xai", DisplayName: "Grok 3 Mini Fast"},
	{ID: "grok-build-0.1", Object: "model", OwnedBy: "xai", DisplayName: "Grok Build 0.1"},
	{ID: "grok-composer-2.5-fast", Object: "model", OwnedBy: "xai", DisplayName: "Grok Composer 2.5 Fast"},
	{ID: "grok-4.20-0309-reasoning", Object: "model", OwnedBy: "xai", DisplayName: "Grok 4.20 Reasoning"},
	{ID: "grok-4.20-0309-non-reasoning", Object: "model", OwnedBy: "xai", DisplayName: "Grok 4.20 Non Reasoning"},
	{ID: "grok-4.20-multi-agent-0309", Object: "model", OwnedBy: "xai", DisplayName: "Grok 4.20 Multi Agent"},
	{ID: "grok-imagine", Object: "model", OwnedBy: "xai", DisplayName: "Grok Imagine"},
	{ID: "grok-imagine-image", Object: "model", OwnedBy: "xai", DisplayName: "Grok Imagine Image"},
	{ID: "grok-imagine-image-quality", Object: "model", OwnedBy: "xai", DisplayName: "Grok Imagine Image Quality"},
	{ID: "grok-imagine-edit", Object: "model", OwnedBy: "xai", DisplayName: "Grok Imagine Edit"},
	{ID: "grok-imagine-video", Object: "model", OwnedBy: "xai", DisplayName: "Grok Imagine Video"},
	{ID: "grok-imagine-video-1.5", Object: "model", OwnedBy: "xai", DisplayName: "Grok Imagine Video 1.5"},
	{ID: "grok-imagine-video-1.5-preview", Object: "model", OwnedBy: "xai", DisplayName: "Grok Imagine Video 1.5 Preview"},
}

var grokTextModelAliases = map[string]string{
	"grok": "grok-4.5", "grok-latest": "grok-4.5", "grok-4.5-latest": "grok-4.5",
	"grok-4.6": "grok-4.6", "grok-4.6-latest": "grok-4.6",
	"grok-4.3-latest": "grok-4.3", "grok-build": "grok-build-0.1", "grok-build-latest": "grok-4.5",
	"grok-composer": "grok-composer-2.5-fast", "composer-2.5": "grok-composer-2.5-fast",
	"grok-4.20-reasoning": "grok-4.20-0309-reasoning", "grok-4.20-non-reasoning": "grok-4.20-0309-non-reasoning",
	"grok-4.20-multi-agent": "grok-4.20-multi-agent-0309",
}

func DefaultModels() []Model {
	out := make([]Model, len(defaultModels))
	copy(out, defaultModels)
	return out
}

func DefaultModelIDs() []string {
	models := DefaultModels()
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

func DefaultModelMapping() map[string]string {
	mapping := make(map[string]string, len(defaultModels)+5)
	for _, model := range defaultModels {
		mapping[model.ID] = model.ID
	}
	mapping["grok"] = "grok-4.5"
	mapping["grok-latest"] = "grok-4.5"
	mapping["grok-4.5-latest"] = "grok-4.5"
	mapping["grok-3-mini"] = "grok-3-mini"
	mapping["grok-3-mini-fast"] = "grok-3-mini-fast"
	mapping["grok-imagine-video-1.5-preview"] = "grok-imagine-video-1.5-preview"
	mapping["grok-build"] = "grok-build-0.1"
	mapping["grok-build-latest"] = "grok-4.5"
	mapping["grok-composer"] = "grok-composer-2.5-fast"
	mapping["composer-2.5"] = "grok-composer-2.5-fast"
	mapping["grok-4.20-reasoning"] = "grok-4.20-0309-reasoning"
	mapping["grok-4.20-non-reasoning"] = "grok-4.20-0309-non-reasoning"
	for alias, canonical := range grokTextModelAliases {
		mapping[alias] = canonical
	}
	// Provider-qualified native IDs are accepted; cross-vendor wildcards remain absent.
	for key, value := range cloneMapping(mapping) {
		if strings.HasPrefix(key, "grok") || strings.HasPrefix(key, "composer") {
			mapping["xai/"+key] = value
			mapping["x-ai/"+key] = value
			mapping["grok/"+key] = value
		}
	}
	return mapping
}

func cloneMapping(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func StripGrokProviderPrefix(model string) string {
	trimmed := strings.TrimSpace(model)
	lower := strings.ToLower(trimmed)
	for _, prefix := range []string{"xai/", "x-ai/", "grok/"} {
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(trimmed[len(prefix):])
		}
	}
	return trimmed
}

func CanonicalImagineVideoModel(model string) string {
	normalized := strings.ToLower(StripGrokProviderPrefix(model))
	switch {
	case normalized == "" || normalized == DefaultImagineVideoModel || normalized == "grok-imagine-video-preview" || normalized == "grok-video" || normalized == "grok-video-latest":
		return DefaultImagineVideoModel
	case strings.HasPrefix(normalized, "grok-imagine-video-1.5") || normalized == "grok-video-1.5":
		return DefaultImagineVideo15Model
	default:
		return normalized
	}
}

// IsGrokModelID reports native xAI/Grok and Imagine identifiers. It is used
// for capability checks only; it never enables cross-vendor model mapping.
func IsGrokModelID(model string) bool {
	normalized := strings.ToLower(StripGrokProviderPrefix(model))
	return normalized != "" && (strings.HasPrefix(normalized, "grok") || strings.HasPrefix(normalized, "imagine"))
}

// IsGrokTextResponsesModelID reports known native text aliases accepted by
// the Responses surface. Unknown/custom IDs remain account-local mappings.
func IsGrokTextResponsesModelID(model string) bool {
	normalized := strings.ToLower(StripGrokProviderPrefix(model))
	_, ok := grokTextModelAliases[normalized]
	if ok {
		return true
	}
	for _, candidate := range defaultModels {
		if candidate.ID == normalized && !strings.Contains(normalized, "imagine") {
			return true
		}
	}
	return false
}

// ResolveGrokTextResponsesModelID canonicalizes only native Grok aliases.
// OpenAI/Anthropic names pass through unchanged, preserving the local policy
// that cross-vendor wildcard mappings are disabled by default.
func ResolveGrokTextResponsesModelID(model string, defaultText ...string) string {
	fallback := DefaultTextModel
	if len(defaultText) > 0 && strings.TrimSpace(defaultText[0]) != "" {
		fallback = strings.TrimSpace(defaultText[0])
	}
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return fallback
	}
	normalized := strings.ToLower(StripGrokProviderPrefix(trimmed))
	if canonical, ok := grokTextModelAliases[normalized]; ok {
		return canonical
	}
	return trimmed
}

// RuntimeModelMappingOptions is intentionally native-only. The local fork
// does not expose a process-wide cross-vendor wildcard switch.
type ModelMappingOptions struct {
	DefaultText string
}

func ResolveDefaultTextModel(model string, defaultText ...string) string {
	if strings.TrimSpace(model) != "" {
		return strings.TrimSpace(model)
	}
	if len(defaultText) > 0 && strings.TrimSpace(defaultText[0]) != "" {
		return strings.TrimSpace(defaultText[0])
	}
	return DefaultTextModel
}
