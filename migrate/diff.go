package migrate

import (
	"fmt"
	"strings"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/internal/pgident"
	"github.com/lega4e/gopgql/schema"
)

// Delta diffs the folded prior schema against the desired schema and renders
// the -- +goose Up and -- +goose Down bodies of the next migration. changed is
// false when the two schemas are already equivalent, in which case there is
// nothing to migrate and up and down are empty.
//
// The Up body transforms prior → desired; the Down body is its exact inverse,
// transforming desired → prior. Property graphs are metadata: every delta drops
// and recreates the graph (SPEC.md §7 → M2), so Up recreates the desired graph
// and Down recreates the prior one. Structural steps are ordered so foreign-key
// dependencies always hold — the graph is dropped before its tables change,
// vertices are created before the edges that reference them, and drops run in
// the reverse order of creates.
func Delta(from, to *schema.Schema) (up, down string, changed bool) {
	d := diffSchemas(from, to)

	upStmts := d.upStatements(from, to)
	downStmts := d.downStatements(from, to)

	graphChanged := generator.GraphDDL(from) != generator.GraphDDL(to)
	if !d.structural() && !graphChanged {
		return "", "", false
	}

	return joinStmts(upStmts), joinStmts(downStmts), true
}

// schemaDiff is the set of structural differences between two schemas.
type schemaDiff struct {
	addedVertices   []string // names of vertex tables only in `to`
	droppedVertices []string // names of vertex tables only in `from`
	addedEdges      []string // names of edge tables only in `to`
	droppedEdges    []string // names of edge tables only in `from`

	// perTable column changes, keyed by a vertex table present in both schemas.
	addedColumns   map[string][]schema.Column // columns to ADD (from `to`)
	droppedColumns map[string][]schema.Column // columns to DROP (from `from`)
	commonVertices []string                   // stable order for the maps above

	addedIndexes   []schema.Index // standalone index adds (table in both)
	droppedIndexes []schema.Index // standalone index drops (table in both)

	// UNIQUE constraints gained / lost by a column that exists in both schemas
	// (SPEC.md §7 → M6). A column that is itself added or dropped carries its
	// constraint with it, so it never appears here.
	addedUniques   []uniqueChange
	droppedUniques []uniqueChange
}

// uniqueChange is one column's UNIQUE constraint, named the way PostgreSQL names
// an implicit one so the ADD and DROP paths agree.
type uniqueChange struct {
	Table  string
	Column string
}

func (u uniqueChange) name() string { return schema.UniqueConstraintName(u.Table, u.Column) }

func (d *schemaDiff) structural() bool {
	return len(d.addedVertices) > 0 || len(d.droppedVertices) > 0 ||
		len(d.addedEdges) > 0 || len(d.droppedEdges) > 0 ||
		len(d.addedIndexes) > 0 || len(d.droppedIndexes) > 0 ||
		len(d.addedUniques) > 0 || len(d.droppedUniques) > 0 ||
		d.hasColumnChanges()
}

func (d *schemaDiff) hasColumnChanges() bool {
	for _, t := range d.commonVertices {
		if len(d.addedColumns[t]) > 0 || len(d.droppedColumns[t]) > 0 {
			return true
		}
	}
	return false
}

func diffSchemas(from, to *schema.Schema) *schemaDiff {
	d := &schemaDiff{
		addedColumns:   map[string][]schema.Column{},
		droppedColumns: map[string][]schema.Column{},
	}

	fromV := vertexIndex(from)
	toV := vertexIndex(to)
	// Vertex tables added / dropped, and column diffs for those in both.
	for _, vt := range to.VertexTables {
		if _, ok := fromV[vt.Name]; !ok {
			d.addedVertices = append(d.addedVertices, vt.Name)
		}
	}
	for _, vt := range from.VertexTables {
		if _, ok := toV[vt.Name]; !ok {
			d.droppedVertices = append(d.droppedVertices, vt.Name)
			continue
		}
	}
	for _, vt := range from.VertexTables {
		toVT, ok := toV[vt.Name]
		if !ok {
			continue
		}
		added, dropped := columnDiff(vt.Columns, toVT.Columns)
		if len(added) > 0 || len(dropped) > 0 {
			d.commonVertices = append(d.commonVertices, vt.Name)
			d.addedColumns[vt.Name] = added
			d.droppedColumns[vt.Name] = dropped
		}
		gained, lost := uniqueDiff(vt.Name, vt.Columns, toVT.Columns)
		d.addedUniques = append(d.addedUniques, gained...)
		d.droppedUniques = append(d.droppedUniques, lost...)
	}

	fromE := edgeIndex(from)
	toE := edgeIndex(to)
	for _, et := range to.EdgeTables {
		if _, ok := fromE[et.Name]; !ok {
			d.addedEdges = append(d.addedEdges, et.Name)
		}
	}
	for _, et := range from.EdgeTables {
		if _, ok := toE[et.Name]; !ok {
			d.droppedEdges = append(d.droppedEdges, et.Name)
		}
	}

	// Standalone index changes: only for tables present in both schemas. An
	// index on an added or dropped table travels with that table's CREATE/DROP.
	addedTables := nameSet(d.addedVertices, d.addedEdges)
	droppedTables := nameSet(d.droppedVertices, d.droppedEdges)
	fromIdx := indexByName(from)
	toIdx := indexByName(to)
	for _, idx := range to.Indexes {
		prior, ok := fromIdx[idx.Name]
		// An index whose definition moved — different columns or a different
		// access method — is dropped and recreated: PostgreSQL has no ALTER for
		// either (SPEC.md §7 → M6).
		if ok && sameIndex(prior, idx) {
			continue
		}
		if addedTables[idx.Table] {
			continue
		}
		d.addedIndexes = append(d.addedIndexes, idx)
	}
	for _, idx := range from.Indexes {
		desired, ok := toIdx[idx.Name]
		if ok && sameIndex(idx, desired) {
			continue
		}
		if droppedTables[idx.Table] {
			continue
		}
		d.droppedIndexes = append(d.droppedIndexes, idx)
	}
	return d
}

