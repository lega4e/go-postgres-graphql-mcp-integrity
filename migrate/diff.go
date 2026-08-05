package migrate

import (
	"fmt"
	"strings"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/internal/pgident"
	"github.com/lega4e/gopgql/schema"
)

// Delta diffs the folded prior schema against the desired schema and renders one
// *combined* migration: the graph dropped, the tables migrated, the graph
// recreated. changed is false when the two schemas are already equivalent.
//
// Nothing emits migrations in this shape any more — a generation is a run of
// single-purpose files (design D2). Like [Init], it is retained as the reference
// the split is measured against: the integration suite applies it to a fresh
// database and asserts the sequence reaches the same state.
//
// The Up body transforms prior → desired; the Down body is its exact inverse,
// transforming desired → prior. Property graphs are metadata: every delta drops
// and recreates the graph (SPEC.md §7 → M2), so Up recreates the desired graph
// and Down recreates the prior one. Structural steps are ordered so foreign-key
// dependencies always hold — the graph is dropped before its tables change,
// vertices are created before the edges that reference them, and drops run in
// the reverse order of creates.
// Renames are resolved before the diff runs, not after: the plan is applied to a
// copy of the prior state, and everything downstream diffs against a prior state
// in which the rename has already happened. A rename therefore *is* the
// suppression of the drop+add pair (task 4.4) rather than something layered over
// it (see rename.go).
func Delta(from, to *schema.Schema) (up, down string, changed bool) {
	// The two halves see different schemas, and they have to. The graph half
	// sees every element including the unmanaged ones — a @readonly type is in
	// the property graph, that is the whole of what it is. The table half sees
	// only what gopgql owns, so nothing it emits can touch a table it must not.
	graphFrom, graphTo := from, to
	from, to = withoutUnmanaged(from, to)

	plan := planRenames(from, to)
	prior, stale := applyRenames(from, plan)

	d := diffSchemas(prior, to)
	d.renames = plan
	d.priorGraph = graphFrom
	d.droppedConstraints = append(staleAsConstraints(stale), d.droppedConstraints...)

	upStmts := d.upStatements(graphFrom, graphTo, to)
	downStmts := d.downStatements(graphTo, prior)

	graphChanged := generator.GraphDDL(graphFrom) != generator.GraphDDL(graphTo)
	if !d.structural() && !graphChanged {
		return "", "", false
	}

	return joinStmts(upStmts), joinStmts(downStmts), true
}

// DeltaTables renders the table half of a generation: the structural difference
// between the two schemas, and nothing about the property graph.
//
// The graph comparison is skipped entirely rather than being found to be equal.
// A history generated with --no-graph contains no CREATE PROPERTY GRAPH, so its
// folded prior state has no graph and a comparison would report a difference on
// every single run — emitting a graph the directory does not manage, forever.
// Which graph statement a generation needs is [Plan]'s decision, not this one's.
//
// Renames are resolved here exactly as [Delta] resolves them, and for the same
// reason: an ALTER TABLE … RENAME is table work, so the tables half is the half
// that has to emit it. Skipping the plan would leave the split path dropping and
// re-adding what Delta renames, which is the data loss design D2 exists to
// prevent. The Down section is rendered against the *renamed* prior state,
// because every statement in it runs before the renames are undone.
func DeltaTables(from, to *schema.Schema) (up, down string, changed bool) {
	from, to = withoutUnmanaged(from, to)
	classified := classifyLike(from, to)
	plan := planRenames(classified, to)
	prior, stale := applyRenames(classified, plan)

	d := diffSchemas(prior, to)
	d.renames = plan
	d.droppedConstraints = append(staleAsConstraints(stale), d.droppedConstraints...)

	if !d.structural() {
		return "", "", false
	}
	return joinStmts(d.upStructural(to)), joinStmts(d.downStructural(prior)), true
}

