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

// dropGraphStmt renders "DROP PROPERTY GRAPH IF EXISTS <name>;".
//
// IF EXISTS because the graph is dropped from more than one place now that
// migrations are split (gopgql#38): a graph-half migration drops it before
// recreating it, and an applier drops it out of band before letting the tables
// half alter columns the graph depends on. Either may find it already gone.
func dropGraphStmt(name string) string {
	return fmt.Sprintf("DROP PROPERTY GRAPH IF EXISTS %s;", quote(name))
}

// DropGraphSQL is the statement an applier runs to release the property graph's
// dependency on its tables before the tables half alters them. PostgreSQL
// refuses to drop or retype a column a live property graph exposes, which is
// why the combined migration always dropped the graph first — split across two
// directories, that step has to be taken explicitly.
func DropGraphSQL(name string) string { return dropGraphStmt(name) }

// InitFilename is the goose filename for the initial migration.
const InitFilename = "0001_init.sql"

// The two migration subdirectories. A directory's path is the only thing that
// says which half it holds — nothing is recorded in the files.
const (
	TablesDir = "tables"
	GraphDir  = "graph"
)

// VersionTable is the goose version table a half's directory records itself in.
//
// Each half needs its own: both directories start at 0001, so with goose's
// single shared table the tables half records version 1 and the graph half's
// 0001 is then considered already applied and silently skipped — the database
// ends up with tables and no property graph, and nothing reports a problem.
// Callers set this before applying a directory (migrate itself has no database
// dependency and never talks to goose — SPEC.md §4.1).
func VersionTable(subdir string) string {
	return "goose_db_version_" + subdir
}

// WriteInitSplit writes the initial migration for each half into its own
// subdirectory of root and returns the directories **in apply order**.
//
// The order is the point: a property graph references its tables, so applying
// the graph half first is refused by the database. Callers that apply
// migrations — the CLI, the integration suites — range over this slice rather
// than each deciding the order for themselves.
func WriteInitSplit(root string, m *schema.Schema) ([]string, error) {
	tablesDir := filepath.Join(root, TablesDir)
	graphDir := filepath.Join(root, GraphDir)
	if _, err := writeInit(tablesDir, InitTables(m)); err != nil {
		return nil, err
	}
	if _, err := writeInit(graphDir, InitGraph(m)); err != nil {
		return nil, err
	}
	return []string{tablesDir, graphDir}, nil
}

// InitTables renders the initial migration for the table half: the CREATE TABLE
// and CREATE INDEX statements, and a Down section dropping them in reverse.
func InitTables(m *schema.Schema) string {
	return section(generator.TablesDDL(m)+"\n", downTablesDDL(m))
}

// InitGraph renders the initial migration for the graph half: CREATE PROPERTY
// GRAPH, and a Down section dropping it. It touches no table.
func InitGraph(m *schema.Schema) string {
	return section(generator.GraphDDL(m)+"\n", dropGraphStmt(m.GraphName)+"\n")
}

// Init renders both halves as one migration: the tables, then the graph.
//
// Nothing writes migrations in this shape any more — GenerateTables and
// GenerateGraph write a file each, into their own directory. It is retained
// because it states the invariant the split rests on and is how the fold tests
// build a whole-schema history in one string: the two halves together are
// exactly what the combined migration used to be.
func Init(m *schema.Schema) string {
	return section(generator.DDL(m), downDDL(m))
}

// downDDL is Init's inverse: the graph first (the tables it references cannot
// be dropped under it), then the tables.
func downDDL(m *schema.Schema) string {
	return dropGraphStmt(m.GraphName) + "\n" + downTablesDDL(m)
}

// section wraps an up and a down body in goose's markers.
func section(up, down string) string {
	var b strings.Builder
	b.WriteString("-- +goose Up\n")
	b.WriteString(up)
	b.WriteString("\n-- +goose Down\n")
	b.WriteString(down)
	return b.String()
}

// writeInit writes content to dir/0001_init.sql, creating dir if necessary.
func writeInit(dir, content string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("migrate: create dir: %w", err)
	}
	path := filepath.Join(dir, InitFilename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("migrate: write %s: %w", path, err)
	}
	return path, nil
}

// GenerateSplit writes the next migration for each half into its own
// subdirectory of root and returns the paths written, in apply order. A half
// with nothing to migrate contributes an empty string, so the caller can tell
// "no change" from "wrote a file" per half.
func GenerateSplit(root string, desired *schema.Schema, name string) ([]string, error) {
	tablesPath, err := GenerateTables(filepath.Join(root, TablesDir), desired, name)
	if err != nil {
		return nil, err
	}
	graphPath, err := GenerateGraph(filepath.Join(root, GraphDir), desired, name)
	if err != nil {
		return nil, err
	}
	return []string{tablesPath, graphPath}, nil
}

// GenerateTables writes the next migration for the table half into dir and
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
func GenerateTables(dir string, desired *schema.Schema, name string) (string, error) {
	return generate(dir, desired, name, InitTables, DeltaTables)
}

// GenerateGraph writes the next migration for the graph half into dir.
//
// It never emits a statement about a table — not even when the folded prior
// state has none. That is the guarantee a graph-only setup rests on: the SDL
// describes the slice of a database that is surfaced as a graph, and what it
// does not mention may still exist and must be left alone (gopgql#38).
func GenerateGraph(dir string, desired *schema.Schema, name string) (string, error) {
	return generate(dir, desired, name, InitGraph, DeltaGraph)
}

// generate is the shared body of GenerateTables and GenerateGraph. The caller's
// choice of renderers is what makes a directory a tables or a graph directory —
// nothing is recorded in the files and nothing is read back out of them, so a
// directory cannot disagree with itself about what it owns.
func generate(
	dir string,
	desired *schema.Schema,
	name string,
	initFn func(*schema.Schema) string,
	deltaFn func(from, to *schema.Schema) (string, string, bool),
) (string, error) {
	files, err := migrationFiles(dir)
	if err != nil {
		return "", err
	}

	if len(files) == 0 {
		// No history yet: this is the initial migration.
		return writeInit(dir, initFn(desired))
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

	up, down, changed := deltaFn(prior, desired)
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

// downTablesDDL builds the inverse of the table half: edge tables first (they
// carry foreign keys into vertex tables), then vertex tables, each in reverse
// creation order. Indexes are dropped implicitly with their tables. The
// property graph is not mentioned — dropping it is the graph half's business.
func downTablesDDL(m *schema.Schema) string {
	var stmts []string
	for i := len(m.EdgeTables) - 1; i >= 0; i-- {
		stmts = append(stmts, fmt.Sprintf("DROP TABLE IF EXISTS %s;", quote(m.EdgeTables[i].Name)))
	}
	for i := len(m.VertexTables) - 1; i >= 0; i-- {
		stmts = append(stmts, fmt.Sprintf("DROP TABLE IF EXISTS %s;", quote(m.VertexTables[i].Name)))
	}
	return strings.Join(stmts, "\n") + "\n"
}
