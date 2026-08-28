package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIExecutionAllowsAccountSwitchStopsPoolReplacement(t *testing.T) {
	account := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true}}
	err := &service.UpstreamFailoverError{ExecutionUnknown: true}
	require.False(t, openAIExecutionAllowsAccountSwitch(account, err))
}

func TestOpenAIExecutionAllowsAccountSwitchStopsOrdinaryAccountReplacement(t *testing.T) {
	account := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Credentials: map[string]any{}}
	err := &service.UpstreamFailoverError{ExecutionUnknown: true}
	require.False(t, openAIExecutionAllowsAccountSwitch(account, err))
}

func TestOpenAIExecutionAllowsAccountSwitchDoesNotAffectKnownOrOtherPlatformFailures(t *testing.T) {
	require.True(t, openAIExecutionAllowsAccountSwitch(&service.Account{Platform: service.PlatformOpenAI}, &service.UpstreamFailoverError{}))
	require.True(t, openAIExecutionAllowsAccountSwitch(&service.Account{Platform: service.PlatformGrok}, &service.UpstreamFailoverError{ExecutionUnknown: true}))
}
