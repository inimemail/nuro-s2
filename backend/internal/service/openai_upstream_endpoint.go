package service

import "github.com/gin-gonic/gin"

const openAIUpstreamEndpointContextKey = "openai_actual_upstream_endpoint"

func SetActualOpenAIUpstreamEndpoint(c *gin.Context, endpoint string) {
	if c == nil || endpoint == "" {
		return
	}
	c.Set(openAIUpstreamEndpointContextKey, endpoint)
}

func ActualOpenAIUpstreamEndpoint(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, _ := c.Get(openAIUpstreamEndpointContextKey)
	endpoint, _ := value.(string)
	return endpoint
}