// withoutUnmanaged removes every table gopgql does not own from both sides of a
// table diff, so nothing the differ can produce mentions one (SPEC.md §7 → M12).
//
// Which tables those are is read from the **desired** schema, and only from
// there. A folded prior state cannot say: the DDL for an unmanaged table is
// exactly what is never written, so ownership leaves no trace in a migration to
// fold back. What the fold *does* leave is a vertex table with a name, no
// columns and no constraints — reconstructed from the property graph, which does
// name it. Diffed against a desired table that has columns, that would emit an
// ADD COLUMN for every one of them, against a table gopgql must not touch. It is
// the removal from the *prior* side, not the desired side, that prevents it.
//
// A type's management therefore cannot change without the SDL saying so, which
// is why it is checked separately (see Generate): dropping @readonly leaves the
// fold still holding no columns for the table, so the next generation would emit
// a CREATE TABLE for a table that already exists — a migration that reads
// correctly and fails at apply.
func withoutUnmanaged(from, to *schema.Schema) (*schema.Schema, *schema.Schema) {
	unmanaged := map[string]bool{}
	for _, vt := range to.VertexTables {
		if vt.Unmanaged {
			unmanaged[vt.Key()] = true
		}
	}
	// An edge mapped onto an existing table is unowned in exactly the same way,
	// and it is the same table as a vertex element when a join table is queried
	// from both roles (SPEC.md §7 → M13). Missing it here is a CREATE TABLE for
	// a table that already exists.
	for _, e := range to.EdgeTables {
		if e.Unmanaged {
			unmanaged[e.Key()] = true
		}
	}
	if len(unmanaged) == 0 {
		return from, to
	}
	return stripTables(from, unmanaged), stripTables(to, unmanaged)
}

// stripTables returns a copy of m without the named tables or anything attached
// to them, on either side of the diff.
func stripTables(m *schema.Schema, drop map[string]bool) *schema.Schema {
	if m == nil {
		return nil
	}
	out := &schema.Schema{GraphName: m.GraphName}
	for _, e := range m.EdgeTables {
		if !drop[e.Key()] && !e.Unmanaged {
			out.EdgeTables = append(out.EdgeTables, e)
		}
	}
	for _, vt := range m.VertexTables {
		if !drop[vt.Key()] {
			out.VertexTables = append(out.VertexTables, vt)
		}
	}
	for _, idx := range m.Indexes {
		if !drop[schema.QualifiedKey(idx.Schema, idx.Table)] {
			out.Indexes = append(out.Indexes, idx)
		}
	}
	return out
}

// classifyLike splits prior's tables into vertex and edge tables the way `like`
// classifies them.
//
// A history generated with --no-graph has no CREATE PROPERTY GRAPH, so folding it
// cannot know which of its tables are vertices and which are edges — it returns
// them all as vertices. The desired schema is the only place those roles are
// recorded, and using it keeps the diff's ordering guarantees intact: edges are
// still dropped before the vertices they reference, and created after them. A
// table whose role genuinely changed is absent from the matching list on one
// side, so it is dropped and recreated, which is correct.
func classifyLike(prior, like *schema.Schema) *schema.Schema {
	if prior == nil || len(prior.EdgeTables) > 0 {
		return prior // already classified (folded from a directory with a graph)
	}
	edges := map[string]bool{}
	for _, e := range like.EdgeTables {
		edges[e.Key()] = true
	}
	out := &schema.Schema{GraphName: prior.GraphName, Indexes: prior.Indexes}
	for _, vt := range prior.VertexTables {
		if edges[vt.Key()] {
			out.EdgeTables = append(out.EdgeTables, schema.EdgeTable{
				Name: vt.Name, Schema: vt.Schema, Columns: vt.Columns,
			})
			continue
		}
		out.VertexTables = append(out.VertexTables, vt)
	}
	return out
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

	// Named constraints — the natural key's UNIQUE and every CHECK — gained and
	// lost by a table present in both schemas (SPEC.md §7 → M7). A changed body
	// appears in both lists: PostgreSQL has no ALTER CONSTRAINT for a CHECK, so
	// the constraint is dropped and re-added by the same name.
	addedConstraints   []constraintChange
	droppedConstraints []constraintChange

	// Column defaults set, changed or dropped on a column present in both
	// schemas. A default is a property of the column and never a reason to
	// recreate it (design D6, task 4.1).
	defaultChanges []defaultChange

	// renames is the hint-driven rename plan, already applied to the prior
	// schema this diff was computed against.
	renames renamePlan
	// priorGraph is the prior schema under its *original* names, whose property
	// graph the Down section recreates once it has undone the renames.
	priorGraph *schema.Schema
}

