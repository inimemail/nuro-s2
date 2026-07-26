package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSanitizeGrokResponsesToolsDropsOrphanChoiceWhenToolsMissing(t *testing.T) {
	body := []byte(`{"model":"grok-4","tool_choice":"auto","input":"hello"}`)
	got, err := sanitizeGrokResponsesTools(body)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(got, "tool_choice").Exists())
	require.Equal(t, "hello", gjson.GetBytes(got, "input").String())
}

func TestGrokRequestContextPrefersResolvedTargetPlatform(t *testing.T) {
	ctx, _ := gin.CreateTestContext(nil)
	ctx.Set("api_key", &APIKey{Group: &Group{Platform: PlatformOpenAI}})
	ctx.Request = requestWithContextForGrokTest(WithResolvedTargetPlatform(context.Background(), PlatformGrok))
	require.True(t, isGrokRequestContext(ctx))

	ctx.Request = requestWithContextForGrokTest(WithResolvedTargetPlatform(context.Background(), PlatformOpenAI))
	ctx.Set("api_key", &APIKey{Group: &Group{Platform: PlatformGrok}})
	require.False(t, isGrokRequestContext(ctx))
}

func requestWithContextForGrokTest(ctx context.Context) *http.Request {
	return (&http.Request{}).WithContext(ctx)
}
