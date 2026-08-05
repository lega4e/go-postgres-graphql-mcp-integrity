// Package generator turns the SDL mapping model into the physical schema model
// and renders that model to PostgreSQL DDL: vertex tables, edge tables, the
// mandatory destination-key indexes, and the CREATE PROPERTY GRAPH statement.
//
// The generated DDL satisfies all five invariants of SPEC.md §5.3, and Build
// re-checks them defensively so a regression fails loudly rather than emitting a
// graph PostgreSQL will reject. It has no database dependency and compiles to
// WASM (SPEC.md §4.1).
package generator

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lega4e/gopgql/internal/pgident"
	"github.com/lega4e/gopgql/schema"
	"github.com/lega4e/gopgql/sdl"
)

// DefaultGraphName is the property-graph name used when none is configured.
const DefaultGraphName = "app_graph"

// scalarMapping is the default GraphQL-scalar → PostgreSQL-type mapping
// (SPEC.md §5.1). ID maps to uuid; the surrogate key adds PRIMARY KEY DEFAULT
// gen_random_uuid() on top.
var scalarMapping = map[string]string{
	"Int":      "integer",
	"Float":    "double precision",
	"String":   "text",
	"Boolean":  "boolean",
	"ID":       "uuid",
	"DateTime": "timestamptz",
	"JSON":     "jsonb",
}

// Build maps an SDL document to the physical schema model and verifies the
// §5.3 invariants. graphName defaults to DefaultGraphName when empty.
func Build(doc *sdl.Document, graphName string) (*schema.Schema, error) {
	if graphName == "" {
		graphName = DefaultGraphName
	}
	m := &schema.Schema{GraphName: graphName}

	for _, n := range doc.Nodes {
		vt, err := buildVertex(n)
		if err != nil {
			return nil, err
		}
		m.VertexTables = append(m.VertexTables, vt)
		// An unmanaged table gets no index, because a CREATE INDEX on it is DDL
		// against a table gopgql does not own. Whether it is indexed is the
		// schema owner's business (SPEC.md §7 → M12).
		if !vt.Unmanaged {
			m.Indexes = append(m.Indexes, fieldIndexes(n)...)
		}
	}

	if err := attachInterfaceLabels(m, doc); err != nil {
		return nil, err
	}

	edges := collectEdges(doc)
	for _, e := range edges {
		m.EdgeTables = append(m.EdgeTables, e)
		// SPEC.md §5.3 invariant 2: every edge table gopgql *generates* has an
		// index on its destination key. An edge mapped onto an existing table
		// gets none — a CREATE INDEX on a table gopgql does not own is DDL it
		// must not emit, and whether that table is indexed is its owner's call.
		if e.Unmanaged {
			continue
		}
		m.Indexes = append(m.Indexes, schema.Index{
			Name:    e.Name + "_target_idx",
			Table:   e.Name,
			Schema:  e.Schema,
			Columns: e.DestKey,
		})
	}

	aliasEdges(m)

	if err := validateInvariants(m); err != nil {
		return nil, err
	}
	return m, nil
}

func buildVertex(n *sdl.Node) (schema.VertexTable, error) {
	vt := schema.VertexTable{
		Name: n.Table, Schema: n.Schema, Unmanaged: n.ReadOnly,
		Label: n.Label, RenamedFrom: n.PriorTableNames(),
	}
	for _, expr := range n.Checks {
		vt.Checks = append(vt.Checks, schema.NormalizeExpr(expr))
	}
	for _, f := range n.Fields {
		if !f.IsScalarColumn() {
			continue
		}
		base, ok := scalarMapping[f.TypeName]
		if !ok {
			return schema.VertexTable{}, fmt.Errorf(
				"generator: %s.%s has unsupported type %q (no default scalar mapping)",
				n.TypeName, f.Name, f.TypeName)
		}
		if f.ColumnType != "" {
			// @column(type:) overrides the default scalar mapping (SPEC.md §5.1).
			// The text is emitted verbatim, so a modified type such as
			// numeric(10,2) round-trips into the DDL exactly as written.
			base = f.ColumnType
		}
		col := schema.Column{
			Name: f.ColumnName(), Type: base, Array: f.List,
			NotNull: f.NonNull, Unique: f.Unique,
			// @default and @check are raw SQL, emitted verbatim (design D6);
			// only their whitespace is normalized, so the text survives the
			// round trip through internal/ddl unchanged.
			Default:     schema.NormalizeExpr(f.Default),
			Check:       schema.NormalizeExpr(f.Check),
			RenamedFrom: f.PriorColumnNames(),
		}
		if f.Name == "id" && f.TypeName == "ID" {
			// Surrogate key (SPEC.md §7 → M1).
			col.PrimaryKey = true
			col.NotNull = false // PRIMARY KEY already implies NOT NULL
			col.Default = "gen_random_uuid()"
		}
		vt.Columns = append(vt.Columns, col)
		vt.Properties = append(vt.Properties, col.Name)
	}
	if err := attachNaturalKey(&vt, n); err != nil {
		return schema.VertexTable{}, err
	}
	return vt, nil
}

