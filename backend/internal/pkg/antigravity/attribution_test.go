package antigravity

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestV027AttributionOnlyRemovesLeadingTransportLine(t *testing.T) {
	require.Equal(t, "Keep these rules", stripLeadingClaudeAttribution(" \nx-anthropic-billing-header: transport metadata\nKeep these rules"))
	require.Empty(t, stripLeadingClaudeAttribution("x-anthropic-billing-header: metadata"))
	for _, text := range []string{"Normal rules\nx-anthropic-billing-header: literal example", "  Normal rules  ", "x-anthropic-billing-header-example: keep"} {
		require.Equal(t, text, stripLeadingClaudeAttribution(text))
	}
}
