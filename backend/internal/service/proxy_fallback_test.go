package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func proxyFallbackTestProxy(id int64, mode string, backup *int64, expiresInDays *int, now time.Time) Proxy {
	p := Proxy{ID: id, FallbackMode: mode, BackupProxyID: backup}
	if expiresInDays != nil {
		t := now.AddDate(0, 0, *expiresInDays)
		p.ExpiresAt = &t
	}
	return p
}

func proxyFallbackTestInt64(v int64) *int64 { return &v }
func proxyFallbackTestInt(v int) *int       { return &v }

func TestResolveProxyFallbackTarget(t *testing.T) {
	now := time.Now()
	t.Run("none keeps original", func(t *testing.T) {
		a := proxyFallbackTestProxy(1, FallbackModeNone, nil, proxyFallbackTestInt(-1), now)
		target, change := ResolveProxyFallbackTarget(a, map[int64]Proxy{1: a}, now)
		require.False(t, change)
		require.Nil(t, target)
	})
	t.Run("direct uses nil target", func(t *testing.T) {
		a := proxyFallbackTestProxy(1, FallbackModeDirect, nil, proxyFallbackTestInt(-1), now)
		target, change := ResolveProxyFallbackTarget(a, map[int64]Proxy{1: a}, now)
		require.True(t, change)
		require.Nil(t, target)
	})
	t.Run("proxy uses healthy backup", func(t *testing.T) {
		b := proxyFallbackTestProxy(2, FallbackModeNone, nil, proxyFallbackTestInt(30), now)
		a := proxyFallbackTestProxy(1, FallbackModeProxy, proxyFallbackTestInt64(2), proxyFallbackTestInt(-1), now)
		target, change := ResolveProxyFallbackTarget(a, map[int64]Proxy{1: a, 2: b}, now)
		require.True(t, change)
		require.NotNil(t, target)
		require.Equal(t, int64(2), *target)
	})
	t.Run("chain skips expired backup", func(t *testing.T) {
		c := proxyFallbackTestProxy(3, FallbackModeNone, nil, proxyFallbackTestInt(30), now)
		b := proxyFallbackTestProxy(2, FallbackModeProxy, proxyFallbackTestInt64(3), proxyFallbackTestInt(-1), now)
		a := proxyFallbackTestProxy(1, FallbackModeProxy, proxyFallbackTestInt64(2), proxyFallbackTestInt(-1), now)
		target, change := ResolveProxyFallbackTarget(a, map[int64]Proxy{1: a, 2: b, 3: c}, now)
		require.True(t, change)
		require.Equal(t, int64(3), *target)
	})
	t.Run("cycle keeps original", func(t *testing.T) {
		b := proxyFallbackTestProxy(2, FallbackModeProxy, proxyFallbackTestInt64(1), proxyFallbackTestInt(-1), now)
		a := proxyFallbackTestProxy(1, FallbackModeProxy, proxyFallbackTestInt64(2), proxyFallbackTestInt(-1), now)
		target, change := ResolveProxyFallbackTarget(a, map[int64]Proxy{1: a, 2: b}, now)
		require.False(t, change)
		require.Nil(t, target)
	})
	t.Run("chain tail direct fallback", func(t *testing.T) {
		b := proxyFallbackTestProxy(2, FallbackModeDirect, nil, proxyFallbackTestInt(-1), now)
		a := proxyFallbackTestProxy(1, FallbackModeProxy, proxyFallbackTestInt64(2), proxyFallbackTestInt(-1), now)
		target, change := ResolveProxyFallbackTarget(a, map[int64]Proxy{1: a, 2: b}, now)
		require.True(t, change)
		require.Nil(t, target)
	})
}
