package migrate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestWriteInit(t *testing.T) {
	doc, err := sdl.Parse(exampleSDL)
	if err != nil {
		t.Fatalf("sdl.Parse: %v", err)
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		t.Fatalf("generator.Build: %v", err)
	}
	dir := t.TempDir()
	path, err := migrate.GenerateTables(dir, m, "init", 1)
	if err != nil {
		t.Fatalf("GenerateTables: %v", err)
	}
	if filepath.Base(path) != migrate.InitFilename {
		t.Errorf("filename = %s, want %s", filepath.Base(path), migrate.InitFilename)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if string(data) != migrate.InitTables(m) {
		t.Error("written file content differs from migrate.InitTables")
	}
}
