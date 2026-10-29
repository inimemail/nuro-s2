package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

const GrokModelCatalogExtraKey = "grok_model_catalog"

// Each observation carries its start, not completion, time. A late response
// cannot replace a later attempt or a catalog from another credential mode.
type GrokCatalogObservation struct {
	Scope       string     `json:"scope"`
	AttemptedAt time.Time  `json:"attempted_at"`
	SucceededAt *time.Time `json:"succeeded_at,omitempty"`
	Models      []string   `json:"models"`
	Status      string     `json:"status"`
}

type GrokCatalogStore interface {
	CompareAndSwapGrokCatalog(context.Context, int64, any, time.Time, GrokCatalogObservation) (bool, error)
}

func GrokCatalogScope(account *Account) string {
	if account == nil {
		return ""
	}
	// Use the persisted configuration revision, never credential material.
	// Conservatively invalidate on any saved edit: a token rotation at the same
	// endpoint must not inherit the old credential's catalog or late response.
	// Catalog-only CAS writes deliberately do not change this revision.
	sum := sha256.Sum256([]byte(strconv.FormatInt(account.ID, 10) + "\x00" + account.Platform + "\x00" + account.Type + "\x00" + strings.TrimSpace(account.GetGrokBaseURL()) + "\x00" + account.GetCredential("oauth_type") + "\x00" + account.UpdatedAt.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:])
}

func decodeGrokCatalog(raw any) GrokCatalogObservation {
	var result GrokCatalogObservation
	if data, err := json.Marshal(raw); err == nil {
		_ = json.Unmarshal(data, &result)
	}
	return result
}

func mergeGrokCatalogObservation(raw any, incoming GrokCatalogObservation) GrokCatalogObservation {
	old := decodeGrokCatalog(raw)
	if old.Scope == incoming.Scope && !old.AttemptedAt.IsZero() && !incoming.AttemptedAt.After(old.AttemptedAt) {
		return old
	}
	if incoming.Status != "success" && old.Scope == incoming.Scope {
		incoming.Models = old.Models
		incoming.SucceededAt = old.SucceededAt
	}
	incoming.Models = dedupeAndSortModelIDs(incoming.Models)
	if len(incoming.Models) > 4096 {
		incoming.Models = incoming.Models[:4096]
	}
	return incoming
}

func (s *AccountTestService) persistGrokCatalog(ctx context.Context, account *Account, started time.Time, models []string, fetchErr error) error {
	store, ok := s.accountRepo.(GrokCatalogStore)
	if !ok || account.ID <= 0 {
		return nil
	}
	observation := GrokCatalogObservation{Scope: GrokCatalogScope(account), AttemptedAt: started, Status: "failed"}
	if fetchErr == nil {
		observation.Status = "success"
		stamp := time.Now().UTC()
		observation.SucceededAt = &stamp
		observation.Models = models
	}
	for attempt := 0; attempt < 3; attempt++ {
		current, err := s.accountRepo.GetByID(ctx, account.ID)
		if err != nil {
			return err
		}
		if GrokCatalogScope(current) != observation.Scope {
			return errors.New("model sync account changed")
		}
		previous := current.Extra[GrokModelCatalogExtraKey]
		merged := mergeGrokCatalogObservation(previous, observation)
		if merged.AttemptedAt != observation.AttemptedAt {
			return nil
		}
		saved, err := store.CompareAndSwapGrokCatalog(ctx, account.ID, previous, current.UpdatedAt, merged)
		if err != nil {
			return err
		}
		if saved {
			return nil
		}
	}
	return errors.New("model catalog changed concurrently; retry sync")
}

type GrokModelDiagnostic struct {
	Model  string `json:"model"`
	Status string `json:"status"`
}
type GrokCatalogDiagnostics struct {
	Status       string                `json:"status"`
	AttemptedAt  time.Time             `json:"attempted_at"`
	SucceededAt  *time.Time            `json:"succeeded_at,omitempty"`
	CatalogCount int                   `json:"catalog_count"`
	Models       []GrokModelDiagnostic `json:"models"`
}

// Pure projection: no token refresh, supplier request, DB write or background task.
func BuildGrokCatalogDiagnostics(account *Account, now time.Time) GrokCatalogDiagnostics {
	result := GrokCatalogDiagnostics{Status: "unknown", Models: []GrokModelDiagnostic{}}
	if account == nil || account.Platform != PlatformGrok {
		return result
	}
	snapshot := decodeGrokCatalog(account.Extra[GrokModelCatalogExtraKey])
	result.AttemptedAt = snapshot.AttemptedAt
	result.SucceededAt = snapshot.SucceededAt
	valid := snapshot.Scope == GrokCatalogScope(account) && snapshot.SucceededAt != nil && !snapshot.SucceededAt.After(now) && now.Sub(*snapshot.SucceededAt) <= 24*time.Hour && !snapshot.AttemptedAt.After(now)
	if valid {
		result.Status = snapshot.Status
		result.CatalogCount = len(snapshot.Models)
	} else if snapshot.Scope != "" && snapshot.Scope != GrokCatalogScope(account) {
		result.Status = "source_changed"
	} else if snapshot.SucceededAt != nil {
		result.Status = "stale"
	} else if snapshot.Status == "failed" {
		result.Status = "failed"
	}
	known := map[string]bool{}
	for _, model := range snapshot.Models {
		known[model] = true
	}
	for requested, target := range account.GetModelMapping() {
		state := "unverifiable"
		lower := strings.ToLower(target)
		if strings.Contains(lower, "image") || strings.Contains(lower, "video") || strings.Contains(lower, "audio") {
			state = "out_of_scope"
		} else if strings.HasPrefix(lower, "grok-") && !strings.ContainsAny(target, "*?") && valid {
			if known[target] {
				state = "listed"
			} else {
				state = "not_listed"
			}
		}
		result.Models = append(result.Models, GrokModelDiagnostic{Model: requested, Status: state})
	}
	sort.Slice(result.Models, func(i, j int) bool { return result.Models[i].Model < result.Models[j].Model })
	return result
}
