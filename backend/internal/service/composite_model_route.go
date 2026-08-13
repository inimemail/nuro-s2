package service

import (
	"context"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"strings"
	"time"
)

const (
	CompositeRouteMatchExact              = "exact"
	CompositeRouteMatchPrefix             = "prefix"
	CompositeRouteEndpointAny             = "any"
	CompositeRouteEndpointMessages        = "messages"
	CompositeRouteEndpointCountTokens     = "count_tokens"
	CompositeRouteEndpointResponses       = "responses"
	CompositeRouteEndpointChatCompletions = "chat_completions"
	CompositeRouteEndpointEmbeddings      = "embeddings"
	CompositeRouteEndpointImages          = "images"
	CompositeRouteEndpointVideos          = "videos"
	CompositeRouteEndpointVoice           = "voice"
	CompositeRouteEndpointSearch          = "search"
	CompositeRouteEndpointGemini          = "gemini"
	CompositeRouteSourceExplicit          = "route"
	CompositeRouteSourceDetector          = "detector"
)

var (
	ErrCompositeRouteNotFound = infraerrors.NotFound("COMPOSITE_ROUTE_NOT_FOUND", "composite route not found")
	ErrCompositeRouteExists   = infraerrors.Conflict("COMPOSITE_ROUTE_EXISTS", "composite route already exists")
)

