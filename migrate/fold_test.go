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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// The M7 statements below are written by hand because the reader has to
// understand them before the differ emits one (design D3). A rename the fold
// cannot read leaves the *next* delta computed against a state where the rename
// never happened: the differ sees the old name still there, emits a drop, and
// the renamed data goes with it. These tests are what make that impossible.

// renameBeforeSDL and renameAfterSDL differ by one column name and one table
// name — the two renames a delta has to be able to express.
const renameBeforeSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}`

const renameAfterColumnSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  contact: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}`

const renameAfterTableSDL = `type Person @node(label: "person", table: "people") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}`

// upBody renders a delta migration's goose Up section from hand-written
// statements. Only the Up section is folded, so no Down body is needed.
func upBody(stmts ...string) string {
	return "-- +goose Up\n" + strings.Join(stmts, "\n") + "\n"
}

// foldedColumn returns the named column of the named vertex or edge table in a
// folded schema.
func foldedColumn(t *testing.T, m *schema.Schema, table, column string) schema.Column {
	t.Helper()
	tables := map[string][]schema.Column{}
	for _, vt := range m.VertexTables {
		tables[vt.Name] = vt.Columns
	}
	for _, e := range m.EdgeTables {
		tables[e.Name] = e.Columns
	}
	for _, c := range tables[table] {
		if c.Name == column {
			return c
		}
	}
	require.FailNowf(t, "column not found", "%s.%s is not in the folded schema", table, column)
	return schema.Column{}
}

// TestFoldRenameColumn folds a column rename and requires the reconstructed
// schema to be exactly the schema the renamed SDL describes — the new name
// present, the old one gone.
func TestFoldRenameColumn(t *testing.T) {
	before := mustSchema(t, renameBeforeSDL)
	after := mustSchema(t, renameAfterColumnSDL)

	folded, err := migrate.FoldContent([]string{
		migrate.Init(before),
		upBody(
			"DROP PROPERTY GRAPH IF EXISTS app_graph;",
			"ALTER TABLE persons RENAME COLUMN email TO contact;",
			generator.GraphDDL(after),
		),
	})
	require.NoError(t, err)
	assert.Equal(t, after, folded,
		"a folded rename must reconstruct the renamed schema, not the old one")
}

// TestFoldRenameTable folds a table rename. The table is referenced by an edge
// table's foreign key, which PostgreSQL carries across the rename without
// restating it — so the folded model has to carry it too, or the next delta
// rebuilds the edge table against a table name that no longer exists.
func TestFoldRenameTable(t *testing.T) {
	before := mustSchema(t, renameBeforeSDL)
	after := mustSchema(t, renameAfterTableSDL)

	folded, err := migrate.FoldContent([]string{
		migrate.Init(before),
		upBody(
			"DROP PROPERTY GRAPH IF EXISTS app_graph;",
			"ALTER TABLE persons RENAME TO people;",
			generator.GraphDDL(after),
		),
	})
	require.NoError(t, err)
	assert.Equal(t, after, folded)
	assert.Equal(t, "people", foldedColumn(t, folded, "follows", "source_id").References.Table,
		"a foreign key follows the table it points at across a rename")
}

// TestDeltaAfterRenameIsEmpty is the hazard this whole section exists to
// prevent, asserted directly: once a rename has been folded, diffing the same
// SDL against it must produce nothing at all. If the fold missed the rename the
// differ would see the old object still present and emit a drop, and the data
// the rename preserved would go with it.
func TestDeltaAfterRenameIsEmpty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		src   string
		stmt  string
		after string
	}{
		{"column", renameBeforeSDL, "ALTER TABLE persons RENAME COLUMN email TO contact;", renameAfterColumnSDL},
		{"table", renameBeforeSDL, "ALTER TABLE persons RENAME TO people;", renameAfterTableSDL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := mustSchema(t, tc.src)
			after := mustSchema(t, tc.after)
			folded, err := migrate.FoldContent([]string{
				migrate.Init(before),
				upBody("DROP PROPERTY GRAPH IF EXISTS app_graph;", tc.stmt, generator.GraphDDL(after)),
			})
			require.NoError(t, err)

			up, down, changed := migrate.Delta(folded, after)
			assert.False(t, changed, "up:\n%s\ndown:\n%s", up, down)
			assert.NotContains(t, up, "DROP COLUMN")
			assert.NotContains(t, up, "DROP TABLE")
		})
	}
}

