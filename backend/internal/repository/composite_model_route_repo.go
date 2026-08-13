package repository

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/compositemodelroute"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type compositeModelRouteRepository struct{ client *ent.Client }

func NewCompositeModelRouteRepository(client *ent.Client) service.CompositeModelRouteRepository {
	return &compositeModelRouteRepository{client: client}
}

func (r *compositeModelRouteRepository) ListByGroup(ctx context.Context, groupID int64, includeDisabled bool) ([]service.CompositeModelRoute, error) {
	q := r.client.CompositeModelRoute.Query().Where(
		compositemodelroute.GroupIDEQ(groupID),
		compositemodelroute.DeletedAtIsNil(),
	).Order(compositemodelroute.ByPriority(), compositemodelroute.ByID())
	if !includeDisabled {
		q = q.Where(compositemodelroute.EnabledEQ(true))
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.CompositeModelRoute, 0, len(rows))
	for _, row := range rows {
		notes := ""
		if row.Notes != nil {
			notes = strings.TrimSpace(*row.Notes)
		}
		out = append(out, service.CompositeModelRoute{ID: row.ID, GroupID: row.GroupID, PublicModel: row.PublicModel, MatchType: row.MatchType, TargetPlatform: row.TargetPlatform, UpstreamModel: row.UpstreamModel, Endpoint: row.Endpoint, Priority: row.Priority, Enabled: row.Enabled, Notes: notes, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt})
	}
	return out, nil
}

func (r *compositeModelRouteRepository) Create(ctx context.Context, in *service.CompositeModelRoute) error {
	b := r.client.CompositeModelRoute.Create().SetGroupID(in.GroupID).SetPublicModel(in.PublicModel).SetMatchType(in.MatchType).SetTargetPlatform(in.TargetPlatform).SetUpstreamModel(in.UpstreamModel).SetEndpoint(in.Endpoint).SetPriority(in.Priority).SetEnabled(in.Enabled).SetNotes(in.Notes)
	row, err := b.Save(ctx)
	if err != nil {
		return err
	}
	in.ID, in.CreatedAt, in.UpdatedAt = row.ID, row.CreatedAt, row.UpdatedAt
	return nil
}

func (r *compositeModelRouteRepository) Update(ctx context.Context, in *service.CompositeModelRoute) error {
	b := r.client.CompositeModelRoute.UpdateOneID(in.ID).Where(compositemodelroute.DeletedAtIsNil()).SetPublicModel(in.PublicModel).SetMatchType(in.MatchType).SetTargetPlatform(in.TargetPlatform).SetUpstreamModel(in.UpstreamModel).SetEndpoint(in.Endpoint).SetPriority(in.Priority).SetEnabled(in.Enabled).SetNotes(in.Notes)
	row, err := b.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return service.ErrCompositeRouteNotFound
		}
		return err
	}
	in.UpdatedAt = row.UpdatedAt
	return nil
}

func (r *compositeModelRouteRepository) Delete(ctx context.Context, id int64) error {
	if _, err := r.client.CompositeModelRoute.UpdateOneID(id).Where(compositemodelroute.DeletedAtIsNil()).SetDeletedAt(time.Now().UTC()).Save(ctx); err != nil {
		if ent.IsNotFound(err) {
			return service.ErrCompositeRouteNotFound
		}
		return err
	}
	return nil
}

func (r *compositeModelRouteRepository) DeleteByGroup(ctx context.Context, groupID int64) error {
	_, err := r.client.CompositeModelRoute.Update().Where(compositemodelroute.GroupIDEQ(groupID), compositemodelroute.DeletedAtIsNil()).SetDeletedAt(time.Now().UTC()).Save(ctx)
	return err
}
