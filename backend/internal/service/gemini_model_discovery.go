package service

import (
	"context"
	"sort"
	"strings"
)

// Model discovery is deliberately separate from the generation hot path.
func (s *GeminiMessagesCompatService) AntigravityGeminiModelIDs(ctx context.Context, groupID *int64, requireMixed bool) ([]string, error) {
	accounts, err := s.listSchedulableAccountsOnce(ctx, groupID, PlatformAntigravity, false)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, account := range accounts {
		if account.Platform != PlatformAntigravity || (requireMixed && !account.IsMixedSchedulingEnabled()) {
			continue
		}
		for model, target := range account.GetModelMapping() {
			model = strings.TrimPrefix(model, "models/")
			if strings.TrimSpace(target) != "" && strings.HasPrefix(model, "gemini-") && IsSafeGeminiModelPathSegment(model) && !strings.ContainsAny(model, "*?") {
				seen[model] = true
				for _, suffix := range geminiThinkingVariantSuffixes {
					if bare, found := strings.CutSuffix(model, suffix); found {
						if _, supported := resolveGeminiThinkingVariantModel(&account, bare, "high"); supported {
							seen[bare] = true
						}
					}
				}
			}
		}
	}
	models := make([]string, 0, len(seen))
	for model := range seen {
		models = append(models, model)
	}
	sort.Strings(models)
	return models, nil
}
