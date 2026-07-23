// Package demo contains the milestone M0 preview generator.
//
// M0 in the gopgql specification is the "Harness" milestone: its job is to
// prove the test infrastructure and confirm that PostgreSQL 19 SQL/PGQ is
// available end to end. It deliberately predates the real sdl/generator/
// compiler packages (those arrive in M1+).
//
// To give the WASM playground something real to run before those packages
// exist, this package implements a small, self-contained translation from an
// annotated GraphQL SDL document to the SQL/PGQ artefacts M0 exercises by
// hand: the vertex/edge CREATE TABLE statements, the CREATE PROPERTY GRAPH
// mapping, and a sample GRAPH_TABLE query. It is intentionally narrow — it
// covers the worked example in SPEC.md §5.2 and simple variations of it — and
// it will be superseded by the full generator/compiler in later milestones.
//
// It has no database dependency and imports only the standard library, so it
// compiles cleanly to GOOS=js GOARCH=wasm.
package demo

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// GraphName is the fixed property-graph name emitted by the M0 preview.
const GraphName = "app_graph"

// ExampleSDL is the worked example from SPEC.md §5.2. The playground loads it
// as its initial input so a visitor sees a working translation immediately.
const ExampleSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// Result is the output of Generate: the DDL that would be emitted as a goose
// migration and a sample GRAPH_TABLE query over the resulting graph.
type Result struct {
	// DDL is the CREATE TABLE / CREATE INDEX / CREATE PROPERTY GRAPH block.
	DDL string
	// Query is a sample GRAPH_TABLE SELECT over the generated graph.
	Query string
}

// field is one member of a @node type.
type field struct {
	name     string
	typeName string // underlying named type, e.g. "String", "Person"
	nonNull  bool   // outer type ends in "!"
	list     bool   // outer type is a list, e.g. [T!]!
	// relationship metadata (nil when this is a plain scalar column)
	rel *relationship
	// ignore is true when the field carries @ignore.
	ignore bool
}

type relationship struct {
	edgeType  string // edge label, from @relationship(type:)
	direction string // "OUT" or "IN"
	table     string // optional explicit edge table
}

// node is a parsed @node type.
type node struct {
	typeName string
	label    string
	table    string
	fields   []field
}

var (
	typeBlockRe = regexp.MustCompile(`(?s)type\s+(\w+)\s+@node\s*\(([^)]*)\)\s*\{(.*?)\}`)
	argRe       = regexp.MustCompile(`(\w+)\s*:\s*"([^"]*)"`)
	relRe       = regexp.MustCompile(`@relationship\s*\(([^)]*)\)`)
	ignoreRe    = regexp.MustCompile(`@ignore\b`)
	// scalarMapping follows SPEC.md §5.1.
	scalarMapping = map[string]string{
		"Int":      "integer",
		"Float":    "double precision",
		"String":   "text",
		"Boolean":  "boolean",
		"ID":       "uuid",
		"DateTime": "timestamptz",
		"JSON":     "jsonb",
	}
)

// Generate translates an annotated GraphQL SDL document into the M0 SQL/PGQ
// artefacts. It returns an error when the document contains no @node type.
func Generate(sdl string) (Result, error) {
	nodes, err := parse(sdl)
	if err != nil {
		return Result{}, err
	}

	byType := make(map[string]*node, len(nodes))
	for i := range nodes {
		byType[nodes[i].typeName] = &nodes[i]
	}

	var ddl strings.Builder
	for i := range nodes {
		writeVertexTable(&ddl, &nodes[i])
		ddl.WriteString("\n")
	}

	edges := collectEdges(nodes, byType)
	for _, e := range edges {
		writeEdgeTable(&ddl, e)
		ddl.WriteString("\n")
	}

	writePropertyGraph(&ddl, nodes, edges)

	query := sampleQuery(nodes[0])

	return Result{DDL: strings.TrimRight(ddl.String(), "\n") + "\n", Query: query}, nil
}

func parse(sdl string) ([]node, error) {
	matches := typeBlockRe.FindAllStringSubmatch(sdl, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no `type ... @node(...)` definitions found; " +
			"the M0 preview needs at least one @node type")
	}

	nodes := make([]node, 0, len(matches))
	for _, m := range matches {
		typeName := m[1]
		args := parseArgs(m[2])
		label := args["label"]
		if label == "" {
			label = strings.ToLower(typeName)
		}
		table := args["table"]
		if table == "" {
			table = pluralize(label)
		}
		n := node{typeName: typeName, label: label, table: table}
		n.fields = parseFields(m[3])
		nodes = append(nodes, n)
	}
	return nodes, nil
}

