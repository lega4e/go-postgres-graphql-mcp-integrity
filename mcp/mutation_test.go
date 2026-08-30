package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/sdl"
)

const mutationSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
}

type Mutation {
  startAgentRun(agentDigest: String! @column(name: "agent_digest")): String!
    @function(schema: "dbos", name: "enqueue_workflow")
}
`

// The MCP server reports a null mutationType even for a schema that declares a
// Mutation type, and that is deliberate: the server holds a read-only pool it
// opened itself and no handle from a caller, so it could not run one.
// Advertising a mutation an agent cannot call would be worse than omitting it.
//
// The omission was previously a consequence of gopgql having no mutations at
// all. Now that it does, it is a decision — and a decision that stops being
// enforced silently is the failure mode this test exists to prevent.
func TestMutationTypeStaysNullForASchemaWithMutations(t *testing.T) {
	doc, err := sdl.Parse(mutationSDL)
	require.NoError(t, err)
	require.Len(t, doc.Mutations, 1, "the fixture must really declare a mutation")

	s, err := New(doc, mutationSDL, nil)
	require.NoError(t, err)
	out := introspect(t, s, `{ __schema { mutationType { name } queryType { name } } }`)

	schema, ok := out["__schema"].(map[string]any)
	require.True(t, ok, "introspection must return a __schema object")
	assert.Nil(t, schema["mutationType"])
	assert.NotNil(t, schema["queryType"])
}

// The query tool refuses a mutation operation, and its reason no longer claims
// the graph's read-only-ness — which was never why a *function call* could not
// be run here.
func TestQueryToolRefusesAMutationOperation(t *testing.T) {
	doc, err := sdl.Parse(mutationSDL)
	require.NoError(t, err)
	s, err := New(doc, mutationSDL, nil)
	require.NoError(t, err)

	_, err = s.Query(context.Background(), `mutation { startAgentRun(agentDigest: "a") }`, nil, FormatJSON)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only query operations are supported by this server")
}
