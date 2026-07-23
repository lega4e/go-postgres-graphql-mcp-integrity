// Package migrate emits gopgql's schema as goose migration files.
//
// In M1 it is one-shot: given the desired schema it writes 0001_init.sql with a
// -- +goose Up section (the full CREATE DDL) and a -- +goose Down section (the
// inverse DROPs). Delta generation — folding prior migrations into a model and
// diffing against the desired model — arrives in M2 (SPEC.md §7). It has no
// database dependency and compiles to WASM (SPEC.md §4.1).
package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/internal/pgident"
	"github.com/lega4e/gopgql/schema"
)

// quote renders an identifier for use in the Down section's DROP statements.
func quote(s string) string { return pgident.Quote(s) }

// InitFilename is the goose filename for the initial migration.
const InitFilename = "0001_init.sql"

// Init renders the initial migration for the desired schema as goose file
// content: a -- +goose Up section with the CREATE DDL and a -- +goose Down
// section with the inverse DROPs.
func Init(m *schema.Schema) string {
	var b strings.Builder
	b.WriteString("-- +goose Up\n")
	b.WriteString(generator.DDL(m))
	b.WriteString("\n-- +goose Down\n")
	b.WriteString(downDDL(m))
	return b.String()
}

// WriteInit writes the initial migration to dir/0001_init.sql, creating dir if
// necessary, and returns the file path.
func WriteInit(dir string, m *schema.Schema) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("migrate: create dir: %w", err)
	}
	path := filepath.Join(dir, InitFilename)
	if err := os.WriteFile(path, []byte(Init(m)), 0o644); err != nil {
		return "", fmt.Errorf("migrate: write %s: %w", path, err)
	}
	return path, nil
}

// downDDL builds the inverse of the init migration: drop the property graph
// first (it depends on the tables), then edge tables (they carry foreign keys
// into vertex tables), then vertex tables — each in reverse creation order.
// Indexes are dropped implicitly with their tables.
func downDDL(m *schema.Schema) string {
	var stmts []string
	stmts = append(stmts, fmt.Sprintf("DROP PROPERTY GRAPH %s;", quote(m.GraphName)))
	for i := len(m.EdgeTables) - 1; i >= 0; i-- {
		stmts = append(stmts, fmt.Sprintf("DROP TABLE IF EXISTS %s;", quote(m.EdgeTables[i].Name)))
	}
	for i := len(m.VertexTables) - 1; i >= 0; i-- {
		stmts = append(stmts, fmt.Sprintf("DROP TABLE IF EXISTS %s;", quote(m.VertexTables[i].Name)))
	}
	return strings.Join(stmts, "\n") + "\n"
}
