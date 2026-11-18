// Package usagetokens validates provider token counts without truncation or wraparound.
package usagetokens

import (
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Parse accepts nonnegative integer counts, including exact decimal/exponent
// representations. Float inputs are restricted to the exact integer range;
// JSON callers should use Decoder.UseNumber to preserve larger integers.
func Parse(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, v >= 0
	case int64:
		n := int(v)
		return n, v >= 0 && int64(n) == v
	case int32:
		return int(v), v >= 0
	case float32:
		if v > 1<<24 {
			return 0, false
		}
		return Parse(float64(v))
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1<<53 || math.Trunc(v) != v || v >= float64(uint(^uint(0)>>1)) {
			return 0, false
		}
		return int(v), true
	case json.Number:
		return parseDecimal(string(v))
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil && n >= 0
	default:
		return 0, false
	}
}

func parseDecimal(s string) (int, bool) {
	if s == "" || len(s) > 128 {
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n == 0 {
			return 0, s == "0" || s == "-0"
		}
		return n, n > 0 && s[0] >= '1' && s[0] <= '9'
	}
	// Only JSON number syntax is allowed, not rational or hexadecimal strings.
	if !json.Valid([]byte(s)) || (s[0] != '-' && (s[0] < '0' || s[0] > '9')) {
		return 0, false
	}
	// Bound exponent work before asking big.Rat to allocate.
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || e < -128 || e > 128 {
			return 0, false
		}
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 || !r.IsInt() || !r.Num().IsInt64() {
		return 0, false
	}
	n := r.Num().Int64()
	result := int(n)
	return result, result >= 0 && int64(result) == n
}

func Add(values ...int) (int, bool) {
	total := 0
	for _, value := range values {
		if value < 0 || value > int(^uint(0)>>1)-total {
			return 0, false
		}
		total += value
	}
	return total, true
}

// Remainder splits mutually exclusive buckets without intermediate underflow.
func Remainder(total int, parts ...int) int {
	if total < 0 {
		return 0
	}
	for _, part := range parts {
		if part < 0 || part > total {
			return 0
		}
		total -= part
	}
	return total
}
