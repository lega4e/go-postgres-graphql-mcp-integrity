package shape_test

import (
	"reflect"
	"testing"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/shape"
)

// flatProjection mirrors what the compiler builds for `{ persons { name } }`:
// one level keyed by v0_k with a single scalar field.
func flatProjection() compiler.Projection {
	return compiler.Projection{Root: &compiler.Selection{
		ResponseKey: "persons",
		Alias:       "v0",
		KeyColumn:   "v0_k",
		Fields:      []compiler.ProjectedField{{ResponseKey: "name", Property: "name", Column: "v0_c0"}},
	}}
}

func TestRowsNestsUnderRoot(t *testing.T) {
	proj := flatProjection()
	rows := []map[string]any{
		{"v0_k": "a", "v0_c0": "Alice"},
		{"v0_k": "b", "v0_c0": "Bob"},
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
	got := shape.Rows(flatProjection(), nil)
	list, ok := got["persons"].([]any)
	if !ok {
		t.Fatalf("expected []any under persons, got %T", got["persons"])
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %v", list)
	}
}

// nestedProjection mirrors `{ persons { name follows { name } } }`.
func nestedProjection() compiler.Projection {
	return compiler.Projection{Root: &compiler.Selection{
		ResponseKey: "persons",
		Alias:       "v0",
		KeyColumn:   "v0_k",
		Fields:      []compiler.ProjectedField{{ResponseKey: "name", Property: "name", Column: "v0_c0"}},
		Children: []*compiler.Selection{{
			ResponseKey: "follows",
			Alias:       "v1",
			KeyColumn:   "v1_k",
			Fields:      []compiler.ProjectedField{{ResponseKey: "name", Property: "name", Column: "v1_c0"}},
		}},
	}}
}

// TestRowsDedupsParents is the core M3 property: a parent that fans out to two
// children appears once, with both children nested beneath it.
func TestRowsDedupsParents(t *testing.T) {
	proj := nestedProjection()
	rows := []map[string]any{
		{"v0_k": "alice", "v0_c0": "Alice", "v1_k": "bob", "v1_c0": "Bob"},
		{"v0_k": "alice", "v0_c0": "Alice", "v1_k": "carol", "v1_c0": "Carol"},
		{"v0_k": "bob", "v0_c0": "Bob", "v1_k": "dave", "v1_c0": "Dave"},
	}
	got := shape.Rows(proj, rows)
	want := map[string]any{
		"persons": []any{
			map[string]any{"name": "Alice", "follows": []any{
				map[string]any{"name": "Bob"},
				map[string]any{"name": "Carol"},
			}},
			map[string]any{"name": "Bob", "follows": []any{
				map[string]any{"name": "Dave"},
			}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Rows = %#v,\nwant %#v", got, want)
	}
}

// TestRowsHandlesNonComparableKeys proves keys that decode to non-comparable
// Go types (e.g. a uuid as a byte slice) still group correctly.
func TestRowsHandlesNonComparableKeys(t *testing.T) {
	proj := nestedProjection()
	id := func(b byte) []byte { return []byte{b} }
	rows := []map[string]any{
		{"v0_k": id(1), "v0_c0": "Alice", "v1_k": id(2), "v1_c0": "Bob"},
		{"v0_k": id(1), "v0_c0": "Alice", "v1_k": id(3), "v1_c0": "Carol"},
	}
	got := shape.Rows(proj, rows)
	persons, _ := got["persons"].([]any)
	if len(persons) != 1 {
		t.Fatalf("expected 1 deduped parent, got %d: %#v", len(persons), persons)
	}
	follows, _ := persons[0].(map[string]any)["follows"].([]any)
	if len(follows) != 2 {
		t.Errorf("expected 2 children, got %d", len(follows))
	}
}