// attachNaturalKey maps a type's @key(fields:) — GraphQL field names — onto the
// physical columns they produce, preserving the declared order (design D1).
//
// sdl.validateNaturalKey has already established that every named field exists
// and maps to a column, so a miss here is a generator bug rather than an SDL
// error; it is still reported rather than silently dropped, because a natural
// key that quietly loses a column is a uniqueness constraint that quietly stops
// constraining.
func attachNaturalKey(vt *schema.VertexTable, n *sdl.Node) error {
	if len(n.NaturalKey) == 0 {
		return nil
	}
	cols := make([]string, 0, len(n.NaturalKey))
	for _, name := range n.NaturalKey {
		var f *sdl.Field
		for _, cand := range n.Fields {
			if cand.Name == name {
				f = cand
				break
			}
		}
		if f == nil || !f.IsScalarColumn() {
			return fmt.Errorf("generator: %s: @key names field %q, which maps to no column on %s",
				n.TypeName, name, n.Table)
		}
		cols = append(cols, f.ColumnName())
	}
	vt.NaturalKey = &schema.NaturalKey{Name: schema.NaturalKeyConstraintName(vt.Name), Columns: cols}
	return nil
}

// fieldIndexes collects the secondary indexes requested with @index on a node's
// scalar fields (SPEC.md §7 → M6). An index with no explicit name is named
// <table>_<column>_idx, matching the convention the mandatory edge-key index
// already uses.
func fieldIndexes(n *sdl.Node) []schema.Index {
	var out []schema.Index
	for _, f := range n.Fields {
		if !f.IsScalarColumn() || f.Index == nil {
			continue
		}
		col := f.ColumnName()
		name := f.Index.Name
		if name == "" {
			name = n.Table + "_" + col + "_idx"
		}
		out = append(out, schema.Index{
			Name: name, Table: n.Table, Schema: n.Schema, Columns: []string{col}, Method: f.Index.Using,
		})
	}
	return out
}

// attachInterfaceLabels gives every implementor's vertex table the shared label
// of each interface it implements, exposing exactly the interface's own scalar
// fields under it (SPEC.md §7 → M4).
//
// Because GraphQL requires an implementor to declare every interface field with
// the same type, the property lists are aligned by count, name and type across
// all the tables carrying that label — SPEC.md §5.3 invariant 5, which
// PostgreSQL enforces when the graph is created. Unlabelled interfaces
// contribute no label: they are matched by alternation over the implementors'
// own labels instead.
func attachInterfaceLabels(m *schema.Schema, doc *sdl.Document) error {
	byTable := map[string]*schema.VertexTable{}
	for i := range m.VertexTables {
		byTable[m.VertexTables[i].Key()] = &m.VertexTables[i]
	}

	for _, iface := range doc.Interfaces {
		if iface.Label == "" {
			continue
		}
		var props []string
		for _, f := range iface.Fields {
			if f.IsScalarColumn() {
				props = append(props, f.Name)
			}
		}
		if len(props) == 0 {
			return fmt.Errorf("generator: interface %s (@node) exposes no columns; "+
				"a shared label needs at least the key property", iface.TypeName)
		}
		for _, impl := range iface.Implementors {
			vt, ok := byTable[schema.QualifiedKey(impl.Schema, impl.Table)]
			if !ok {
				return fmt.Errorf("generator: interface %s is implemented by %s, whose table %q has no vertex table",
					iface.TypeName, impl.TypeName, impl.Table)
			}
			if iface.Label == vt.Label {
				return fmt.Errorf("generator: interface %s and type %s both claim the label %q",
					iface.TypeName, impl.TypeName, iface.Label)
			}
			vt.ExtraLabels = append(vt.ExtraLabels, schema.LabelProperties{Label: iface.Label, Properties: props})
		}
	}

	// Stable label order per table so generated DDL is deterministic.
	for i := range m.VertexTables {
		sort.SliceStable(m.VertexTables[i].ExtraLabels, func(a, b int) bool {
			return m.VertexTables[i].ExtraLabels[a].Label < m.VertexTables[i].ExtraLabels[b].Label
		})
	}
	return nil
}

