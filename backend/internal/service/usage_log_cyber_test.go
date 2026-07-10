package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestTypeCyberBlocked(t *testing.T) {
	require.True(t, RequestTypeCyberBlocked.IsValid())
	require.Equal(t, "cyber", RequestTypeCyberBlocked.String())

	rt, err := ParseUsageRequestType("cyber")
	require.NoError(t, err)
	require.Equal(t, RequestTypeCyberBlocked, rt)

	u := &UsageLog{RequestType: RequestTypeCyberBlocked, Stream: true, OpenAIWSMode: true}
	require.Equal(t, RequestTypeCyberBlocked, u.EffectiveRequestType())

	u.SyncRequestTypeAndLegacyFields()
	require.Equal(t, RequestTypeCyberBlocked, u.RequestType)
	require.True(t, u.Stream, "cyber request_type should not rewrite legacy stream flag")
	require.True(t, u.OpenAIWSMode, "cyber request_type should not rewrite legacy ws flag")
}
