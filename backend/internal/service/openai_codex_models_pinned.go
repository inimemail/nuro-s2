package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrNoPinnedCodexModelsAccounts = errors.New("no usable pinned codex models manifest accounts")

const pinnedCodexModelsManifestTimeout = 30 * time.Second

func (s *OpenAIGatewayService) FetchPinnedCodexModelsManifest(ctx context.Context, group *Group, clientVersion string) (*CodexModelsManifest, *Account, error) {
	if s == nil || s.accountRepo == nil || group == nil || group.Platform != PlatformOpenAI || !group.CodexModelsManifestConfig.Enabled {
		return nil, nil, ErrNoPinnedCodexModelsAccounts
	}
	fetchCtx, cancel := context.WithTimeout(ctx, pinnedCodexModelsManifestTimeout)
	defer cancel()
	members, err := s.accountRepo.ListByGroup(fetchCtx, group.ID)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[int64]*Account, len(members))
	for i := range members {
		byID[members[i].ID] = &members[i]
	}
	var bodies [][]byte
	var first *Account
	var lastErr error
	for _, id := range group.CodexModelsManifestConfig.AccountIDs {
		account := byID[id]
		// Fixed manifest accounts must honor every existing scheduler exclusion
		// (expiry, cooldown, rate limits, temporary pauses, and billing guard),
		// not merely their persisted schedulable flag.
		if account == nil || account.Platform != PlatformOpenAI || !account.IsSchedulable() {
			continue
		}
		manifest, fetchErr := s.FetchCodexModelsManifest(fetchCtx, account, clientVersion, "")
		if fetchErr != nil {
			lastErr = fetchErr
			continue
		}
		if manifest == nil || len(manifest.Body) == 0 {
			continue
		}
		bodies = append(bodies, manifest.Body)
		if first == nil {
			copy := *account
			first = &copy
		}
	}
	if len(bodies) == 0 {
		if lastErr != nil {
			return nil, nil, lastErr
		}
		return nil, nil, ErrNoPinnedCodexModelsAccounts
	}
	merged, err := mergePinnedCodexManifestBodies(bodies)
	if err != nil {
		return nil, nil, err
	}
	return &CodexModelsManifest{Body: merged, ETag: codexModelsManifestBodyETag(merged)}, first, nil
}

func mergePinnedCodexManifestBodies(bodies [][]byte) ([]byte, error) {
	var base map[string]json.RawMessage
	if err := json.Unmarshal(bodies[0], &base); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var merged []json.RawMessage
	for _, body := range bodies {
		var env map[string]json.RawMessage
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		var models []json.RawMessage
		if raw := env["models"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &models); err != nil {
				return nil, err
			}
		}
		for _, model := range models {
			var item struct {
				Slug string `json:"slug"`
			}
			_ = json.Unmarshal(model, &item)
			key := strings.TrimSpace(item.Slug)
			if key == "" {
				key = string(model)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, model)
		}
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	base["models"] = encoded
	return json.Marshal(base)
}

func validatePinnedCodexModelsManifestConfig(cfg GroupCodexModelsManifestConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if len(cfg.AccountIDs) == 0 || len(cfg.AccountIDs) > 10 {
		return fmt.Errorf("codex manifest pinned accounts must contain 1-10 IDs")
	}
	return nil
}
