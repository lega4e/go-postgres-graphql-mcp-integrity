package migrate_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/schema"
	"github.com/lega4e/gopgql/sdl"
)

// mustSchema parses an SDL source and builds its physical schema model.
func mustSchema(t *testing.T, src string) *schema.Schema {
	t.Helper()
	doc, err := sdl.Parse(src)
	if err != nil {
		t.Fatalf("sdl.Parse: %v", err)
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		t.Fatalf("generator.Build: %v", err)
	}
	return m
}

// canonicalize sorts every schema's columns and properties by name so two
// schemas that differ only in physical column order (an ALTER appends, while a
// direct build keeps declaration order) compare equal.
func canonicalize(m *schema.Schema) *schema.Schema {
	c := *m
	c.VertexTables = append([]schema.VertexTable(nil), m.VertexTables...)
	for i := range c.VertexTables {
		c.VertexTables[i].Columns = sortedColumns(c.VertexTables[i].Columns)
		c.VertexTables[i].Properties = sortedStrings(c.VertexTables[i].Properties)
	}
	c.EdgeTables = append([]schema.EdgeTable(nil), m.EdgeTables...)
	for i := range c.EdgeTables {
		c.EdgeTables[i].Columns = sortedColumns(c.EdgeTables[i].Columns)
		c.EdgeTables[i].Properties = sortedStrings(c.EdgeTables[i].Properties)
	}
	c.Indexes = append([]schema.Index(nil), m.Indexes...)
	sort.Slice(c.Indexes, func(i, j int) bool { return c.Indexes[i].Name < c.Indexes[j].Name })
	return &c
}

func sortedColumns(cols []schema.Column) []schema.Column {
	out := append([]schema.Column(nil), cols...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortedStrings(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	return out
}

// TestFoldInitRoundTrip folds the initial migration for a schema back into a
// model and requires it to equal the model exactly — the emitter and the
// interpreter are two halves of one contract.
func TestFoldInitRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"worked example", exampleSDL},
		{"all scalar types", `type Event @node(label: "event") {
  id: ID!
  count: Int!
  ratio: Float
  active: Boolean!
  payload: JSON
  at: DateTime!
  tags: [String!]!
}`},
		{"keyword identifiers", `type Order @node(label: "order", table: "order") {
  id: ID!
  total: Float!
}`},
		{"two node types", `type Person @node(label: "person") {
  id: ID!
  name: String!
  posts: [Post!]! @relationship(type: "authored", direction: OUT)
}
type Post @node(label: "post") {
  id: ID!
  title: String!
  author: [Person!]! @relationship(type: "authored", direction: IN) @hasInverse(field: "posts")
}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mustSchema(t, tc.src)
			folded, err := migrate.FoldContent([]string{migrate.Init(m)})
			if err != nil {
				t.Fatalf("FoldContent: %v", err)
			}
			if !reflect.DeepEqual(folded, m) {
				t.Errorf("folded schema != original\n--- folded ---\n%s\n--- original ---\n%s",
					generator.DDL(folded), generator.DDL(m))
			}
		})
	}
}

// TestFoldRebuildsIdenticalDDL is a weaker but independent check: DDL(fold(x))
// == x for the initial migration's Up body.
func TestFoldRebuildsIdenticalDDL(t *testing.T) {
	m := mustSchema(t, exampleSDL)
	folded, err := migrate.FoldContent([]string{migrate.Init(m)})
	if err != nil {
		t.Fatalf("FoldContent: %v", err)
	}
	if got, want := generator.DDL(folded), generator.DDL(m); got != want {
		t.Errorf("DDL round-trip mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestFoldEmptyIsNil(t *testing.T) {
	folded, err := migrate.FoldContent(nil)
	if err == nil {
		t.Fatalf("folding no migrations should error (no graph), got %+v", folded)
	}
}

// TestFoldInterfaceLabelsRoundTrip covers the M4 addition to the emitter /
// interpreter contract: a vertex table carrying a shared interface label must
// fold back with that label intact, or a delta would drop and recreate the graph
// without it.
func TestFoldInterfaceLabelsRoundTrip(t *testing.T) {
	const src = `interface Actor @node(label: "actor") {
  id: ID!
  name: String!
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}

type Person implements Actor @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}

type Bot implements Actor @node(label: "bot") {
  id: ID!
  name: String!
  vendor: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT, table: "bot_follows")
}`
	m := mustSchema(t, src)
	folded, err := migrate.FoldContent([]string{migrate.Init(m)})
	if err != nil {
		t.Fatalf("FoldContent: %v", err)
	}
	if !reflect.DeepEqual(folded, m) {
		t.Errorf("folded schema != original\n--- folded ---\n%s\n--- original ---\n%s",
			generator.DDL(folded), generator.DDL(m))
	}

	// Adding a column to an interface-bearing schema still produces a delta that
	// recreates the graph with both labels.
	revised := strings.Replace(src, "  vendor: String\n", "  vendor: String\n  rank: Int\n", 1)
	up, _, changed := migrate.Delta(folded, mustSchema(t, revised))
	if !changed {
		t.Fatal("adding a column must produce a delta")
	}
	if !strings.Contains(up, "LABEL actor PROPERTIES (id, name)") {
		t.Errorf("delta must recreate the graph with the shared label:\n%s", up)
	}
}
