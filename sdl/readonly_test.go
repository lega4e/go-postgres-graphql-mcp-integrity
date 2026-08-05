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
			wantErr: "Map an existing table instead: @relationship(table:, sourceKey:, destKey:)",
		},
		{
			name: "relationship pointing at an unmanaged type",
			sdl: `
type Person @node(label: "person") {
  id: ID!
  streams: [Stream!]! @relationship(type: "owns", direction: OUT)
}
type Stream @node(label: "stream") @readonly { id: ID! }`,
			wantErr: "Map an existing table instead",
		},
		{
			name: "@relationship(schema:)",
			sdl: `
type Person @node(label: "person") {
  id: ID!
  follows: [Person!]! @relationship(type: "follows", direction: OUT, schema: "dbos")
}`,
			wantErr: "name its key columns with @relationship(sourceKey:, destKey:)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.sdl)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestUnmanagedTypeIdentity is the M13 exit at the SDL: a @readonly type over a
// table with **no `id` column** is expressible, and identifies its rows by the
// natural key it declares.
//
// These are the real shapes the requirement came from —
// `dbos.operation_outputs` keyed (workflow_uuid, function_id) and `dbos.streams`
// keyed (workflow_uuid, key, offset), neither of which has an `id`, and neither
// of which gopgql may add one to.
func TestUnmanagedTypeIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		sdl  string
		want []string
	}{
		{
			name: "operation_outputs — a two-column key and no id",
			sdl: `
type Step @node(label: "step", table: "operation_outputs", schema: "dbos") @readonly
  @key(fields: ["workflowUuid", "functionId"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  functionId: Int! @column(name: "function_id")
  output: JSON
}`,
			want: []string{"workflow_uuid", "function_id"},
		},
		{
			name: "streams — a three-column key, one of them a reserved word",
			sdl: `
type Stream @node(label: "stream", table: "streams", schema: "dbos") @readonly
  @key(fields: ["workflowUuid", "key", "seq"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  key: String!
  seq: Int! @column(name: "offset")
  value: JSON
}`,
			want: []string{"workflow_uuid", "key", "offset"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := Parse(tc.sdl)
			require.NoError(t, err)

			n := doc.Nodes[0]
			assert.Equal(t, tc.want, n.IdentityColumns(),
				"the declared key is the identity, in declaration order and in *column* names")

			target := doc.TargetForType(n.TypeName)
			require.NotNil(t, target)
			assert.Equal(t, tc.want, target.Identity,
				"the compiler reads identity from the target, so it has to arrive there")
		})
	}
}

// A type gopgql owns is unchanged: its identity is the surrogate id, and a
// @key(fields:) stays a uniqueness constraint alongside it. SPEC.md §9's open
// decision stays open for managed tables, and M13 narrows it only for tables
// gopgql does not own.
func TestManagedTypeIdentityIsUnchanged(t *testing.T) {
	doc, err := Parse(`
type Person @node(label: "person") @key(fields: ["handle"]) {
  id: ID!
  handle: String!
}`)
	require.NoError(t, err)

	n := doc.NodeByType("Person")
	require.NotNil(t, n)
	assert.Equal(t, []string{"id"}, n.IdentityColumns())
	assert.Equal(t, []string{"handle"}, n.NaturalKey, "the natural key is still declared, and still a constraint")
}

// What is refused now is an unowned type that says **neither** how it is
// identified. Without `id` or `@key` there is nothing to group a response by,
// and a wrong identity does not fail loudly — it merges rows that are not the
// same row.
func TestUnmanagedTypeWithNoIdentityIsRefused(t *testing.T) {
	_, err := Parse(`
type Step @node(label: "step", table: "operation_outputs", schema: "dbos") @readonly {
  workflowUuid: ID! @column(name: "workflow_uuid")
  output: JSON
}`)
	require.Error(t, err)
	assert.EqualError(t, err,
		"sdl: type Step is @readonly and declares neither `id: ID!` nor @key(fields:); "+
			"a table gopgql does not own may have no surrogate key, so one or the other must say how a "+
			"row is identified")
}

// A type gopgql owns still must declare `id`: M13 changes nothing for it, and
// §9's decision to make a natural key *the* identity stays open for managed
// tables.
func TestManagedTypeStillNeedsAnID(t *testing.T) {
	_, err := Parse(`
type Person @node(label: "person") @key(fields: ["handle"]) {
  handle: String!
}`)
	require.Error(t, err)
	assert.EqualError(t, err, "sdl: type Person must declare a surrogate key field `id: ID!`")
}

// One position, one notion of identity. Implementors that disagreed would make
// one label expression mean two things, and the damage would show up as rows
// silently mis-grouped rather than as an error.
func TestInterfaceImplementorsMustAgreeOnIdentity(t *testing.T) {
	_, err := Parse(`
interface Item {
  workflowUuid: ID!
}
type Owned implements Item @node(label: "owned") {
  id: ID!
  workflowUuid: ID! @column(name: "workflow_uuid")
}
type Borrowed implements Item @node(label: "borrowed", table: "borrowed", schema: "dbos") @readonly
  @key(fields: ["workflowUuid"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identify a row differently")
	assert.Contains(t, err.Error(), "must share its identity columns")
}
