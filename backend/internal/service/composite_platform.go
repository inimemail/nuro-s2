package service

import (
	"context"
	"strings"
	"sync/atomic"
)

var compositeResolverRegistry atomic.Pointer[CompositeRouteResolver]

func registerCompositeRouteResolver(r *CompositeRouteResolver) { compositeResolverRegistry.Store(r) }
func DefaultCompositeRouteResolver() *CompositeRouteResolver   { return compositeResolverRegistry.Load() }

func WithResolvedUpstreamModel(ctx context.Context, model string) context.Context {
	if ctx == nil || strings.TrimSpace(model) == "" {
		return ctx
	}
	return context.WithValue(ctx, compositeContextKey("resolved_upstream_model"), strings.TrimSpace(model))
}
func ResolvedUpstreamModelFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(compositeContextKey("resolved_upstream_model")).(string)
	return strings.TrimSpace(v), ok && strings.TrimSpace(v) != ""
}

type compositeContextKey string
