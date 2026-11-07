package service

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"math"
	"sort"
	"strings"
	"time"
)

type AntigravityQuotaGroup struct {
	Key         string                    `json:"key"`
	Name        string                    `json:"name"`
	Description string                    `json:"description,omitempty"`
	Windows     []*AntigravityQuotaWindow `json:"windows"`
}
type AntigravityQuotaWindow struct {
	Window      string     `json:"window"`
	Utilization *float64   `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
}
type AntigravityQuotaSource struct {
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	Error     string     `json:"error,omitempty"`
	Stale     bool       `json:"stale,omitempty"`
}

func validAntigravityFraction(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f <= 1
}

func buildAntigravityQuotaGroups(summary *antigravity.QuotaSummaryResponse) []*AntigravityQuotaGroup {
	if summary == nil {
		return nil
	}
	var out []*AntigravityQuotaGroup
	for _, group := range summary.Groups {
		if len(group.Buckets) == 0 {
			continue
		}
		g := &AntigravityQuotaGroup{Name: group.DisplayName, Description: group.Description}
		for _, bucket := range group.Buckets {
			if g.Key == "" {
				g.Key, _, _ = strings.Cut(bucket.BucketID, "-")
			}
			w := &AntigravityQuotaWindow{Window: bucket.Window}
			if f := bucket.RemainingFraction; f != nil && validAntigravityFraction(*f) {
				used := (1 - *f) * 100
				w.Utilization = &used
			}
			if reset, err := time.Parse(time.RFC3339, bucket.ResetTime); err == nil {
				w.ResetsAt = &reset
			}
			g.Windows = append(g.Windows, w)
		}
		order := func(w string) int {
			switch w {
			case "5h":
				return 0
			case "weekly":
				return 1
			}
			return 2
		}
		sort.SliceStable(g.Windows, func(i, j int) bool { return order(g.Windows[i].Window) < order(g.Windows[j].Window) })
		out = append(out, g)
	}
	return out
}

// Retained values are display-only and explicitly stale. Current authentication
// failures always take precedence; this never clears account health or quota.
func mergeAntigravityLastGood(current, previous *UsageInfo) *UsageInfo {
	if current == nil || previous == nil || current.IsForbidden || current.NeedsReauth {
		return current
	}
	old := cloneAntigravityUsageForPresentation(previous)
	if current.AntigravityQuotaSources == nil {
		current.AntigravityQuotaSources = make(map[string]*AntigravityQuotaSource)
	}
	retain := func(key string, present bool) bool {
		fresh := current.AntigravityQuotaSources[key]
		if present || (fresh != nil && fresh.UpdatedAt != nil && fresh.Error == "") {
			return false
		}
		prior := old.AntigravityQuotaSources[key]
		if prior == nil && old.UpdatedAt != nil {
			prior = &AntigravityQuotaSource{UpdatedAt: cloneUsageValue(old.UpdatedAt)}
		}
		if prior == nil || prior.UpdatedAt == nil {
			return false
		}
		prior.Stale, prior.Error = true, "refresh_failed"
		current.AntigravityQuotaSources[key] = prior
		return true
	}
	if retain("basic", len(current.AntigravityQuota) > 0) {
		current.AntigravityQuota, current.AntigravityQuotaDetails = old.AntigravityQuota, old.AntigravityQuotaDetails
		current.ModelForwardingRules, current.FiveHour = old.ModelForwardingRules, old.FiveHour
	}
	if retain("summary", len(current.AntigravityQuotaGroups) > 0) {
		current.AntigravityQuotaGroups = old.AntigravityQuotaGroups
	}
	if retain("tier", current.SubscriptionTier != "") {
		current.SubscriptionTier, current.SubscriptionTierRaw, current.AICredits = old.SubscriptionTier, old.SubscriptionTierRaw, old.AICredits
	}
	return current
}