// sameIndex reports whether two same-named indexes are identical in definition.
func sameIndex(a, b schema.Index) bool {
	if a.Table != b.Table || a.Method != b.Method || len(a.Columns) != len(b.Columns) {
		return false
	}
	for i := range a.Columns {
		if a.Columns[i] != b.Columns[i] {
			return false
		}
	}
	return true
}

// uniqueDiff reports the UNIQUE constraints a table gains and loses on columns
// present in both schemas.
func uniqueDiff(table string, fromCols, toCols []schema.Column) (gained, lost []uniqueChange) {
	prior := map[string]schema.Column{}
	for _, c := range fromCols {
		prior[c.Name] = c
	}
	for _, c := range toCols {
		p, ok := prior[c.Name]
		if !ok {
			continue // a new column carries its own constraint in ADD COLUMN
		}
		switch {
		case c.Unique && !p.Unique:
			gained = append(gained, uniqueChange{Table: table, Column: c.Name})
		case !c.Unique && p.Unique:
			lost = append(lost, uniqueChange{Table: table, Column: c.Name})
		}
	}
	return gained, lost
}

// upStatements renders the forward migration: prior → desired.
func (d *schemaDiff) upStatements(from, to *schema.Schema) []string {
	var s []string
	if from.GraphName != "" {
		s = append(s, dropGraphStmt(from.GraphName))
	}
	for _, idx := range d.droppedIndexes {
		s = append(s, dropIndexStmt(idx.Name))
	}
	for _, u := range d.droppedUniques {
		s = append(s, dropConstraintStmt(u))
	}
	for _, t := range d.commonVertices {
		for _, c := range d.droppedColumns[t] {
			s = append(s, dropColumnStmt(t, c.Name))
		}
	}
	for _, name := range d.droppedEdges {
		s = append(s, dropTableStmt(name))
	}
	for _, name := range d.droppedVertices {
		s = append(s, dropTableStmt(name))
	}
	for _, name := range d.addedVertices {
		s = append(s, tableCreateStmts(to, name)...)
	}
	for _, name := range d.addedEdges {
		s = append(s, tableCreateStmts(to, name)...)
	}
	for _, t := range d.commonVertices {
		for _, c := range d.addedColumns[t] {
			s = append(s, addColumnStmt(t, c))
		}
	}
	for _, u := range d.addedUniques {
		s = append(s, addConstraintStmt(u))
	}
	for _, idx := range d.addedIndexes {
		s = append(s, generator.IndexDDL(idx))
	}
	s = append(s, generator.GraphDDL(to))
	return s
}