// aliasEdges gives an edge element an explicit alias wherever its bare table
// name is already claimed — by a vertex element over the same table, or by
// another edge label over it. The alias is the edge's label, which is the name a
// reader already associates with that traversal; a label that would itself
// collide is reported by invariant 4 rather than silently suffixed.
func aliasEdges(m *schema.Schema) {
	claims := map[string]int{}
	for _, vt := range m.VertexTables {
		claims[vt.ElementAlias()]++
	}
	for _, e := range m.EdgeTables {
		claims[e.Name]++
	}
	for i := range m.EdgeTables {
		if claims[m.EdgeTables[i].Name] > 1 {
			m.EdgeTables[i].Alias = m.EdgeTables[i].Label
		}
	}
}

// collectEdges builds one graph edge element per relationship label. An OUT
// field and its @hasInverse IN partner map to the same table under the same
// label (SPEC.md §5.2), so OUT fields are processed first and IN fields only
// contribute an element when no OUT field already produced it.
func collectEdges(doc *sdl.Document) []schema.EdgeTable {
	seen := map[string]bool{}
	var edges []schema.EdgeTable

	add := func(n *sdl.Node, f *sdl.Field) {
		target := doc.NodeByType(f.TypeName)
		table := f.Rel.Table
		if table == "" {
			table = f.Rel.Type
		}
		// One edge per relationship *label* on a table, not one per table. For a
		// table gopgql creates the two coincide — its name is the label — but an
		// edge mapped onto an existing table is named independently of it, and
		// two labels over one join table is the ordinary shape: a row of
		// dbos.operation_outputs is both the step a workflow HAS_STEP and, when
		// it starts a child, the SPAWNED edge back out. Keying on the table alone
		// dropped the second label silently, and which one survived depended on
		// doc.Nodes order. The schema is part of the key for the same reason it
		// is part of every other table identity here: two same-named tables in
		// two schemas are two tables.
		key := schema.QualifiedKey(f.Rel.Schema, table) + "\x00" + f.Rel.Type
		if seen[key] {
			return
		}
		seen[key] = true

		src, dst := n, target
		if f.Rel.Direction == sdl.In {
			// IN reverses orientation: the declaring type is the destination.
			src, dst = target, n
		}

		// An edge mapped onto an existing table names its own key columns and
		// references each endpoint's *identity* — which may be multi-column for
		// a @readonly type. gopgql creates no table for it and knows no columns
		// of it beyond the keys, so Columns stays nil: the graph element is the
		// whole of what is emitted (SPEC.md §7 → M13).
		if len(f.Rel.SourceKey) > 0 {
			edges = append(edges, schema.EdgeTable{
				Name:         table,
				Schema:       f.Rel.Schema,
				Unmanaged:    true,
				Label:        f.Rel.Type,
				SourceKey:    f.Rel.SourceKey,
				SourceSchema: src.Schema,
				SourceTable:  src.Table,
				SourceRef:    src.IdentityColumns(),
				DestKey:      f.Rel.DestKey,
				DestSchema:   dst.Schema,
				DestTable:    dst.Table,
				DestRef:      dst.IdentityColumns(),
				// The two key lists can overlap — a join table keyed by
				// (workflow_uuid, function_id) uses workflow_uuid as its source
				// key too — and a label cannot expose one property twice, so the
				// element publishes their distinct union in first-seen order.
				Properties: distinctColumns(f.Rel.SourceKey, f.Rel.DestKey),
			})
			return
		}

		edges = append(edges, schema.EdgeTable{
			Name:         table,
			Schema:       f.Rel.Schema,
			Label:        f.Rel.Type,
			SourceKey:    []string{"source_id"},
			SourceSchema: src.Schema,
			SourceTable:  src.Table,
			SourceRef:    []string{"id"},
			DestKey:      []string{"target_id"},
			DestSchema:   dst.Schema,
			DestTable:    dst.Table,
			DestRef:      []string{"id"},
			Columns: []schema.Column{
				{Name: "source_id", Type: "uuid", NotNull: true,
					References: &schema.Reference{Schema: src.Schema, Table: src.Table, Column: "id"}},
				{Name: "target_id", Type: "uuid", NotNull: true,
					References: &schema.Reference{Schema: dst.Schema, Table: dst.Table, Column: "id"}},
			},
			Properties: []string{"source_id", "target_id"},
		})
	}

	// Two passes so OUT definitions win over their IN mirrors deterministically.
	for _, dir := range []sdl.Direction{sdl.Out, sdl.In} {
		for _, n := range doc.Nodes {
			for _, f := range n.Fields {
				if f.Rel == nil || f.Rel.Direction != dir {
					continue
				}
				add(n, f)
			}
		}
	}
	// Name first, so the emitted order is unchanged for every schema whose edges
	// are one per table; schema and label break the tie a shared table creates,
	// because leaving it to doc.Nodes order is what made the collapse look like a
	// coin toss rather than a bug.
	sort.SliceStable(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Schema != b.Schema {
			return a.Schema < b.Schema
		}
		return a.Label < b.Label
	})
	return edges
}