// constraintChange is one named constraint to add or drop. The name is the whole
// point: a delta drops a constraint by name, so both halves derive it from the
// same rule (the schema package's *ConstraintName functions).
type constraintChange struct {
	Table   string
	Name    string
	Kind    string   // "UNIQUE" or "CHECK"
	Columns []string // UNIQUE
	Expr    string   // CHECK
}

// defaultChange is one column's default moving from From to To; an empty string
// on either side means "no default", so a set, a change and a drop are one shape.
type defaultChange struct {
	Table  string
	Column string
	From   string
	To     string
}

// uniqueChange is one column's UNIQUE constraint, named the way PostgreSQL names
// an implicit one so the ADD and DROP paths agree.
type uniqueChange struct {
	Table  string
	Column string
}

// name derives the constraint's name from the *bare* table name: PostgreSQL's
// implicit constraint names carry no schema, and the drop has to find the name
// the database actually gave it.
func (u uniqueChange) name() string { return schema.UniqueConstraintName(bare(u.Table), u.Column) }

func (d *schemaDiff) structural() bool {
	return len(d.addedVertices) > 0 || len(d.droppedVertices) > 0 ||
		len(d.addedEdges) > 0 || len(d.droppedEdges) > 0 ||
		len(d.addedIndexes) > 0 || len(d.droppedIndexes) > 0 ||
		len(d.addedUniques) > 0 || len(d.droppedUniques) > 0 ||
		len(d.addedConstraints) > 0 || len(d.droppedConstraints) > 0 ||
		len(d.defaultChanges) > 0 || !d.renames.empty() ||
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
		if _, ok := fromV[vt.Key()]; !ok {
			d.addedVertices = append(d.addedVertices, vt.Key())
		}
	}
	for _, vt := range from.VertexTables {
		if _, ok := toV[vt.Key()]; !ok {
			d.droppedVertices = append(d.droppedVertices, vt.Key())
			continue
		}
	}
	for _, vt := range from.VertexTables {
		toVT, ok := toV[vt.Key()]
		if !ok {
			continue
		}
		added, dropped := columnDiff(vt.Columns, toVT.Columns)
		if len(added) > 0 || len(dropped) > 0 {
			d.commonVertices = append(d.commonVertices, vt.Key())
			d.addedColumns[vt.Key()] = added
			d.droppedColumns[vt.Key()] = dropped
		}
		gained, lost := uniqueDiff(vt.Key(), vt.Columns, toVT.Columns)
		d.addedUniques = append(d.addedUniques, gained...)
		d.droppedUniques = append(d.droppedUniques, lost...)

		addedCons, droppedCons := constraintDiff(&vt, &toVT)
		d.addedConstraints = append(d.addedConstraints, addedCons...)
		d.droppedConstraints = append(d.droppedConstraints, droppedCons...)

		d.defaultChanges = append(d.defaultChanges, defaultDiff(vt.Key(), vt.Columns, toVT.Columns)...)
	}

	fromE := edgeIndex(from)
	toE := edgeIndex(to)
	for _, et := range to.EdgeTables {
		if _, ok := fromE[et.Key()]; !ok {
			d.addedEdges = append(d.addedEdges, et.Key())
		}
	}
	for _, et := range from.EdgeTables {
		if _, ok := toE[et.Key()]; !ok {
			d.droppedEdges = append(d.droppedEdges, et.Key())
		}
	}

	// Standalone index changes: only for tables present in both schemas. An
	// index on an added or dropped table travels with that table's CREATE/DROP.
	addedTables := nameSet(d.addedVertices, d.addedEdges)
	droppedTables := nameSet(d.droppedVertices, d.droppedEdges)
	fromIdx := indexByName(from)
	toIdx := indexByName(to)
	for _, idx := range to.Indexes {
		prior, ok := fromIdx[idx.Key()]
		// An index whose definition moved — different columns or a different
		// access method — is dropped and recreated: PostgreSQL has no ALTER for
		// either (SPEC.md §7 → M6).
		if ok && sameIndex(prior, idx) {
			continue
		}
		if addedTables[schema.QualifiedKey(idx.Schema, idx.Table)] {
			continue
		}
		d.addedIndexes = append(d.addedIndexes, idx)
	}
	for _, idx := range from.Indexes {
		desired, ok := toIdx[idx.Key()]
		if ok && sameIndex(idx, desired) {
			continue
		}
		if droppedTables[schema.QualifiedKey(idx.Schema, idx.Table)] {
			continue
		}
		d.droppedIndexes = append(d.droppedIndexes, idx)
	}
	return d
}

