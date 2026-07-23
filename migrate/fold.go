// Fold reconstructs a schema.Schema from gopgql's own prior goose migrations.
//
// It is an interpreter over the canonical statement set gopgql emits — CREATE
// TABLE, ALTER TABLE ADD/DROP COLUMN, CREATE/DROP INDEX, CREATE/DROP PROPERTY
// GRAPH — not a general DDL parser (SPEC.md §7 → M2). Folding replays the
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
		for _, stmt := range statements(upSection(content)) {
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
	graph *graphModel
}

func newFolder() *folder {
	return &folder{cols: map[string][]schema.Column{}, indexes: map[string]schema.Index{}}
}

// apply interprets a single canonical statement.
func (f *folder) apply(stmt string) error {
	norm := strings.Join(strings.Fields(stmt), " ")
	if norm == "" {
		return nil
	}
	upper := strings.ToUpper(norm)
	switch {
	case strings.HasPrefix(upper, "CREATE TABLE "):
		return f.applyCreateTable(norm)
	case strings.HasPrefix(upper, "ALTER TABLE "):
		return f.applyAlterTable(norm)
	case strings.HasPrefix(upper, "CREATE INDEX "):
		return f.applyCreateIndex(norm)
	case strings.HasPrefix(upper, "DROP INDEX "):
		return f.applyDropIndex(norm)
	case strings.HasPrefix(upper, "DROP TABLE "):
		return f.applyDropTable(norm)
	case strings.HasPrefix(upper, "CREATE PROPERTY GRAPH "):
		return f.applyCreateGraph(norm)
	case strings.HasPrefix(upper, "DROP PROPERTY GRAPH "):
		// The following CREATE PROPERTY GRAPH re-establishes the graph; a bare
		// drop just clears it.
		f.graph = nil
		return nil
	default:
		return fmt.Errorf("unrecognised statement %q (fold interprets gopgql's own DDL only)", norm)
	}
}

func (f *folder) applyCreateTable(norm string) error {
	rest := norm[len("CREATE TABLE "):]
	name, rest := readIdent(rest)
	if name == "" {
		return fmt.Errorf("CREATE TABLE missing table name")
	}
	inner, ok := betweenParens(rest)
	if !ok {
		return fmt.Errorf("CREATE TABLE %s missing column list", name)
	}
	var cols []schema.Column
	for _, part := range splitTopLevel(inner, ',') {
		if c, isCol := parseColumn(part); isCol {
			cols = append(cols, c)
		}
	}
	f.cols[name] = cols
	return nil
}

func (f *folder) applyAlterTable(norm string) error {
	rest := norm[len("ALTER TABLE "):]
	name, rest := readIdent(rest)
	rest = strings.TrimSpace(rest)
	upper := strings.ToUpper(rest)
	switch {
	case strings.HasPrefix(upper, "ADD COLUMN "):
		c, isCol := parseColumn(rest[len("ADD COLUMN "):])
		if !isCol {
			return fmt.Errorf("ALTER TABLE %s ADD COLUMN: unparseable column", name)
		}
		f.cols[name] = append(f.cols[name], c)
		return nil
	case strings.HasPrefix(upper, "DROP COLUMN "):
		col, _ := readIdent(rest[len("DROP COLUMN "):])
		f.cols[name] = removeColumn(f.cols[name], col)
		return nil
	default:
		return fmt.Errorf("ALTER TABLE %s: unsupported action %q", name, rest)
	}
}

func (f *folder) applyCreateIndex(norm string) error {
	rest := norm[len("CREATE INDEX "):]
	name, rest := readIdent(rest)
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(strings.ToUpper(rest), "ON ") {
		return fmt.Errorf("CREATE INDEX %s missing ON clause", name)
	}
	table, rest := readIdent(strings.TrimSpace(rest[len("ON "):]))
	inner, ok := betweenParens(rest)
	if !ok {
		return fmt.Errorf("CREATE INDEX %s missing column list", name)
	}
	var cols []string
	for _, part := range splitTopLevel(inner, ',') {
		id, _ := readIdent(part)
		if id != "" {
			cols = append(cols, id)
		}
	}
	if _, exists := f.indexes[name]; !exists {
		f.idxOrder = append(f.idxOrder, name)
	}
	f.indexes[name] = schema.Index{Name: name, Table: table, Columns: cols}
	return nil
}

func (f *folder) applyDropIndex(norm string) error {
	rest := strings.TrimSpace(norm[len("DROP INDEX "):])
	rest = trimIfExists(rest)
	name, _ := readIdent(rest)
	f.removeIndex(name)
	return nil
}

func (f *folder) applyDropTable(norm string) error {
	rest := strings.TrimSpace(norm[len("DROP TABLE "):])
	rest = trimIfExists(rest)
	name, _ := readIdent(rest)
	delete(f.cols, name)
	// Indexes on a dropped table go with it.
	for _, idx := range f.indexList() {
		if idx.Table == name {
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

func (f *folder) applyCreateGraph(norm string) error {
	g, err := parseGraph(norm)
	if err != nil {
		return err
	}
	f.graph = g
	return nil
}

// build assembles the folded schema from the accumulated tables, indexes and
// graph. The graph classifies every table as a vertex or an edge and supplies
// labels, property lists and edge key metadata; the CREATE TABLE statements
// supply the columns.
func (f *folder) build() (*schema.Schema, error) {
	if f.graph == nil {
		return nil, fmt.Errorf("migrate: folded migrations declare no property graph")
	}
	m := &schema.Schema{GraphName: f.graph.name}
	for _, v := range f.graph.vertices {
		cols, ok := f.cols[v.table]
		if !ok {
			return nil, fmt.Errorf("migrate: graph vertex table %q was never created", v.table)
		}
		m.VertexTables = append(m.VertexTables, schema.VertexTable{
			Name:       v.table,
			Label:      v.label,
			Columns:    cols,
			Properties: v.properties,
		})
	}
	for _, e := range f.graph.edges {
		cols, ok := f.cols[e.table]
		if !ok {
			return nil, fmt.Errorf("migrate: graph edge table %q was never created", e.table)
		}
		m.EdgeTables = append(m.EdgeTables, schema.EdgeTable{
			Name:        e.table,
			Label:       e.label,
			Columns:     cols,
			SourceKey:   e.sourceKey,
			SourceTable: e.sourceTable,
			SourceRef:   e.sourceRef,
			DestKey:     e.destKey,
			DestTable:   e.destTable,
			DestRef:     e.destRef,
			Properties:  e.properties,
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

// statements splits a SQL block into individual statements on top-level
// semicolons. gopgql's DDL contains no semicolons inside statements, so a
// depth-aware split is exact.
func statements(sql string) []string {
	var out []string
	for _, part := range splitTopLevel(sql, ';') {
		if strings.TrimSpace(part) != "" {
			out = append(out, part)
		}
	}
	return out
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

// trimIfExists drops a leading "IF EXISTS " (case-insensitive).
func trimIfExists(s string) string {
	if strings.HasPrefix(strings.ToUpper(s), "IF EXISTS ") {
		return strings.TrimSpace(s[len("IF EXISTS "):])
	}
	return s
}
