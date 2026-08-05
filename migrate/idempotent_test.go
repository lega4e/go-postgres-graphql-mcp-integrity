package migrate

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/generator"
)

// idempotencySchemas are the shapes a second `gopgql generate` has to be a
// no-op for. Each one exercises a clause the folder has to read back out of the
// migration it just wrote; a clause the emitter writes and the reader drops
// re-proposes a graph that is already there.
var idempotencySchemas = map[string]string{
	// Two labels over one table gopgql does not own. Every element carries an
	// alias, which is the clause that made the second generation exit 1 with
	// `ddl: expected "SOURCE KEY", got "AS"`.
	"two labels on one existing table": `
type Workflow @node(label: "workflow", table: "operations", schema: "dbos") @readonly
  @key(fields: ["workflowUuid"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  steps: [Step!]! @relationship(
    type: "HAS_STEP" direction: OUT table: "operation_outputs" schema: "dbos"
    sourceKey: ["workflow_uuid"] destKey: ["workflow_uuid", "function_id"]
  )
}
type Step @node(label: "step", table: "operation_outputs", schema: "dbos") @readonly
  @key(fields: ["workflowUuid", "functionId"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  functionId: Int! @column(name: "function_id")
  childWorkflows: [Workflow!]! @relationship(
    type: "SPAWNED" direction: OUT table: "operation_outputs" schema: "dbos"
    sourceKey: ["workflow_uuid", "function_id"] destKey: ["child_workflow_uuid"]
  )
}`,
	// One table serving as both a vertex and an edge — the M13 shape v0.2.0
	// shipped. Its @key is a KEY (...) clause on a table with no CREATE TABLE,
	// so the graph statement is the only place it is recorded.
	"one table as vertex and edge": `
type Workflow @node(label: "workflow", table: "workflows", schema: "dbos") @readonly
  @key(fields: ["workflowUuid"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  steps: [Step!]! @relationship(
    type: "has_step" direction: OUT table: "operation_outputs" schema: "dbos"
    sourceKey: ["workflow_uuid"] destKey: ["workflow_uuid", "function_id"]
  )
}
type Step @node(label: "step", table: "operation_outputs", schema: "dbos") @readonly
  @key(fields: ["workflowUuid", "functionId"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  functionId: Int! @column(name: "function_id")
}`,
	// Two same-named tables in two schemas, so both edges are aliased without
	// any vertex being involved.
	"same-named edge tables in two schemas": `
type AlphaNode @node(label: "alpha_node", table: "alpha_nodes", schema: "alpha") @readonly
  @key(fields: ["alphaId"]) {
  alphaId: ID! @column(name: "alpha_id")
  peers: [AlphaNode!]! @relationship(
    type: "ALPHA_LINK" direction: OUT table: "links" schema: "alpha"
    sourceKey: ["alpha_src"] destKey: ["alpha_dst"]
  )
}
type BetaNode @node(label: "beta_node", table: "beta_nodes", schema: "beta") @readonly
  @key(fields: ["betaId"]) {
  betaId: ID! @column(name: "beta_id")
  peers: [BetaNode!]! @relationship(
    type: "BETA_LINK" direction: OUT table: "links" schema: "beta"
    sourceKey: ["beta_src"] destKey: ["beta_dst"]
  )
}`,
	// The managed baseline, so "nothing was written" is asserted for a schema
	// gopgql owns end to end as well.
	"a schema gopgql owns": `
type Person @node(label: "person") @key(fields: ["email"]) {
  id: ID!
  email: String!
  name: String! @index
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}`,
}

// TestGenerateIsIdempotent is the gate `go generate ./... && git diff
// --exit-code` rests on. A second generation of an unchanged schema must write
// nothing — and before this it did not merely write a spurious delta, it exited
// 1, because gopgql could not re-read the migration it had just written.
func TestGenerateIsIdempotent(t *testing.T) {
	for name, src := range idempotencySchemas {
		t.Run(name, func(t *testing.T) {
			m := build(t, src)
			dir := t.TempDir()

			first, err := Generate(dir, m, "schema", Halves{})
			require.NoError(t, err)
			require.NotEmpty(t, first, "the first generation writes the schema")
			before := generated(t, dir)

			second, err := Generate(dir, m, "schema", Halves{})
			require.NoError(t, err, "gopgql must be able to re-read the migration it just wrote")
			assert.Empty(t, second, "a second generation of an unchanged schema writes nothing")
			assert.Equal(t, before, generated(t, dir), "the migration directory is unchanged")

			// A third pass catches a fold that only stabilises after one round.
			third, err := Generate(dir, m, "schema", Halves{})
			require.NoError(t, err)
			assert.Empty(t, third)
		})
	}
}

// TestFoldedGraphRendersIdentically is the reason the above holds, asserted
// directly: whatever GraphDDL writes, the reader has to give back a model that
// renders the same bytes. Plan compares exactly those two strings, so a clause
// the reader drops is a migration that re-proposes itself forever.
func TestFoldedGraphRendersIdentically(t *testing.T) {
	for name, src := range idempotencySchemas {
		t.Run(name, func(t *testing.T) {
			m := build(t, src)
			dir := t.TempDir()
			_, err := Generate(dir, m, "schema", Halves{})
			require.NoError(t, err)

			files, err := migrationFiles(dir)
			require.NoError(t, err)
			contents, err := readMigrations(files)
			require.NoError(t, err)
			folded, err := FoldContent(contents)
			require.NoError(t, err)

			assert.Equal(t, generator.GraphDDL(m), generator.GraphDDL(folded))
		})
	}
}

// A second generation must also leave the directory alone on disk, not merely
// return no paths — the two are the same thing today and a regression in
// either is the same broken CI step.
func TestSecondGenerationTouchesNoFile(t *testing.T) {
	m := build(t, idempotencySchemas["two labels on one existing table"])
	dir := t.TempDir()
	_, err := Generate(dir, m, "schema", Halves{})
	require.NoError(t, err)

	before, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, before, 1)
	stat, err := os.Stat(dir + "/" + before[0].Name())
	require.NoError(t, err)
	written := stat.ModTime()

	_, err = Generate(dir, m, "schema", Halves{})
	require.NoError(t, err)

	after, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, after, 1)
	stat, err = os.Stat(dir + "/" + after[0].Name())
	require.NoError(t, err)
	assert.Equal(t, written, stat.ModTime(), "the existing migration was rewritten")
}