// sameIndex reports whether two same-named indexes are identical in definition.
func sameIndex(a, b schema.Index) bool {
	if a.Table != b.Table || a.Schema != b.Schema || a.Method != b.Method ||
		len(a.Columns) != len(b.Columns) {
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

// constraintDiff reports the named constraints a table gains and loses: the
// natural key's UNIQUE and every CHECK, column-level and table-level.
//
// The comparison is by name *and* body. A constraint whose body moved — a check
// rewritten, a natural key gaining a column — is reported as both a drop and an
// add, because PostgreSQL has no ALTER for either: the only way to change one is
// to remove it and put the new one back. That is exactly task 4.3's "drop and
// re-add the named unique constraint".
//
// It is rendered from generator.TableConstraints on both sides rather than from
// the model fields directly, so the constraint a delta adds is textually the
// constraint CREATE TABLE would have written.
func constraintDiff(from, to *schema.VertexTable) (added, dropped []constraintChange) {
	prior := constraintsByName(from)
	desired := constraintsByName(to)

	for _, c := range generator.TableConstraints(to) {
		if p, ok := prior[c.Name]; ok && sameConstraint(p, c) {
			continue
		}
		added = append(added, constraintChange{
			Table: to.Key(), Name: c.Name, Kind: c.Kind, Columns: c.Columns, Expr: c.Expr,
		})
	}
	for _, c := range generator.TableConstraints(from) {
		if w, ok := desired[c.Name]; ok && sameConstraint(c, w) {
			continue
		}
		dropped = append(dropped, constraintChange{
			Table: from.Key(), Name: c.Name, Kind: c.Kind, Columns: c.Columns, Expr: c.Expr,
		})
	}
	return added, dropped
}

func constraintsByName(vt *schema.VertexTable) map[string]generator.TableConstraint {
	out := map[string]generator.TableConstraint{}
	for _, c := range generator.TableConstraints(vt) {
		out[c.Name] = c
	}
	return out
}

func sameConstraint(a, b generator.TableConstraint) bool {
	if a.Kind != b.Kind || a.Expr != b.Expr || len(a.Columns) != len(b.Columns) {
		return false
	}
	for i := range a.Columns {
		if a.Columns[i] != b.Columns[i] {
			return false
		}
	}
	return true
}

// defaultDiff reports the defaults that changed on columns present in both
// schemas. A column that is itself added or dropped carries its default in its
// own definition, so it never appears here — and a column that stays never gets
// recreated for a default, which is the whole of task 4.1.
func defaultDiff(table string, fromCols, toCols []schema.Column) []defaultChange {
	prior := map[string]schema.Column{}
	for _, c := range fromCols {
		prior[c.Name] = c
	}
	var out []defaultChange
	for _, c := range toCols {
		p, ok := prior[c.Name]
		if !ok || p.Default == c.Default {
			continue
		}
		out = append(out, defaultChange{Table: table, Column: c.Name, From: p.Default, To: c.Default})
	}
	return out
}

// staleAsConstraints turns the constraints a rename left under an old name into
// ordinary drops, so they travel through the same emission and inversion path as
// every other constraint change.
func staleAsConstraints(stale []staleConstraint) []constraintChange {
	if len(stale) == 0 {
		return nil
	}
	out := make([]constraintChange, len(stale))
	for i, s := range stale {
		// The two structs are field-for-field identical, so a conversion says
		// "this is the same record, re-labelled" without listing every field.
		out[i] = constraintChange(s)
	}
	return out
}

// upStatements renders the forward migration: prior → desired, with the
// property graph dropped first and recreated last so table changes are never
// blocked by a graph depending on them.
//
// graphFrom/graphTo carry every element; tablesTo carries only the tables gopgql
// owns, and is what the structural statements are rendered from.
func (d *schemaDiff) upStatements(graphFrom, graphTo, tablesTo *schema.Schema) []string {
	var s []string
	if graphFrom.GraphName != "" {
		s = append(s, dropGraphStmt(graphFrom.GraphName))
	}
	s = append(s, d.upStructural(tablesTo)...)
	s = append(s, generator.GraphDDL(graphTo))
	return s
}

// upStructural renders the table half of the forward migration: everything
// except the property graph. The renames and the named-constraint drops belong
// here rather than in [upStatements] — they are ALTER TABLE work, so the tables
// half has to emit them when the two halves are generated separately.
func (d *schemaDiff) upStructural(to *schema.Schema) []string {
	var s []string
	// Renames run first, on the objects as the database still knows them, and
	// everything after this point speaks in the new names — including the drops
	// of constraints the rename left holding a stale name.
	s = append(s, d.renames.upStmts()...)
	for _, c := range d.droppedConstraints {
		s = append(s, dropNamedConstraintStmt(c))
	}
	for _, idx := range d.droppedIndexes {
		s = append(s, dropIndexStmt(idx))
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
	for _, c := range d.defaultChanges {
		s = append(s, setDefaultStmt(c.Table, c.Column, c.To))
	}
	for _, u := range d.addedUniques {
		s = append(s, addConstraintStmt(u))
	}
	for _, c := range d.addedConstraints {
		s = append(s, addNamedConstraintStmt(c))
	}
	for _, idx := range d.addedIndexes {
		s = append(s, generator.IndexDDL(idx))
	}
	return s
}

// downStatements renders the reverse migration: desired → prior. It is the
// mirror of upStatements — every add becomes a drop and every drop is recreated
// from the prior schema.
func (d *schemaDiff) downStatements(graphTo, tablesFrom *schema.Schema) []string {
	var s []string
	if graphTo.GraphName != "" {
		s = append(s, dropGraphStmt(graphTo.GraphName))
	}
	s = append(s, d.downStructural(tablesFrom)...)
	// The prior schema under its *original* names: the Down section has just
	// reversed the renames, so the graph it recreates is the one those names
	// describe.
	s = append(s, generator.GraphDDL(d.graphOf(tablesFrom)))
	return s
}

// downStructural renders the table half of the reverse migration.
func (d *schemaDiff) downStructural(from *schema.Schema) []string {
	var s []string
	for _, c := range d.addedConstraints {
		s = append(s, dropNamedConstraintStmt(c))
	}
	for _, idx := range d.addedIndexes {
		s = append(s, dropIndexStmt(idx))
	}
	for _, u := range d.addedUniques {
		s = append(s, dropConstraintStmt(u))
	}
	for _, c := range d.defaultChanges {
		s = append(s, setDefaultStmt(c.Table, c.Column, c.From))
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
	for _, c := range d.droppedConstraints {
		s = append(s, addNamedConstraintStmt(c))
	}
	for _, idx := range d.droppedIndexes {
		s = append(s, generator.IndexDDL(idx))
	}
	// The renames come undone last, mirroring Up where they came first: every
	// statement above still speaks the new names, and only once they are all
	// reversed does the schema go back to the names its property graph names.
	s = append(s, d.renames.downStmts()...)
	return s
}

// graphOf returns the schema whose property graph the Down section recreates: the
// prior schema under its *original* names, which differs from the schema the diff
// was computed against exactly when a rename was applied to it.
func (d *schemaDiff) graphOf(prior *schema.Schema) *schema.Schema {
	if d.priorGraph != nil {
		return d.priorGraph
	}
	return prior
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
func tableCreateStmts(m *schema.Schema, key string) []string {
	var out []string
	for i := range m.VertexTables {
		if m.VertexTables[i].Key() == key {
			out = append(out, generator.VertexTableDDL(&m.VertexTables[i]))
		}
	}
	for i := range m.EdgeTables {
		if m.EdgeTables[i].Key() == key {
			out = append(out, generator.EdgeTableDDL(&m.EdgeTables[i]))
		}
	}
	for _, idx := range m.Indexes {
		if schema.QualifiedKey(idx.Schema, idx.Table) == key {
			out = append(out, generator.IndexDDL(idx))
		}
	}
	return out
}

func addColumnStmt(table string, c schema.Column) string {
	return fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;", qualify(table), generator.ColumnDDL(c))
}

func dropColumnStmt(table, col string) string {
	return fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s;", qualify(table), quote(col))
}

func dropTableStmt(key string) string {
	return fmt.Sprintf("DROP TABLE IF EXISTS %s;", qualify(key))
}

// addConstraintStmt / dropConstraintStmt render a single-column UNIQUE
// constraint change. The name matches PostgreSQL's own implicit naming, so a
// constraint created inline by CREATE TABLE can later be dropped by name.
func addConstraintStmt(u uniqueChange) string {
	return fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s UNIQUE (%s);",
		qualify(u.Table), pgident.Quote(u.name()), pgident.Quote(u.Column))
}

func dropConstraintStmt(u uniqueChange) string {
	return fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s;",
		qualify(u.Table), pgident.Quote(u.name()))
}

// addNamedConstraintStmt / dropNamedConstraintStmt render the M7 constraints —
// the natural key's UNIQUE and every CHECK. The body comes from
// generator.TableConstraint, so the constraint a delta adds is textually the
// constraint CREATE TABLE writes; the name is derived by the schema package, so
// the drop can find it.
func addNamedConstraintStmt(c constraintChange) string {
	body := generator.TableConstraint{Name: c.Name, Kind: c.Kind, Columns: c.Columns, Expr: c.Expr}
	return fmt.Sprintf("ALTER TABLE %s ADD %s;", qualify(c.Table), body.Body())
}

func dropNamedConstraintStmt(c constraintChange) string {
	return fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s;",
		qualify(c.Table), pgident.Quote(c.Name))
}

// setDefaultStmt renders a default change. An empty expression is a DROP: the
// column keeps its type, its constraints and every row it holds, because a
// default describes what happens to *future* rows that omit it and nothing else
// (design D6).
func setDefaultStmt(table, column, expr string) string {
	if expr == "" {
		return fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT;",
			qualify(table), pgident.Quote(column))
	}
	return fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s;",
		qualify(table), pgident.Quote(column), expr)
}