// TablesDDL renders the table half of the physical schema: vertex tables, then
// edge tables, each followed by its indexes. It emits nothing about the
// property graph.
//
// This is what the `_tables` migration of a generation carries. It is separate
// from GraphDDL because no migration mixes the two, and because a project whose
// tables are managed elsewhere generates only the graph half (gopgql#38).
//
// An unmanaged (@readonly) vertex table contributes nothing here — not its
// CREATE TABLE, not its constraints, not its indexes. It is still a full member
// of the property graph GraphDDL renders; the two halves disagree about it on
// purpose, because that disagreement is exactly "surfaced but not owned".
func TablesDDL(m *schema.Schema) string {
	var blocks []string
	for i := range m.VertexTables {
		vt := &m.VertexTables[i]
		if vt.Unmanaged {
			continue
		}
		blocks = append(blocks, withIndexes(m, vt.Key(), VertexTableDDL(vt)))
	}
	for i := range m.EdgeTables {
		e := &m.EdgeTables[i]
		if e.Unmanaged {
			continue
		}
		blocks = append(blocks, withIndexes(m, e.Key(), EdgeTableDDL(e)))
	}
	return strings.Join(blocks, "\n\n")
}

// DDL renders both halves as one block: the tables, then the property graph.
//
// Nothing generates migrations in this shape any more — a generation is a run of
// single-purpose files (design D2). It is retained because it states the invariant
// the split rests on: the two halves concatenated are exactly the whole schema,
// which generator_test asserts, and because the integration suite applies it to a
// fresh database to prove the sequence reaches the same place.
func DDL(m *schema.Schema) string {
	return TablesDDL(m) + "\n\n" + GraphDDL(m) + "\n"
}

// withIndexes appends a table's CREATE INDEX statements to its CREATE TABLE
// block: the mandatory destination-key index on every edge table (SPEC.md §5.3
// invariant 2) and any index a field asked for with @index (SPEC.md §7 → M6).
func withIndexes(m *schema.Schema, tableKey, block string) string {
	for _, idx := range m.Indexes {
		if schema.QualifiedKey(idx.Schema, idx.Table) == tableKey {
			block += "\n" + IndexDDL(idx)
		}
	}
	return block
}

// VertexTableDDL renders a single CREATE TABLE for a vertex table. It is
// exported so the migrate package can re-create a vertex table in a delta
// migration (SPEC.md §7 → M2).
func VertexTableDDL(vt *schema.VertexTable) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", vt.QualifiedName())
	lines := make([]string, 0, len(vt.Columns))
	for _, c := range vt.Columns {
		lines = append(lines, "    "+ColumnDDL(c))
	}
	for _, tc := range TableConstraints(vt) {
		lines = append(lines, "    "+tc.Body())
	}
	b.WriteString(strings.Join(lines, ",\n"))
	b.WriteString("\n);")
	return b.String()
}

