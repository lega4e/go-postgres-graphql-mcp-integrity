package playground_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/playground"
)

func TestMigration(t *testing.T) {
	mig, err := playground.Migration(playground.ExampleSDL)
	require.NoError(t, err, "Migration")

	// One directory, consecutive single-purpose files, each labelled with the
	// name gopgql writes it under (gopgql#38).
	for _, want := range []string{
		"-- migrations/0001_init_tables.sql",
		"-- migrations/0002_init_graph.sql",
		"CREATE PROPERTY GRAPH app_graph",
	} {
		assert.Contains(t, mig, want)
	}
	assert.Equal(t, 2, strings.Count(mig, "-- +goose Up\n"), "expected two goose files:\n%s", mig)
	assert.NotContains(t, mig, "migrations/tables/", "there are no per-half subdirectories any more")

	// Neither file carries the other's statements.
	tables, graph, ok := strings.Cut(mig, "-- migrations/0002_init_graph.sql")
	require.True(t, ok, "could not split the sequence:\n%s", mig)
	assert.NotContains(t, tables, "CREATE PROPERTY GRAPH",
		"the tables migration must not create the property graph")
	assert.NotContains(t, graph, "CREATE TABLE",
		"the graph migration must not create tables")
}

// TestSchema covers the DDL each scenario shows beside its compiled query: the
// same model Migration renders, without the goose framing.
func TestSchema(t *testing.T) {
	ddl, err := playground.Schema(playground.ExampleSDL)
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	if strings.Contains(ddl, "goose") {
		t.Errorf("Schema must not carry goose framing:\n%s", ddl)
	}
	for _, want := range []string{"CREATE TABLE persons (", "CREATE PROPERTY GRAPH app_graph"} {
		if !strings.Contains(ddl, want) {
			t.Errorf("expected schema DDL to contain %q:\n%s", want, ddl)
		}
	}
	if _, err := playground.Schema(`type Person { id: ID! }`); err == nil {
		t.Error("expected error for SDL without @node")
	}
}

func TestMigrationError(t *testing.T) {
	if _, err := playground.Migration(`type Person { id: ID! }`); err == nil {
		t.Error("expected error for SDL without @node")
	}
}

func TestCompileNestedWithVariable(t *testing.T) {
	vars := map[string]any{"n": "Alice"}
	out, err := playground.Compile(playground.ExampleSDL, playground.ExampleQuery, vars)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// A three-hop MATCH chain with a bound predicate and the isomorphism
	// guards that keep the path from walking back over itself.
	for _, want := range []string{"-[e0 IS follows]->", "-[e1 IS follows]->", "-[e2 IS follows]->"} {
		if !strings.Contains(out.SQL, want) {
			t.Errorf("compiled SQL must contain %q:\n%s", want, out.SQL)
		}
	}
	if !strings.Contains(out.SQL, "WHERE v0.name = $1") {
		t.Errorf("compiled SQL must bind the variable as $1:\n%s", out.SQL)
	}
	if !strings.Contains(out.SQL, "v0.id <> v3.id") {
		t.Errorf("compiled SQL must guard against a path returning to its origin:\n%s", out.SQL)
	}
	if !strings.Contains(out.Params, "$1 = Alice") {
		t.Errorf("params must report the ordered bind value, got %q", out.Params)
	}
}

// TestCompileDepthExceeded proves the playground can tell a depth rejection from
// any other compile error, which is what lets the page present it as the
// designed outcome (SPEC.md §10).
func TestCompileDepthExceeded(t *testing.T) {
	vars := map[string]any{"n": "Alice"}
	_, err := playground.Compile(playground.ExampleSDL, playground.ExampleDeepQuery, vars)
	if err == nil {
		t.Fatal("expected the deep example query to be rejected")
	}
	limit, ok := playground.DepthExceeded(err)
	if !ok {
		t.Fatalf("DepthExceeded did not classify %v as a depth rejection", err)
	}
	if limit != playground.MaxDepth() {
		t.Errorf("reported MaxDepth = %d, want %d", limit, playground.MaxDepth())
	}

	// Any other compile error must not be misreported as a depth rejection.
	_, err = playground.Compile(playground.ExampleSDL, `{ persons { bogus } }`, nil)
	if _, ok := playground.DepthExceeded(err); ok {
		t.Errorf("an unknown-field error must not classify as a depth rejection: %v", err)
	}
}

