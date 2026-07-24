// Package migrate emits gopgql's schema as goose migration files and folds its
// own prior migrations back into a schema model.
//
// The initial migration (0001_init.sql) is one-shot: given the desired schema,
// Init writes a -- +goose Up section (the full CREATE DDL) and a -- +goose Down
// section (the inverse DROPs).
//
// From M2 the generator stops being one-shot (SPEC.md §7 → M2). Fold interprets
// gopgql's own canonical goose statement set — the migrations it already emitted
// (0001, 0002, …) — back into an in-memory schema.Schema, without a database and
// without a sidecar state artifact (§3 decision 6). Delta diffs that folded
// state against the desired state and emits the next migration: ALTER TABLE
// ADD/DROP COLUMN, CREATE/DROP INDEX, CREATE/DROP TABLE, and a DROP + CREATE
// PROPERTY GRAPH (graphs are metadata, always recreated). Generate ties them
// together over a migration directory.
//
// It has no database dependency and compiles to WASM (SPEC.md §4.1).
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

// dropGraphStmt renders "DROP PROPERTY GRAPH <name>;".
func dropGraphStmt(name string) string {
	return fmt.Sprintf("DROP PROPERTY GRAPH %s;", quote(name))
}

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

// Generate writes the next migration for the desired schema into dir and
// returns its path.
//
// It folds the migrations already in dir into the prior schema (Fold), diffs
// that against desired, and emits the delta as NNNN_<name>.sql — where NNNN is
// one past the highest version present. When dir holds no migrations yet the
// prior schema is empty, so Generate emits the full initial migration as
// 0001_init.sql, identical to WriteInit.
//
// name is the goose descriptive suffix (e.g. "add_age"); it is sanitised to a
// safe snake_case token. When the folded and desired schemas are already
// identical Generate makes no change and returns ("", nil): there is nothing to
// migrate.
func Generate(dir string, desired *schema.Schema, name string) (string, error) {
	files, err := migrationFiles(dir)
	if err != nil {
		return "", err
	}

	if len(files) == 0 {
		// No history yet: this is the initial migration.
		return WriteInit(dir, desired)
	}

	contents := make([]string, len(files))
	for i, f := range files {
		data, err := os.ReadFile(f.path)
		if err != nil {
			return "", fmt.Errorf("migrate: read %s: %w", f.path, err)
		}
		contents[i] = string(data)
	}
	prior, err := FoldContent(contents)
	if err != nil {
		return "", err
	}

	up, down, changed := Delta(prior, desired)
	if !changed {
		return "", nil
	}

	version := files[len(files)-1].version + 1
	filename := fmt.Sprintf("%04d_%s.sql", version, sanitizeName(name))
	path := filepath.Join(dir, filename)
	content := "-- +goose Up\n" + up + "\n-- +goose Down\n" + down
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("migrate: write %s: %w", path, err)
	}
	return path, nil
}

// sanitizeName reduces an arbitrary migration name to a safe snake_case token so
// the emitted filename is a well-formed goose migration name. An empty result
// falls back to "delta".
func sanitizeName(name string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "delta"
	}
	return out
}

// downDDL builds the inverse of the init migration: drop the property graph
// first (it depends on the tables), then edge tables (they carry foreign keys
// into vertex tables), then vertex tables — each in reverse creation order.
// Indexes are dropped implicitly with their tables.
func downDDL(m *schema.Schema) string {
	var stmts []string
	stmts = append(stmts, dropGraphStmt(m.GraphName))
	for i := len(m.EdgeTables) - 1; i >= 0; i-- {
		stmts = append(stmts, fmt.Sprintf("DROP TABLE IF EXISTS %s;", quote(m.EdgeTables[i].Name)))
	}
	for i := len(m.VertexTables) - 1; i >= 0; i-- {
		stmts = append(stmts, fmt.Sprintf("DROP TABLE IF EXISTS %s;", quote(m.VertexTables[i].Name)))
	}
	return strings.Join(stmts, "\n") + "\n"
}
