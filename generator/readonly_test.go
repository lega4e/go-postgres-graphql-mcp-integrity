package generator

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/sdl"
)

// mixedSchema has one table gopgql owns and one it does not, so every assertion
// below can say which half moved rather than only that something did.
const mixedSchema = `
type Person @node(label: "person") {
  id: ID!
  name: String! @index
}
type Stream @node(label: "stream", table: "streams", schema: "dbos") @readonly {
  id: ID!
  seq: Int! @column(name: "offset") @index
  topic: String!
}
`

// buildMixed returns the table half and the graph half of the mixed schema —
// the two things every assertion below is made against.
func buildMixed(t *testing.T) (tables, graph string) {
	t.Helper()
	doc, err := sdl.Parse(mixedSchema)
	require.NoError(t, err)
	m, err := Build(doc, "")
	require.NoError(t, err)
	return TablesDDL(m), GraphDDL(m)
}

// TestReadOnlyEmitsNoTableDDL is the generator half of "no DDL and no
// migrations for those tables". The migrate half is
// TestReadOnlyEmitsNoMigration.
func TestReadOnlyEmitsNoTableDDL(t *testing.T) {
	tables, _ := buildMixed(t)

	assert.NotContains(t, tables, "streams",
		"a @readonly type contributes no CREATE TABLE, and no index and no constraint on one either")
	assert.NotContains(t, tables, "dbos")
	assert.NotContains(t, tables, "offset")

	// The managed half is untouched: its table and its index are still emitted.
	assert.Contains(t, tables, "CREATE TABLE persons")
	assert.Contains(t, tables, "CREATE INDEX persons_name_idx ON persons (name);")
}

// The other half of the same rule: @readonly constrains DDL emission, not
// visibility. The table is a full member of the property graph, which is the
// whole point of declaring it.
func TestReadOnlyIsStillInThePropertyGraph(t *testing.T) {
	_, graph := buildMixed(t)

	assert.Contains(t, graph, `dbos.streams LABEL stream PROPERTIES (id, "offset", topic)`)
	assert.Contains(t, graph, "persons LABEL person PROPERTIES (id, name)")
}

// Item 4, at the generator: `seq Int! @column(name: "offset")` must produce
// correctly quoted SQL. `offset` is reserved, so an unquoted one is a syntax
// error the moment the graph is created — and `dbos.streams.offset` is the
// column that motivated the requirement.
func TestReservedColumnNameIsQuoted(t *testing.T) {
	_, graph := buildMixed(t)

	assert.Contains(t, graph, `"offset"`)
	assert.NotContains(t, graph, "(id, offset,",
		"an unquoted `offset` in a property list is a syntax error, not a style choice")
}

// A managed type may also rename a column onto a reserved word, and the DDL for
// it is quoted in every position: the column definition, the index, and the
// property list.
func TestReservedColumnNameIsQuotedInManagedDDL(t *testing.T) {
	doc, err := sdl.Parse(`
type Event @node(label: "event") {
  id: ID!
  seq: Int! @column(name: "offset") @index
  kind: String! @column(name: "user")
}`)
	require.NoError(t, err)
	m, err := Build(doc, "")
	require.NoError(t, err)

	tables := TablesDDL(m)
	assert.Contains(t, tables, `    "offset" integer NOT NULL`)
	assert.Contains(t, tables, `    "user" text NOT NULL`)
	assert.Contains(t, tables, `CREATE INDEX events_offset_idx ON events ("offset");`)
	assert.Contains(t, GraphDDL(m), `PROPERTIES (id, "offset", "user")`)
}

// Schema qualification changes nothing for a schema that declares none. This is
// asserted directly rather than left to the golden files, because "unqualified
// emission is byte-identical to today" is the claim the whole feature rests on.
func TestUnqualifiedEmissionIsUnchanged(t *testing.T) {
	doc, err := sdl.Parse(`
type Person @node(label: "person") {
  id: ID!
  name: String!
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}`)
	require.NoError(t, err)
	m, err := Build(doc, "")
	require.NoError(t, err)

	ddl := DDL(m)
	for _, want := range []string{
		"CREATE TABLE persons (",
		"CREATE TABLE follows (",
		"REFERENCES persons (id)",
		"CREATE INDEX follows_target_idx ON follows (target_id);",
		"persons LABEL person PROPERTIES (id, name)",
	} {
		assert.Contains(t, ddl, want)
	}
	assert.False(t, strings.Contains(ddl, "public."),
		"nothing is qualified unless the SDL asked for it")
}