type CompositeModelRoute struct {
	ID             int64     `json:"id"`
	GroupID        int64     `json:"group_id"`
	PublicModel    string    `json:"public_model"`
	MatchType      string    `json:"match_type"`
	TargetPlatform string    `json:"target_platform"`
	UpstreamModel  string    `json:"upstream_model"`
	Endpoint       string    `json:"endpoint"`
	Priority       int       `json:"priority"`
	Enabled        bool      `json:"enabled"`
	Notes          string    `json:"notes"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
type CompositeRoutePreviewRequest struct {
	Model    string `json:"model"`
	Endpoint string `json:"endpoint"`
}
type CompositeRouteDecision struct {
	Matched        bool                 `json:"matched"`
	Source         string               `json:"source"`
	GroupID        int64                `json:"group_id"`
	PublicModel    string               `json:"public_model"`
	TargetPlatform string               `json:"target_platform"`
	UpstreamModel  string               `json:"upstream_model"`
	Endpoint       string               `json:"endpoint"`
	Route          *CompositeModelRoute `json:"route,omitempty"`
	Reason         string               `json:"reason,omitempty"`
}
type CompositeRouteInput struct {
	PublicModel    string
	MatchType      string
	TargetPlatform string
	UpstreamModel  string
	Endpoint       string
	Priority       int
	Enabled        bool
	Notes          string
}
type CompositeModelRouteRepository interface {
	ListByGroup(context.Context, int64, bool) ([]CompositeModelRoute, error)
	Create(context.Context, *CompositeModelRoute) error
	Update(context.Context, *CompositeModelRoute) error
	Delete(context.Context, int64) error
	DeleteByGroup(context.Context, int64) error
}

func normalizeCompositeRouteEndpoint(endpoint string) string {
	switch strings.ToLower(strings.TrimSpace(endpoint)) {
	case CompositeRouteEndpointMessages, CompositeRouteEndpointCountTokens, CompositeRouteEndpointResponses, CompositeRouteEndpointChatCompletions, CompositeRouteEndpointEmbeddings, CompositeRouteEndpointImages, CompositeRouteEndpointVideos, CompositeRouteEndpointVoice, CompositeRouteEndpointSearch, CompositeRouteEndpointGemini:
		return strings.ToLower(strings.TrimSpace(endpoint))
	default:
		return CompositeRouteEndpointAny
	}
}
func normalizeCompositeRouteMatchType(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), CompositeRouteMatchPrefix) {
		return CompositeRouteMatchPrefix
	}
	return CompositeRouteMatchExact
}
func normalizeCompositeRouteInput(in CompositeRouteInput) CompositeRouteInput {
	in.PublicModel = strings.TrimSpace(in.PublicModel)
	in.MatchType = normalizeCompositeRouteMatchType(in.MatchType)
	in.TargetPlatform = strings.TrimSpace(in.TargetPlatform)
	in.UpstreamModel = strings.TrimSpace(in.UpstreamModel)
	in.Endpoint = normalizeCompositeRouteEndpoint(in.Endpoint)
	in.Notes = strings.TrimSpace(in.Notes)
	if in.UpstreamModel == "" && in.MatchType == CompositeRouteMatchExact {
		in.UpstreamModel = in.PublicModel
	}
	return in
}

type CompositeRouteResolver struct{ repo CompositeModelRouteRepository }

func NewCompositeRouteResolver(repo CompositeModelRouteRepository) *CompositeRouteResolver {
	resolver := &CompositeRouteResolver{repo: repo}
	registerCompositeRouteResolver(resolver)
	return resolver
}

func (r *CompositeRouteResolver) List(ctx context.Context, groupID int64) ([]CompositeModelRoute, error) {
	if r == nil || r.repo == nil {
		return nil, nil
	}
	return r.repo.ListByGroup(ctx, groupID, true)
}

func (r *CompositeRouteResolver) CreateRoute(ctx context.Context, groupID int64, in CompositeRouteInput) (*CompositeModelRoute, error) {
	in = normalizeCompositeRouteInput(in)
	if in.PublicModel == "" || in.UpstreamModel == "" || !isCompositeConcretePlatform(in.TargetPlatform) {
		return nil, infraerrors.BadRequest("COMPOSITE_ROUTE_INVALID", "invalid composite route")
	}
	routes, err := r.repo.ListByGroup(ctx, groupID, true)
	if err != nil {
		return nil, err
	}
	for _, existing := range routes {
		if existing.PublicModel == in.PublicModel && existing.MatchType == in.MatchType && existing.Endpoint == in.Endpoint {
			return nil, ErrCompositeRouteExists
		}
	}
	row := &CompositeModelRoute{GroupID: groupID, PublicModel: in.PublicModel, MatchType: in.MatchType, TargetPlatform: in.TargetPlatform, UpstreamModel: in.UpstreamModel, Endpoint: in.Endpoint, Priority: in.Priority, Enabled: in.Enabled, Notes: in.Notes}
	if err := r.repo.Create(ctx, row); err != nil {
		return nil, err
	}
	return row, nil
}

func (r *CompositeRouteResolver) UpdateRoute(ctx context.Context, id int64, in CompositeRouteInput) (*CompositeModelRoute, error) {
	in = normalizeCompositeRouteInput(in)
	if in.PublicModel == "" || in.UpstreamModel == "" || !isCompositeConcretePlatform(in.TargetPlatform) {
		return nil, infraerrors.BadRequest("COMPOSITE_ROUTE_INVALID", "invalid composite route")
	}
	row := &CompositeModelRoute{ID: id, PublicModel: in.PublicModel, MatchType: in.MatchType, TargetPlatform: in.TargetPlatform, UpstreamModel: in.UpstreamModel, Endpoint: in.Endpoint, Priority: in.Priority, Enabled: in.Enabled, Notes: in.Notes}
	if err := r.repo.Update(ctx, row); err != nil {
		return nil, err
	}
	return row, nil
}

// UpdateRouteForGroup enforces the group boundary before mutating a route.
// Route IDs are globally unique, but the admin URL is group-scoped; checking
// membership here prevents a stale or forged route_id from crossing groups.
func (r *CompositeRouteResolver) UpdateRouteForGroup(ctx context.Context, groupID, id int64, in CompositeRouteInput) (*CompositeModelRoute, error) {
	if r == nil || r.repo == nil {
		return nil, ErrCompositeRouteNotFound
	}
	routes, err := r.repo.ListByGroup(ctx, groupID, true)
	if err != nil {
		return nil, err
	}
	in = normalizeCompositeRouteInput(in)
	found := false
	for _, route := range routes {
		if route.ID == id {
			found = true
			continue
		}
		if route.PublicModel == in.PublicModel && route.MatchType == in.MatchType && route.Endpoint == in.Endpoint {
			return nil, ErrCompositeRouteExists
		}
	}
	if !found {
		return nil, ErrCompositeRouteNotFound
	}
	return r.UpdateRoute(ctx, id, in)
}

func (r *CompositeRouteResolver) DeleteRoute(ctx context.Context, id int64) error {
	if r == nil || r.repo == nil {
		return ErrCompositeRouteNotFound
	}
	return r.repo.Delete(ctx, id)
}

func (r *CompositeRouteResolver) DeleteRouteForGroup(ctx context.Context, groupID, id int64) error {
	if r == nil || r.repo == nil {
		return ErrCompositeRouteNotFound
	}
	routes, err := r.repo.ListByGroup(ctx, groupID, true)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if route.ID == id {
			return r.DeleteRoute(ctx, id)
		}
	}
	return ErrCompositeRouteNotFound
}
func (r *CompositeRouteResolver) Resolve(ctx context.Context, groupID int64, model, endpoint string) (CompositeRouteDecision, error) {
	model = strings.TrimSpace(model)
	endpoint = normalizeCompositeRouteEndpoint(endpoint)
	d := CompositeRouteDecision{GroupID: groupID, PublicModel: model, Endpoint: endpoint}
	if model == "" {
		d.Reason = "model is required"
		return d, nil
	}
	if r != nil && r.repo != nil {
		routes, err := r.repo.ListByGroup(ctx, groupID, false)
		if err != nil {
			return d, err
		}
		if route, ok := matchCompositeRoute(routes, model, endpoint); ok {
			upstream := strings.TrimSpace(route.UpstreamModel)
			if upstream == "" {
				upstream = model
			}
			return CompositeRouteDecision{Matched: true, Source: CompositeRouteSourceExplicit, GroupID: groupID, PublicModel: model, TargetPlatform: route.TargetPlatform, UpstreamModel: upstream, Endpoint: endpoint, Route: &route}, nil
		}
	}
	if platform, ok := DetectCompositeModelPlatform(model); ok {
		return CompositeRouteDecision{Matched: true, Source: CompositeRouteSourceDetector, GroupID: groupID, PublicModel: model, TargetPlatform: platform, UpstreamModel: model, Endpoint: endpoint}, nil
	}
	d.Reason = "no explicit route or model detector match"
	return d, nil
}
func matchCompositeRoute(routes []CompositeModelRoute, model, endpoint string) (CompositeModelRoute, bool) {
	var best *CompositeModelRoute
	strength, endpointWeight, prefixLen := -1, -1, -1
	for i := range routes {
		r := routes[i]
		if !r.Enabled || (normalizeCompositeRouteEndpoint(r.Endpoint) != endpoint && normalizeCompositeRouteEndpoint(r.Endpoint) != CompositeRouteEndpointAny) {
			continue
		}
		r.MatchType = normalizeCompositeRouteMatchType(r.MatchType)
		p := strings.TrimSpace(r.PublicModel)
		s := 0
		if r.MatchType == CompositeRouteMatchExact {
			if p != model {
				continue
			}
			s = 2
		} else if strings.HasPrefix(model, p) {
			s = 1
		} else {
			continue
		}
		ew := 0
		if normalizeCompositeRouteEndpoint(r.Endpoint) == endpoint {
			ew = 1
		}
		if best == nil || s > strength || (s == strength && (ew > endpointWeight || (ew == endpointWeight && (len(p) > prefixLen || (len(p) == prefixLen && (r.Priority < best.Priority || (r.Priority == best.Priority && r.ID < best.ID))))))) {
			copy := r
			best = &copy
			strength, endpointWeight, prefixLen = s, ew, len(p)
		}
	}
	if best == nil {
		return CompositeModelRoute{}, false
	}
	return *best, true
}

func DetectCompositeModelPlatform(model string) (string, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(m, "claude-") || strings.HasPrefix(m, "anthropic/") {
		return PlatformAnthropic, true
	}
	if strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "o1-") || strings.HasPrefix(m, "o3-") || strings.HasPrefix(m, "o4-") || strings.HasPrefix(m, "codex-") || strings.HasPrefix(m, "openai/") {
		return PlatformOpenAI, true
	}
	if strings.HasPrefix(m, "gemini-") || strings.HasPrefix(m, "google/") {
		return PlatformGemini, true
	}
	if strings.HasPrefix(m, "grok-") || m == "grok" || strings.HasPrefix(m, "xai/") {
		return PlatformGrok, true
	}
	return "", false
}

func isCompositeConcretePlatform(platform string) bool {
	switch platform {
	case PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok:
		return true
	default:
		return false
	}
}
