package service

import (
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
	// Deprecated compatibility method. Cells never own scheduler buckets: the
	// global snapshot must remain visible to every scheduler instance.
	return true
}

func (r *SchedulerCellRouter) cellForBucket(bucket SchedulerBucket) string {
	// Kept for source compatibility with older integrations. Account Cell
	// ownership is now resolved by AccountCellDirectory, never by bucket hash.
	if !r.Enabled() || len(r.cells) == 0 {
		return ""
	}
	return r.cellID
}

func (r *SchedulerCellRouter) Cells() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.cells...)
}
