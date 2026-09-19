package service

import (
	"fmt"
	"math"
	"strconv"
)

// Validate only the new fields; do not normalize or touch existing capabilities.
func validateSeedanceCredentialPatch(credentials map[string]any) error {
	if value, ok := credentials["seedance_enabled"]; ok && value != nil {
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("seedance_enabled must be a boolean")
		}
	}
	if value, ok := credentials["seedance_max_inflight"]; ok && value != nil {
		n, err := strconv.ParseFloat(fmt.Sprint(value), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || n < 1 || n > 100 {
			return fmt.Errorf("seedance_max_inflight must be an integer between 1 and 100")
		}
	}
	return nil
}
