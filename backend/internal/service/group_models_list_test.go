package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestGroupModelAllowlistUsesExactAndPrefixPatterns(t *testing.T) {
	group := &Group{ModelsListConfig: GroupModelsListConfig{Enabled: true, Models: []string{"gpt-5.5", "deepseek-*"}}}
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	got, ok := groupFromRoutingContext(ctx)
	require.True(t, ok)
	require.True(t, got.CustomModelsListEnabled())
	require.True(t, groupModelAllowed(got.ModelsListConfig.Models, "gpt-5.5"))
	require.True(t, groupModelAllowed(got.ModelsListConfig.Models, "deepseek-chat"))
	require.False(t, groupModelAllowed(got.ModelsListConfig.Models, "gpt-4o"))
}

func TestGroupModelAllowlistEmptyPreservesLegacyBehavior(t *testing.T) {
	require.True(t, groupModelAllowed(nil, "anything"))
	require.True(t, groupModelAllowed([]string{""}, "anything"))
}