// TestCompileWithMaxDepth proves the ceiling is configuration, not a constant:
// the same query the playground shows being refused compiles once the limit is
// raised, and a shorter one is refused once it is lowered.
func TestCompileWithMaxDepth(t *testing.T) {
	vars := map[string]any{"n": "Alice"}

	out, err := playground.CompileWithMaxDepth(
		playground.ExampleSDL, playground.ExampleDeepQuery, vars, 4)
	if err != nil {
		t.Fatalf("four hops at MaxDepth 4: %v", err)
	}
	if !strings.Contains(out.SQL, "-[e3 IS follows]->") {
		t.Errorf("expected a fourth hop in the compiled SQL:\n%s", out.SQL)
	}

	_, err = playground.CompileWithMaxDepth(
		playground.ExampleSDL, `{ persons { follows { name } } }`, nil, 0)
	if limit, ok := playground.DepthExceeded(err); !ok || limit != 0 {
		t.Errorf("one hop at MaxDepth 0: got (%d, %v) from %v, want a rejection at 0", limit, ok, err)
	}

	// Negative ceilings clamp to zero rather than erroring: no traversal, but a
	// plain root selection still compiles.
	if _, err := playground.CompileWithMaxDepth(
		playground.ExampleSDL, `{ persons { name } }`, nil, -2); err != nil {
		t.Errorf("root-only query at a negative MaxDepth: %v", err)
	}
}

