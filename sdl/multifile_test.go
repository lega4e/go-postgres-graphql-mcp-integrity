package sdl_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/sdl"
)

// The two files a consumer splits a schema into, and why (gopgql#54).
//
// AgentIQ owns `agentiq.*` and migrates it; `dbos.*` belongs to DBOS and is only
// read. That boundary is the most important fact about the repository, and it is
// visible in the file layout — but the relationship joining the two crosses it,
// and a property graph can only span two PostgreSQL schemas if one *schema*
// describes both. Parsing several documents as one schema is what lets the
// boundary stay in the files while the graph spans them.
const (
	readOnlySDL = `type Workflow @node(label: "workflow", table: "workflow_status", schema: "dbos") @readonly {
  id: ID! @column(name: "workflow_uuid")
  status: String!
  sessions: [Session!]! @relationship(type: "HAS_SESSION", direction: OUT,
                                      table: "session", schema: "agentiq",
                                      sourceKey: ["workflow_uuid"], destKey: ["id"])
}`

	ownedSDL = `type Session @node(label: "session", table: "session", schema: "agentiq") {
  id: ID!
  transcript: JSON
}`
)

// TestParseSourcesSpansTwoPostgresSchemas is the requirement the repeatable flag
// exists for: one property graph over a read-only schema and an owned one,
// declared in separate files.
func TestParseSourcesSpansTwoPostgresSchemas(t *testing.T) {
	doc, err := sdl.ParseSources(
		sdl.Source{Name: "schema/00-dbos.graphql", Input: readOnlySDL},
		sdl.Source{Name: "schema/10-agentiq.graphql", Input: ownedSDL},
	)
	require.NoError(t, err)

	workflow := doc.NodeByType("Workflow")
	require.NotNil(t, workflow, "the read-only file's type")
	session := doc.NodeByType("Session")
	require.NotNil(t, session, "the owned file's type")

	assert.Equal(t, "dbos", workflow.Schema)
	assert.Equal(t, "agentiq", session.Schema)
	assert.True(t, workflow.ReadOnly, "@readOnly survives the split")

	// The edge is the whole point: it is declared in one file and resolves to a
	// type declared in the other.
	var rel *sdl.Relationship
	for _, f := range workflow.Fields {
		if f.Name == "sessions" {
			rel = f.Rel
		}
	}
	require.NotNil(t, rel, "the relationship crossing the file boundary")
	assert.Equal(t, "HAS_SESSION", rel.Type)
}

// TestParseSourcesEqualsParsingTheConcatenation states the property as a
// property: splitting a schema across files is an editorial decision and must
// change nothing about the model it produces.
func TestParseSourcesEqualsParsingTheConcatenation(t *testing.T) {
	split, err := sdl.ParseSources(
		sdl.Source{Name: "a.graphql", Input: readOnlySDL},
		sdl.Source{Name: "b.graphql", Input: ownedSDL},
	)
	require.NoError(t, err)

	joined, err := sdl.Parse(readOnlySDL + "\n" + ownedSDL + "\n")
	require.NoError(t, err)

	require.Len(t, split.Nodes, len(joined.Nodes))
	for i := range split.Nodes {
		a, b := split.Nodes[i], joined.Nodes[i]
		assert.Equal(t, b.TypeName, a.TypeName, "the model is sorted by type name, not by file")
		assert.Equal(t, b.QualifiedTable(), a.QualifiedTable())
		assert.Equal(t, b.Label, a.Label)
		assert.Equal(t, len(b.Fields), len(a.Fields))
	}
}

// TestParseSourcesReversedOrderIsTheSameSchema: the files are merged before
// anything is resolved, so a type may be referenced before the file declaring it
// is read. Otherwise the split would have to follow the dependency order rather
// than the ownership boundary — and there is no order that works for a cycle.
func TestParseSourcesReversedOrderIsTheSameSchema(t *testing.T) {
	forward, err := sdl.ParseSources(
		sdl.Source{Name: "a.graphql", Input: readOnlySDL},
		sdl.Source{Name: "b.graphql", Input: ownedSDL},
	)
	require.NoError(t, err)
	backward, err := sdl.ParseSources(
		sdl.Source{Name: "b.graphql", Input: ownedSDL},
		sdl.Source{Name: "a.graphql", Input: readOnlySDL},
	)
	require.NoError(t, err)

	require.Len(t, backward.Nodes, len(forward.Nodes))
	for i := range forward.Nodes {
		assert.Equal(t, forward.Nodes[i].TypeName, backward.Nodes[i].TypeName)
	}
}

// TestParseSourcesNamesTheFileThatIsWrong: the reason the documents are kept
// apart rather than concatenated before parsing. A merged buffer's line numbers
// match nothing on disk.
func TestParseSourcesNamesTheFileThatIsWrong(t *testing.T) {
	_, err := sdl.ParseSources(
		sdl.Source{Name: "schema/00-dbos.graphql", Input: readOnlySDL},
		sdl.Source{Name: "schema/10-agentiq.graphql", Input: `type Session @node(label: "session") {
  id: ID!
  transcript: Bytes
}`},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema/10-agentiq.graphql",
		"the error has to name the file the author opens")
	assert.NotContains(t, err.Error(), "schema/00-dbos.graphql")
	// The scalar guidance survives the multi-document path.
	assert.Contains(t, err.Error(), `@column(type: "bytea")`)
}

// TestParseSourcesRefusesNothing: parsing no documents is a caller bug, and an
// empty schema would otherwise fail as "no @node definitions found" — true, and
// about the wrong thing.
func TestParseSourcesRefusesNothing(t *testing.T) {
	_, err := sdl.ParseSources()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no schema documents")
}

// TestParseIsParseSourcesOfOne pins the single-document path to the same code,
// so the two cannot drift.
func TestParseIsParseSourcesOfOne(t *testing.T) {
	_, err := sdl.Parse(`type Person @node(label: "person") { id: ID! bad: Bytes }`)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), sdl.DefaultSourceName),
		"an anonymous document is still named in diagnostics")
}
