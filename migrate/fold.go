// Fold reconstructs a schema.Schema from gopgql's own prior goose migrations.
//
// It is an interpreter over the canonical statement set gopgql emits — CREATE
// TABLE, ALTER TABLE ADD/DROP COLUMN, CREATE/DROP INDEX, CREATE/DROP PROPERTY
// GRAPH — not a general DDL parser (SPEC.md §7 → M2). The reading is done by the
// internal/ddl lexer+parser, which turns each statement into a typed AST node;
// this file is the interpreter that walks those nodes. Folding replays the
// -- +goose Up sections of every migration in order, so the resulting model is
// the state a database reaches after applying them all. That is what lets the
// generator emit a delta with no database and no sidecar state file (§3
// decision 6).
//
// The emitter (generator + this package's Init/Delta) and this interpreter are
// two halves of one contract: whatever gopgql writes, Fold must read back into
// the same model. The fold_test round-trips guard that; the M2 integration
// suite proves it against a real database.
package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/lega4e/gopgql/internal/ddl"
	"github.com/lega4e/gopgql/schema"
)

// migFile is one migration file on disk, with its parsed version number.
type migFile struct {
	path    string
	version int
}

// versionRe matches a goose migration filename: a leading integer version, an
// underscore, a name, and the .sql extension.
var versionRe = regexp.MustCompile(`^(\d+)_.*\.sql$`)

// migrationFiles lists the goose migration files in dir, sorted ascending by
// version. A missing directory yields no files (nothing has been generated).
func migrationFiles(dir string) ([]migFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("migrate: read dir %s: %w", dir, err)
	}
	var files []migFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := versionRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		files = append(files, migFile{path: filepath.Join(dir, e.Name()), version: v})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	return files, nil
}

// Fold reads the migrations in dir and folds their Up sections into the schema
// model they collectively produce. An empty or missing directory yields a nil
// schema and no error: there is no prior state.
func Fold(dir string) (*schema.Schema, error) {
	files, err := migrationFiles(dir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	contents := make([]string, len(files))
	for i, f := range files {
		data, err := os.ReadFile(f.path)
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", f.path, err)
		}
		contents[i] = string(data)
	}
	return FoldContent(contents)
}

// FoldContent folds an ordered list of goose migration file contents into the
// schema model they produce. It is the database-free core shared by Fold (which
// reads files) and the WASM playground (which folds in-memory content).
func FoldContent(contents []string) (*schema.Schema, error) {
	f := newFolder()
	for i, content := range contents {
		stmts, err := ddl.Parse(upSection(content))
		if err != nil {
			return nil, fmt.Errorf("migrate: fold migration %d: %w", i+1, err)
		}
		for _, stmt := range stmts {
			if err := f.apply(stmt); err != nil {
				return nil, fmt.Errorf("migrate: fold migration %d: %w", i+1, err)
			}
		}
	}
	return f.build()
}

// folder accumulates the effect of applying migration statements in order.
type folder struct {
	// cols maps a table name to its columns in physical (creation/append) order.
	cols map[string][]schema.Column
	// indexes maps an index name to its definition; idxOrder preserves the order
	// they were created so the folded schema is deterministic.
	indexes  map[string]schema.Index
	idxOrder []string
	// graph is the most recent CREATE PROPERTY GRAPH, which classifies tables as
	// vertices or edges and carries labels, properties and edge key metadata.
	graph *ddl.CreatePropertyGraphStmt
}

func newFolder() *folder {
	return &folder{cols: map[string][]schema.Column{}, indexes: map[string]schema.Index{}}
}

// apply interprets a single parsed statement.
func (f *folder) apply(stmt ddl.Statement) error {
	switch s := stmt.(type) {
	case *ddl.CreateTableStmt:
		return f.applyCreateTable(s)
	case *ddl.AlterTableStmt:
		return f.applyAlterTable(s)
	case *ddl.CreateIndexStmt:
		return f.applyCreateIndex(s)
	case *ddl.DropIndexStmt:
		f.removeIndex(s.Name)
		return nil
	case *ddl.DropTableStmt:
		return f.applyDropTable(s)
	case *ddl.CreatePropertyGraphStmt:
		f.graph = s
		return nil
	case *ddl.DropPropertyGraphStmt:
		// The following CREATE PROPERTY GRAPH re-establishes the graph; a bare
		// drop just clears it.
		f.graph = nil
		return nil
	default:
		return fmt.Errorf("unrecognised statement %T (fold interprets gopgql's own DDL only)", stmt)
	}
}