// downStatements renders the reverse migration: desired → prior. It is the
// mirror of upStatements — every add becomes a drop and every drop is recreated
// from the prior schema.
func (d *schemaDiff) downStatements(from, to *schema.Schema) []string {
	var s []string
	if to.GraphName != "" {
		s = append(s, dropGraphStmt(to.GraphName))
	}
	for _, idx := range d.addedIndexes {
		s = append(s, dropIndexStmt(idx.Name))
	}
	for _, u := range d.addedUniques {
		s = append(s, dropConstraintStmt(u))
	}
	for _, t := range d.commonVertices {
		for _, c := range d.addedColumns[t] {
			s = append(s, dropColumnStmt(t, c.Name))
		}
	}
	for _, name := range d.addedEdges {
		s = append(s, dropTableStmt(name))
	}
	for _, name := range d.addedVertices {
		s = append(s, dropTableStmt(name))
	}
	for _, name := range d.droppedVertices {
		s = append(s, tableCreateStmts(from, name)...)
	}
	for _, name := range d.droppedEdges {
		s = append(s, tableCreateStmts(from, name)...)
	}
	for _, t := range d.commonVertices {
		for _, c := range d.droppedColumns[t] {
			s = append(s, addColumnStmt(t, c))
		}
	}
	for _, u := range d.droppedUniques {
		s = append(s, addConstraintStmt(u))
	}
	for _, idx := range d.droppedIndexes {
		s = append(s, generator.IndexDDL(idx))
	}
	s = append(s, generator.GraphDDL(from))
	return s
}

// columnDiff compares two column lists by name. added is the columns present
// only in to (rendered with their desired definition); dropped is the columns
// present only in from (rendered with their prior definition so Down can
// restore them). Type changes on a shared column are out of M2's scope and are
// left untouched.
func columnDiff(fromCols, toCols []schema.Column) (added, dropped []schema.Column) {
	fromSet := map[string]bool{}
	for _, c := range fromCols {
		fromSet[c.Name] = true
	}
	toSet := map[string]bool{}
	for _, c := range toCols {
		toSet[c.Name] = true
	}
	for _, c := range toCols {
		if !fromSet[c.Name] {
			added = append(added, c)
		}
	}
	for _, c := range fromCols {
		if !toSet[c.Name] {
			dropped = append(dropped, c)
		}
	}
	return added, dropped
}

// tableCreateStmts renders the CREATE TABLE for the named table in m, followed
// by the CREATE INDEX for each index m declares on it (in m's index order) — so
// a table created in a delta arrives with its mandatory destination-key index
// (SPEC.md §5.3 invariant 2).
func tableCreateStmts(m *schema.Schema, name string) []string {
	var out []string
	for i := range m.VertexTables {
		if m.VertexTables[i].Name == name {
			out = append(out, generator.VertexTableDDL(&m.VertexTables[i]))
		}
	}
	for i := range m.EdgeTables {
		if m.EdgeTables[i].Name == name {
			out = append(out, generator.EdgeTableDDL(&m.EdgeTables[i]))
		}
	}
	for _, idx := range m.Indexes {
		if idx.Table == name {
			out = append(out, generator.IndexDDL(idx))
		}
	}
	return out
}

func addColumnStmt(table string, c schema.Column) string {
	return fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;", quote(table), generator.ColumnDDL(c))
}

func dropColumnStmt(table, col string) string {
	return fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s;", quote(table), quote(col))
}

func dropTableStmt(name string) string {
	return fmt.Sprintf("DROP TABLE IF EXISTS %s;", quote(name))
}

// addConstraintStmt / dropConstraintStmt render a single-column UNIQUE
// constraint change. The name matches PostgreSQL's own implicit naming, so a
// constraint created inline by CREATE TABLE can later be dropped by name.
func addConstraintStmt(u uniqueChange) string {
	return fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s UNIQUE (%s);",
		pgident.Quote(u.Table), pgident.Quote(u.name()), pgident.Quote(u.Column))
}

func dropConstraintStmt(u uniqueChange) string {
	return fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s;",
		pgident.Quote(u.Table), pgident.Quote(u.name()))
}

func dropIndexStmt(name string) string {
	return fmt.Sprintf("DROP INDEX IF EXISTS %s;", quote(name))
}

// joinStmts joins statements with blank lines and a trailing newline, matching
// the layout of the init migration.
func joinStmts(stmts []string) string {
	return strings.Join(stmts, "\n\n") + "\n"
}

func vertexIndex(m *schema.Schema) map[string]schema.VertexTable {
	out := make(map[string]schema.VertexTable, len(m.VertexTables))
	for _, vt := range m.VertexTables {
		out[vt.Name] = vt
	}
	return out
}

func edgeIndex(m *schema.Schema) map[string]schema.EdgeTable {
	out := make(map[string]schema.EdgeTable, len(m.EdgeTables))
	for _, et := range m.EdgeTables {
		out[et.Name] = et
	}
	return out
}

func indexByName(m *schema.Schema) map[string]schema.Index {
	out := make(map[string]schema.Index, len(m.Indexes))
	for _, idx := range m.Indexes {
		out[idx.Name] = idx
	}
	return out
}

func nameSet(groups ...[]string) map[string]bool {
	out := map[string]bool{}
	for _, g := range groups {
		for _, n := range g {
			out[n] = true
		}
	}
	return out
}
