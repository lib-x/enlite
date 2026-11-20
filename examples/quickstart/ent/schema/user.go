package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// User is the Ent schema for the quickstart.
type User struct {
	ent.Schema
}

// Fields describes users and the optional foreign key to their manager.
func (User) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty().Unique(),
		field.Int("manager_id").Optional(),
	}
}

// Edges describes the self-referencing manager relationship.
func (User) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("reports", User.Type).
			From("manager").
			Unique().
			Field("manager_id"),
	}
}
