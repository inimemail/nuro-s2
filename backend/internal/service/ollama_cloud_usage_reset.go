package service

import "time"

func ollamaCloudUsageExhaustionResetAt(snapshot *OllamaCloudUsageSnapshot, now, minFetchedAt time.Time) (time.Time, bool) {
	if snapshot == nil || snapshot.Status != "ok" || snapshot.Data == nil || snapshot.FetchedAt == nil || snapshot.FetchedAt.Before(minFetchedAt) {
		return time.Time{}, false
	}
	var latest time.Time
	for _, window := range []*OllamaCloudUsageWindow{snapshot.Data.FiveHour, snapshot.Data.SevenDay} {
		if window == nil || window.UsedPercent < 100 {
			continue
		}
		if window.ResetAt == nil || !window.ResetAt.After(now) {
			return time.Time{}, false
		}
		if latest.IsZero() || window.ResetAt.After(latest) {
			latest = window.ResetAt.UTC()
		}
	}
	return latest, !latest.IsZero()
}
