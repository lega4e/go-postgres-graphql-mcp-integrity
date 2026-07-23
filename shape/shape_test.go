package shape_test

import (
	"reflect"
	"testing"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/shape"
)

func TestRowsNestsUnderRoot(t *testing.T) {
	proj := compiler.Projection{
		ResponseKey: "persons",
		Fields: []compiler.ProjectedField{
			{ResponseKey: "name", Property: "name"},
		},
	}
	rows := []map[string]any{
		{"name": "Alice"},
		{"name": "Bob"},
	}
	got := shape.Rows(proj, rows)
	want := map[string]any{
		"persons": []any{
			map[string]any{"name": "Alice"},
			map[string]any{"name": "Bob"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Rows = %#v, want %#v", got, want)
	}
}

func TestRowsEmpty(t *testing.T) {
	proj := compiler.Projection{ResponseKey: "persons", Fields: []compiler.ProjectedField{{ResponseKey: "name", Property: "name"}}}
	got := shape.Rows(proj, nil)
	list, ok := got["persons"].([]any)
	if !ok {
		t.Fatalf("expected []any under persons, got %T", got["persons"])
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %v", list)
	}
}