// TestCompileInterfaceExamples proves the interface panel's inputs really
// compile: one shared label spanning two tables, and label alternation.
func TestCompileInterfaceExamples(t *testing.T) {
	out, err := playground.Compile(playground.ExampleInterfaceSDL, playground.ExampleInterfaceQuery, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !strings.Contains(out.SQL, "(v0 IS actor)") {
		t.Errorf("shared-label interface must compile to one label:\n%s", out.SQL)
	}
	if !strings.Contains(out.SQL, "v0.id <> v1.id") {
		t.Errorf("an actor may be the person it follows; a guard is required:\n%s", out.SQL)
	}

	alt, err := playground.Compile(playground.ExampleInterfaceSDL, `{ profiles { name } }`, nil)
	if err != nil {
		t.Fatalf("Compile alternation: %v", err)
	}
	if !strings.Contains(alt.SQL, "(v0 IS bot|person)") {
		t.Errorf("an unlabelled interface must compile to label alternation:\n%s", alt.SQL)
	}

	mig, err := playground.Migration(playground.ExampleInterfaceSDL)
	if err != nil {
		t.Fatalf("Migration: %v", err)
	}
	if n := strings.Count(mig, "LABEL actor PROPERTIES (id, name)"); n != 2 {
		t.Errorf("the shared label must appear on both vertex tables, got %d:\n%s", n, mig)
	}
}

func TestCompileNoParams(t *testing.T) {
	out, err := playground.Compile(playground.ExampleSDL, `{ persons { name } }`, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if strings.Contains(out.Params, "$") {
		t.Errorf("expected no bind params, got %q", out.Params)
	}
}

func TestCompileQueryError(t *testing.T) {
	if _, err := playground.Compile(playground.ExampleSDL, `{ persons { bogus } }`, nil); err == nil {
		t.Error("expected error for unknown field")
	}
}

func TestCompileMissingVariable(t *testing.T) {
	// The query references $n but no value is supplied.
	if _, err := playground.Compile(playground.ExampleSDL, playground.ExampleQuery, nil); err == nil {
		t.Error("expected error for a missing variable value")
	}
}

func TestDeltaAddsColumn(t *testing.T) {
	delta, changed, err := playground.Delta(playground.ExampleSDL, playground.RevisedExampleSDL)
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if !changed {
		t.Fatal("adding a field must produce a delta")
	}
	if !strings.Contains(delta, "ALTER TABLE persons ADD COLUMN age integer;") {
		t.Errorf("delta must add the new column:\n%s", delta)
	}
	if !strings.Contains(delta, "CREATE PROPERTY GRAPH app_graph") {
		t.Errorf("delta must recreate the property graph:\n%s", delta)
	}
}

func TestDeltaNoChange(t *testing.T) {
	_, changed, err := playground.Delta(playground.ExampleSDL, playground.ExampleSDL)
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if changed {
		t.Error("identical SDL revisions must not produce a delta")
	}
}

func TestDeltaError(t *testing.T) {
	if _, _, err := playground.Delta(`type Person { id: ID! }`, playground.RevisedExampleSDL); err == nil {
		t.Error("expected error for invalid prior SDL")
	}
}

// TestCompileMultiPatternExample proves the Multi-pattern panel's input really
// compiles to the M5 workaround through the same entry point the WASM module
// exports: separate GRAPH_TABLE calls joined on projected ids, and no
// comma-separated pattern anywhere (SPEC.md §7 → M5).
func TestCompileMultiPatternExample(t *testing.T) {
	out, err := playground.Compile(
		playground.ExampleSDL, playground.ExampleMultiPatternQuery,
		map[string]any{"n": "Alice"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if n := strings.Count(out.SQL, "GRAPH_TABLE"); n != 3 {
		t.Errorf("got %d GRAPH_TABLE calls, want 3 (spine + two branches):\n%s", n, out.SQL)
	}
	if !strings.Contains(out.SQL, "LEFT JOIN") {
		t.Errorf("the branches must be joined back together:\n%s", out.SQL)
	}
	for _, line := range strings.Split(out.SQL, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "MATCH ") && strings.Contains(trimmed, ",") {
			t.Errorf("emitted a comma-separated pattern PG19 will not execute:\n%s", line)
		}
	}
	if !strings.Contains(out.Params, "$1 = Alice") {
		t.Errorf("Params = %q, want the root filter bound as $1", out.Params)
	}
}

// TestDirectivesExample proves the Directives panel's inputs really produce the
// M6 DDL through the same entry point the WASM module exports, and that the
// compiled query reads the renamed column (SPEC.md §7 → M6).
func TestDirectivesExample(t *testing.T) {
	ddl, err := playground.Schema(playground.ExampleDirectivesSDL)
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	for _, want := range []string{
		"sku text NOT NULL UNIQUE",
		"price numeric(10,2) NOT NULL",
		"CREATE INDEX products_category_idx ON products USING btree (category);",
		"CREATE INDEX products_vendor_idx ON products (vendor);",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("generated DDL is missing %q:\n%s", want, ddl)
		}
	}

	out, err := playground.Compile(
		playground.ExampleDirectivesSDL, playground.ExampleDirectivesQuery,
		map[string]any{"t": "Chain"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !strings.Contains(out.SQL, "v0.name AS") {
		t.Errorf("the compiled query must project the renamed column:\n%s", out.SQL)
	}
	if strings.Contains(out.SQL, "v0.title") {
		t.Errorf("the compiled query must not use the GraphQL field name as a column:\n%s", out.SQL)
	}
}

// --- M8: the shaping toggle ---

// TestCompileWithShapingEmitsTwoDifferentStatements is task 8.6. The toggle's
// whole claim is that the two strategies compile the same query to different
// SQL, so a toggle that silently rendered the same text twice would be a
// decoration. This fails if that ever becomes true.
func TestCompileWithShapingEmitsTwoDifferentStatements(t *testing.T) {
	vars := map[string]any{"n": "Alice"}

	goSide, err := playground.CompileWithShaping(
		playground.ExampleSDL, playground.ExampleShapingQuery, vars, false)
	require.NoError(t, err, "the example query must compile under Go-side shaping")

	sqlSide, err := playground.CompileWithShaping(
		playground.ExampleSDL, playground.ExampleShapingQuery, vars, true)
	require.NoError(t, err, "the example query must compile under SQL-side shaping")

	assert.Equal(t, "go-side", goSide.Strategy)
	assert.Equal(t, "sql-side", sqlSide.Strategy)
	assert.NotEqual(t, goSide.SQL, sqlSide.SQL,
		"the toggle would be showing the same statement under both labels")

	assert.Contains(t, sqlSide.SQL, "json_agg", "the SQL-side statement aggregates in-database")
	assert.NotContains(t, goSide.SQL, "json_agg", "the Go-side statement projects flat columns")
	assert.NotContains(t, sqlSide.SQL, "jsonb_build_object",
		"jsonb sorts keys by length-then-bytes and drops duplicates; the strategy uses json")

	// The values travel as parameters under both, and the same ones: only the
	// projection differs between the strategies, never the pattern.
	assert.Equal(t, goSide.Params, sqlSide.Params)
	assert.Contains(t, goSide.Params, "$1 = Alice")
	assert.NotContains(t, sqlSide.SQL, "'Alice'", "a filter value is bound, never interpolated")
}

// TestShapingResultShapeIsDerived checks the one thing the panel computes rather
// than shows: the result set each strategy asks the database for. It is the
// point of the whole milestone in a line, and it is derived from the projection
// with no database consulted.
func TestShapingResultShapeIsDerived(t *testing.T) {
	vars := map[string]any{"n": "Alice"}

	goSide, err := playground.CompileWithShaping(
		playground.ExampleSDL, playground.ExampleShapingQuery, vars, false)
	require.NoError(t, err)
	sqlSide, err := playground.CompileWithShaping(
		playground.ExampleSDL, playground.ExampleShapingQuery, vars, true)
	require.NoError(t, err)

	// { persons { name follows { name } followedBy { name } } } is three levels,
	// each contributing its scalar plus the hidden key column shape groups by.
	assert.Contains(t, goSide.ResultShape, "6 columns across 3 level(s)")
	assert.Contains(t, goSide.ResultShape, "one row per matched path")
	assert.Contains(t, sqlSide.ResultShape, "1 column (response), 1 row")
}

// TestCompileWithShapingReportsAnError confirms a bad input is reported rather
// than rendered as an empty panel.
func TestCompileWithShapingReportsAnError(t *testing.T) {
	_, err := playground.CompileWithShaping(playground.ExampleSDL, `{ persons { nope } }`, nil, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope")

	_, err = playground.CompileWithShaping("type Broken {", `{ persons { name } }`, nil, false)
	require.Error(t, err)
}
