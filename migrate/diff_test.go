package migrate_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/schema"
)

const baseSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// widenedSDL adds a nullable `age` field at the end of the type.
const widenedSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  age: Int
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

func TestDeltaAddColumn(t *testing.T) {
	from := mustSchema(t, baseSDL)
	to := mustSchema(t, widenedSDL)

	up, down, changed := migrate.Delta(from, to)
	if !changed {
		t.Fatal("expected a change when a field is added")
	}
	if !strings.Contains(up, "ALTER TABLE persons ADD COLUMN age integer;") {
		t.Errorf("Up missing ADD COLUMN:\n%s", up)
	}
	if !strings.Contains(up, "DROP PROPERTY GRAPH IF EXISTS app_graph;") ||
		!strings.Contains(up, "CREATE PROPERTY GRAPH app_graph") {
		t.Errorf("Up must drop and recreate the graph:\n%s", up)
	}
	// The recreated desired graph exposes the new property.
	if !strings.Contains(up, "PROPERTIES (id, name, email, age)") {
		t.Errorf("recreated graph must list the new property:\n%s", up)
	}
	// Down reverses the add.
	if !strings.Contains(down, "ALTER TABLE persons DROP COLUMN age;") {
		t.Errorf("Down missing DROP COLUMN:\n%s", down)
	}
	// Down restores the prior graph (without age).
	if !strings.Contains(down, "PROPERTIES (id, name, email)") {
		t.Errorf("Down must restore the prior graph:\n%s", down)
	}

	// The ADD COLUMN must precede the CREATE PROPERTY GRAPH in Up (the graph
	// references the new property).
	if addIdx, graphIdx := strings.Index(up, "ADD COLUMN age"), strings.Index(up, "CREATE PROPERTY GRAPH"); addIdx > graphIdx {
		t.Errorf("ADD COLUMN must come before CREATE PROPERTY GRAPH in Up:\n%s", up)
	}
	// The DROP PROPERTY GRAPH must precede DROP COLUMN in Down.
	if dropG, dropC := strings.Index(down, "DROP PROPERTY GRAPH"), strings.Index(down, "DROP COLUMN age"); dropG > dropC {
		t.Errorf("DROP PROPERTY GRAPH must come before DROP COLUMN in Down:\n%s", down)
	}
}

// TestDeltaAddColumnFoldsToDesired proves the delta is correct by folding the
// init + delta and requiring the result to match the desired schema (with the
// new column appended physically, so compared canonically).
func TestDeltaAddColumnFoldsToDesired(t *testing.T) {
	from := mustSchema(t, baseSDL)
	to := mustSchema(t, widenedSDL)

	up, down, _ := migrate.Delta(from, to)
	deltaFile := "-- +goose Up\n" + up + "\n-- +goose Down\n" + down

	folded, err := migrate.FoldContent([]string{migrate.Init(from), deltaFile})
	if err != nil {
		t.Fatalf("FoldContent: %v", err)
	}
	if !reflect.DeepEqual(canonicalize(folded), canonicalize(to)) {
		t.Errorf("folded init+delta != desired\nfolded: %+v\ndesired: %+v", folded, to)
	}
	// The new column is physically present.
	if !hasPersonColumn(folded, "age") {
		t.Errorf("folded schema missing appended column age: %+v", folded)
	}
}

func TestDeltaDropColumn(t *testing.T) {
	from := mustSchema(t, widenedSDL) // has age
	to := mustSchema(t, baseSDL)      // no age

	up, down, changed := migrate.Delta(from, to)
	if !changed {
		t.Fatal("expected a change when a field is removed")
	}
	if !strings.Contains(up, "ALTER TABLE persons DROP COLUMN age;") {
		t.Errorf("Up missing DROP COLUMN:\n%s", up)
	}
	if !strings.Contains(down, "ALTER TABLE persons ADD COLUMN age integer;") {
		t.Errorf("Down must restore the dropped column:\n%s", down)
	}

	// Fold init(from) + delta → should equal `to` (age gone).
	deltaFile := "-- +goose Up\n" + up + "\n-- +goose Down\n" + down
	folded, err := migrate.FoldContent([]string{migrate.Init(from), deltaFile})
	if err != nil {
		t.Fatalf("FoldContent: %v", err)
	}
	if hasPersonColumn(folded, "age") {
		t.Errorf("folded schema should not contain dropped column age: %+v", folded)
	}
	if !reflect.DeepEqual(canonicalize(folded), canonicalize(to)) {
		t.Errorf("folded init+delta != desired after drop\nfolded: %+v\ndesired: %+v", folded, to)
	}
}