// TestFoldSetAndDropDefault covers the statements that change a default without
// touching the column's data: a default is a property of the column, so folding
// one must never look like a drop and an add (design D6).
func TestFoldSetAndDropDefault(t *testing.T) {
	base := mustSchema(t, exampleSDL)

	withDefault, err := migrate.FoldContent([]string{
		migrate.Init(base),
		upBody("ALTER TABLE persons ALTER COLUMN email SET DEFAULT 'unknown';"),
	})
	require.NoError(t, err)
	assert.Equal(t, "'unknown'", foldedColumn(t, withDefault, "persons", "email").Default,
		"the default is carried verbatim, quotes and all")

	want := mustSchema(t, exampleSDL)
	setDefault(t, want, "persons", "email", "'unknown'")
	assert.Equal(t, want, withDefault, "setting a default changes nothing else about the column")

	cleared, err := migrate.FoldContent([]string{
		migrate.Init(base),
		upBody("ALTER TABLE persons ALTER COLUMN email SET DEFAULT 'unknown';"),
		upBody("ALTER TABLE persons ALTER COLUMN email DROP DEFAULT;"),
	})
	require.NoError(t, err)
	assert.Equal(t, base, cleared, "dropping the default returns the model to where it started")
}

// setDefault sets a column's default in a schema built for a test expectation.
func setDefault(t *testing.T, m *schema.Schema, table, column, expr string) {
	t.Helper()
	for i := range m.VertexTables {
		if m.VertexTables[i].Name != table {
			continue
		}
		for j := range m.VertexTables[i].Columns {
			if m.VertexTables[i].Columns[j].Name == column {
				m.VertexTables[i].Columns[j].Default = expr
				return
			}
		}
	}
	require.FailNowf(t, "column not found", "%s.%s", table, column)
}

// TestFoldUniqueConstraint guards the single constraint kind the model can hold
// today. An ADD states its column; a DROP states only a name, so the column is
// recovered from the name gopgql chose — which is why it chooses one.
func TestFoldUniqueConstraint(t *testing.T) {
	base := mustSchema(t, exampleSDL)
	const add = "ALTER TABLE persons ADD CONSTRAINT persons_email_key UNIQUE (email);"

	added, err := migrate.FoldContent([]string{migrate.Init(base), upBody(add)})
	require.NoError(t, err)
	assert.True(t, foldedColumn(t, added, "persons", "email").Unique)

	dropped, err := migrate.FoldContent([]string{
		migrate.Init(base),
		upBody(add, "ALTER TABLE persons DROP CONSTRAINT persons_email_key;"),
	})
	require.NoError(t, err)
	assert.Equal(t, base, dropped)
}

// TestFoldUnmodelledConstraints covers the M7 constraint forms that the schema
// model has no field for yet — a CHECK body and a natural key's multi-column
// UNIQUE. They must be read without error: refusing a statement gopgql itself
// emitted would make the whole prior state unreadable, which is far worse than
// a constraint the model cannot yet represent.
func TestFoldUnmodelledConstraints(t *testing.T) {
	base := mustSchema(t, exampleSDL)
	folded, err := migrate.FoldContent([]string{
		migrate.Init(base),
		upBody(
			"ALTER TABLE persons ADD CONSTRAINT persons_name_check CHECK (name <> '');",
			"ALTER TABLE persons ADD CONSTRAINT persons_key UNIQUE (name, email);",
			"ALTER TABLE persons DROP CONSTRAINT persons_name_check;",
			"ALTER TABLE persons DROP CONSTRAINT persons_key;",
		),
	})
	require.NoError(t, err)
	assert.Equal(t, base, folded, "an unmodelled constraint must not disturb the rest of the model")
}

// TestFoldRenameErrors covers the renames that cannot be true. A rename of
// something the prior migrations never created means the reader and the writer
// have diverged, and silently inventing the object would hide that.
func TestFoldRenameErrors(t *testing.T) {
	base := mustSchema(t, exampleSDL)
	for _, tc := range []struct {
		name string
		stmt string
	}{
		{"unknown table", "ALTER TABLE ghosts RENAME TO shades;"},
		{"unknown column", "ALTER TABLE persons RENAME COLUMN ghost TO shade;"},
		{"default on unknown column", "ALTER TABLE persons ALTER COLUMN ghost SET DEFAULT 1;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := migrate.FoldContent([]string{migrate.Init(base), upBody(tc.stmt)})
			assert.Error(t, err)
		})
	}
}
