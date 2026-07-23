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
		col := schema.Column{Name: f.Name, Type: base, Array: f.List, NotNull: f.NonNull}
		if f.Name == "id" && f.TypeName == "ID" {
			// Surrogate key (SPEC.md §7 → M1).
			col.PrimaryKey = true
			col.NotNull = false // PRIMARY KEY already implies NOT NULL
			col.Default = "gen_random_uuid()"
		}
		vt.Columns = append(vt.Columns, col)
		vt.Properties = append(vt.Properties, f.Name)
	}
	return vt, nil
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

// DDL renders the physical schema to a single DDL block: vertex tables, edge
// tables with their destination-key indexes, then the property graph.
func DDL(m *schema.Schema) string {
	var blocks []string
	for i := range m.VertexTables {
		blocks = append(blocks, vertexDDL(&m.VertexTables[i]))
	}
	for i := range m.EdgeTables {
		e := &m.EdgeTables[i]
		block := edgeDDL(e)
		for _, idx := range m.Indexes {
			if idx.Table == e.Name {
				block += "\n" + indexDDL(idx)
			}
		}
		blocks = append(blocks, block)
	}
	blocks = append(blocks, graphDDL(m))
	return strings.Join(blocks, "\n\n") + "\n"
}

func vertexDDL(vt *schema.VertexTable) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", pgident.Quote(vt.Name))
	lines := make([]string, len(vt.Columns))
	for i, c := range vt.Columns {
		lines[i] = "    " + columnDDL(c)
	}
	b.WriteString(strings.Join(lines, ",\n"))
	b.WriteString("\n);")
	return b.String()
}

func columnDDL(c schema.Column) string {
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
	if c.Default != "" {
		parts = append(parts, "DEFAULT "+c.Default)
	}
	if c.References != nil {
		parts = append(parts, fmt.Sprintf("REFERENCES %s (%s)",
			pgident.Quote(c.References.Table), pgident.Quote(c.References.Column)))
	}
	return strings.Join(parts, " ")
}

func edgeDDL(e *schema.EdgeTable) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", pgident.Quote(e.Name))
	for _, c := range e.Columns {
		fmt.Fprintf(&b, "    %s,\n", columnDDL(c))
	}
	fmt.Fprintf(&b, "    PRIMARY KEY (%s, %s)\n", pgident.Quote(e.SourceKey), pgident.Quote(e.DestKey))
	b.WriteString(");")
	return b.String()
}

func indexDDL(idx schema.Index) string {
	cols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		cols[i] = pgident.Quote(c)
	}
	return fmt.Sprintf("CREATE INDEX %s ON %s (%s);",
		pgident.Quote(idx.Name), pgident.Quote(idx.Table), strings.Join(cols, ", "))
}

func graphDDL(m *schema.Schema) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE PROPERTY GRAPH %s\n", pgident.Quote(m.GraphName))
	b.WriteString("  VERTEX TABLES (\n")
	vlines := make([]string, len(m.VertexTables))
	for i, vt := range m.VertexTables {
		vlines[i] = fmt.Sprintf("    %s LABEL %s PROPERTIES (%s)",
			pgident.Quote(vt.Name), pgident.Quote(vt.Label), quoteList(vt.Properties))
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
	// 4 & 5. Table names (aliases) are unique within the graph, and no label
	// spans multiple tables (M1 is one table per label).
	tables := map[string]bool{}
	vLabels := map[string]bool{}
	eLabels := map[string]bool{}
	for _, vt := range m.VertexTables {
		if tables[vt.Name] {
			return fmt.Errorf("generator: duplicate table name %q in graph (invariant 4)", vt.Name)
		}
		tables[vt.Name] = true
		if vLabels[vt.Label] {
			return fmt.Errorf("generator: vertex label %q spans multiple tables; unsupported in M1 (invariant 5)", vt.Label)
		}
		vLabels[vt.Label] = true
	}
	for _, e := range m.EdgeTables {
		if tables[e.Name] {
			return fmt.Errorf("generator: duplicate table name %q in graph (invariant 4)", e.Name)
		}
		tables[e.Name] = true
		if eLabels[e.Label] {
			return fmt.Errorf("generator: edge label %q spans multiple tables; unsupported in M1 (invariant 5)", e.Label)
		}
		eLabels[e.Label] = true
	}
	return nil
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
