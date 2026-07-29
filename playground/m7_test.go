// The M7 panels: the constraint directives in the generated DDL, the rename
// hint in a delta, and the conformance report.
//
// The first two go through the same entry points the WASM module exports, so
// what the page shows is asserted here rather than trusted. The third cannot:
// conform needs a live database and so sits on the pgx side of the WASM
// boundary (SPEC.md §4.1), which is exactly why the page shows a fixture. What
// is asserted about the fixture is that it is not fiction — every name in it
// lines up with the schema it claims to have been recorded against.
package playground_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/playground"
)

// TestConstraintsExampleDDL proves the Constraints panel's SDL really generates
// the M7 constraint surface, and — the part most easily got wrong — that the
// natural key arrived *alongside* the surrogate id rather than in place of it
// (design D1).
func TestConstraintsExampleDDL(t *testing.T) {
	ddl, err := playground.Schema(playground.ExampleConstraintsSDL)
	require.NoError(t, err)

	for _, want := range []string{
		// The surrogate key is untouched: it is still the physical identity,
		// and still what the edge table references.
		"id uuid PRIMARY KEY DEFAULT gen_random_uuid()",
		// @default, emitted verbatim.
		"role text NOT NULL DEFAULT 'member'",
		"joined_at timestamptz NOT NULL DEFAULT now()",
		// @key: a named UNIQUE over two existing columns.
		"CONSTRAINT employees_key UNIQUE (tenant, email)",
		// @check, column-level and table-level, each named so a later delta
		// can drop it without asking the database what name it invented.
		"CONSTRAINT employees_role_check CHECK (role IN ('member', 'admin'))",
		"CONSTRAINT employees_check_1 CHECK (left_at IS NULL OR left_at >= joined_at)",
		// The key's columns are listed in the graph, which is what makes them
		// selectable from a MATCH.
		"employees KEY (tenant, email) LABEL employee",
		"REFERENCES employees (id)",
	} {
		assert.Contains(t, ddl, want)
	}
}

// TestConstraintsExampleQuery proves the milestone's exit criterion through the
// playground: a vertex carrying a natural key is matchable by it. Both key
// columns become predicates inside the MATCH, bound as parameters.
func TestConstraintsExampleQuery(t *testing.T) {
	out, err := playground.Compile(
		playground.ExampleConstraintsSDL, playground.ExampleConstraintsQuery,
		map[string]any{"t": "acme", "e": "ada@acme.example"})
	require.NoError(t, err)

	assert.Contains(t, out.SQL, "WHERE v0.tenant = $1 AND v0.email = $2")
	assert.Contains(t, out.SQL, "v0.joined_at AS")
	assert.Equal(t, "$1 = acme, $2 = ada@acme.example", out.Params)
}

// TestRenameDeltaMovesTheColumn proves the Constraints panel's second scenario:
// with the hint, the delta *moves* the column; without it, the same edit would
// be a drop and an add. Asserting the absence of those two statements is the
// substance of the panel — a RENAME that arrived next to a DROP COLUMN would
// still lose the data.
func TestRenameDeltaMovesTheColumn(t *testing.T) {
	delta, changed, err := playground.Delta(
		playground.ExampleConstraintsSDL, playground.RevisedConstraintsSDL)
	require.NoError(t, err)
	require.True(t, changed)

	// The generation is three files; the rename is in the tables one, between
	// the graph teardown and the rebuild.
	up, down, ok := strings.Cut(tablesMigration(t, delta), "-- +goose Down")
	require.True(t, ok, "the tables migration must carry a Down section")

	assert.Contains(t, up, "ALTER TABLE employees RENAME COLUMN email TO work_email;")
	assert.NotContains(t, delta, "DROP COLUMN")
	assert.NotContains(t, delta, "ADD COLUMN")
	// The natural key's constraint name omits its columns, and the rename is
	// applied to the prior state before the diff runs, so a key column moving
	// is not constraint churn: the UNIQUE is neither dropped nor re-added.
	assert.NotContains(t, delta, "DROP CONSTRAINT")
	assert.NotContains(t, delta, "ADD CONSTRAINT")
	// Down is the exact inverse.
	assert.Contains(t, down, "ALTER TABLE employees RENAME COLUMN work_email TO email;")

	// Without the hint the differ has nothing to go on, and the same edit is a
	// drop and an add — which is what makes the hint load-bearing rather than
	// decorative.
	unhinted := strings.Replace(playground.RevisedConstraintsSDL, ` @renamedFrom(name: "email")`, "", 1)
	require.NotEqual(t, playground.RevisedConstraintsSDL, unhinted, "the hint must be present to be removed")
	bare, _, err := playground.Delta(playground.ExampleConstraintsSDL, unhinted)
	require.NoError(t, err)
	assert.Contains(t, bare, "DROP COLUMN")
	assert.NotContains(t, bare, "RENAME COLUMN")
}

