package sdl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadOnlyAndSchemaQualification(t *testing.T) {
	doc, err := Parse(`
type Person @node(label: "person") {
  id: ID!
  name: String!
}
type Stream @node(label: "stream", table: "streams", schema: "dbos") @readonly {
  id: ID!
  seq: Int! @column(name: "offset")
}`)
	require.NoError(t, err)

	person := doc.NodeByType("Person")
	require.NotNil(t, person)
	assert.False(t, person.ReadOnly)
	assert.Empty(t, person.Schema, "a type that declares no schema keeps resolving through search_path")

	stream := doc.NodeByType("Stream")
	require.NotNil(t, stream)
	assert.True(t, stream.ReadOnly)
	assert.Equal(t, "dbos", stream.Schema)
	assert.Equal(t, "streams", stream.Table)

	// Item 4: the rename hint is expressible on an unmanaged type, and the
	// column it names is a reserved word.
	seq := stream.Fields[1]
	assert.Equal(t, "offset", seq.ColumnName())
}

// A managed type may be schema-qualified too: qualification is about naming a
// table, not about who owns it. gopgql emits no CREATE SCHEMA either way, so the
// schema has to exist — which is the author's business, exactly as the table's
// existence is for a @readonly type.
func TestManagedTypeMayBeSchemaQualified(t *testing.T) {
	doc, err := Parse(`
type Person @node(label: "person", schema: "app") {
  id: ID!
  name: String!
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}`)
	require.NoError(t, err)

	person := doc.NodeByType("Person")
	require.NotNil(t, person)
	assert.Equal(t, "app", person.Schema)
	assert.False(t, person.ReadOnly)
}

func TestUnmanagedRejections(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sdl     string
		wantErr string
	}{
		{
			// An edge is a table gopgql would create, and over a table it does
			// not own it would also have to guess the key columns. Emitting a
			// graph whose edge is silently missing is the fallback SPEC.md §10
			// forbids.
			name: "relationship declared on an unmanaged type",
			sdl: `
type Person @node(label: "person") { id: ID! name: String! }
type Stream @node(label: "stream") @readonly {
  id: ID!
  authors: [Person!]! @relationship(type: "wrote", direction: OUT)
}`,
			wantErr: "@relationship(sourceKey:/destKey:) arrives in M13",
		},
		{
			name: "relationship pointing at an unmanaged type",
			sdl: `
type Person @node(label: "person") {
  id: ID!
  streams: [Stream!]! @relationship(type: "owns", direction: OUT)
}
type Stream @node(label: "stream") @readonly { id: ID! }`,
			wantErr: "which is @readonly",
		},
		{
			name: "@relationship(schema:)",
			sdl: `
type Person @node(label: "person") {
  id: ID!
  follows: [Person!]! @relationship(type: "follows", direction: OUT, schema: "dbos")
}`,
			wantErr: "@relationship(sourceKey:/destKey:), which arrives in M13",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.sdl)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestUnmanagedTypeStillNeedsASurrogateKey pins the boundary of what M12
// delivers, because it is the difference between the consuming project's real
// tables working and not.
//
// `dbos.operation_outputs` is keyed (workflow_uuid, function_id) and
// `dbos.streams` by (workflow_uuid, key, offset); neither has an `id` column,
// and @readonly forbids adding one. Making a declared @key(fields:) the vertex
// identity instead is M13 (SPEC.md §7 → M13), which is not implemented.
//
// Until it is, such a type is refused **at generate time**, by name, with no
// migration written and nothing sent to a database. That is the safe failure —
// the alternative would be emitting a property graph naming a column that does
// not exist, which fails only when the migration is applied. The message is
// asserted verbatim because it is the one a user actually sees.
func TestUnmanagedTypeStillNeedsASurrogateKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		sdl  string
		want string
	}{
		{
			name: "operation_outputs — a two-column natural key and no id",
			sdl: `
type Step @node(label: "step", table: "operation_outputs", schema: "dbos") @readonly
  @key(fields: ["workflowUuid", "functionId"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  functionId: Int! @column(name: "function_id")
  output: JSON
}`,
			want: "sdl: type Step must declare a surrogate key field `id: ID!`",
		},
		{
			name: "streams — a three-column natural key and no id",
			sdl: `
type Stream @node(label: "stream", table: "streams", schema: "dbos") @readonly
  @key(fields: ["workflowUuid", "key", "seq"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  key: String!
  seq: Int! @column(name: "offset")
  value: JSON
}`,
			want: "sdl: type Stream must declare a surrogate key field `id: ID!`",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.sdl)
			require.Error(t, err, "M13 is not implemented; this must not silently succeed")
			assert.EqualError(t, err, tc.want)
		})
	}
}