// TableConstraint is one named table-level constraint of a vertex table: the
// natural key's UNIQUE, a column-level CHECK, or a table-level CHECK.
//
// It is exported, with TableConstraints, because the migrate package has to emit
// exactly the same constraints in ALTER form when they appear on a table that
// already exists — and has to drop them by exactly the same name. Two
// independent renderings of one naming rule is how the ADD and the DROP end up
// disagreeing (SPEC.md §7 → M7).
type TableConstraint struct {
	// Name is the deterministic constraint name (see the schema package's
	// *ConstraintName functions).
	Name string
	// Kind is "UNIQUE" or "CHECK".
	Kind string
	// Columns is the constrained column list for a UNIQUE.
	Columns []string
	// Expr is the raw body of a CHECK, emitted verbatim (design D6).
	Expr string
}

// Body renders the constraint as it appears inside CREATE TABLE, and after
// ALTER TABLE … ADD.
func (c TableConstraint) Body() string {
	if c.Kind == "CHECK" {
		return fmt.Sprintf("CONSTRAINT %s CHECK (%s)", pgident.Quote(c.Name), c.Expr)
	}
	return fmt.Sprintf("CONSTRAINT %s UNIQUE (%s)", pgident.Quote(c.Name), quoteList(c.Columns))
}

// TableConstraints lists a vertex table's named constraints in emission order:
// the natural key first, then one check per column that declares one (in column
// order), then the table-level checks (in declaration order).
//
// A column-level check is emitted as a *named table-level* constraint rather
// than inline after the column, for two reasons: an anonymous inline check
// cannot be dropped by a later delta without asking the database what name it
// invented (design D6), and a named one keeps ColumnDDL — which ADD COLUMN also
// uses — free of anything the check has to be paired with.
//
// It returns nil for a table that declares none, which is what keeps the DDL for
// a pre-M7 schema byte-identical (task 3.6).
func TableConstraints(vt *schema.VertexTable) []TableConstraint {
	var out []TableConstraint
	if vt.NaturalKey != nil {
		out = append(out, TableConstraint{
			Name: vt.NaturalKey.Name, Kind: "UNIQUE", Columns: vt.NaturalKey.Columns,
		})
	}
	for _, c := range vt.Columns {
		if c.Check == "" {
			continue
		}
		out = append(out, TableConstraint{
			Name: schema.ColumnCheckConstraintName(vt.Name, c.Name), Kind: "CHECK", Expr: c.Check,
		})
	}
	for i, expr := range vt.Checks {
		out = append(out, TableConstraint{
			Name: schema.TableCheckConstraintName(vt.Name, i+1), Kind: "CHECK", Expr: expr,
		})
	}
	return out
}

// ColumnDDL renders a single column definition (name, type, constraints), as it
// appears inside CREATE TABLE or after ADD COLUMN. Exported for the migrate
// package's delta ALTER statements (SPEC.md §7 → M2).
func ColumnDDL(c schema.Column) string {
	typ := c.Type
	if c.Array {
		typ += "[]"
	}
	parts := []string{pgident.Quote(c.Name), typ}
	switch {
	case c.PrimaryKey:
		parts = append(parts, "PRIMARY KEY")
	case c.NotNull:
		parts = append(parts, "NOT NULL")
	}
	if c.Unique && !c.PrimaryKey {
		parts = append(parts, "UNIQUE")
	}
	if c.Default != "" {
		parts = append(parts, "DEFAULT "+c.Default)
	}
	if c.References != nil {
		parts = append(parts, fmt.Sprintf("REFERENCES %s (%s)",
			c.References.QualifiedTable(), pgident.Quote(c.References.Column)))
	}
	return strings.Join(parts, " ")
}

