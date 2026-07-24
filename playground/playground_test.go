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
	// A one-hop MATCH with an outgoing edge and a bound predicate.
	if !strings.Contains(out.SQL, "-[e0 IS follows]->") {
		t.Errorf("compiled SQL must traverse the follows edge:\n%s", out.SQL)
	}
	if !strings.Contains(out.SQL, "WHERE v0.name = $1") {
		t.Errorf("compiled SQL must bind the variable as $1:\n%s", out.SQL)
	}
	if !strings.Contains(out.Params, "$1 = Alice") {
		t.Errorf("params must report the ordered bind value, got %q", out.Params)
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
