package sdl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mutationSDL wraps a Mutation type in the minimum an SDL needs around it.
func mutationSDL(mutation string) string {
	return `
type Person @node(label: "person") {
  id: ID!
  name: String!
}
type Mutation {
` + mutation + `
}
`
}

func TestFunctionDirectiveIsReadIntoTheModel(t *testing.T) {
	doc, err := Parse(mutationSDL(`
  startAgentRun(
    agentDigest: String! @column(name: "agent_digest")
    userId: String! @column(name: "user_id")
    queue: String = "agent"
    priority: Int
  ): String! @function(schema: "dbos", name: "enqueue_workflow")
  sendMessage(destination: String!): Boolean! @function(schema: "dbos", name: "send_message", returns: VOID)`))
	require.NoError(t, err)

	require.Len(t, doc.Mutations, 2)
	assert.Equal(t, []string{"sendMessage", "startAgentRun"}, doc.MutationFields())

	m := doc.MutationByField("startAgentRun")
	require.NotNil(t, m)
	assert.Equal(t, "dbos", m.Schema)
	assert.Equal(t, "enqueue_workflow", m.Function)
	assert.Equal(t, ReturnScalar, m.Returns, "SCALAR is the directive's own default")
	assert.Equal(t, "dbos.enqueue_workflow", m.QualifiedName())

	// Arguments keep declaration order, and each carries the *parameter* name it
	// maps to — @column(name:) where the GraphQL name and the SQL name differ.
	require.Len(t, m.Args, 4)
	assert.Equal(t, []string{"agentDigest", "userId", "queue", "priority"},
		[]string{m.Args[0].Name, m.Args[1].Name, m.Args[2].Name, m.Args[3].Name})
	assert.Equal(t, []string{"agent_digest", "user_id", "queue", "priority"},
		[]string{m.Args[0].Param, m.Args[1].Param, m.Args[2].Param, m.Args[3].Param})

	assert.True(t, m.Args[0].NonNull)
	assert.False(t, m.Args[3].NonNull)
	require.NotNil(t, m.Args[2].Default, "a GraphQL default declared in the SDL is carried")
	assert.Equal(t, "agent", m.Args[2].Default.Raw)
	assert.Nil(t, m.Args[3].Default)

	void := doc.MutationByField("sendMessage")
	require.NotNil(t, void)
	assert.Equal(t, ReturnVoid, void.Returns)
}

func TestFunctionDirectiveRejections(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sdl     string
		wantErr string
	}{
		{
			// No default target and no inference: a mutation field that named no
			// function would parse and never run.
			name:    "mutation field without @function",
			sdl:     mutationSDL(`  doThing(a: String!): String!`),
			wantErr: "declares no @function",
		},
		{
			// gqlparser permits @function on any FIELD_DEFINITION, so without a
			// check of its own one on a @node field would generate a column and
			// never be called.
			name: "@function outside the Mutation type",
			sdl: `
type Person @node(label: "person") {
  id: ID!
  name: String! @function(schema: "dbos", name: "f")
}`,
			wantErr: "only a field of the Mutation type maps to a function call",
		},
		{
			// Boolean! cannot tell a function that returned false from one that
			// returned nothing, so VOID is declared and the field type is pinned.
			name:    "VOID with a non-Boolean! result",
			sdl:     mutationSDL(`  f(a: String!): String! @function(schema: "s", name: "f", returns: VOID)`),
			wantErr: "must be `Boolean!`",
		},
		{
			name:    "set-returning declaration",
			sdl:     mutationSDL(`  f(a: String!): [String!]! @function(schema: "s", name: "f")`),
			wantErr: "set-returning function is not supported",
		},
		{
			name: "composite return",
			sdl: `
type Person @node(label: "person") { id: ID! name: String! }
type Mutation {
  f(a: String!): Person @function(schema: "s", name: "f")
}`,
			wantErr: "not one of the scalars gopgql maps",
		},
		{
			// `"agentDigest" => $1` does not match a parameter named
			// agent_digest, and the call fails at run time with 42883 saying
			// nothing about why. One @column(name:) fixes it; the error says so.
			name:    "camelCase argument with no @column(name:)",
			sdl:     mutationSDL(`  f(agentDigest: String!): String! @function(schema: "s", name: "f")`),
			wantErr: "@column(name:) naming the parameter the function declares",
		},
		{
			name:    "@column(type:) on an argument",
			sdl:     mutationSDL(`  f(a: String! @column(type: "text")): String! @function(schema: "s", name: "f")`),
			wantErr: "@column(type:) has no meaning on an argument",
		},
		{
			name:    "empty schema",
			sdl:     mutationSDL(`  f(a: String!): String! @function(schema: "", name: "f")`),
			wantErr: "@function(schema:) is empty",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.sdl)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// A document that declares only a command surface is as complete as one that
// declares only a graph: before mutations existed the check could only name
// @node, and it should not now refuse a schema that is entirely mutations.
func TestMutationOnlyDocumentParses(t *testing.T) {
	doc, err := Parse(`
type Mutation {
  sendMessage(destination: String!): Boolean! @function(schema: "dbos", name: "send_message", returns: VOID)
}`)
	require.NoError(t, err)
	assert.Empty(t, doc.Nodes)
	assert.Len(t, doc.Mutations, 1)
}

func TestSchemaWithNeitherNodesNorMutationsIsRejected(t *testing.T) {
	_, err := Parse(`type Plain { id: ID! }`)
	require.ErrorContains(t, err, "no `type ... @node(...)` definitions found")
}
