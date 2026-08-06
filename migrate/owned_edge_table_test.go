package migrate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// twoSchemaSDL is the shape gopgql#53 was found in: an application schema gopgql
// owns, joined to a schema it does not, with every join expressed as a
// foreign-key-mapped edge.
//
// It is not a contrived arrangement, it is the only one the SDL permits. An edge
// touching a @readonly type must be mapped onto an existing table with
// @relationship(table:, sourceKey:, destKey:) — sdl.validateRelationshipMapping
// refuses the alternative, because gopgql would otherwise create an edge table
// with a foreign key into a table it does not own. And the table such an edge is
// mapped onto is the child of the foreign key, which for a
// somebody-else's-parent → our-child join is *our* table.
//
// So the SDL below contains, deliberately, one of each of the four cases:
//
//	agentiq.session          owned vertex, and the HAS_SESSION edge's table
//	dbos.operation_outputs   unmanaged vertex, and the HAS_STEP edge's table
//	dbos.workflow_status     unmanaged vertex, no edge
//	agentiq.event, HAS_EVENT owned, untouched by any unmanaged edge
//
// The first two are the pair the fix has to tell apart, and they are the reason
// the drop set is split by role rather than keyed by table name.
const twoSchemaSDL = `
type Workflow @node(label: "workflow", table: "workflow_status", schema: "dbos") @readonly
  @key(fields: ["workflowUuid"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  sessions: [Session!]! @relationship(
    type: "HAS_SESSION" direction: OUT table: "session" schema: "agentiq"
    sourceKey: ["workflow_uuid"] destKey: ["id"]
  )
  steps: [Step!]! @relationship(
    type: "HAS_STEP" direction: OUT table: "operation_outputs" schema: "dbos"
    sourceKey: ["workflow_uuid"] destKey: ["workflow_uuid", "function_id"]
  )
}
type Step @node(label: "step", table: "operation_outputs", schema: "dbos") @readonly
  @key(fields: ["workflowUuid", "functionId"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  functionId: Int! @column(name: "function_id")
}
type Session @node(label: "session", table: "session", schema: "agentiq") {
  id: ID!
  workflowUuid: ID! @column(name: "workflow_uuid")
  events: [Event!]! @relationship(type: "HAS_EVENT", direction: OUT)
}
type Event @node(label: "event", table: "event", schema: "agentiq") {
  id: ID!
  kind: String!
}
`

// TestEveryOwnedTableIsCreated is the acceptance for gopgql#53 item A.
//
// It asserts on the *emitted DDL*, deliberately. The bug it covers exited 0 and
// wrote no files at all, so a test that checked the exit status — or even that a
// generation happened — passed against the broken generator: the graph half
// still changed, so files were still written, and only the table half was empty.
// What was wrong was the content, so the content is what is asserted.
func TestEveryOwnedTableIsCreated(t *testing.T) {
	dir := t.TempDir()
	paths, err := Generate(dir, build(t, twoSchemaSDL), "init", Halves{})
	require.NoError(t, err)
	require.NotEmpty(t, paths)

	out := generated(t, dir)
	for _, owned := range []string{
		"CREATE TABLE agentiq.session",
		"CREATE TABLE agentiq.event",
		`CREATE TABLE "HAS_EVENT"`,
	} {
		assert.Contains(t, out, owned,
			"every table gopgql owns is created, including one an unmanaged edge is mapped onto")
	}
}

// TestOwnedTableIsCreatedBeforeItIsReferenced is the half that makes the
// previous test more than a text search.
//
// The generated migration did not merely omit agentiq.session. It emitted
// "HAS_EVENT" with REFERENCES agentiq.session (id) — a foreign key into a table
// the same migration never creates. That does not apply; PostgreSQL rejects it.
// So the assertion is ordering, not presence: every table a foreign key names
// must be created earlier in the same Up section.
func TestOwnedTableIsCreatedBeforeItIsReferenced(t *testing.T) {
	dir := t.TempDir()
	_, err := Generate(dir, build(t, twoSchemaSDL), "init", Halves{})
	require.NoError(t, err)

	up := upSection(readFile(t, dir, "0001_init_tables.sql"))
	referenced := "agentiq.session (id)"
	require.Contains(t, up, referenced, "the edge table carries the foreign key this test is about")

	// Presence is required before the comparison, and not merely implied by it:
	// strings.Index returns -1 for a string that is not there, and -1 is less
	// than every real index. Comparing the two indexes alone would report the
	// missing CREATE TABLE as correct ordering — the exact bug, passing.
	created := strings.Index(up, "CREATE TABLE agentiq.session")
	require.NotEqual(t, -1, created, "the referenced table is created by this migration")
	assert.Less(t, created, strings.Index(up, referenced),
		"a foreign key must name a table the same migration has already created; "+
			"the migration gopgql#53 emitted referenced a table nothing created, and did not apply")
}