// EdgeTableDDL renders a single CREATE TABLE for an edge table (its two key
// columns and the composite primary key). Exported for the migrate package's
// delta migrations (SPEC.md §7 → M2).
func EdgeTableDDL(e *schema.EdgeTable) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", e.QualifiedName())
	for _, c := range e.Columns {
		fmt.Fprintf(&b, "    %s,\n", ColumnDDL(c))
	}
	fmt.Fprintf(&b, "    PRIMARY KEY (%s)\n", quoteList(append(append([]string{}, e.SourceKey...), e.DestKey...)))
	b.WriteString(");")
	return b.String()
}

// IndexDDL renders a CREATE INDEX statement. Exported for the migrate package's
// delta migrations (SPEC.md §7 → M2).
func IndexDDL(idx schema.Index) string {
	cols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		cols[i] = pgident.Quote(c)
	}
	using := ""
	if idx.Method != "" {
		using = " USING " + idx.Method
	}
	return fmt.Sprintf("CREATE INDEX %s ON %s%s (%s);",
		pgident.Quote(idx.Name), idx.QualifiedTable(), using, strings.Join(cols, ", "))
}

// GraphDDL renders the CREATE PROPERTY GRAPH statement for a schema. Exported
// because property graphs are metadata that a delta migration always drops and
// recreates (SPEC.md §7 → M2); the migrate package renders both the desired
// graph (Up) and the folded prior graph (Down) with it.
func GraphDDL(m *schema.Schema) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE PROPERTY GRAPH %s\n", pgident.Quote(m.GraphName))
	b.WriteString("  VERTEX TABLES (\n")
	vlines := make([]string, len(m.VertexTables))
	for i, vt := range m.VertexTables {
		var vb strings.Builder
		// A natural key names the element's key columns, so a MATCH can select
		// a vertex by its data (design D1). Without one the clause is omitted
		// and PostgreSQL falls back to the table's primary key — the surrogate
		// id — exactly as it did before M7.
		key := ""
		if vt.NaturalKey != nil {
			key = fmt.Sprintf(" KEY (%s)", quoteList(vt.NaturalKey.Columns))
		}
		fmt.Fprintf(&vb, "    %s%s LABEL %s PROPERTIES (%s)",
			vt.QualifiedName(), key, pgident.Quote(vt.Label), quoteList(vt.Properties))
		// A shared label — one interface, several tables — is a further LABEL
		// clause on the same table (SPEC.md §7 → M4).
		for _, extra := range vt.ExtraLabels {
			fmt.Fprintf(&vb, "\n            LABEL %s PROPERTIES (%s)",
				pgident.Quote(extra.Label), quoteList(extra.Properties))
		}
		vlines[i] = vb.String()
	}
	b.WriteString(strings.Join(vlines, ",\n"))
	b.WriteString("\n  )")

	if len(m.EdgeTables) > 0 {
		b.WriteString("\n  EDGE TABLES (\n")
		elines := make([]string, len(m.EdgeTables))
		for i, e := range m.EdgeTables {
			var eb strings.Builder
			// REFERENCES names a *vertex element* of this graph, not a table, so
			// it is the element's alias and is never schema-qualified —
			// PostgreSQL rejects a qualified name there outright, and resolves a
			// bare one against the graph rather than through search_path.
			fmt.Fprintf(&eb, "    %s SOURCE KEY (%s) REFERENCES %s (%s)\n",
				e.ElementRef(), quoteList(e.SourceKey),
				pgident.Quote(e.SourceTable), quoteList(e.SourceRef))
			fmt.Fprintf(&eb, "            DESTINATION KEY (%s) REFERENCES %s (%s)\n",
				quoteList(e.DestKey), pgident.Quote(e.DestTable), quoteList(e.DestRef))
			fmt.Fprintf(&eb, "            LABEL %s PROPERTIES (%s)",
				pgident.Quote(e.Label), quoteList(e.Properties))
			elines[i] = eb.String()
		}
		b.WriteString(strings.Join(elines, ",\n"))
		b.WriteString("\n  )")
	}
	b.WriteString(";")
	return b.String()
}

func quoteList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = pgident.Quote(n)
	}
	return strings.Join(out, ", ")
}