func parseArgs(s string) map[string]string {
	out := map[string]string{}
	for _, a := range argRe.FindAllStringSubmatch(s, -1) {
		out[a[1]] = a[2]
	}
	return out
}

// parseFields walks a type body line by line. A field definition may span the
// following lines when its directives wrap, so directives are accumulated
// until the next `name: Type` line begins.
func parseFields(body string) []field {
	fieldStartRe := regexp.MustCompile(`^\s*(\w+)\s*:\s*(\[?)(\w+)(!?)\]?(!?)\s*(.*)$`)
	var fields []field
	var cur *field
	var directives strings.Builder

	flush := func() {
		if cur == nil {
			return
		}
		applyDirectives(cur, directives.String())
		fields = append(fields, *cur)
		cur = nil
		directives.Reset()
	}

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if m := fieldStartRe.FindStringSubmatch(line); m != nil {
			flush()
			list := m[2] == "["
			nonNull := m[5] == "!" || (!list && m[4] == "!")
			cur = &field{
				name:     m[1],
				typeName: m[3],
				nonNull:  nonNull,
				list:     list,
			}
			directives.WriteString(m[6])
			directives.WriteString(" ")
			continue
		}
		// continuation line: more directives for the current field
		directives.WriteString(trimmed)
		directives.WriteString(" ")
	}
	flush()
	return fields
}

func applyDirectives(f *field, directives string) {
	if ignoreRe.MatchString(directives) {
		f.ignore = true
	}
	if m := relRe.FindStringSubmatch(directives); m != nil {
		args := parseArgs(m[1])
		dir := "OUT"
		if strings.Contains(m[1], "direction:") {
			// direction is an enum, not a quoted string, so read it directly.
			if strings.Contains(m[1], "IN") && !strings.Contains(m[1], "direction: OUT") {
				dir = "IN"
			}
		}
		f.rel = &relationship{
			edgeType:  args["type"],
			direction: dir,
			table:     args["table"],
		}
	}
}

type edge struct {
	table    string
	label    string
	srcTable string
	dstTable string
}

// collectEdges builds one physical edge table per relationship label. Only
// OUT-direction relationships contribute a table; an IN field paired via
// @hasInverse is the reverse view of the same table (SPEC.md §5.2) and is
// folded away here.
func collectEdges(nodes []node, byType map[string]*node) []edge {
	seen := map[string]bool{}
	var edges []edge
	for i := range nodes {
		src := &nodes[i]
		for _, f := range src.fields {
			if f.rel == nil || f.rel.direction != "OUT" {
				continue
			}
			label := f.rel.edgeType
			if label == "" || seen[label] {
				continue
			}
			seen[label] = true
			table := f.rel.table
			if table == "" {
				table = label
			}
			dstTable := src.table
			if dst, ok := byType[f.typeName]; ok {
				dstTable = dst.table
			}
			edges = append(edges, edge{
				table:    table,
				label:    label,
				srcTable: src.table,
				dstTable: dstTable,
			})
		}
	}
	sort.SliceStable(edges, func(i, j int) bool { return edges[i].table < edges[j].table })
	return edges
}

func writeVertexTable(b *strings.Builder, n *node) {
	cols := columnDefs(n)
	fmt.Fprintf(b, "CREATE TABLE %s (\n", ident(n.table))
	for i, c := range cols {
		sep := ","
		if i == len(cols)-1 {
			sep = ""
		}
		fmt.Fprintf(b, "    %s%s\n", c, sep)
	}
	b.WriteString(");\n")
}

// columnDefs returns the SQL column definitions for a vertex table.
func columnDefs(n *node) []string {
	var cols []string
	for _, f := range n.fields {
		if f.ignore || f.rel != nil {
			continue
		}
		if f.name == "id" && f.typeName == "ID" {
			cols = append(cols, fmt.Sprintf("%s uuid PRIMARY KEY DEFAULT gen_random_uuid()", ident(f.name)))
			continue
		}
		cols = append(cols, fmt.Sprintf("%s %s", ident(f.name), sqlType(f)))
	}
	return cols
}

func sqlType(f field) string {
	base, ok := scalarMapping[f.typeName]
	if !ok {
		base = "text"
	}
	if f.list {
		base += "[]"
	}
	if f.nonNull {
		base += " NOT NULL"
	}
	return base
}

