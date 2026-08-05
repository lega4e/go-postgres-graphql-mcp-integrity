package generator

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/sdl"
)

// twoLabelsSDL is the shape M13 was built for and did not survive: one table
// gopgql does not own, carrying two relationship labels. A row of
// dbos.operation_outputs is a step of the workflow that ran it (HAS_STEP) and,
// when that step started a child, the edge to the workflow it spawned
// (SPAWNED).
const twoLabelsSDL = `
type Workflow @node(label: "workflow", table: "operations", schema: "dbos") @readonly
  @key(fields: ["workflowUuid"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  steps: [Step!]! @relationship(
    type: "HAS_STEP"
    direction: OUT
    table: "operation_outputs"
    schema: "dbos"
    sourceKey: ["workflow_uuid"]
    destKey: ["workflow_uuid", "function_id"]
  )
}
type Step @node(label: "step", table: "operation_outputs", schema: "dbos") @readonly
  @key(fields: ["workflowUuid", "functionId"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  functionId: Int! @column(name: "function_id")
  childWorkflows: [Workflow!]! @relationship(
    type: "SPAWNED"
    direction: OUT
    table: "operation_outputs"
    schema: "dbos"
    sourceKey: ["workflow_uuid", "function_id"]
    destKey: ["child_workflow_uuid"]
  )
}
`

// TestTwoLabelsOnOneExistingTable is the regression for the collapse. The
// dedup key was the physical table name, which for a table gopgql creates is
// also the label — so every existing test passed while an edge mapped onto an
// existing table lost its second label with exit status 0.
//
// It asserts the emitted DDL, not the exit status: a generator that silently
// drops an edge still succeeds, and that is precisely what happened.
func TestTwoLabelsOnOneExistingTable(t *testing.T) {
	doc, err := sdl.Parse(twoLabelsSDL)
	require.NoError(t, err)
	m, err := Build(doc, "")
	require.NoError(t, err)

	require.Len(t, m.EdgeTables, 2, "one table carrying two labels is two edge elements")

	graph := GraphDDL(m)
	assert.Contains(t, graph, `dbos.operation_outputs AS "HAS_STEP" SOURCE KEY (workflow_uuid) `+
		`REFERENCES operations (workflow_uuid)`)
	assert.Contains(t, graph, `dbos.operation_outputs AS "SPAWNED" SOURCE KEY (workflow_uuid, function_id) `+
		`REFERENCES operation_outputs (workflow_uuid, function_id)`)
	assert.Contains(t, graph, `LABEL "HAS_STEP" PROPERTIES (workflow_uuid, function_id)`)
	assert.Contains(t, graph, `LABEL "SPAWNED" PROPERTIES (workflow_uuid, function_id, child_workflow_uuid)`)

	// Neither label depends on which type happened to be visited first.
	assert.Equal(t, "HAS_STEP", m.EdgeTables[0].Label)
	assert.Equal(t, "SPAWNED", m.EdgeTables[1].Label)

	// gopgql owns neither, so nothing is created for either of them.
	assert.NotContains(t, TablesDDL(m), "operation_outputs")
}

// TestSameTableNameInTwoSchemasIsTwoEdges covers the other half of the key: it
// ignored the schema too, so two tools each owning a `links` table produced one
// edge between them.
func TestSameTableNameInTwoSchemasIsTwoEdges(t *testing.T) {
	doc, err := sdl.Parse(`
type AlphaNode @node(label: "alpha_node", table: "alpha_nodes", schema: "alpha") @readonly
  @key(fields: ["alphaId"]) {
  alphaId: ID! @column(name: "alpha_id")
  peers: [AlphaNode!]! @relationship(
    type: "ALPHA_LINK"
    direction: OUT
    table: "links"
    schema: "alpha"
    sourceKey: ["alpha_src"]
    destKey: ["alpha_dst"]
  )
}
type BetaNode @node(label: "beta_node", table: "beta_nodes", schema: "beta") @readonly
  @key(fields: ["betaId"]) {
  betaId: ID! @column(name: "beta_id")
  peers: [BetaNode!]! @relationship(
    type: "BETA_LINK"
    direction: OUT
    table: "links"
    schema: "beta"
    sourceKey: ["beta_src"]
    destKey: ["beta_dst"]
  )
}`)
	require.NoError(t, err)
	m, err := Build(doc, "")
	require.NoError(t, err)

	require.Len(t, m.EdgeTables, 2, "two tables of one name in two schemas are two edges")

	graph := GraphDDL(m)
	assert.Contains(t, graph, `alpha.links AS "ALPHA_LINK" SOURCE KEY (alpha_src)`)
	assert.Contains(t, graph, `beta.links AS "BETA_LINK" SOURCE KEY (beta_src)`)
	// An element alias is unqualified, so both had to be given one; neither may
	// be left as the bare `links` they would otherwise collide on.
	assert.Equal(t, 2, strings.Count(graph, "links AS "))
}

// An edge over a table gopgql *creates* is created once, so two labels asking
// for the same table are two CREATE TABLEs for one name. Keying edges on the
// label rather than the table made that expressible for the first time, and it
// is refused here rather than reaching PostgreSQL as a duplicate relation
// halfway through a migration.
func TestTwoManagedLabelsOnOneTableIsRefused(t *testing.T) {
	doc, err := sdl.Parse(`
type Person @node(label: "person") {
  id: ID!
  follows: [Person!]! @relationship(type: "follows", direction: OUT, table: "links")
  blocks: [Person!]! @relationship(type: "blocks", direction: OUT, table: "links")
}`)
	require.NoError(t, err, "the SDL is well-formed; the clash is physical")

	_, err = Build(doc, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both create the edge table links")
	assert.Contains(t, err.Error(), "already exists")
}

// The alias exists to keep element names unique, so an edge only carries one
// where its bare table name is actually contested. A schema whose edges each
// have a table to themselves emits exactly what it did before M13.
func TestUncontestedEdgeCarriesNoAlias(t *testing.T) {
	doc, err := sdl.Parse(`
type Person @node(label: "person") {
  id: ID!
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}`)
	require.NoError(t, err)
	m, err := Build(doc, "")
	require.NoError(t, err)

	require.Len(t, m.EdgeTables, 1)
	assert.Empty(t, m.EdgeTables[0].Alias)
	assert.Contains(t, GraphDDL(m), "follows SOURCE KEY (source_id)")
	assert.NotContains(t, GraphDDL(m), " AS ")
}
