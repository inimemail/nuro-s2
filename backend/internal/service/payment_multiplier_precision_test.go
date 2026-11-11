package service

import (
	"github.com/stretchr/testify/require"
	"math"
	"strconv"
	"testing"
)

func TestBalanceRechargeMultiplierSixDecimals(t *testing.T) {
	for _, v := range []float64{1, 1.12, 1.123456, .000001, 9007199254} {
		require.False(t, balanceRechargeMultiplierHasExcessPrecision(v))
		got, err := strconv.ParseFloat(formatBalanceRechargeMultiplier(&v), 64)
		require.NoError(t, err)
		require.Equal(t, v, got)
	}
	for _, v := range []float64{0, -1, math.NaN(), math.Inf(1), 1.1234567, .0000001, 9007199255} {
		require.True(t, balanceRechargeMultiplierHasExcessPrecision(v))
		require.Empty(t, formatBalanceRechargeMultiplier(&v))
	}
	v := 1.123456
	require.Equal(t, "1.12", formatPositiveFloat(&v), "other settings retain their existing precision")
}
