package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/schema"
	"github.com/lega4e/gopgql/sdl"
)

func build(t *testing.T, src string) *schema.Schema {
	t.Helper()
	doc, err := sdl.Parse(src)
	require.NoError(t, err)
	m, err := generator.Build(doc, "")
	require.NoError(t, err)
	return m
}

// generated returns every generated file's text, so an assertion can be made
// over what a generation *emitted* rather than over a model.
func generated(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var b strings.Builder
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		b.WriteString("-- " + e.Name() + "\n")
		b.Write(data)
		b.WriteString("\n")
	}
	return b.String()
}

const mixed = `
type Person @node(label: "person") {
  id: ID!
  name: String!
}
type Stream @node(label: "stream", table: "streams", schema: "dbos") @readonly {
  id: ID!
  seq: Int! @column(name: "offset")
  topic: String!
}
`

// TestReadOnlyEmitsNoMigration is the migrate half of item 3: not one
// statement about an unmanaged table, in any file of any generation, in either
// direction.
func TestReadOnlyEmitsNoMigration(t *testing.T) {
	dir := t.TempDir()
	paths, err := Generate(dir, build(t, mixed), "init", Halves{})
	require.NoError(t, err)
	require.NotEmpty(t, paths)

	out := generated(t, dir)
	for _, forbidden := range []string{
		"CREATE TABLE dbos.streams",
		"ALTER TABLE dbos.streams",
		"DROP TABLE IF EXISTS dbos.streams",
		"CREATE INDEX",
		`"offset" integer`,
	} {
		assert.NotContains(t, out, forbidden,
			"an unmanaged table is never created, altered, indexed or dropped")
	}
	// The managed table is generated as usual, and the graph names both.
	assert.Contains(t, out, "CREATE TABLE persons")
	assert.Contains(t, out, `dbos.streams LABEL stream PROPERTIES (id, "offset", topic)`)
}

// The round trip is what makes the rule hold on the *second* run. Folding a
// generation whose graph names an unmanaged table gives that table back with no
// columns; diffed naively against a desired table that has three, it would emit
// an ADD COLUMN for each — against a table gopgql must not touch. Generating
// twice from one SDL must produce nothing the second time.
func TestReadOnlyIsStableAcrossRegeneration(t *testing.T) {
	dir := t.TempDir()
	desired := build(t, mixed)

	_, err := Generate(dir, desired, "init", Halves{})
	require.NoError(t, err)

	again, err := Generate(dir, build(t, mixed), "again", Halves{})
	require.NoError(t, err)
	assert.Empty(t, again,
		"an unchanged schema plans nothing; an unmanaged table must not re-propose its own columns")
}

// Adding a column to an unmanaged type is a change to what gopgql *surfaces*,
// so the graph moves; the tables half still emits nothing about it.
func TestAddingAColumnToAnUnmanagedTypeEmitsNoTableDDL(t *testing.T) {
	dir := t.TempDir()
	_, err := Generate(dir, build(t, mixed), "init", Halves{})
	require.NoError(t, err)

	widened := strings.Replace(mixed, "  topic: String!\n", "  topic: String!\n  createdAt: DateTime!\n", 1)
	paths, err := Generate(dir, build(t, widened), "widen", Halves{})
	require.NoError(t, err)
	require.NotEmpty(t, paths, "the property graph changed, so a generation is emitted")

	var newFiles strings.Builder
	for _, p := range paths {
		data, err := os.ReadFile(p)
		require.NoError(t, err)
		newFiles.Write(data)
	}
	out := newFiles.String()
	assert.NotContains(t, out, "ALTER TABLE")
	assert.Contains(t, out, "createdAt", "the graph exposes the new property")
}

// Dropping a column from a *managed* type still emits the ALTER the differ has
// always emitted — the suppression is per table, not a blanket one.
func TestManagedTablesAreStillDiffed(t *testing.T) {
	dir := t.TempDir()
	_, err := Generate(dir, build(t, mixed), "init", Halves{})
	require.NoError(t, err)

	narrowed := strings.Replace(mixed, "  name: String!\n", "", 1)
	paths, err := Generate(dir, build(t, narrowed), "narrow", Halves{})
	require.NoError(t, err)

	var out strings.Builder
	for _, p := range paths {
		data, err := os.ReadFile(p)
		require.NoError(t, err)
		out.Write(data)
	}
	assert.Contains(t, out.String(), "ALTER TABLE persons DROP COLUMN name;")
}

// Whether gopgql owns a table cannot change in a delta, and both directions
// fail *after* review rather than at generate time — which is why they are
// refused here, by name.
func TestChangingManagementIsRefused(t *testing.T) {
	// Adoption is asserted on an *unqualified* unmanaged type, so that @readonly
	// is the only thing that moves. A schema-qualified one cannot be adopted at
	// all — @node(schema:) is accepted only together with @readonly — and that is
	// a different refusal, in the SDL.
	unmanaged := `
type Person @node(label: "person") { id: ID! name: String! }
type Stream @node(label: "stream", table: "streams") @readonly {
  id: ID!
  topic: String!
}
`
	adopted := strings.Replace(unmanaged, ` @readonly`, "", 1)
	disowned := `
type Person @node(label: "person", table: "persons") @readonly {
  id: ID!
  name: String!
}
`

	t.Run("adoption", func(t *testing.T) {
		dir := t.TempDir()
		_, err := Generate(dir, build(t, unmanaged), "init", Halves{})
		require.NoError(t, err)

		_, err = Generate(dir, build(t, adopted), "adopt", Halves{})
		require.ErrorIs(t, err, ErrManagementChanged)
		assert.Contains(t, err.Error(), "CREATE TABLE for a table that already exists")
	})

	t.Run("disowning", func(t *testing.T) {
		dir := t.TempDir()
		_, err := Generate(dir, build(t, mixed), "init", Halves{})
		require.NoError(t, err)

		_, err = Generate(dir, build(t, disowned), "disown", Halves{})
		require.ErrorIs(t, err, ErrManagementChanged)
		assert.Contains(t, err.Error(), "fresh --dir")
	})
}

// A schema-qualified table survives the fold: the emitter writes dbos.streams
// into the property graph and the reader has to give the same two parts back,
// or the *next* delta is computed against a state that never had it.
func TestQualifiedNameRoundTripsThroughTheFold(t *testing.T) {
	dir := t.TempDir()
	_, err := Generate(dir, build(t, mixed), "init", Halves{})
	require.NoError(t, err)

	folded, err := Fold(dir)
	require.NoError(t, err)

	var stream *schema.VertexTable
	for i := range folded.VertexTables {
		if folded.VertexTables[i].Name == "streams" {
			stream = &folded.VertexTables[i]
		}
	}
	require.NotNil(t, stream, "the graph names the table, so the fold must find it")
	assert.Equal(t, "dbos", stream.Schema)
	assert.Equal(t, "dbos.streams", stream.Key())
	assert.Empty(t, stream.Columns,
		"nothing in this history creates it, so the fold knows no columns for it — which is correct")
}