// TestGraphMappingIsOnlyTheGraph proves the pane the Conformance panel puts
// beside its fixture shows only what the check can see. Everything the same SDL
// declares about the tables is absent from the mapping, and is equally absent
// from a conformance report.
func TestGraphMappingIsOnlyTheGraph(t *testing.T) {
	graph, err := playground.GraphMapping(playground.ExampleConstraintsSDL)
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(graph, "CREATE PROPERTY GRAPH app_graph"))
	assert.Contains(t, graph, "employees KEY (tenant, email) LABEL employee")
	for _, absent := range []string{"CREATE TABLE", "CONSTRAINT", "DEFAULT", "CREATE INDEX"} {
		assert.NotContains(t, graph, absent,
			"the graph mapping is the whole of what conform compares; %q is not part of it", absent)
	}

	_, err = playground.GraphMapping(`type Person { id: ID! }`)
	assert.Error(t, err, "an SDL without @node must not silently produce an empty graph")
}

// findingRow matches one row of the fixture report: five whitespace-separated
// columns, the first of which is a finding kind.
var findingRow = regexp.MustCompile(
	`^(MissingElement|UnexpectedElement|MissingProperty|UnexpectedProperty|LabelMismatch)\s+(\S+)\s+(\S+)\s+(\S+)\s+(\S+)$`)

// TestConformanceReportIsNotFiction is what keeps a fixture from rotting into a
// made-up screenshot.
//
// The report claims to have been recorded against ExampleConstraintsSDL, so
// every name it attributes to the SDL side must actually be in the graph that
// SDL generates, and every name it attributes only to the database must not be.
// A finding naming an element the schema never had would teach a reader a
// vocabulary that does not exist.
func TestConformanceReportIsNotFiction(t *testing.T) {
	graph, err := playground.GraphMapping(playground.ExampleConstraintsSDL)
	require.NoError(t, err)

	kinds := map[string]bool{}
	rows := 0
	for _, line := range strings.Split(playground.ExampleConformanceReport, "\n") {
		m := findingRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		rows++
		kind, element, property, want, got := m[1], m[2], m[3], m[4], m[5]
		kinds[kind] = true

		// Every name the report attributes to the SDL must be in the generated
		// graph, and every name it attributes only to the database must not be.
		declared, undeclared := []string{}, []string{}
		switch kind {
		case "MissingElement":
			declared = append(declared, element, want)
		case "UnexpectedElement":
			undeclared = append(undeclared, element, got)
		case "MissingProperty":
			declared = append(declared, element, property, want)
		case "UnexpectedProperty":
			declared = append(declared, element)
			undeclared = append(undeclared, property, got)
		case "LabelMismatch":
			declared = append(declared, element, want)
			undeclared = append(undeclared, got)
		}
		for _, name := range declared {
			if name == "-" {
				continue
			}
			assert.Contains(t, graph, name,
				"%s names %q on the SDL side, but the generated graph has no such name", kind, name)
		}
		for _, name := range undeclared {
			if name == "-" {
				continue
			}
			assert.NotContains(t, graph, name,
				"%s names %q as the database's alone, but the SDL declares it too", kind, name)
		}
	}

	require.Equal(t, 5, rows, "the fixture documents the vocabulary; keep one row per kind")
	assert.Len(t, kinds, 5, "every finding kind must appear exactly once")

	// The parts a reader will copy: the exit status that means drift, and the
	// caveat that the check is about the graph and nothing else.
	assert.Contains(t, playground.ExampleConformanceReport, "exit 2")
	assert.Contains(t, playground.ExampleConformanceReport,
		"compared elements, labels and properties only; defaults, constraints and indexes are not covered.")
}

// tablesMigration picks the tables migration out of a rendered generation. The
// playground shows the whole run of files in one pane, and only one of them ever
// carries table DDL (gopgql#38, design D2).
func tablesMigration(t *testing.T, rendered string) string {
	t.Helper()
	for _, block := range strings.Split(rendered, "-- migrations/") {
		name, _, ok := strings.Cut(block, "\n")
		if ok && strings.HasSuffix(name, "_tables.sql") {
			return block
		}
	}
	require.FailNow(t, "no tables migration in:\n"+rendered)
	return ""
}
