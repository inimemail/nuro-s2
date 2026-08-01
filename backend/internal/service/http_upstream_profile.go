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
	case HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileBillingProbe:
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