func TestDeltaNoChange(t *testing.T) {
	m := mustSchema(t, baseSDL)
	up, down, changed := migrate.Delta(m, m)
	if changed || up != "" || down != "" {
		t.Errorf("identical schemas must yield no change, got changed=%v up=%q down=%q", changed, up, down)
	}
}

// TestGenerateWritesDelta walks a tables-only directory — every generation with
// --no-graph, which is the "someone else owns the graph, or nobody does" case —
// through two generations and a no-op third.
func TestGenerateWritesDelta(t *testing.T) {
	dir := t.TempDir()
	tablesOnly := migrate.Halves{NoGraph: true}

	first, err := migrate.Generate(dir, mustSchema(t, baseSDL), "init", tablesOnly)
	require.NoError(t, err, "first Generate")
	require.Len(t, first, 1, "with the graph half off there is only the tables file")
	require.Equal(t, "0001_init_tables.sql", filepath.Base(first[0]))

	second, err := migrate.Generate(dir, mustSchema(t, widenedSDL), "add age", tablesOnly)
	require.NoError(t, err, "second Generate")
	require.Len(t, second, 1)
	assert.Equal(t, "0002_add_age_tables.sql", filepath.Base(second[0]),
		"the version is one past the highest already in the directory")

	content := readFile(t, second[0])
	assert.True(t, strings.HasPrefix(content, "-- +goose Up\n"), "not goose-formatted:\n%s", content)
	assert.Contains(t, content, "\n-- +goose Down\n")

	// Folding the whole directory reproduces the widened schema.
	folded, err := migrate.Fold(dir)
	require.NoError(t, err, "Fold")
	assert.True(t, hasPersonColumn(folded, "age"), "folded directory missing age column: %+v", folded)

	// A third generation against an unchanged schema writes nothing at all.
	third, err := migrate.Generate(dir, mustSchema(t, widenedSDL), "noop", tablesOnly)
	require.NoError(t, err, "third Generate")
	assert.Empty(t, third, "an unchanged schema must emit no migration")
}

func hasPersonColumn(m *schema.Schema, col string) bool {
	for _, vt := range m.VertexTables {
		if vt.Name != "persons" {
			continue
		}
		for _, c := range vt.Columns {
			if c.Name == col {
				return true
			}
		}
	}
	return false
}

// TestDeltaConstraintsAndIndexes covers the M6 differ additions: a UNIQUE
// constraint gained by an existing column, an index added, and an index whose
// definition moved — each with its exact inverse in the Down section
// (SPEC.md §7 → M6).
func TestDeltaConstraintsAndIndexes(t *testing.T) {
	before := `type Product @node(label: "product") {
  id: ID!
  sku: String!
  category: String! @index(name: "products_category_idx")
}`
	after := `type Product @node(label: "product") {
  id: ID!
  sku: String! @unique
  category: String! @index(name: "products_category_idx", using: "hash")
  vendor: String @index
}`

	up, down, changed := migrate.Delta(mustSchema(t, before), mustSchema(t, after))
	if !changed {
		t.Fatal("the schemas differ; changed must be true")
	}
	for _, want := range []string{
		"ALTER TABLE products ADD CONSTRAINT products_sku_key UNIQUE (sku);",
		"CREATE INDEX products_vendor_idx ON products (vendor);",
		"DROP INDEX IF EXISTS products_category_idx;",
		"CREATE INDEX products_category_idx ON products USING hash (category);",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("Up is missing %q:\n%s", want, up)
		}
	}
	for _, want := range []string{
		"ALTER TABLE products DROP CONSTRAINT products_sku_key;",
		"DROP INDEX IF EXISTS products_vendor_idx;",
		"CREATE INDEX products_category_idx ON products (category);",
	} {
		if !strings.Contains(down, want) {
			t.Errorf("Down is missing %q:\n%s", want, down)
		}
	}
}

// TestDeltaUniqueDropIsExactInverse proves losing @unique emits the drop, and
// that the Down puts the same constraint back.
func TestDeltaUniqueDropIsExactInverse(t *testing.T) {
	with := `type Product @node(label: "product") {
  id: ID!
  sku: String! @unique
}`
	without := `type Product @node(label: "product") {
  id: ID!
  sku: String!
}`
	up, down, changed := migrate.Delta(mustSchema(t, with), mustSchema(t, without))
	if !changed {
		t.Fatal("dropping @unique is a change")
	}
	if !strings.Contains(up, "ALTER TABLE products DROP CONSTRAINT products_sku_key;") {
		t.Errorf("Up does not drop the constraint:\n%s", up)
	}
	if !strings.Contains(down, "ALTER TABLE products ADD CONSTRAINT products_sku_key UNIQUE (sku);") {
		t.Errorf("Down does not restore the constraint:\n%s", down)
	}
}
