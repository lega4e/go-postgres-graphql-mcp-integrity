package migrate

import (
	"fmt"

	"github.com/lega4e/gopgql/internal/pgident"
	"github.com/lega4e/gopgql/schema"
)

// A rename is a hint, never an inference (design D2).
//
// The differ sees "column email disappeared, column contact appeared" and cannot
// tell a rename from a genuine drop-and-add; guessing wrong destroys data one way
// or loses it the other. So nothing here looks for renames. It looks for
// *declarations* of renames — the candidate prior names the generator derived
// from @renamedFrom — and accepts one only when the prior state actually holds
// the name it claims and the desired state has let go of it.
//
// The whole plan is then applied to the folded prior state *before* the diff runs.
// That is what suppresses the drop+add pair task 4.4 asks about: once the prior
// state says the column is already called `contact`, there is no disappeared
// column for the differ to drop and no appeared column for it to add. Suppressing
// the pair after the fact would mean the two halves of the differ disagreeing
// about what the schema is.

// tableRename renames a vertex table; columnRename renames one of its columns,
// naming the table by the name it has *after* any table rename, because that is
// the order the statements run in.
type tableRename struct{ From, To string }

type columnRename struct{ Table, From, To string }

// renamePlan is the set of renames declared by the desired schema that the prior
// state can actually satisfy.
type renamePlan struct {
	tables  []tableRename
	columns []columnRename
}

func (p renamePlan) empty() bool { return len(p.tables) == 0 && len(p.columns) == 0 }

// planRenames matches each @renamedFrom-derived candidate against the folded
// prior state and keeps the ones that describe a rename that has not happened
// yet.
//
// Three conditions have to hold, and each rules out a different way of getting it
// wrong:
//
//   - the prior state has the old name — otherwise there is nothing to rename, and
//     the hint emits nothing (task 4.5). This is the case that lets the same SDL,
//     hint and all, keep generating cleanly after the rename has landed.
//   - the desired state does *not* have the old name — otherwise the object still
//     exists under it and the hint describes two objects, not one moved one.
//   - the prior state does not already have the new name — otherwise the rename
//     would collide with something real.
func planRenames(from, to *schema.Schema) renamePlan {
	var p renamePlan
	fromV := vertexIndex(from)
	toV := vertexIndex(to)

	// Old table name for each renamed table, so column renames on it can be
	// resolved against the prior state under the name it had there.
	priorTable := map[string]string{}
	for _, vt := range to.VertexTables {
		if _, taken := fromV[vt.Name]; taken {
			continue
		}
		for _, cand := range vt.RenamedFrom {
			if cand == vt.Name {
				continue
			}
			if _, ok := fromV[cand]; !ok {
				continue
			}
			if _, stillDesired := toV[cand]; stillDesired {
				continue
			}
			p.tables = append(p.tables, tableRename{From: cand, To: vt.Name})
			priorTable[vt.Name] = cand
			break
		}
	}

	for _, vt := range to.VertexTables {
		old := vt.Name
		if prior, ok := priorTable[vt.Name]; ok {
			old = prior
		}
		priorVT, ok := fromV[old]
		if !ok {
			continue // a table created by this delta: its columns are all new
		}
		for _, c := range vt.Columns {
			if hasColumn(priorVT.Columns, c.Name) {
				continue
			}
			for _, cand := range c.RenamedFrom {
				if cand == c.Name || !hasColumn(priorVT.Columns, cand) || hasColumn(vt.Columns, cand) {
					continue
				}
				p.columns = append(p.columns, columnRename{Table: vt.Name, From: cand, To: c.Name})
				break
			}
		}
	}
	return p
}

// staleConstraint is a constraint the database still carries under a name derived
// from an object's *old* name.
//
// PostgreSQL renames no constraint when a table or column is renamed, but every
// name gopgql derives moves with the object — so after a rename the database and
// the model disagree about what a constraint is called. The delta closes the gap
// the only way a delta can: drop it by the name it really has, and add it back
// under the name the model expects. That is why these are recorded during the
// rename rather than left to the ordinary constraint diff, which only ever sees
// the new names.
type staleConstraint struct {
	// Table is the table's name *after* the rename — the drop runs after it.
	Table string
	// Name is the constraint's name before the rename: what the database calls it.
	Name string
	Kind string
	// Columns and Expr are the constraint's body, with post-rename column names,
	// so the Down section can restore it verbatim.
	Columns []string
	Expr    string
}

