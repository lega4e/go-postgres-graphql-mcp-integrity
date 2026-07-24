package playground_test

import (
	"strings"
	"testing"

	"github.com/lega4e/gopgql/playground"
)

func TestRunExample(t *testing.T) {
	res, err := playground.Run(playground.ExampleSDL, playground.ExampleQuery)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(res.Migration, "-- +goose Up\n") {
		t.Error("migration must be goose-formatted")
	}
	if !strings.Contains(res.Migration, "CREATE PROPERTY GRAPH app_graph") {
		t.Error("migration must create the property graph")
	}
	if !strings.Contains(res.SQL, "GRAPH_TABLE") || !strings.Contains(res.SQL, "MATCH (v0 IS person)") {
		t.Errorf("compiled SQL unexpected:\n%s", res.SQL)
	}
}

func TestRunSDLError(t *testing.T) {
	if _, err := playground.Run(`type Person { id: ID! }`, playground.ExampleQuery); err == nil {
		t.Error("expected error for SDL without @node")
	}
}

func TestRunQueryError(t *testing.T) {
	if _, err := playground.Run(playground.ExampleSDL, `{ persons { bogus } }`); err == nil {
		t.Error("expected error for unknown field")
	}
}

func TestRunNoQuery(t *testing.T) {
	res, err := playground.Run(playground.ExampleSDL, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SQL != "" {
		t.Errorf("expected no SQL without a query, got %q", res.SQL)
	}
}

func TestRunDeltaExample(t *testing.T) {
	res, err := playground.RunDelta(playground.ExampleSDL, playground.RevisedExampleSDL, playground.RevisedExampleQuery)
	if err != nil {
		t.Fatalf("RunDelta: %v", err)
	}
	if !strings.HasPrefix(res.Init, "-- +goose Up\n") {
		t.Error("init migration must be goose-formatted")
	}
	if !res.Changed {
		t.Error("adding a field must produce a delta")
	}
	if !strings.Contains(res.Delta, "ALTER TABLE persons ADD COLUMN age integer;") {
		t.Errorf("delta must add the new column:\n%s", res.Delta)
	}
	if !strings.Contains(res.Delta, "CREATE PROPERTY GRAPH app_graph") {
		t.Errorf("delta must recreate the property graph:\n%s", res.Delta)
	}
	if !strings.Contains(res.SQL, "v0.age AS") {
		t.Errorf("compiled SQL must project the new field:\n%s", res.SQL)
	}
}

func TestRunNested(t *testing.T) {
	vars := map[string]any{playground.NestedExampleVarName: playground.NestedExampleVarValue}
	res, err := playground.RunNested(playground.ExampleSDL, playground.NestedExampleQuery, vars)
	if err != nil {
		t.Fatalf("RunNested: %v", err)
	}
	// One-hop MATCH with an outgoing edge and a bound predicate.
	if !strings.Contains(res.SQL, "-[e0 IS follows]->") {
		t.Errorf("nested SQL must traverse the follows edge:\n%s", res.SQL)
	}
	if !strings.Contains(res.SQL, "WHERE v0.name = $1") {
		t.Errorf("nested SQL must bind the variable as $1:\n%s", res.SQL)
	}
	if !strings.Contains(res.Params, "$1 = Alice") {
		t.Errorf("params must report the ordered bind value, got %q", res.Params)
	}
	// The shaped JSON must nest both children under a single deduplicated parent.
	for _, want := range []string{`"persons"`, `"follows"`, `"Alice"`, `"Bob"`, `"Carol"`} {
		if !strings.Contains(res.JSON, want) {
			t.Errorf("shaped JSON missing %s:\n%s", want, res.JSON)
		}
	}
	// Exactly one parent object: "Alice" appears once as the parent name.
	if n := strings.Count(res.JSON, `"name": "Alice"`); n != 1 {
		t.Errorf("expected the parent Alice exactly once (dedup), got %d:\n%s", n, res.JSON)
	}
}

func TestRunNestedError(t *testing.T) {
	vars := map[string]any{playground.NestedExampleVarName: playground.NestedExampleVarValue}
	if _, err := playground.RunNested(`type Person { id: ID! }`, playground.NestedExampleQuery, vars); err == nil {
		t.Error("expected error for SDL without @node")
	}
}

func TestRunDeltaNoChange(t *testing.T) {
	res, err := playground.RunDelta(playground.ExampleSDL, playground.ExampleSDL, "")
	if err != nil {
		t.Fatalf("RunDelta: %v", err)
	}
	if res.Changed {
		t.Error("identical SDL revisions must not produce a delta")
	}
}

func TestRunDeltaError(t *testing.T) {
	if _, err := playground.RunDelta(`type Person { id: ID! }`, playground.RevisedExampleSDL, ""); err == nil {
		t.Error("expected error for invalid prior SDL")
	}
}
