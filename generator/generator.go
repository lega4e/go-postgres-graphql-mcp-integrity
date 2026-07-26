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
		m.Indexes = append(m.Indexes, fieldIndexes(n)...)
	}

	if err := attachInterfaceLabels(m, doc); err != nil {
		return nil, err
	}

	edges := collectEdges(doc)
	for _, e := range edges {
		m.EdgeTables = append(m.EdgeTables, e)
		// SPEC.md §5.3 invariant 2: every edge table has an index on its
		// destination key column.
		m.Indexes = append(m.Indexes, schema.Index{
			Name:    e.Name + "_target_idx",
			Table:   e.Name,
			Columns: []string{e.DestKey},
		})
	}

	if err := validateInvariants(m); err != nil {
		return nil, err
	}
	return m, nil
}

func buildVertex(n *sdl.Node) (schema.VertexTable, error) {
	vt := schema.VertexTable{Name: n.Table, Label: n.Label}
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
	return vt, nil
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
			Name: name, Table: n.Table, Columns: []string{col}, Method: f.Index.Using,
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
		byTable[m.VertexTables[i].Name] = &m.VertexTables[i]
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
			vt, ok := byTable[impl.Table]
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

// collectEdges builds one physical edge table per relationship label. An OUT
// field and its @hasInverse IN partner map to the same table (SPEC.md §5.2), so
// OUT fields are processed first and IN fields only contribute a table when no
// OUT field already produced it.
func collectEdges(doc *sdl.Document) []schema.EdgeTable {
	seen := map[string]bool{}
	var edges []schema.EdgeTable

	add := func(n *sdl.Node, f *sdl.Field) {
		target := doc.NodeByType(f.TypeName)
		table := f.Rel.Table
		if table == "" {
			table = f.Rel.Type
		}
		if seen[table] {
			return
		}
		seen[table] = true

		srcTable, dstTable := n.Table, target.Table
		if f.Rel.Direction == sdl.In {
			// IN reverses orientation: the declaring type is the destination.
			srcTable, dstTable = target.Table, n.Table
		}
		edges = append(edges, schema.EdgeTable{
			Name:        table,
			Label:       f.Rel.Type,
			SourceKey:   "source_id",
			SourceTable: srcTable,
			SourceRef:   "id",
			DestKey:     "target_id",
			DestTable:   dstTable,
			DestRef:     "id",
			Columns: []schema.Column{
				{Name: "source_id", Type: "uuid", NotNull: true, References: &schema.Reference{Table: srcTable, Column: "id"}},
				{Name: "target_id", Type: "uuid", NotNull: true, References: &schema.Reference{Table: dstTable, Column: "id"}},
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
	sort.SliceStable(edges, func(i, j int) bool { return edges[i].Name < edges[j].Name })
	return edges
}

// TablesDDL renders the table half of the physical schema: vertex tables, then
// edge tables, each followed by its indexes. It emits nothing about the
// property graph.
//
// This is the half a `tables/` migration directory owns. It is separate from
// GraphDDL because the two are generated, applied and released independently —
// a project whose tables are managed elsewhere takes only the graph half
// (gopgql#38).
func TablesDDL(m *schema.Schema) string {
	var blocks []string
	for i := range m.VertexTables {
		vt := &m.VertexTables[i]
		blocks = append(blocks, withIndexes(m, vt.Name, VertexTableDDL(vt)))
	}
	for i := range m.EdgeTables {
		e := &m.EdgeTables[i]
		blocks = append(blocks, withIndexes(m, e.Name, EdgeTableDDL(e)))
	}
	return strings.Join(blocks, "\n\n")
}

// DDL renders both halves as one block: the tables, then the property graph.
//
// Nothing generates migrations in this shape any more — migrations are split
// into a tables half and a graph half. It is retained because it states the
// invariant the split rests on: the two halves concatenated are exactly the
// whole schema, which generator_test asserts.
func DDL(m *schema.Schema) string {
	return TablesDDL(m) + "\n\n" + GraphDDL(m) + "\n"
}

// withIndexes appends a table's CREATE INDEX statements to its CREATE TABLE
// block: the mandatory destination-key index on every edge table (SPEC.md §5.3
// invariant 2) and any index a field asked for with @index (SPEC.md §7 → M6).
func withIndexes(m *schema.Schema, table, block string) string {
	for _, idx := range m.Indexes {
		if idx.Table == table {
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
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", pgident.Quote(vt.Name))
	lines := make([]string, len(vt.Columns))
	for i, c := range vt.Columns {
		lines[i] = "    " + ColumnDDL(c)
	}
	b.WriteString(strings.Join(lines, ",\n"))
	b.WriteString("\n);")
	return b.String()
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
			pgident.Quote(c.References.Table), pgident.Quote(c.References.Column)))
	}
	return strings.Join(parts, " ")
}

// EdgeTableDDL renders a single CREATE TABLE for an edge table (its two key
// columns and the composite primary key). Exported for the migrate package's
// delta migrations (SPEC.md §7 → M2).
func EdgeTableDDL(e *schema.EdgeTable) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", pgident.Quote(e.Name))
	for _, c := range e.Columns {
		fmt.Fprintf(&b, "    %s,\n", ColumnDDL(c))
	}
	fmt.Fprintf(&b, "    PRIMARY KEY (%s, %s)\n", pgident.Quote(e.SourceKey), pgident.Quote(e.DestKey))
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
		pgident.Quote(idx.Name), pgident.Quote(idx.Table), using, strings.Join(cols, ", "))
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
		fmt.Fprintf(&vb, "    %s LABEL %s PROPERTIES (%s)",
			pgident.Quote(vt.Name), pgident.Quote(vt.Label), quoteList(vt.Properties))
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
			fmt.Fprintf(&eb, "    %s SOURCE KEY (%s) REFERENCES %s (%s)\n",
				pgident.Quote(e.Name), pgident.Quote(e.SourceKey),
				pgident.Quote(e.SourceTable), pgident.Quote(e.SourceRef))
			fmt.Fprintf(&eb, "            DESTINATION KEY (%s) REFERENCES %s (%s)\n",
				pgident.Quote(e.DestKey), pgident.Quote(e.DestTable), pgident.Quote(e.DestRef))
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
	}
	for _, e := range m.EdgeTables {
		if !contains(e.Properties, e.SourceKey) || !contains(e.Properties, e.DestKey) {
			return fmt.Errorf("generator: edge %q keys missing from PROPERTIES (invariant 1)", e.Name)
		}
		// 2. Every edge table has an index on its destination key column.
		if !hasDestIndex(m, e) {
			return fmt.Errorf("generator: edge %q has no index on destination key %q (invariant 2)", e.Name, e.DestKey)
		}
	}
	// 4. Table names (aliases) are unique within the graph.
	tables := map[string]bool{}
	for _, vt := range m.VertexTables {
		if tables[vt.Name] {
			return fmt.Errorf("generator: duplicate table name %q in graph (invariant 4)", vt.Name)
		}
		tables[vt.Name] = true
	}
	for _, e := range m.EdgeTables {
		if tables[e.Name] {
			return fmt.Errorf("generator: duplicate table name %q in graph (invariant 4)", e.Name)
		}
		tables[e.Name] = true
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
		add(vt.Label, labelUse{table: vt.Name, props: vt.Properties, types: types})
		for _, extra := range vt.ExtraLabels {
			add(extra.Label, labelUse{table: vt.Name, props: extra.Properties, types: types})
		}
	}
	for _, e := range m.EdgeTables {
		add(e.Label, labelUse{table: e.Name, props: e.Properties, types: columnTypes(e.Columns)})
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
		if idx.Table == e.Name && len(idx.Columns) == 1 && idx.Columns[0] == e.DestKey {
			return true
		}
	}
	return false
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
