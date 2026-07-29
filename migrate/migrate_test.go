package migrate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/sdl"
)

const exampleSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

func build(t *testing.T) *migrateSchema {
	t.Helper()
	doc, err := sdl.Parse(exampleSDL)
	if err != nil {
		t.Fatalf("sdl.Parse: %v", err)
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		t.Fatalf("generator.Build: %v", err)
	}
	return &migrateSchema{content: migrate.Init(m)}
}

type migrateSchema struct{ content string }

func TestInitGooseFormat(t *testing.T) {
	got := build(t).content
	if !strings.HasPrefix(got, "-- +goose Up\n") {
		t.Error("migration must start with -- +goose Up")
	}
	if !strings.Contains(got, "\n-- +goose Down\n") {
		t.Error("migration must contain a -- +goose Down section")
	}
	up, down, ok := strings.Cut(got, "\n-- +goose Down\n")
	if !ok {
		t.Fatal("could not split Up/Down")
	}
	// Up creates the schema.
	for _, want := range []string{
		"CREATE TABLE persons",
		"CREATE TABLE follows",
		"CREATE INDEX follows_target_idx",
		"CREATE PROPERTY GRAPH app_graph",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("Up section missing %q", want)
		}
	}
	// Down drops in dependency-safe order: graph, edge table, vertex table.
	graphIdx := strings.Index(down, "DROP PROPERTY GRAPH IF EXISTS app_graph")
	edgeIdx := strings.Index(down, "DROP TABLE IF EXISTS follows")
	vertexIdx := strings.Index(down, "DROP TABLE IF EXISTS persons")
	if graphIdx < 0 || edgeIdx < 0 || vertexIdx < 0 {
		t.Fatalf("Down section missing drops:\n%s", down)
	}
	if graphIdx >= edgeIdx || edgeIdx >= vertexIdx {
		t.Errorf("Down drops out of order (graph=%d edge=%d vertex=%d)", graphIdx, edgeIdx, vertexIdx)
	}
}

// TestGenerateWritesTheFirstGeneration covers a fresh directory: the tables
// migration, then the graph migration over them, both in the directory itself.
func TestGenerateWritesTheFirstGeneration(t *testing.T) {
	doc, err := sdl.Parse(exampleSDL)
	require.NoError(t, err, "sdl.Parse")
	m, err := generator.Build(doc, "")
	require.NoError(t, err, "generator.Build")

	dir := t.TempDir()
	paths, err := migrate.Generate(dir, m, "init", migrate.Halves{})
	require.NoError(t, err, "Generate")
	require.Len(t, paths, 2, "a first generation has nothing to tear down")

	assert.Equal(t, []string{"0001_init_tables.sql", "0002_init_graph.sql"},
		[]string{filepath.Base(paths[0]), filepath.Base(paths[1])})
	for _, p := range paths {
		assert.Equal(t, dir, filepath.Dir(p), "everything goes into --dir itself")
	}

	tables, graph := readFile(t, paths[0]), readFile(t, paths[1])
	assert.Contains(t, tables, "CREATE TABLE persons")
	assert.NotContains(t, tables, "PROPERTY GRAPH", "no migration mixes the two")
	assert.Contains(t, graph, "CREATE PROPERTY GRAPH")
	assert.NotContains(t, graph, "CREATE TABLE", "no migration mixes the two")
}

// readFile reads a file the caller has just had written.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a path the test just generated
	require.NoError(t, err, "read %s", path)
	return string(data)
}
