package service

import (
	"slices"
	"strings"
)

const (
	vertexGlobalLocation = "global"
	vertexLocationUS     = "us"
	vertexLocationEU     = "eu"

	// vertexMultiRegionPriceMultiplier 是 vertexModelLocations 内的模型走多区域端点时相对 global 价的倍数。
	vertexMultiRegionPriceMultiplier = 1.1
)

// vertexModelLocations 列出 Vertex Gemini 模型可用的区域。Gemini 账号区域配置为多区域（us/eu）时，
// 表内声明支持该多区域的模型走对应多区域端点，其余模型（含未列出的模型）走 global。
// 账号区域为 global 或单区域时、以及非 Gemini 平台账号不查此表。
var vertexModelLocations = map[string][]string{
	"gemini-3.1-flash-image": {vertexGlobalLocation, vertexLocationUS, vertexLocationEU},
	"gemini-3.1-flash-lite":  {vertexGlobalLocation, vertexLocationUS, vertexLocationEU},
	"gemini-3.5-flash":       {vertexGlobalLocation, vertexLocationUS, vertexLocationEU},
	"gemini-3.5-flash-lite":  {vertexGlobalLocation, vertexLocationUS, vertexLocationEU},
	"gemini-3.6-flash":       {vertexGlobalLocation, vertexLocationUS, vertexLocationEU},
	"gemini-3.7-flash":       {vertexGlobalLocation, vertexLocationUS, vertexLocationEU},
	"gemini-3.8-flash":       {vertexGlobalLocation, vertexLocationUS, vertexLocationEU},
	"gemini-4-argon":         {vertexGlobalLocation, vertexLocationUS, vertexLocationEU},
}

// isVertexMultiRegion 判断区域是否为 Vertex 多区域（us/eu）。
func isVertexMultiRegion(location string) bool {
	return location == vertexLocationUS || location == vertexLocationEU
}

// vertexAPIHost 返回区域对应的 Vertex AI API 主机：global 用全局主机，多区域用 aiplatform.{loc}.rep 主机，
// 单区域用 {loc}-aiplatform 主机。
func vertexAPIHost(location string) string {
	switch {
	case location == vertexGlobalLocation:
		return "aiplatform.googleapis.com"
	case isVertexMultiRegion(location):
		return "aiplatform." + location + ".rep.googleapis.com"
	default:
		return location + "-aiplatform.googleapis.com"
	}
}

// vertexModelSupportsMultiRegion 判断模型是否在 vertexModelLocations 中声明支持该多区域。
func vertexModelSupportsMultiRegion(model, location string) bool {
	if !isVertexMultiRegion(location) {
		return false
	}
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	return slices.Contains(vertexModelLocations[strings.TrimSpace(model)], location)
}

// resolveVertexModelLocation 返回模型在账号区域下实际使用的区域：账号为多区域而模型不支持该多区域时用 global。
func resolveVertexModelLocation(accountLocation, model string) string {
	if isVertexMultiRegion(accountLocation) && !vertexModelSupportsMultiRegion(model, accountLocation) {
		return vertexGlobalLocation
	}
	return accountLocation
}

// vertexGeminiRequestLocation 返回 Vertex Gemini 请求实际发往的区域：未配置 project_id 的 API Key 账号
// 走 Express mode 全局端点；引用显式缓存时为缓存所在区域；其余按 VertexLocation(model)。
func vertexGeminiRequestLocation(account *Account, model, cachedContentLocation string) string {
	if account.IsVertexAPIKey() && account.VertexProjectID() == "" {
		return vertexGlobalLocation
	}
	if bound := account.GetCredential("_gemini_cache_bound_location"); bound != "" {
		return bound
	}
	if cachedContentLocation != "" {
		return cachedContentLocation
	}
	return account.VertexLocation(model)
}

// VertexUpstreamPriceMultiplier 返回 Vertex Gemini 账号请求 model 时相对 global 价的倍数：请求发往多区域端点
// 且模型在 vertexModelLocations 中声明支持该多区域时为 vertexMultiRegionPriceMultiplier，其余为 1。
// cachedContentLocation 为请求引用的显式缓存所在区域，未引用时为空。
func VertexUpstreamPriceMultiplier(account *Account, model, cachedContentLocation string) float64 {
	if account == nil || account.Platform != PlatformGemini || !account.IsVertexServiceAccount() {
		return 1
	}
	if vertexModelSupportsMultiRegion(model, vertexGeminiRequestLocation(account, model, cachedContentLocation)) {
		return vertexMultiRegionPriceMultiplier
	}
	return 1
}

func (a *Account) resolveVertexGeminiLocation(location, model string) string {
	if a == nil || a.Platform != PlatformGemini || model == "" {
		return location
	}
	return resolveVertexModelLocation(location, model)
}

func applyVertexDefaultTokenPricing(pricing *ModelPricing, multiplier float64) *ModelPricing {
	if pricing == nil || multiplier != vertexMultiRegionPriceMultiplier {
		return pricing
	}
	p := *pricing
	if !p.PricingOverrideInput {
		p.InputPricePerToken *= multiplier
	}
	if !p.PricingOverrideOutput {
		p.OutputPricePerToken *= multiplier
	}
	if !p.PricingOverrideCacheRead {
		p.CacheReadPricePerToken *= multiplier
	}
	// Scale token components independently; explicit overrides (including zero)
	// remain authoritative. Never scale a shared price card or the final total.
	fields := []struct {
		key   string
		price *float64
	}{
		{"input_cost_per_token_priority", &p.InputPricePerTokenPriority},
		{"output_cost_per_token_priority", &p.OutputPricePerTokenPriority},
		{"cache_read_input_token_cost_priority", &p.CacheReadPricePerTokenPriority},
		{"cache_creation_input_token_cost", &p.CacheCreationPricePerToken},
		{"cache_creation_input_token_cost_priority", &p.CacheCreationPricePerTokenPriority},
		{"cache_creation_input_token_cost", &p.CacheCreation5mPrice},
		{"cache_creation_input_token_cost_above_1hr", &p.CacheCreation1hPrice},
		{"input_cost_per_image_token", &p.ImageInputPricePerToken},
		{"output_cost_per_image_token", &p.ImageOutputPricePerToken},
		{"cache_read_input_image_token_cost", &p.ImageCacheReadPricePerToken},
	}
	for _, field := range fields {
		if !p.vertexPriceOverrides[field.key] {
			*field.price *= multiplier
		}
	}
	return &p
}

// Per-image prices are independent of token/storage prices and configured group
// tiers. This helper is only used after explicit group/channel pricing is ruled out.
func (s *BillingService) vertexDefaultImagePriceMultiplier(model string, regionMultiplier float64) float64 {
	if regionMultiplier != vertexMultiRegionPriceMultiplier {
		return 1
	}
	if s.pricingService != nil {
		if p := s.pricingService.GetIdentifiedModelPricing(model); p != nil && p.vertexPriceOverrides["output_cost_per_image"] {
			return 1
		}
	}
	return regionMultiplier
}