func writeEdgeTable(b *strings.Builder, e edge) {
	fmt.Fprintf(b, "CREATE TABLE %s (\n", ident(e.table))
	fmt.Fprintf(b, "    source_id uuid NOT NULL REFERENCES %s (id),\n", ident(e.srcTable))
	fmt.Fprintf(b, "    target_id uuid NOT NULL REFERENCES %s (id),\n", ident(e.dstTable))
	b.WriteString("    PRIMARY KEY (source_id, target_id)\n")
	b.WriteString(");\n")
	fmt.Fprintf(b, "CREATE INDEX %s_target_idx ON %s (target_id);\n", e.table, ident(e.table))
}

func writePropertyGraph(b *strings.Builder, nodes []node, edges []edge) {
	fmt.Fprintf(b, "CREATE PROPERTY GRAPH %s\n", ident(GraphName))
	b.WriteString("  VERTEX TABLES (\n")
	for i := range nodes {
		n := &nodes[i]
		props := vertexProps(n)
		sep := ","
		if i == len(nodes)-1 {
			sep = ""
		}
		fmt.Fprintf(b, "    %s LABEL %s PROPERTIES (%s)%s\n",
			ident(n.table), ident(n.label), strings.Join(props, ", "), sep)
	}
	b.WriteString("  )")
	if len(edges) > 0 {
		b.WriteString("\n  EDGE TABLES (\n")
		for i, e := range edges {
			sep := ","
			if i == len(edges)-1 {
				sep = ""
			}
			fmt.Fprintf(b, "    %s SOURCE KEY (source_id) REFERENCES %s (id)\n", ident(e.table), ident(e.srcTable))
			fmt.Fprintf(b, "            DESTINATION KEY (target_id) REFERENCES %s (id)\n", ident(e.dstTable))
			fmt.Fprintf(b, "            LABEL %s PROPERTIES (source_id, target_id)%s\n", ident(e.label), sep)
		}
		b.WriteString("  )")
	}
	b.WriteString(";\n")
}

// vertexProps lists the queryable properties of a vertex label: every scalar
// column, including the key (KEY columns must be re-listed in PROPERTIES per
// SPEC.md §5.3 invariant 1).
func vertexProps(n *node) []string {
	var props []string
	for _, f := range n.fields {
		if f.ignore || f.rel != nil {
			continue
		}
		props = append(props, ident(f.name))
	}
	return props
}

func sampleQuery(n node) string {
	var props []string
	for _, f := range n.fields {
		if f.ignore || f.rel != nil {
			continue
		}
		props = append(props, f.name)
	}
	if len(props) == 0 {
		props = []string{"*"}
	}
	var cols []string
	var selects []string
	for _, p := range props {
		cols = append(cols, fmt.Sprintf("v.%s AS %s", ident(p), ident(p)))
		selects = append(selects, ident(p))
	}
	orderBy := ""
	// Order by the first non-key column for a stable sample result.
	if len(props) > 1 {
		orderBy = fmt.Sprintf("\nORDER BY %s", ident(props[1]))
	} else {
		orderBy = fmt.Sprintf("\nORDER BY %s", ident(props[0]))
	}
	return fmt.Sprintf(
		"SELECT %s\nFROM GRAPH_TABLE (%s\n  MATCH (v IS %s)\n  COLUMNS (%s)\n)%s;",
		strings.Join(selects, ", "),
		ident(GraphName),
		ident(n.label),
		strings.Join(cols, ", "),
		orderBy,
	)
}

// pluralize applies the naive pluralisation rule used for deriving a table
// name from a label ("person" -> "persons"). @node(table:) overrides it.
func pluralize(s string) string {
	if s == "" {
		return s
	}
	switch {
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "z"),
		strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	case strings.HasSuffix(s, "y") && !endsInVowelY(s):
		return s[:len(s)-1] + "ies"
	default:
		return s + "s"
	}
}

func endsInVowelY(s string) bool {
	if len(s) < 2 {
		return false
	}
	switch s[len(s)-2] {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}

// sqlKeywords is a small set of reserved words that must be double-quoted when
// used as identifiers (SPEC.md §5.3 invariant 3). It is not exhaustive; the
// full generator in later milestones quotes via pgx.Identifier.Sanitize.
var sqlKeywords = map[string]bool{
	"user": true, "order": true, "group": true, "table": true, "select": true,
	"from": true, "where": true, "index": true, "column": true, "default": true,
	"check": true, "primary": true, "key": true, "references": true, "graph": true,
}

var safeIdentRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ident double-quotes an identifier when it collides with a SQL keyword or is
// not a plain lowercase identifier.
func ident(s string) string {
	if sqlKeywords[strings.ToLower(s)] || !safeIdentRe.MatchString(s) {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}
