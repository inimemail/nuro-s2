package admin

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

func TestUsageStatsCacheKeyIncludesUpstreamModelMismatch(t *testing.T) {
	matched := false
	mismatched := true
	base := usagestats.UsageLogFilters{Model: "gpt-5.6"}
	matchKey := usageStatsCacheKey(func() usagestats.UsageLogFilters {
		f := base
		f.UpstreamModelMismatch = &matched
		return f
	}())
	mismatchKey := usageStatsCacheKey(func() usagestats.UsageLogFilters {
		f := base
		f.UpstreamModelMismatch = &mismatched
		return f
	}())
	require.NotEqual(t, matchKey, mismatchKey)
}
