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

// DeclaredModelIDs returns a complete native declaration only when every eligible
// native account restricts its models. Empty declarations retain upstream discovery.
func (s *GeminiMessagesCompatService) DeclaredModelIDs(ctx context.Context, groupID *int64) ([]string, error) {
	accounts, err := s.listSchedulableAccountsOnce(ctx, groupID, PlatformGemini, true)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range accounts {
		if accounts[i].Platform != PlatformGemini {
			continue
		}
		mapping := accounts[i].GetModelMapping()
		if len(mapping) == 0 {
			return nil, nil
		}
		for model, target := range mapping {
			if strings.TrimSpace(target) != "" {
				seen[strings.TrimPrefix(model, "models/")] = true
			}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func DeclaredModelsCover(declared []string, model string) bool {
	normalized := normalizeRequestedModelForLookup(PlatformGemini, model)
	for _, pattern := range declared {
		if matchWildcard(pattern, model) || matchWildcard(pattern, normalized) {
			return true
		}
	}
	return false
}
