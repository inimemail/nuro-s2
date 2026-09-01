package service

import (
	"context"
	"time"
)

// HTTPUpstreamProfile marks HTTP upstream requests that need provider-specific
// transport policy.
type HTTPUpstreamProfile string

const (
	HTTPUpstreamProfileDefault HTTPUpstreamProfile = ""
	HTTPUpstreamProfileOpenAI  HTTPUpstreamProfile = "openai"
	// Dedicated Responses health probes share one request-level deadline across
	// all candidate accounts. Keep their HTTP behavior OpenAI-compatible while
	// disabling the ordinary per-attempt response-header timeout.
	HTTPUpstreamProfileOpenAIHealthProbe HTTPUpstreamProfile = "openai_health_probe"
	// Media generation may legitimately complete expensive work before returning
	// response headers. Keep the provider's default transport behavior while
	// disabling only the response-header deadline.
	HTTPUpstreamProfileMedia HTTPUpstreamProfile = "media"
	// OpenAI media generation can legitimately finish the expensive work before
	// returning response headers. Keep OpenAI transport behavior, but do not
	// apply the text/Responses response-header deadline.
	HTTPUpstreamProfileOpenAIMedia HTTPUpstreamProfile = "openai_media"
	// Billing probes use a separate connection pool so a short probe cadence
	// cannot consume production first-token connections.
	HTTPUpstreamProfileBillingProbe HTTPUpstreamProfile = "billing_probe"
)

type httpUpstreamProfileContextKey struct{}
type httpUpstreamDisableRedirectsContextKey struct{}
type httpUpstreamResponseHeaderDeadlineContextKey struct{}

// WithHTTPUpstreamProfile injects an upstream transport profile into ctx.
func WithHTTPUpstreamProfile(ctx context.Context, profile HTTPUpstreamProfile) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if profile == HTTPUpstreamProfileOpenAI && IsOpenAIHealthProbeRequestContext(ctx) {
		profile = HTTPUpstreamProfileOpenAIHealthProbe
	}
	if profile == HTTPUpstreamProfileDefault {
		return ctx
	}
	return context.WithValue(ctx, httpUpstreamProfileContextKey{}, profile)
}

// HTTPUpstreamProfileFromContext resolves the upstream transport profile from ctx.
func HTTPUpstreamProfileFromContext(ctx context.Context) HTTPUpstreamProfile {
	if ctx == nil {
		return HTTPUpstreamProfileDefault
	}
	profile, ok := ctx.Value(httpUpstreamProfileContextKey{}).(HTTPUpstreamProfile)
	if !ok {
		return HTTPUpstreamProfileDefault
	}
	switch profile {
	case HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileOpenAIHealthProbe, HTTPUpstreamProfileMedia, HTTPUpstreamProfileOpenAIMedia, HTTPUpstreamProfileBillingProbe:
		return profile
	default:
		return HTTPUpstreamProfileDefault
	}
}

func WithHTTPUpstreamRedirectsDisabled(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, httpUpstreamDisableRedirectsContextKey{}, true)
}

func HTTPUpstreamRedirectsDisabled(ctx context.Context) bool {
	return ctx != nil && ctx.Value(httpUpstreamDisableRedirectsContextKey{}) == true
}

// WithHTTPUpstreamResponseHeaderDeadline bounds only the wait for upstream
// response headers. Callers must not turn this into a request Context deadline,
// because a successful SSE body can legitimately outlive the race budget.
func WithHTTPUpstreamResponseHeaderDeadline(ctx context.Context, deadline time.Time) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if deadline.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, httpUpstreamResponseHeaderDeadlineContextKey{}, deadline)
}

func HTTPUpstreamResponseHeaderDeadline(ctx context.Context) (time.Time, bool) {
	if ctx == nil {
		return time.Time{}, false
	}
	deadline, ok := ctx.Value(httpUpstreamResponseHeaderDeadlineContextKey{}).(time.Time)
	return deadline, ok && !deadline.IsZero()
}
