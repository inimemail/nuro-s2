package service

import (
	"math"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

const (
	VideoPriceFamilyGrokImagineVideo   = "grok-imagine-video"
	VideoPriceFamilyGrokImagineVideo15 = "grok-imagine-video-1.5"
)

func CanonicalGrokImagineVideoPriceFamily(model string) string {
	canonical := xai.CanonicalImagineVideoModel(model)
	switch canonical {
	case xai.DefaultImagineVideoModel:
		return VideoPriceFamilyGrokImagineVideo
	case xai.DefaultImagineVideo15Model:
		return VideoPriceFamilyGrokImagineVideo15
	default:
		if strings.HasPrefix(canonical, "grok-imagine-video-") {
			return canonical
		}
		return ""
	}
}

func NormalizeVideoModelPrices(in map[string]map[string]float64) map[string]map[string]float64 {
	if len(in) == 0 {
		return nil
	}
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(map[string]map[string]float64)
	for _, key := range keys {
		family := CanonicalGrokImagineVideoPriceFamily(key)
		if family == "" {
			family = strings.ToLower(strings.TrimSpace(key))
		}
		if family == "" {
			continue
		}
		for tier, price := range in[key] {
			normalizedTier, ok := LookupVideoBillingResolution(tier)
			if !ok || price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
				continue
			}
			if out[family] == nil {
				out[family] = make(map[string]float64)
			}
			out[family][normalizedTier] = price
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func LookupVideoModelPrice(prices map[string]map[string]float64, model, resolution string) *float64 {
	family := CanonicalGrokImagineVideoPriceFamily(model)
	if family == "" {
		family = strings.ToLower(strings.TrimSpace(model))
	}
	if family == "" {
		return nil
	}
	price, ok := prices[family][NormalizeVideoBillingResolutionOrDefault(resolution)]
	if !ok {
		return nil
	}
	return &price
}
