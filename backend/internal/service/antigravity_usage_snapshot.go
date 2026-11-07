package service

import "maps"

// Cached Antigravity usage is immutable after publication, including values
// shared by singleflight. Each caller owns its presentation and countdowns.
func cloneAntigravityUsageForPresentation(src *UsageInfo) *UsageInfo {
	if src == nil {
		return nil
	}
	out := *src
	out.UpdatedAt = cloneUsageValue(src.UpdatedAt)
	out.FiveHour = cloneUsageProgress(src.FiveHour)
	out.SevenDay = cloneUsageProgress(src.SevenDay)
	out.SevenDaySonnet = cloneUsageProgress(src.SevenDaySonnet)
	out.SevenDayFable = cloneUsageProgress(src.SevenDayFable)
	out.AntigravityQuota = make(map[string]*AntigravityModelQuota, len(src.AntigravityQuota))
	for key, quota := range src.AntigravityQuota {
		out.AntigravityQuota[key] = cloneUsageValue(quota)
	}
	if src.AntigravityQuota == nil {
		out.AntigravityQuota = nil
	}
	out.AntigravityQuotaDetails = make(map[string]*AntigravityModelDetail, len(src.AntigravityQuotaDetails))
	for key, detail := range src.AntigravityQuotaDetails {
		if detail == nil {
			out.AntigravityQuotaDetails[key] = nil
			continue
		}
		d := *detail
		d.SupportsImages = cloneUsageValue(detail.SupportsImages)
		d.SupportsThinking = cloneUsageValue(detail.SupportsThinking)
		d.ThinkingBudget = cloneUsageValue(detail.ThinkingBudget)
		d.Recommended = cloneUsageValue(detail.Recommended)
		d.MaxTokens = cloneUsageValue(detail.MaxTokens)
		d.MaxOutputTokens = cloneUsageValue(detail.MaxOutputTokens)
		d.SupportedMimeTypes = maps.Clone(detail.SupportedMimeTypes)
		out.AntigravityQuotaDetails[key] = &d
	}
	if src.AntigravityQuotaDetails == nil {
		out.AntigravityQuotaDetails = nil
	}
	out.AntigravityQuotaGroups = nil
	for _, group := range src.AntigravityQuotaGroups {
		if group == nil {
			out.AntigravityQuotaGroups = append(out.AntigravityQuotaGroups, nil)
			continue
		}
		g := *group
		g.Windows = nil
		for _, window := range group.Windows {
			w := cloneUsageValue(window)
			if w != nil {
				w.Utilization = cloneUsageValue(window.Utilization)
				w.ResetsAt = cloneUsageValue(window.ResetsAt)
			}
			g.Windows = append(g.Windows, w)
		}
		out.AntigravityQuotaGroups = append(out.AntigravityQuotaGroups, &g)
	}
	out.AntigravityQuotaSources = nil
	if src.AntigravityQuotaSources != nil {
		out.AntigravityQuotaSources = make(map[string]*AntigravityQuotaSource, len(src.AntigravityQuotaSources))
		for key, source := range src.AntigravityQuotaSources {
			v := cloneUsageValue(source)
			if v != nil {
				v.UpdatedAt = cloneUsageValue(source.UpdatedAt)
			}
			out.AntigravityQuotaSources[key] = v
		}
	}
	out.AICredits = append([]AICredit(nil), src.AICredits...)
	out.ModelForwardingRules = maps.Clone(src.ModelForwardingRules)
	recalcAntigravityRemainingSeconds(&out)
	return &out
}

func cloneUsageProgress(src *UsageProgress) *UsageProgress {
	out := cloneUsageValue(src)
	if out != nil {
		out.ResetsAt = cloneUsageValue(src.ResetsAt)
		out.WindowStats = cloneUsageValue(src.WindowStats)
	}
	return out
}

func cloneUsageValue[T any](src *T) *T {
	if src == nil {
		return nil
	}
	out := *src
	return &out
}