// applyRenames returns the prior schema as it stands once the plan's renames have
// been applied, together with the constraints left holding a stale name.
//
// from is not modified: the caller still needs the original to render the Down
// section's property graph, which recreates the schema under its old names.
func applyRenames(from *schema.Schema, p renamePlan) (*schema.Schema, []staleConstraint) {
	if p.empty() {
		return from, nil
	}
	m := cloneSchema(from)

	// oldTable / oldColumn record where each object came from, so a constraint's
	// prior name can be re-derived after the identifiers have moved.
	oldTable := map[string]string{}
	oldColumn := map[string]map[string]string{}

	for _, r := range p.tables {
		renameTableIn(m, r.From, r.To)
		oldTable[r.To] = r.From
	}
	for _, r := range p.columns {
		renameColumnIn(m, r.Table, r.From, r.To)
		if oldColumn[r.Table] == nil {
			oldColumn[r.Table] = map[string]string{}
		}
		oldColumn[r.Table][r.To] = r.From
	}

	var stale []staleConstraint
	for i := range m.VertexTables {
		vt := &m.VertexTables[i]
		prevTable, tableMoved := oldTable[vt.Name]
		if !tableMoved {
			prevTable = vt.Name
		}
		prevCol := func(name string) string {
			if was, ok := oldColumn[vt.Name][name]; ok {
				return was
			}
			return name
		}

		if vt.NaturalKey != nil {
			if old := schema.NaturalKeyConstraintName(prevTable); old != vt.NaturalKey.Name {
				stale = append(stale, staleConstraint{
					Table: vt.Name, Name: old, Kind: "UNIQUE", Columns: vt.NaturalKey.Columns,
				})
				vt.NaturalKey = nil
			}
		}
		for j := range vt.Columns {
			c := &vt.Columns[j]
			if c.Check != "" {
				old := schema.ColumnCheckConstraintName(prevTable, prevCol(c.Name))
				if old != schema.ColumnCheckConstraintName(vt.Name, c.Name) {
					stale = append(stale, staleConstraint{
						Table: vt.Name, Name: old, Kind: "CHECK", Expr: c.Check,
					})
					c.Check = ""
				}
			}
			if c.Unique && !c.PrimaryKey {
				old := schema.UniqueConstraintName(prevTable, prevCol(c.Name))
				if old != schema.UniqueConstraintName(vt.Name, c.Name) {
					stale = append(stale, staleConstraint{
						Table: vt.Name, Name: old, Kind: "UNIQUE", Columns: []string{c.Name},
					})
					c.Unique = false
				}
			}
		}
		if tableMoved && len(vt.Checks) > 0 {
			for n, expr := range vt.Checks {
				stale = append(stale, staleConstraint{
					Table: vt.Name, Name: schema.TableCheckConstraintName(prevTable, n+1),
					Kind: "CHECK", Expr: expr,
				})
			}
			vt.Checks = nil
		}
	}
	return m, stale
}

// renameTableIn moves a vertex table and everything in the model that points at
// it. Indexes keep their names — PostgreSQL does not rename them either — so the
// ordinary index diff drops the old-named index and creates the new-named one.
func renameTableIn(m *schema.Schema, from, to string) {
	for i := range m.VertexTables {
		if m.VertexTables[i].Name == from {
			m.VertexTables[i].Name = to
		}
	}
	for i := range m.EdgeTables {
		e := &m.EdgeTables[i]
		if e.SourceTable == from {
			e.SourceTable = to
		}
		if e.DestTable == from {
			e.DestTable = to
		}
	}
	forEachColumn(m, func(_ string, c *schema.Column) {
		if c.References != nil && c.References.Table == from {
			c.References.Table = to
		}
	})
	for i := range m.Indexes {
		if m.Indexes[i].Table == from {
			m.Indexes[i].Table = to
		}
	}
}

// renameColumnIn renames a column of one table and every reference to it: the
// graph property lists that expose it, the natural key that constrains it, the
// indexes over it, and the foreign keys of other tables that point at it.
func renameColumnIn(m *schema.Schema, table, from, to string) {
	for i := range m.VertexTables {
		vt := &m.VertexTables[i]
		if vt.Name != table {
			continue
		}
		for j := range vt.Columns {
			if vt.Columns[j].Name == from {
				vt.Columns[j].Name = to
			}
		}
		vt.Properties = replaceIn(vt.Properties, from, to)
		for k := range vt.ExtraLabels {
			vt.ExtraLabels[k].Properties = replaceIn(vt.ExtraLabels[k].Properties, from, to)
		}
		if vt.NaturalKey != nil {
			vt.NaturalKey.Columns = replaceIn(vt.NaturalKey.Columns, from, to)
		}
	}
	for i := range m.EdgeTables {
		e := &m.EdgeTables[i]
		if e.SourceTable == table && e.SourceRef == from {
			e.SourceRef = to
		}
		if e.DestTable == table && e.DestRef == from {
			e.DestRef = to
		}
	}
	forEachColumn(m, func(_ string, c *schema.Column) {
		if c.References != nil && c.References.Table == table && c.References.Column == from {
			c.References.Column = to
		}
	})
	for i := range m.Indexes {
		if m.Indexes[i].Table == table {
			m.Indexes[i].Columns = replaceIn(m.Indexes[i].Columns, from, to)
		}
	}
}