// validateInvariants re-checks the five SPEC.md §5.3 invariants against the
// built model. The generator constructs the model to satisfy them; this guards
// against regressions.
func validateInvariants(m *schema.Schema) error {
	// 1. Every KEY / SOURCE KEY / DESTINATION KEY column appears in PROPERTIES.
	for _, vt := range m.VertexTables {
		for _, c := range vt.Columns {
			if c.PrimaryKey && !contains(vt.Properties, c.Name) {
				return fmt.Errorf("generator: vertex %q key %q missing from PROPERTIES (invariant 1)", vt.Name, c.Name)
			}
		}
		// A natural key emits a KEY (...) clause, so its columns are KEY columns
		// and invariant 1 binds them too — which is also what makes the key
		// filterable from a MATCH (design D1).
		if vt.NaturalKey != nil {
			for _, col := range vt.NaturalKey.Columns {
				if !contains(vt.Properties, col) {
					return fmt.Errorf("generator: vertex %q natural key column %q missing from PROPERTIES (invariant 1)", vt.Name, col)
				}
			}
		}
	}
	for _, e := range m.EdgeTables {
		for _, k := range append(append([]string{}, e.SourceKey...), e.DestKey...) {
			if !contains(e.Properties, k) {
				return fmt.Errorf("generator: edge %q key %q missing from PROPERTIES (invariant 1)", e.Name, k)
			}
		}
		// 2. Every edge table gopgql *generates* has an index on its destination
		// key. An edge mapped onto an existing table is exempt: gopgql cannot
		// create an index on a table it does not own, and a warning it could not
		// act on would be noise (SPEC.md §5.3 invariant 2).
		if !e.Unmanaged && !hasDestIndex(m, e) {
			return fmt.Errorf("generator: edge %q has no index on destination key %v (invariant 2)", e.Name, e.DestKey)
		}
	}
	// 4. Distinct graph elements may share a table only when at most one of them
	// is a **vertex** element (SPEC.md §5.3 invariant 4, narrowed in M13).
	//
	// One table serving as both a vertex and an edge is the shape an externally
	// owned join table takes: a row of dbos.operation_outputs is a step, and the
	// same row is the edge connecting a workflow to it. Two *vertex* elements
	// over one table stays refused — that really is one table declared twice,
	// and the property lists would have to be identical for it to mean anything.
	// The uniqueness PostgreSQL actually enforces is over *element aliases*, and
	// an alias is a bare name — so two tables of one name in two schemas collide
	// as elements even though they are two tables, and a table serving as both a
	// vertex and an edge needs the edge to carry an explicit alias. Both are
	// checked here so the failure names the SDL rather than arriving as
	// "alias used more than once as element table" from the server.
	//
	// Two labels over one table is legal only where gopgql does not own it. An
	// edge table gopgql *creates* is created once, so two labels asking it to
	// create the same table are two CREATE TABLEs for one name — refused here
	// rather than at the database, which would report it as an unrelated
	// duplicate-relation error halfway through a migration.
	managed := map[string]string{}
	for _, e := range m.EdgeTables {
		if e.Unmanaged {
			continue
		}
		if prev, dup := managed[e.Key()]; dup {
			return fmt.Errorf("generator: the labels %q and %q both create the edge table %s; "+
				"gopgql creates one table per label, so two labels can share a table only when it "+
				"already exists — map it with @relationship(table:, sourceKey:, destKey:) (invariant 4)",
				prev, e.Label, e.QualifiedName())
		}
		managed[e.Key()] = e.Label
	}

	aliases := map[string]string{}
	claim := func(alias, what string) error {
		if prev, dup := aliases[alias]; dup {
			return fmt.Errorf("generator: %s and %s are both the graph element %q; "+
				"element aliases are unqualified and must be unique (invariant 4)", prev, what, alias)
		}
		aliases[alias] = what
		return nil
	}
	for _, vt := range m.VertexTables {
		if err := claim(vt.ElementAlias(), "vertex table "+vt.QualifiedName()); err != nil {
			return err
		}
	}
	for _, e := range m.EdgeTables {
		if err := claim(e.ElementAlias(), "edge table "+e.QualifiedName()); err != nil {
			return err
		}
	}

	// 5. A label may span several tables, but every table carrying it must
	// expose the same properties under it.
	return validateLabelAlignment(m)
}

