package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

var errFixtureStickyMiss = errors.New("fixture cache miss")

type stickyMissCacheFixture struct{ GatewayCache }

func (stickyMissCacheFixture) IsSessionAccountNotFound(err error) bool {
	return errors.Is(err, errFixtureStickyMiss)
}

func TestV0222StickyObservationDistinguishesMissFromFailure(t *testing.T) {
	cache := stickyMissCacheFixture{}
	require.True(t, stickyReadObserved(cache, nil))
	require.True(t, stickyReadObserved(cache, errFixtureStickyMiss))
	require.True(t, stickyReadObserved(cache, fmt.Errorf("cache: %w", errFixtureStickyMiss)))
	require.False(t, stickyReadObserved(cache, context.DeadlineExceeded))
	require.False(t, stickyReadObserved(cache, fmt.Errorf("connection reset")))
	require.False(t, stickyReadObserved(nil, errFixtureStickyMiss))
}