// cloneSchema copies a schema deeply enough that renaming in the copy cannot be
// seen through the original — including the Reference pointers, which are shared
// structs and would otherwise carry an edit back.
func cloneSchema(m *schema.Schema) *schema.Schema {
	c := *m
	c.VertexTables = make([]schema.VertexTable, len(m.VertexTables))
	for i, vt := range m.VertexTables {
		vt.Columns = cloneColumns(vt.Columns)
		vt.Properties = append([]string(nil), vt.Properties...)
		vt.Checks = append([]string(nil), vt.Checks...)
		if vt.NaturalKey != nil {
			nk := *vt.NaturalKey
			nk.Columns = append([]string(nil), nk.Columns...)
			vt.NaturalKey = &nk
		}
		extra := make([]schema.LabelProperties, len(vt.ExtraLabels))
		for j, l := range vt.ExtraLabels {
			l.Properties = append([]string(nil), l.Properties...)
			extra[j] = l
		}
		vt.ExtraLabels = extra
		c.VertexTables[i] = vt
	}
	c.EdgeTables = make([]schema.EdgeTable, len(m.EdgeTables))
	for i, e := range m.EdgeTables {
		e.Columns = cloneColumns(e.Columns)
		e.Properties = append([]string(nil), e.Properties...)
		c.EdgeTables[i] = e
	}
	c.Indexes = make([]schema.Index, len(m.Indexes))
	for i, idx := range m.Indexes {
		idx.Columns = append([]string(nil), idx.Columns...)
		c.Indexes[i] = idx
	}
	return &c
}

func cloneColumns(cols []schema.Column) []schema.Column {
	if cols == nil {
		return nil
	}
	out := make([]schema.Column, len(cols))
	for i, c := range cols {
		if c.References != nil {
			ref := *c.References
			c.References = &ref
		}
		c.RenamedFrom = append([]string(nil), c.RenamedFrom...)
		out[i] = c
	}
	return out
}

// forEachColumn visits every column of every table in the schema.
func forEachColumn(m *schema.Schema, fn func(table string, c *schema.Column)) {
	for i := range m.VertexTables {
		for j := range m.VertexTables[i].Columns {
			fn(m.VertexTables[i].Name, &m.VertexTables[i].Columns[j])
		}
	}
	for i := range m.EdgeTables {
		for j := range m.EdgeTables[i].Columns {
			fn(m.EdgeTables[i].Name, &m.EdgeTables[i].Columns[j])
		}
	}
}

func replaceIn(names []string, from, to string) []string {
	out := append([]string(nil), names...)
	for i, n := range out {
		if n == from {
			out[i] = to
		}
	}
	return out
}

func hasColumn(cols []schema.Column, name string) bool {
	for _, c := range cols {
		if c.Name == name {
			return true
		}
	}
	return false
}

func renameTableStmt(r tableRename) string {
	return fmt.Sprintf("ALTER TABLE %s RENAME TO %s;", pgident.Quote(r.From), pgident.Quote(r.To))
}

func renameColumnStmt(r columnRename) string {
	return fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s;",
		pgident.Quote(r.Table), pgident.Quote(r.From), pgident.Quote(r.To))
}

// invert turns a plan into the plan that undoes it: tables and columns swap
// endpoints, and the order reverses so a column rename is undone while its table
// still carries the name the column rename named it by.
func (p renamePlan) invert() renamePlan {
	var out renamePlan
	for i := len(p.columns) - 1; i >= 0; i-- {
		r := p.columns[i]
		out.columns = append(out.columns, columnRename{Table: r.Table, From: r.To, To: r.From})
	}
	for i := len(p.tables) - 1; i >= 0; i-- {
		r := p.tables[i]
		out.tables = append(out.tables, tableRename{From: r.To, To: r.From})
	}
	return out
}

// upStmts renders the plan: tables first, then columns, because a column rename
// names its table by the name the table already has.
func (p renamePlan) upStmts() []string {
	var s []string
	for _, r := range p.tables {
		s = append(s, renameTableStmt(r))
	}
	for _, r := range p.columns {
		s = append(s, renameColumnStmt(r))
	}
	return s
}

// downStmts renders the inverse: columns back first, while their table is still
// under its new name, then the tables back.
func (p renamePlan) downStmts() []string {
	inv := p.invert()
	var s []string
	for _, r := range inv.columns {
		s = append(s, renameColumnStmt(r))
	}
	for _, r := range inv.tables {
		s = append(s, renameTableStmt(r))
	}
	return s
}