// TestATableOnlyAnUnmanagedEdgeUsesIsNeverCreated is the boundary the fix must
// not cross.
//
// dbos.operation_outputs is in the same position as agentiq.session in every
// respect but the one that matters: an unmanaged edge is mapped onto it, and it
// is also a vertex — but a @readonly one. Nothing about it may be emitted. A fix
// that widened "an unmanaged edge's table might be owned" into "an unmanaged
// edge's table is owned" would create a table belonging to somebody else, and it
// would do it silently, which is the failure shape of gopgql#49.
func TestATableOnlyAnUnmanagedEdgeUsesIsNeverCreated(t *testing.T) {
	dir := t.TempDir()
	_, err := Generate(dir, build(t, twoSchemaSDL), "init", Halves{})
	require.NoError(t, err)

	out := generated(t, dir)
	for _, forbidden := range []string{
		"CREATE TABLE dbos.operation_outputs",
		"ALTER TABLE dbos.operation_outputs",
		"DROP TABLE IF EXISTS dbos.operation_outputs",
		"CREATE TABLE dbos.workflow_status",
		"DROP TABLE IF EXISTS dbos.workflow_status",
		`CREATE INDEX "HAS_STEP`,
		`CREATE TABLE "HAS_STEP"`,
		`CREATE TABLE "HAS_SESSION"`,
	} {
		assert.NotContains(t, out, forbidden,
			"a table gopgql does not own gets no DDL, in either role")
	}
	// The graph still names all of them: surfaced but not owned is precisely the
	// two halves disagreeing.
	assert.Contains(t, out, "dbos.operation_outputs")
	assert.Contains(t, out, `LABEL "HAS_SESSION"`)
}

// A schema whose owned tables now generate must still generate nothing on the
// second run. The fix changes what is stripped from the *prior* side too, and a
// prior side that lost a table it should have kept re-proposes that table on
// every run — the failure the @readonly round trip exists to prevent.
func TestOwnedEdgeTableGenerationIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	m := build(t, twoSchemaSDL)

	first, err := Generate(dir, m, "init", Halves{})
	require.NoError(t, err)
	require.NotEmpty(t, first)
	before := generated(t, dir)

	second, err := Generate(dir, build(t, twoSchemaSDL), "again", Halves{})
	require.NoError(t, err)
	assert.Empty(t, second, "an unchanged schema plans nothing")
	assert.Equal(t, before, generated(t, dir))
}

// everyTableClaimedSDL is the worst case of item A, and the one the issue was
// filed from: *every* table gopgql owns is also some unmanaged edge's table, so
// the pre-fix drop set removed all of them and the tables half planned nothing
// at all. The report's "0 of 7" is this, at seven tables instead of two.
const everyTableClaimedSDL = `
type Workflow @node(label: "workflow", table: "workflow_status", schema: "dbos") @readonly
  @key(fields: ["workflowUuid"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  sessions: [Session!]! @relationship(
    type: "HAS_SESSION" direction: OUT table: "session" schema: "agentiq"
    sourceKey: ["workflow_uuid"] destKey: ["id"]
  )
  events: [Event!]! @relationship(
    type: "HAS_WF_EVENT" direction: OUT table: "event" schema: "agentiq"
    sourceKey: ["workflow_uuid"] destKey: ["id"]
  )
}
type Session @node(label: "session", table: "session", schema: "agentiq") {
  id: ID!
  workflowUuid: ID! @column(name: "workflow_uuid")
}
type Event @node(label: "event", table: "event", schema: "agentiq") {
  id: ID!
  kind: String!
}
`

// TestTheReportedInvocationGeneratesItsTables reproduces the command in
// gopgql#53 — `gopgql generate --sdl merged.graphql --dir migrations --no-graph`
// against a fresh directory — at the layer under the CLI.
//
// --no-graph matters and is not incidental: with the graph half turned off the
// tables half is the *only* half, so a differ that dropped every owned table
// left the whole plan empty. That is what produced "already up to date" and exit
// 0 on a directory holding no files, which is the report's opening line.
//
// The two assertions are the two halves of the acceptance, and neither is
// sufficient alone: that something was written (against the silent no-op), and
// that what was written creates every owned table (against a partial fix).
func TestTheReportedInvocationGeneratesItsTables(t *testing.T) {
	dir := t.TempDir()
	paths, err := Generate(dir, build(t, everyTableClaimedSDL), "init", Halves{NoGraph: true})
	require.NoError(t, err, "the reported invocation must not fail")
	require.NotEmpty(t, paths, "and must not silently write nothing, which is what it did")

	out := generated(t, dir)
	assert.Contains(t, out, "CREATE TABLE agentiq.session")
	assert.Contains(t, out, "CREATE TABLE agentiq.event")
	assert.NotContains(t, out, "dbos.workflow_status", "and still creates nothing it does not own")
}

// The same invocation, run again, is a genuine no-op — and the refusal that now
// guards no-ops must not fire on one.
//
// This is the pairing that makes ErrNothingWritten safe to add at all: a check
// that refuses an empty plan is only correct while an empty plan can be right,
// and here it is, on the second run of every schema that ever generated.
func TestTheReportedInvocationIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	_, err := Generate(dir, build(t, everyTableClaimedSDL), "init", Halves{NoGraph: true})
	require.NoError(t, err)

	again, err := Generate(dir, build(t, everyTableClaimedSDL), "again", Halves{NoGraph: true})
	require.NoError(t, err, "a directory that holds every owned table is genuinely up to date")
	assert.Empty(t, again)
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	all := generated(t, dir)
	_, after, found := strings.Cut(all, "-- "+name+"\n")
	require.True(t, found, "generation %s was not written", name)
	if before, _, more := strings.Cut(after, "\n-- 0"); more {
		return before
	}
	return after
}
