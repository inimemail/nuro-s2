package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/domain"
)

type CompositeModelRoute struct{ ent.Schema }

func (CompositeModelRoute) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "composite_model_routes"}}
}
func (CompositeModelRoute) Mixin() []ent.Mixin {
	return []ent.Mixin{mixins.TimeMixin{}, mixins.SoftDeleteMixin{}}
}
func (CompositeModelRoute) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("group_id"),
		field.String("public_model").MaxLen(200).NotEmpty(),
		field.String("match_type").MaxLen(20).Default("exact"),
		field.String("target_platform").MaxLen(50).Default(domain.PlatformOpenAI),
		field.String("upstream_model").MaxLen(200).Default(""),
		field.String("endpoint").MaxLen(50).Default("any"),
		field.Int("priority").Default(100),
		field.Bool("enabled").Default(true),
		field.String("notes").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "text"}),
	}
}
func (CompositeModelRoute) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("group", Group.Type).
			Unique().
			Required().
			Field("group_id").
			Annotations(entsql.OnDelete(entsql.Cascade)),
	}
}
