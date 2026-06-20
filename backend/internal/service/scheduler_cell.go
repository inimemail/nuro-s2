package service

import (
	"hash/fnv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

type SchedulerCellRouter struct {
	enabled bool
	cellID  string
	cells   []string
}

func NewSchedulerCellRouter(cfg config.GatewaySchedulingConfig) *SchedulerCellRouter {
	cells := make([]string, 0, len(cfg.CellIDs))
	seen := make(map[string]struct{}, len(cfg.CellIDs))
	for _, raw := range cfg.CellIDs {
		cell := strings.TrimSpace(raw)
		if cell == "" {
			continue
		}
		if _, ok := seen[cell]; ok {
			continue
		}
		seen[cell] = struct{}{}
		cells = append(cells, cell)
	}
	cellID := strings.TrimSpace(cfg.CellID)
	enabled := cfg.CellEnabled && cellID != "" && len(cells) > 0
	return &SchedulerCellRouter{
		enabled: enabled,
		cellID:  cellID,
		cells:   cells,
	}
}

func (r *SchedulerCellRouter) Enabled() bool {
	return r != nil && r.enabled
}

func (r *SchedulerCellRouter) OwnsBucket(bucket SchedulerBucket) bool {
	if !r.Enabled() {
		return true
	}
	return r.cellForBucket(bucket) == r.cellID
}

func (r *SchedulerCellRouter) cellForBucket(bucket SchedulerBucket) string {
	if !r.Enabled() {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(bucket.String()))
	idx := int(h.Sum64() % uint64(len(r.cells)))
	return r.cells[idx]
}