// dropIndexStmt qualifies the index by its table's schema: PostgreSQL creates an
// index in the schema of the table it is on, so a DROP that named it bare would
// resolve through search_path and miss it.
func dropIndexStmt(idx schema.Index) string {
	return fmt.Sprintf("DROP INDEX IF EXISTS %s;", idx.QualifiedName())
}

// joinStmts joins statements with blank lines and a trailing newline, matching
// the layout of the init migration.
func joinStmts(stmts []string) string {
	return strings.Join(stmts, "\n\n") + "\n"
}

// The three indexes below are keyed by *qualified* name, not by bare name, and
// so are every name list on schemaDiff. A schema qualifier is part of the
// identifier PostgreSQL resolves, so two tables of one name in two schemas are
// two tables — keying by the bare name would let the differ read one table's
// columns onto the other and emit an ALTER against the wrong one.
func vertexIndex(m *schema.Schema) map[string]schema.VertexTable {
	out := make(map[string]schema.VertexTable, len(m.VertexTables))
	for _, vt := range m.VertexTables {
		out[vt.Key()] = vt
	}
	return out
}

func edgeIndex(m *schema.Schema) map[string]schema.EdgeTable {
	out := make(map[string]schema.EdgeTable, len(m.EdgeTables))
	for _, et := range m.EdgeTables {
		out[et.Key()] = et
	}
	return out
}

func indexByName(m *schema.Schema) map[string]schema.Index {
	out := make(map[string]schema.Index, len(m.Indexes))
	for _, idx := range m.Indexes {
		out[idx.Key()] = idx
	}
	return out
}

// qualify renders a table key back as the identifier a statement names it by.
func qualify(key string) string { return schema.QualifiedName(schema.SplitKey(key)) }

// bare is the table name a constraint name is derived from. Constraint names are
// per-table and never carry a schema, so the derivation uses the bare half.
func bare(key string) string {
	_, name := schema.SplitKey(key)
	return name
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
