package usagetokens

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"
)

func TestParseExactCounts(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, tc := range []struct {
		value any
		want  int
		valid bool
	}{
		{json.Number("0"), 0, true}, {json.Number("1.0"), 1, true}, {json.Number("12e2"), 1200, true},
		{json.Number("1.5"), 0, false}, {json.Number("1e999"), 0, false}, {json.Number("-1"), 0, false},
		{json.Number("+1"), 0, false}, {json.Number("01"), 0, false}, {json.Number("-00"), 0, false}, {json.Number("-0"), 0, true},
		{json.Number(strconv.Itoa(maxInt)), maxInt, true}, {json.Number("9223372036854775808"), 0, false},
		{json.Number("9007199254740993"), 9007199254740993, true},
		{" 123 ", 123, true}, {"1/2", 0, false}, {nil, 0, false}, {true, 0, false},
		{math.NaN(), 0, false}, {math.Inf(1), 0, false}, {float64(1.5), 0, false}, {float64(1 << 54), 0, false},
		{float32(1 << 24), 1 << 24, true}, {float32(1 << 25), 0, false},
	} {
		got, ok := Parse(tc.value)
		if ok != tc.valid || ok && got != tc.want {
			t.Errorf("Parse(%v)=(%d,%v), want (%d,%v)", tc.value, got, ok, tc.want, tc.valid)
		}
	}
	if _, ok := Add(maxInt, 1); ok {
		t.Fatal("accepted overflow")
	}
	if got := Remainder(0, maxInt, maxInt); got != 0 {
		t.Fatalf("wrapped remainder %d", got)
	}
}