// column converts a parsed column definition to the schema model's column.
func column(c ddl.ColumnDef) schema.Column {
	col := schema.Column{
		Name:       c.Name,
		Type:       c.Type,
		Array:      c.Array,
		NotNull:    c.NotNull,
		PrimaryKey: c.PrimaryKey,
		Default:    c.Default,
	}
	if c.References != nil {
		col.References = &schema.Reference{Table: c.References.Table, Column: c.References.Column}
	}
	return col
}

func (f *folder) applyCreateTable(s *ddl.CreateTableStmt) error {
	cols := make([]schema.Column, 0, len(s.Columns))
	for _, c := range s.Columns {
		cols = append(cols, column(c))
	}
	f.cols[s.Name] = cols
	return nil
}

func (f *folder) applyAlterTable(s *ddl.AlterTableStmt) error {
	switch a := s.Action.(type) {
	case *ddl.AddColumn:
		f.cols[s.Name] = append(f.cols[s.Name], column(a.Column))
		return nil
	case *ddl.DropColumn:
		f.cols[s.Name] = removeColumn(f.cols[s.Name], a.Name)
		return nil
	default:
		return fmt.Errorf("ALTER TABLE %s: unsupported action %T", s.Name, s.Action)
	}
}

func (f *folder) applyCreateIndex(s *ddl.CreateIndexStmt) error {
	if _, exists := f.indexes[s.Name]; !exists {
		f.idxOrder = append(f.idxOrder, s.Name)
	}
	f.indexes[s.Name] = schema.Index{Name: s.Name, Table: s.Table, Columns: s.Columns}
	return nil
}

func (f *folder) applyDropTable(s *ddl.DropTableStmt) error {
	delete(f.cols, s.Name)
	// Indexes on a dropped table go with it.
	for _, idx := range f.indexList() {
		if idx.Table == s.Name {
			f.removeIndex(idx.Name)
		}
	}
	return nil
}

func (f *folder) removeIndex(name string) {
	if _, ok := f.indexes[name]; !ok {
		return
	}
	delete(f.indexes, name)
	for i, n := range f.idxOrder {
		if n == name {
			f.idxOrder = append(f.idxOrder[:i], f.idxOrder[i+1:]...)
			break
		}
	}
}

func (f *folder) indexList() []schema.Index {
	if len(f.idxOrder) == 0 {
		// Match the generator, which leaves Indexes nil when there are none,
		// so folded and freshly built schemas compare equal.
		return nil
	}
	out := make([]schema.Index, 0, len(f.idxOrder))
	for _, n := range f.idxOrder {
		out = append(out, f.indexes[n])
	}
	return out
}

// build assembles the folded schema from the accumulated tables, indexes and
// graph. The graph classifies every table as a vertex or an edge and supplies
// labels, property lists and edge key metadata; the CREATE TABLE statements
// supply the columns.
func (f *folder) build() (*schema.Schema, error) {
	if f.graph == nil {
		return nil, fmt.Errorf("migrate: folded migrations declare no property graph")
	}
	m := &schema.Schema{GraphName: f.graph.Name}
	for _, v := range f.graph.Vertices {
		cols, ok := f.cols[v.Table]
		if !ok {
			return nil, fmt.Errorf("migrate: graph vertex table %q was never created", v.Table)
		}
		m.VertexTables = append(m.VertexTables, schema.VertexTable{
			Name:       v.Table,
			Label:      v.Label,
			Columns:    cols,
			Properties: v.Properties,
		})
	}
	for _, e := range f.graph.Edges {
		cols, ok := f.cols[e.Table]
		if !ok {
			return nil, fmt.Errorf("migrate: graph edge table %q was never created", e.Table)
		}
		m.EdgeTables = append(m.EdgeTables, schema.EdgeTable{
			Name:        e.Table,
			Label:       e.Label,
			Columns:     cols,
			SourceKey:   e.SourceKey,
			SourceTable: e.SourceTable,
			SourceRef:   e.SourceRef,
			DestKey:     e.DestKey,
			DestTable:   e.DestTable,
			DestRef:     e.DestRef,
			Properties:  e.Properties,
		})
	}
	m.Indexes = f.indexList()
	return m, nil
}

// upSection returns the -- +goose Up portion of a goose migration file: the
// lines between the Up marker and the Down marker (or end of file). Comment
// lines are dropped so only executable statements remain.
func upSection(content string) string {
	var b strings.Builder
	inUp := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "-- +goose") {
			inUp = strings.EqualFold(trimmed, "-- +goose Up")
			continue
		}
		if !inUp {
			continue
		}
		// Strip any trailing line comment; gopgql emits none, but be tolerant.
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// removeColumn returns cols without the column named name.
func removeColumn(cols []schema.Column, name string) []schema.Column {
	out := cols[:0:0]
	for _, c := range cols {
		if c.Name != name {
			out = append(out, c)
		}
	}
	return out
}
