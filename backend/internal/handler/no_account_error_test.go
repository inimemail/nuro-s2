package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type stubModelAvailabilityDiagnoser struct {
	result service.ModelAvailabilityDiagnosis
}

func (s stubModelAvailabilityDiagnoser) DiagnoseModelAvailabilityForPlatform(
	context.Context,
	*int64,
	string,
	string,
) service.ModelAvailabilityDiagnosis {
	return s.result
}

func TestClassifyNoAccountError_ModelUnsupportedByGroupDoesNotLookLikeUpstreamModelNotFound(t *testing.T) {
	groupID := int64(12)
	cls := classifyNoAccountError(
		context.Background(),
		stubModelAvailabilityDiagnoser{
			result: service.ModelAvailabilityDiagnosis{
				HasAccountsInPool: true,
				HasModelSupport:   false,
			},
		},
		&service.APIKey{GroupID: &groupID},
		"gpt-5.4-mini",
		"gpt-5.4-mini",
		service.PlatformOpenAI,
	)

	require.Equal(t, http.StatusBadRequest, cls.Status)
	require.Equal(t, "invalid_request_error", cls.ErrType)
	require.Contains(t, cls.Message, "gpt-5.4-mini")
	require.NotContains(t, cls.ErrType, "model_not_found")
	require.NotContains(t, cls.Message, "not supported by any configured account")
	require.True(t, cls.ModelNotFound)
}
