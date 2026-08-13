package routes

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCompositeEndpointForAllGatewaySurfaces(t *testing.T) {
	tests := map[string]string{
		"/v1/messages":                 service.CompositeRouteEndpointMessages,
		"/v1/messages/count_tokens":    service.CompositeRouteEndpointCountTokens,
		"/responses":                   service.CompositeRouteEndpointResponses,
		"/backend-api/codex/responses": service.CompositeRouteEndpointResponses,
		"/chat/completions":            service.CompositeRouteEndpointChatCompletions,
		"/embeddings":                  service.CompositeRouteEndpointEmbeddings,
		"/images/generations":          service.CompositeRouteEndpointImages,
		"/videos/generations":          service.CompositeRouteEndpointVideos,
		"/tts":                         service.CompositeRouteEndpointVoice,
		"/realtime":                    service.CompositeRouteEndpointVoice,
		"/web_search":                  service.CompositeRouteEndpointSearch,
	}
	for path, want := range tests {
		endpoint, _, routed := compositeEndpointForPath(path)
		require.True(t, routed, path)
		require.Equal(t, want, endpoint, path)
	}
}

func TestCompositeLocalImageTaskReadsDoNotNeedModelRouting(t *testing.T) {
	for _, path := range []string{"/v1/images/tasks/task-1", "/images/tasks/task-1", "/v1/image-tasks", "/image-tasks"} {
		_, _, routed := compositeEndpointForPath(path)
		require.False(t, routed, path)
	}
}

func TestCompositeGatewayAPIKeyFastPath(t *testing.T) {
	require.False(t, isCompositeGatewayAPIKey(nil))
	require.False(t, isCompositeGatewayAPIKey(&service.APIKey{}))
	require.False(t, isCompositeGatewayAPIKey(&service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}}))
	require.True(t, isCompositeGatewayAPIKey(&service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}))
}
