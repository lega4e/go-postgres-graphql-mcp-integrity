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
