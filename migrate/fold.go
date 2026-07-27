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
	"math"
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

// NextVersion is the version the next generation of migrations takes: one past
// the highest version either half under root already holds.
//
// The two halves share this counter rather than each numbering its own files,
// so tables/0002 and graph/0002 are always the same edit of the SDL. A half
// with nothing to emit for a generation leaves a gap at that number, which
// goose is content with — it only objects to a migration appearing *below* a
// version it has already applied.
//
// Numbering each half by its own file count instead lets the two drift, and the
// drift is not cosmetic. A generation that changes only the tables — an index,
// a UNIQUE — advances one half and not the other, and a later graph migration
// then carries a number lower than the tables migration it was generated
// against. Since the applier walks the halves by version (gopgql#38), that
// graph migration is paired with an earlier generation of tables and names a
// column those tables have not created yet.
//
// It is computed once per generation, before either half is written. Deriving
// it inside each half would cascade: the second half would read the number the
// first had just written and land a generation further on.
func NextVersion(root string) (int, error) {
	last := 0
	for _, subdir := range []string{TablesDir, GraphDir} {
		files, err := migrationFiles(filepath.Join(root, subdir))
		if err != nil {
			return 0, err
		}
		if n := len(files); n > 0 && files[n-1].version > last {
			last = files[n-1].version
		}
	}
	return last + 1, nil
}

// Versions lists the goose migration versions present in dir, ascending. A
// missing or empty directory has none: nothing has been generated for that half
// yet.
//
// It is exported because an applier walks the two halves' version lists in
// lockstep — tables/0001, graph/0001, tables/0002, … — rather than applying
// either directory whole (gopgql#38).
func Versions(dir string) ([]int64, error) {
	files, err := migrationFiles(dir)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(files))
	for _, f := range files {
		out = append(out, int64(f.version))
	}
	return out, nil
}

// Fold reads the migrations in dir and folds their Up sections into the schema
// model they collectively produce. An empty or missing directory yields a nil
// schema and no error: there is no prior state.
func Fold(dir string) (*schema.Schema, error) { return FoldUpTo(dir, math.MaxInt64) }

// FoldUpTo folds only the migrations at or below version: the state a database
// reaches once that generation of the half has been applied, and not a step
// further.
//
// Generations are the unit because the two halves are applied in lockstep
// (gopgql#38). The graph that has to come down before the tables half lands
// version n is the one graph migrations 1…n-1 built — folding the whole
// directory would name the graph the directory *ends* at, which does not exist
// yet. A version below the first migration yields a nil schema: nothing has
// been applied.
func FoldUpTo(dir string, version int64) (*schema.Schema, error) {
	files, err := migrationFiles(dir)
	if err != nil {
		return nil, err
	}
	var contents []string
	for _, f := range files {
		if int64(f.version) > version {
			break
		}
		data, err := os.ReadFile(f.path)
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", f.path, err)
		}
		contents = append(contents, string(data))
	}
	if len(contents) == 0 {
		return nil, nil
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
		Unique:     c.Unique,
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
	case *ddl.AddConstraint:
		// gopgql only ever emits a single-column UNIQUE, which folds back onto
		// the column it constrains (SPEC.md §7 → M6).
		return f.setUnique(s.Name, a, true)
	case *ddl.DropConstraint:
		return f.setUnique(s.Name, &ddl.AddConstraint{Name: a.Name, Kind: "UNIQUE"}, false)
	default:
		return fmt.Errorf("ALTER TABLE %s: unsupported action %T", s.Name, s.Action)
	}
}

func (f *folder) applyCreateIndex(s *ddl.CreateIndexStmt) error {
	if _, exists := f.indexes[s.Name]; !exists {
		f.idxOrder = append(f.idxOrder, s.Name)
	}
	f.indexes[s.Name] = schema.Index{
		Name: s.Name, Table: s.Table, Columns: s.Columns, Method: s.Method,
	}
	return nil
}

// setUnique folds a UNIQUE constraint statement back onto its column. The
// constraint carries the column list when it is added; a DROP carries only the
// name, so the column is recovered from the naming convention gopgql and
// PostgreSQL share (schema.UniqueConstraintName).
func (f *folder) setUnique(table string, c *ddl.AddConstraint, unique bool) error {
	cols := f.cols[table]
	if cols == nil {
		return fmt.Errorf("ALTER TABLE %s: constraint %s on an unknown table", table, c.Name)
	}
	target := ""
	switch {
	case len(c.Columns) == 1:
		target = c.Columns[0]
	case strings.HasPrefix(c.Name, table+"_") && strings.HasSuffix(c.Name, "_key"):
		target = strings.TrimSuffix(strings.TrimPrefix(c.Name, table+"_"), "_key")
	default:
		return fmt.Errorf("ALTER TABLE %s: cannot tell which column constraint %q covers; "+
			"gopgql names single-column UNIQUE constraints <table>_<column>_key", table, c.Name)
	}
	for i := range cols {
		if cols[i].Name == target {
			cols[i].Unique = unique
			f.cols[table] = cols
			return nil
		}
	}
	return fmt.Errorf("ALTER TABLE %s: constraint %s covers unknown column %q", table, c.Name, target)
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

// buildTablesOnly assembles a schema from the CREATE TABLE statements alone,
// for a directory that never declared a property graph. It carries no labels,
// no properties and no edge metadata, because nothing in such a directory says
// what they would be.
func (f *folder) buildTablesOnly() *schema.Schema {
	m := &schema.Schema{}
	names := make([]string, 0, len(f.cols))
	for name := range f.cols {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m.VertexTables = append(m.VertexTables, schema.VertexTable{Name: name, Columns: f.cols[name]})
	}
	m.Indexes = f.indexList()
	return m
}

// build assembles the folded schema from the accumulated tables, indexes and
// graph. The graph classifies every table as a vertex or an edge and supplies
// labels, property lists and edge key metadata; the CREATE TABLE statements
// supply the columns.
func (f *folder) build() (*schema.Schema, error) {
	if f.graph == nil {
		// A tables-only migration directory (gopgql#38) has no CREATE PROPERTY
		// GRAPH to classify its tables with, and that is not an error — the
		// graph half is generated and applied somewhere else, or by nobody.
		// Return the tables as they were created; DeltaTables classifies them
		// against the desired schema, which is the only place the roles are
		// known. Vertex is the neutral bucket here, not a claim.
		return f.buildTablesOnly(), nil
	}
	m := &schema.Schema{GraphName: f.graph.Name}
	for _, v := range f.graph.Vertices {
		// A missing CREATE TABLE is not an error: a graph-only directory
		// (gopgql#38) declares a property graph over tables owned elsewhere and
		// never creates them. The columns stay nil — the graph half's diff
		// compares the graph statement, which is self-describing, and the SDL
		// is a description of the slice being surfaced, not an inventory of the
		// database.
		cols := f.cols[v.Table]
		var extra []schema.LabelProperties
		for _, l := range v.ExtraLabels {
			extra = append(extra, schema.LabelProperties{Label: l.Label, Properties: l.Properties})
		}
		m.VertexTables = append(m.VertexTables, schema.VertexTable{
			Name:        v.Table,
			Label:       v.Label,
			Columns:     cols,
			Properties:  v.Properties,
			ExtraLabels: extra,
		})
	}
	for _, e := range f.graph.Edges {
		cols := f.cols[e.Table]
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
