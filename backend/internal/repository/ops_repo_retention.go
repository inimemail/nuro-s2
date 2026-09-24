package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Planner estimates avoid an unbounded COUNT scan when previewing a policy.
func (r *opsRepository) EstimateRequestRetention(ctx context.Context, cutoff time.Time) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var raw []byte
	if err := r.db.QueryRowContext(ctx, "EXPLAIN (FORMAT JSON) SELECT id FROM usage_logs WHERE created_at < $1", cutoff).Scan(&raw); err != nil {
		return 0, err
	}
	var plans []struct {
		Plan struct {
			Rows int64 `json:"Plan Rows"`
		} `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		return 0, err
	}
	if len(plans) != 1 {
		return 0, fmt.Errorf("missing retention estimate")
	}
	return plans[0].Plan.Rows, nil
}
