package ddl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVertexKeyClause covers the KEY (...) clause a vertex table carries when its
// type declares a natural key (design D1).
//
// The reader has to accept it before the generator emits one, for the same reason
// the rename statements had to land first: fold reconstructs prior state by
// re-parsing gopgql's own migrations, so a clause it cannot read makes every
// migration after the first natural key unreadable.
func TestVertexKeyClause(t *testing.T) {
	g := parseOne(t, `CREATE PROPERTY GRAPH app_graph
  VERTEX TABLES (
    persons KEY (tenant, email) LABEL person PROPERTIES (id, tenant, email),
    companies LABEL company PROPERTIES (id, name)
  )`).(*CreatePropertyGraphStmt)

	require.Len(t, g.Vertices, 2)
	assert.Equal(t, []string{"tenant", "email"}, g.Vertices[0].Key,
		"the key's columns are read in the order the clause lists them")
	assert.Equal(t, "person", g.Vertices[0].Label)
	assert.Equal(t, []string{"id", "tenant", "email"}, g.Vertices[0].Properties,
		"a KEY clause must not swallow the PROPERTIES that follow it")
	assert.Nil(t, g.Vertices[1].Key, "a vertex without a natural key carries no KEY clause")
}

// TestNamedTableConstraintsRoundTrip covers the CREATE TABLE forms the generator
// emits for a natural key and the two kinds of check. A constraint gopgql names
// and cannot read back is a constraint a later delta proposes a second time.
func TestNamedTableConstraintsRoundTrip(t *testing.T) {
	ct := parseOne(t, `CREATE TABLE persons (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant text NOT NULL,
    age integer DEFAULT 0,
    CONSTRAINT persons_key UNIQUE (tenant, age),
    CONSTRAINT persons_age_check CHECK (age >= 0),
    CONSTRAINT persons_check_1 CHECK (char_length(tenant) > 0)
)`).(*CreateTableStmt)

	require.Len(t, ct.Constraints, 3)
	assert.Equal(t, TableConstraint{
		Name: "persons_key", Kind: "UNIQUE", Columns: []string{"tenant", "age"},
	}, ct.Constraints[0])
	assert.Equal(t, TableConstraint{
		Name: "persons_age_check", Kind: "CHECK", Expr: "age >= 0",
	}, ct.Constraints[1])
	assert.Equal(t, TableConstraint{
		Name: "persons_check_1", Kind: "CHECK", Expr: "char_length(tenant) > 0",
	}, ct.Constraints[2],
		"a check body with its own parentheses is captured whole")
}