// labelUse is one table's exposure of a label: the property list it publishes
// under that label, and each property's PostgreSQL type.
type labelUse struct {
	table string
	props []string
	types map[string]string
	// typesKnown is false for an element gopgql has no column list for — an edge
	// mapped onto a table it does not own. Its properties take part in the
	// per-label alignment and sit out the graph-wide type check, because gopgql
	// has nothing to check them against and PostgreSQL will do it anyway when
	// the graph is created.
	typesKnown bool
}

// validateLabelAlignment enforces SPEC.md §5.3 invariant 5 and the graph-wide
// property typing rule PostgreSQL applies.
//
// A label may be carried by several tables — that is how an interface is mapped
// — but PostgreSQL rejects a graph whose tables disagree about that label's
// properties ("mismatching number of properties" / "mismatching property
// names"). It further requires one type per property name across the whole
// graph. Checking both here turns a database error into a generate-time one.
func validateLabelAlignment(m *schema.Schema) error {
	uses := map[string][]labelUse{}
	order := []string{}
	add := func(label string, use labelUse) {
		if _, seen := uses[label]; !seen {
			order = append(order, label)
		}
		uses[label] = append(uses[label], use)
	}

	for _, vt := range m.VertexTables {
		types := columnTypes(vt.Columns)
		add(vt.Label, labelUse{table: vt.QualifiedName(), props: vt.Properties, types: types, typesKnown: true})
		for _, extra := range vt.ExtraLabels {
			add(extra.Label, labelUse{
				table: vt.QualifiedName(), props: extra.Properties, types: types, typesKnown: true,
			})
		}
	}
	for _, e := range m.EdgeTables {
		// An edge mapped onto an existing table contributes no columns: gopgql
		// knows the key columns it was told about and nothing else of that
		// table. Its property list is still aligned against other tables sharing
		// the label; its property *types* cannot be, and pretending otherwise
		// would report every one of them as a column that does not exist.
		add(e.Label, labelUse{
			table: e.QualifiedName(), props: e.Properties,
			types: columnTypes(e.Columns), typesKnown: len(e.Columns) > 0,
		})
	}

	// Per-label alignment: same count, same names in order, same types.
	for _, label := range order {
		group := uses[label]
		first := group[0]
		for _, use := range group[1:] {
			if len(use.props) != len(first.props) {
				return fmt.Errorf("generator: label %q exposes %d properties on %q but %d on %q; "+
					"tables sharing a label must align (invariant 5)",
					label, len(first.props), first.table, len(use.props), use.table)
			}
			for i, name := range first.props {
				if use.props[i] != name {
					return fmt.Errorf("generator: label %q exposes property %d as %q on %q but %q on %q; "+
						"tables sharing a label must align (invariant 5)",
						label, i+1, name, first.table, use.props[i], use.table)
				}
			}
		}
	}

	// Graph-wide: one type per property name.
	propType := map[string]string{}
	propOwner := map[string]string{}
	for _, label := range order {
		for _, use := range uses[label] {
			if !use.typesKnown {
				continue
			}
			for _, name := range use.props {
				typ, ok := use.types[name]
				if !ok {
					return fmt.Errorf("generator: label %q exposes property %q, which table %q has no column for",
						label, name, use.table)
				}
				if prev, seen := propType[name]; seen && prev != typ {
					return fmt.Errorf("generator: property %q is %s on %q but %s on %q; "+
						"a property name has one type across the whole graph",
						name, prev, propOwner[name], typ, use.table)
				}
				propType[name] = typ
				propOwner[name] = use.table
			}
		}
	}
	return nil
}

// columnTypes maps a table's column names to their rendered PostgreSQL types.
func columnTypes(cols []schema.Column) map[string]string {
	out := make(map[string]string, len(cols))
	for _, c := range cols {
		typ := c.Type
		if c.Array {
			typ += "[]"
		}
		out[c.Name] = typ
	}
	return out
}

func hasDestIndex(m *schema.Schema, e schema.EdgeTable) bool {
	for _, idx := range m.Indexes {
		if schema.QualifiedKey(idx.Schema, idx.Table) == e.Key() && sameStrings(idx.Columns, e.DestKey) {
			return true
		}
	}
	return false
}

// distinctColumns returns the union of column lists, de-duplicated, in
// first-seen order.
func distinctColumns(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, c := range l {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
