package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBulkUpdateDisablesUpstreamBillingProbeOnlyForExplicitFalseOrRemoval(t *testing.T) {
	for _, test := range []struct {
		name     string
		extra    map[string]any
		remove   []string
		disables bool
	}{
		{name: "explicit false", extra: map[string]any{UpstreamBillingProbeEnabledExtraKey: false}, disables: true},
		{name: "explicit true", extra: map[string]any{UpstreamBillingProbeEnabledExtraKey: true}},
		{name: "remove key", remove: []string{UpstreamBillingProbeEnabledExtraKey}, disables: true},
		{name: "unrelated", extra: map[string]any{"privacy_mode": "training_off"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := bulkUpdateDisablesUpstreamBillingProbe(test.extra, test.remove)
			require.NoError(t, err)
			require.Equal(t, test.disables, got)
		})
	}
}

func TestBulkUpdateRejectsMalformedUpstreamBillingProbeFlag(t *testing.T) {
	_, err := bulkUpdateDisablesUpstreamBillingProbe(
		map[string]any{UpstreamBillingProbeEnabledExtraKey: "false"},
		nil,
	)
	require.Error(t, err)
}
