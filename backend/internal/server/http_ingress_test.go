package server

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestProvideHTTPServerAppliesIngressLimits(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Host:              "127.0.0.1",
			Port:              8080,
			ReadHeaderTimeout: 1,
			MaxHeaderBytes:    8 * 1024,
		},
	}

	server := ProvideHTTPServer(cfg, gin.New())
	require.Equal(t, 8*1024, server.MaxHeaderBytes)
	require.Equal(t, time.Second, server.ReadHeaderTimeout)
	require.NotNil(t, server.Handler)
}
