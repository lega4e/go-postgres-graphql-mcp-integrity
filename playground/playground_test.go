package playground_test

import (
	"strings"
	"testing"

	"github.com/lega4e/gopgql/playground"
)

func TestMigration(t *testing.T) {
	mig, err := playground.Migration(playground.ExampleSDL)
	if err != nil {
		t.Fatalf("Migration: %v", err)
	}
	if !strings.HasPrefix(mig, "-- +goose Up\n") {
		t.Error("migration must be goose-formatted")
	}
	if !strings.Contains(mig, "CREATE PROPERTY GRAPH app_graph") {
		t.Error("migration must create the property graph")
	}
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
