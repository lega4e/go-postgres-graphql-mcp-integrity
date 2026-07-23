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
	if !strings.Contains(res.SQL, "GRAPH_TABLE") || !strings.Contains(res.SQL, "MATCH (v IS person)") {
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
	if !strings.Contains(res.SQL, "v.age AS age") {
		t.Errorf("compiled SQL must project the new field:\n%s", res.SQL)
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
