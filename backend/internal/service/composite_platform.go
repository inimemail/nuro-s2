package service

import (
	"context"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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

func isCompositeModelResponse(ctx context.Context) bool {
	_, ok := ResolvedUpstreamModelFromContext(ctx)
	return ok
}

func restoreCompositeAnthropicResponseModel(ctx context.Context, body []byte, stream bool) []byte {
	if ctx == nil {
		return body
	}
	public, _ := ctx.Value(compositeContextKey("public_response_model")).(string)
	if public == "" {
		return body
	}
	path := "model"
	if stream {
		if gjson.GetBytes(body, "type").String() != "message_start" {
			return body
		}
		path = "message.model"
	}
	if gjson.GetBytes(body, path).Type != gjson.String {
		return body
	}
	out, err := sjson.SetBytes(body, path, public)
	if err != nil {
		return body
	}
	return out
}
