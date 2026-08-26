package service

import (
	"context"
	"testing"
)

type compositeRouteRepoStub struct{ routes []CompositeModelRoute }

func (s *compositeRouteRepoStub) ListByGroup(_ context.Context, groupID int64, includeDisabled bool) ([]CompositeModelRoute, error) {
	rows := make([]CompositeModelRoute, 0, len(s.routes))
	for _, route := range s.routes {
		if route.GroupID == groupID && (includeDisabled || route.Enabled) {
			rows = append(rows, route)
		}
	}
	return rows, nil
}
func (*compositeRouteRepoStub) Create(context.Context, *CompositeModelRoute) error { return nil }
func (*compositeRouteRepoStub) Update(context.Context, *CompositeModelRoute) error { return nil }
func (*compositeRouteRepoStub) Delete(context.Context, int64) error                { return nil }
func (*compositeRouteRepoStub) DeleteByGroup(context.Context, int64) error         { return nil }

func TestCompositeRouteResolverExplicitRoutePrecedesDetector(t *testing.T) {
	r := NewCompositeRouteResolver(&compositeRouteRepoStub{routes: []CompositeModelRoute{{ID: 7, GroupID: 1, PublicModel: "gpt-public", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformGrok, UpstreamModel: "grok-3", Endpoint: CompositeRouteEndpointResponses, Priority: 1, Enabled: true}}})
	d, err := r.Resolve(context.Background(), 1, "gpt-public", CompositeRouteEndpointResponses)
	if err != nil || !d.Matched || d.Source != CompositeRouteSourceExplicit || d.TargetPlatform != PlatformGrok || d.UpstreamModel != "grok-3" {
		t.Fatalf("unexpected decision: %#v %v", d, err)
	}
}

func TestDetectCompositeModelPlatformKimiCodeAliases(t *testing.T) {
	for _, model := range []string{"k3", "k3-256k", "kimi-k3", "kimi-code/k3", "moonshot-v1-128k"} {
		platform, ok := DetectCompositeModelPlatform(model)
		if !ok || platform != PlatformKimi {
			t.Fatalf("model %q resolved to %q, ok=%v; want kimi", model, platform, ok)
		}
	}
	if _, ok := DetectCompositeModelPlatform("k3-preview"); ok {
		t.Fatal("unknown k3 alias must fail closed")
	}
}

func TestCompositeRouteResolverFailsClosedForUnknownModel(t *testing.T) {
	r := NewCompositeRouteResolver(&compositeRouteRepoStub{})
	d, err := r.Resolve(context.Background(), 1, "vendor-private-model", CompositeRouteEndpointResponses)
	if err != nil || d.Matched {
		t.Fatalf("expected unresolved route, got %#v %v", d, err)
	}
}

func TestCompositeRouteResolverRejectsCrossGroupMutation(t *testing.T) {
	r := NewCompositeRouteResolver(&compositeRouteRepoStub{routes: []CompositeModelRoute{{ID: 7, GroupID: 1, PublicModel: "gpt-public", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformGrok, UpstreamModel: "grok-3", Endpoint: CompositeRouteEndpointResponses, Enabled: true}}})
	input := CompositeRouteInput{PublicModel: "gpt-public", TargetPlatform: PlatformGrok, UpstreamModel: "grok-3", Endpoint: CompositeRouteEndpointResponses, Enabled: true}
	if _, err := r.UpdateRouteForGroup(context.Background(), 2, 7, input); err != ErrCompositeRouteNotFound {
		t.Fatalf("expected cross-group update rejection, got %v", err)
	}
	if err := r.DeleteRouteForGroup(context.Background(), 2, 7); err != ErrCompositeRouteNotFound {
		t.Fatalf("expected cross-group delete rejection, got %v", err)
	}
}

func TestCompositeRouteResolverRejectsDisabledAndUpdateKeyConflicts(t *testing.T) {
	r := NewCompositeRouteResolver(&compositeRouteRepoStub{routes: []CompositeModelRoute{
		{ID: 7, GroupID: 1, PublicModel: "public-a", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformGrok, UpstreamModel: "grok-3", Endpoint: CompositeRouteEndpointResponses, Enabled: false},
		{ID: 8, GroupID: 1, PublicModel: "public-b", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-5", Endpoint: CompositeRouteEndpointResponses, Enabled: true},
	}})
	input := CompositeRouteInput{PublicModel: "public-a", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformGrok, UpstreamModel: "grok-3", Endpoint: CompositeRouteEndpointResponses, Enabled: true}
	if _, err := r.CreateRoute(context.Background(), 1, input); err != ErrCompositeRouteExists {
		t.Fatalf("expected disabled route key conflict, got %v", err)
	}
	if _, err := r.UpdateRouteForGroup(context.Background(), 1, 8, input); err != ErrCompositeRouteExists {
		t.Fatalf("expected update route key conflict, got %v", err)
	}
}
