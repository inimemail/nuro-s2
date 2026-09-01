package service

import (
	"context"
	"testing"
)

func TestWithHTTPUpstreamProfile_DefaultKeepsContext(t *testing.T) {
	ctx := context.Background()
	got := WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileDefault)
	if got != ctx {
		t.Fatal("default profile should not wrap context")
	}
}

func TestWithHTTPUpstreamProfile_OpenAI(t *testing.T) {
	ctx := WithHTTPUpstreamProfile(context.TODO(), HTTPUpstreamProfileOpenAI)
	if profile := HTTPUpstreamProfileFromContext(ctx); profile != HTTPUpstreamProfileOpenAI {
		t.Fatalf("expected profile %q, got %q", HTTPUpstreamProfileOpenAI, profile)
	}
}

func TestOpenAIHTTPUpstreamProfileUsesDedicatedHealthProbeProfile(t *testing.T) {
	normal := WithHTTPUpstreamProfile(context.Background(), HTTPUpstreamProfileOpenAI)
	if got := HTTPUpstreamProfileFromContext(normal); got != HTTPUpstreamProfileOpenAI {
		t.Fatalf("normal profile = %q, want %q", got, HTTPUpstreamProfileOpenAI)
	}

	probeCtx := WithOpenAIHealthProbeRequestContext(context.Background())
	probe := WithHTTPUpstreamProfile(probeCtx, HTTPUpstreamProfileOpenAI)
	if got := HTTPUpstreamProfileFromContext(probe); got != HTTPUpstreamProfileOpenAIHealthProbe {
		t.Fatalf("probe profile = %q, want %q", got, HTTPUpstreamProfileOpenAIHealthProbe)
	}
	media := WithHTTPUpstreamProfile(probeCtx, HTTPUpstreamProfileOpenAIMedia)
	if got := HTTPUpstreamProfileFromContext(media); got != HTTPUpstreamProfileOpenAIMedia {
		t.Fatalf("probe media profile = %q, want %q", got, HTTPUpstreamProfileOpenAIMedia)
	}
}

func TestWithHTTPUpstreamProfile_MediaProfiles(t *testing.T) {
	for _, profile := range []HTTPUpstreamProfile{HTTPUpstreamProfileMedia, HTTPUpstreamProfileOpenAIMedia} {
		ctx := WithHTTPUpstreamProfile(context.Background(), profile)
		if got := HTTPUpstreamProfileFromContext(ctx); got != profile {
			t.Fatalf("expected profile %q, got %q", profile, got)
		}
	}
}
