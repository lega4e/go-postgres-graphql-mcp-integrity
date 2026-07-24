// Package compiler turns a GraphQL operation into a SQL/PGQ GRAPH_TABLE query
// plus ordered bind parameters. Compilation is pure — it never contacts a
// database (SPEC.md §6.1) — so it compiles to WASM.
//
// M3 extended the M1 single-vertex compiler with one-hop traversal, field
// arguments and query variables (SPEC.md §7 → M3):
//
//   - A nested relationship selection extends the MATCH pattern with a single
//     edge — one GRAPH_TABLE, never a second query (SPEC.md §6.2). An OUT
//     relationship emits `-[IS type]->`; an IN relationship emits `<-[IS type]-`.
//   - Field arguments become predicates in the pattern WHERE, always as bind
//     parameters — never interpolated (SPEC.md §6.2). GraphQL variables resolve
//     against the vars map; literal argument values are bound just the same.
//   - Every projected level also selects its `id` as a hidden key column so the
//     shape package can regroup the flat rows and deduplicate parents across a
//     one-to-many fan-out.
//
// M4 lifts the one-hop restriction and adds the three things that come with
// longer patterns (SPEC.md §7 → M4):
//
//   - Multi-hop MATCH chains. Nesting keeps extending one pattern, so a 3-hop
//     query is still a single GRAPH_TABLE.
//   - A depth ceiling. A selection nested deeper than MaxDepth (default 3)
//     fails compilation with a typed *DepthExceededError and never reaches the
//     database. PG19 has no variable-length paths, so gopgql rejects rather than
//     truncates (SPEC.md §3, decision 3).
//   - Interfaces. A vertex position may be an interface, matched either by the
//     shared label its implementors carry or by label alternation over their own
//     labels, `(v0 IS bot|person)`.
//   - Edge-isomorphism guards. PostgreSQL does not enforce isomorphism, so a
//     pattern will happily bind one row to two positions — a self-follow, or a
//     three-hop path that walks back to where it started. Wherever two positions
//     could bind the same row, the compiler emits `vi.id <> vj.id` (SPEC.md
//     §2.2).
//
// A selection that needs comma-separated patterns (more than one relationship at
// a level) is still rejected pointing at M5 — no silent fallbacks (SPEC.md §10).
package compiler

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/internal/pgident"
	"github.com/lega4e/gopgql/sdl"
)

// DefaultMaxDepth is the traversal-depth ceiling applied when none is
// configured: three hops from the root field (SPEC.md §6.2).
const DefaultMaxDepth = 3

// DepthExceededError reports a selection nested deeper than the compiler's
// MaxDepth. It is returned at compile time — before any SQL is emitted and so
// before anything is sent to the database.
//
// SQL/PGQ has no quantified or variable-length paths, so an unbounded traversal
// cannot be expressed at all; gopgql rejects rather than silently truncating
// the pattern (SPEC.md §3, decision 3, and §10).
type DepthExceededError struct {
	// MaxDepth is the configured ceiling, in hops from the root field.
	MaxDepth int
	// Depth is the hop count the offending selection would have required.
	Depth int
	// Path is the response-key path to the offending field, root first.
	Path []string
	// TypeName is the GraphQL type declaring the offending field.
	TypeName string
	// Field is the offending relationship field's name.
	Field string
}

func (e *DepthExceededError) Error() string {
	return fmt.Sprintf(
		"compiler: %s.%s at %s is %d hops from the root, past MaxDepth %d; "+
			"SQL/PGQ has no variable-length paths, so gopgql rejects rather than truncating",
		e.TypeName, e.Field, strings.Join(e.Path, "."), e.Depth, e.MaxDepth)
}

// Compiler compiles GraphQL operations against a fixed SDL document.
type Compiler struct {
	doc       *sdl.Document
	graphName string
	maxDepth  int
}

// Option configures a Compiler.
type Option func(*Compiler)

// WithGraphName sets the property-graph name the compiled queries target. It
// must match the name the generator used (DefaultGraphName by default).
func WithGraphName(name string) Option {
	return func(c *Compiler) { c.graphName = name }
}

// WithMaxDepth sets the traversal-depth ceiling, in hops from the root field
// (DefaultMaxDepth by default). A selection past it fails compilation with a
// *DepthExceededError. Zero permits no traversal at all; negatives are clamped
// to zero.
func WithMaxDepth(n int) Option {
	return func(c *Compiler) {
		if n < 0 {
			n = 0
		}
		c.maxDepth = n
	}
}

// MaxDepth reports the configured traversal-depth ceiling.
func (c *Compiler) MaxDepth() int { return c.maxDepth }

// New returns a Compiler for the given SDL document.
func New(doc *sdl.Document, opts ...Option) *Compiler {
	c := &Compiler{doc: doc, graphName: generator.DefaultGraphName, maxDepth: DefaultMaxDepth}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ProjectedField is one scalar column of the response: the GraphQL response key
// and the graph property it reads.
type ProjectedField struct {
	// ResponseKey is the GraphQL alias (or field name) — the key in the JSON
	// response.
	ResponseKey string
	// Property is the vertex property / column name read from the graph.
	Property string
	// Column is the unique SQL output-column name this field is projected as
	// (the AS alias) and the key it appears under in each flat result row. It is
	// distinct from ResponseKey so the same field name at two nesting levels does
	// not collide.
	Column string
}

// Selection is one vertex level of the projected result tree: its scalar fields,
// the hidden column that uniquely identifies a row at this level (used by shape
// to deduplicate parents), and any nested relationship level.
type Selection struct {
	// ResponseKey is this level's key in the JSON response — the root field's
	// response key at the top, or a relationship field's response key when
	// nested.
	ResponseKey string
	// Alias is the vertex's SQL alias in the MATCH pattern (v0, v1, …).
	Alias string
	// KeyColumn is the SQL output-column name under which this level's identifying
	// value (its `id`) appears in each flat row. shape groups rows by it.
	KeyColumn string
	// Fields are the projected scalar fields in selection order.
	Fields []ProjectedField
	// Children are nested relationship levels. At most one per level until
	// comma-separated patterns arrive in M5, but nesting may now run several
	// levels deep (SPEC.md §7 → M4).
	Children []*Selection
}

// Projection describes how to shape a compiled query's flat rows into the nested
// GraphQL response: the root selection tree.
type Projection struct {
	// Root is the top-level (root-field) selection.
	Root *Selection
}

// Compiled is the result of compiling an operation.
type Compiled struct {
	// SQL is the GRAPH_TABLE query.
	SQL string
	// Args are the ordered bind parameters ($1, $2, … in emission order).
	Args []any
	// Projection describes how to shape the returned rows into the GraphQL
	// response.
	Projection Projection
}

// Compile matches the SPEC.md §6.1 contract: it returns the SQL and ordered
// bind parameters for an operation, resolving variables against vars.
func (c *Compiler) Compile(op string, vars map[string]any) (sql string, args []any, err error) {
	cq, err := c.CompileQuery(op, vars)
	if err != nil {
		return "", nil, err
	}
	return cq.SQL, cq.Args, nil
}

// CompileQuery compiles an operation and returns the SQL, bind parameters and
// the projection used to shape the result.
func (c *Compiler) CompileQuery(op string, vars map[string]any) (*Compiled, error) {
	doc, gqlErr := parser.ParseQuery(&ast.Source{Name: "operation", Input: op})
	if gqlErr != nil {
		return nil, fmt.Errorf("compiler: parse operation: %w", gqlErr)
	}
	if len(doc.Operations) != 1 {
		return nil, fmt.Errorf("compiler: exactly one operation is supported, got %d", len(doc.Operations))
	}
	operation := doc.Operations[0]
	if operation.Operation != ast.Query {
		return nil, fmt.Errorf("compiler: only query operations are supported (SQL/PGQ graphs are read-only)")
	}

	roots := fieldSelections(operation.SelectionSet)
	if len(roots) != 1 {
		return nil, fmt.Errorf("compiler: exactly one root field is supported, got %d", len(roots))
	}
	root := roots[0]

	target := c.doc.RootTarget(root.Name)
	if target == nil {
		return nil, fmt.Errorf("compiler: unknown root field %q; queryable root fields are %s",
			root.Name, strings.Join(c.doc.RootFields(), ", "))
	}

	varDefs := map[string]*ast.VariableDefinition{}
	for _, vd := range operation.VariableDefinitions {
		varDefs[vd.Variable] = vd
	}

	b := &builder{doc: c.doc, vars: vars, varDefs: varDefs, maxDepth: c.maxDepth}
	rootSel, err := b.vertex(target, root, 0, nil)
	if err != nil {
		return nil, err
	}

	sql := b.render(c.graphName)
	return &Compiled{SQL: sql, Args: b.args, Projection: Projection{Root: rootSel}}, nil
}

// builder accumulates the MATCH pattern, projected columns, WHERE predicates and
// bind parameters while walking a single root field's selection tree.
type builder struct {
	doc      *sdl.Document
	vars     map[string]any
	varDefs  map[string]*ast.VariableDefinition
	maxDepth int

	aliasN  int
	edgeN   int
	match   strings.Builder // the MATCH path, e.g. "(v0 IS person) -[e0 IS follows]-> (v1 IS person)"
	columns []string        // COLUMNS entries, e.g. "v0.name AS v0_c0"
	outs    []string        // SELECT output names, e.g. "v0_c0"
	preds   []string        // WHERE predicates, e.g. "v0.name = $1"
	args    []any           // bind parameters in $-order
	verts   []vertexPos     // every vertex position, for the isomorphism guards
}

// vertexPos is one vertex position in the emitted pattern: its alias and the
// tables a row bound there can come from.
type vertexPos struct {
	alias  string
	tables []string
}

// vertex walks one vertex level: it writes the vertex pattern, projects the
// hidden key column and the requested scalar fields, turns field arguments into
// bind-parameter predicates, and recurses into a single nested relationship.
//
// depth is the current traversal depth in hops from the root field (0 at the
// root); path is the response-key trail to this level, used to name the
// offending selection when the depth ceiling is hit.
func (b *builder) vertex(t *sdl.Target, field *ast.Field, depth int, path []string) (*Selection, error) {
	if len(field.SelectionSet) == 0 {
		return nil, fmt.Errorf("compiler: %s (%q) must select at least one field", t.TypeName, responseKey(field))
	}

	alias := b.newAlias()
	b.writeVertex(alias, t.Labels)
	b.verts = append(b.verts, vertexPos{alias: alias, tables: t.Tables})
	path = append(append([]string{}, path...), responseKey(field))

	sel := &Selection{ResponseKey: responseKey(field), Alias: alias}

	// Hidden key column: every level projects its id so shape can regroup rows
	// and deduplicate parents across the fan-out.
	keyCol := alias + "_k"
	b.addColumn(alias, "id", keyCol)
	sel.KeyColumn = keyCol

	// Field arguments become predicates on this vertex, bound as parameters.
	for _, arg := range field.Arguments {
		if err := b.predicate(t, alias, arg); err != nil {
			return nil, err
		}
	}

	var relField *ast.Field
	var relDef *sdl.Field
	for _, s := range field.SelectionSet {
		f, ok := s.(*ast.Field)
		if !ok {
			return nil, fmt.Errorf("compiler: fragments are not supported until a later milestone")
		}
		def := findField(t.Fields, f.Name)
		if def == nil {
			return nil, fmt.Errorf("compiler: %s has no field %q", t.TypeName, f.Name)
		}
		switch {
		case def.IsScalarColumn():
			if len(f.SelectionSet) > 0 {
				return nil, fmt.Errorf("compiler: %s.%s is a scalar and cannot have a subselection", t.TypeName, f.Name)
			}
			if len(f.Arguments) > 0 {
				return nil, fmt.Errorf("compiler: arguments on scalar field %s.%s are not supported", t.TypeName, f.Name)
			}
			col := fmt.Sprintf("%s_c%d", alias, len(sel.Fields))
			b.addColumn(alias, def.Name, col)
			sel.Fields = append(sel.Fields, ProjectedField{ResponseKey: responseKey(f), Property: def.Name, Column: col})
		case def.Rel != nil:
			if relField != nil {
				return nil, fmt.Errorf(
					"compiler: %s selects more than one relationship (%q and %q); comma-separated patterns arrive in M5",
					t.TypeName, relField.Name, f.Name)
			}
			relField, relDef = f, def
		default:
			// @ignore fields have no column and no relationship.
			return nil, fmt.Errorf("compiler: %s.%s is not queryable (it maps to no column or relationship)",
				t.TypeName, f.Name)
		}
	}

	if relField != nil {
		if depth+1 > b.maxDepth {
			return nil, &DepthExceededError{
				MaxDepth: b.maxDepth,
				Depth:    depth + 1,
				Path:     append(append([]string{}, path...), responseKey(relField)),
				TypeName: t.TypeName,
				Field:    relField.Name,
			}
		}
		child := b.doc.TargetForType(relDef.TypeName)
		if child == nil {
			return nil, fmt.Errorf("compiler: %s.%s targets %q, which is not a @node type",
				t.TypeName, relField.Name, relDef.TypeName)
		}
		b.writeEdge(relDef.Rel)
		childSel, err := b.vertex(child, relField, depth+1, path)
		if err != nil {
			return nil, err
		}
		sel.Children = append(sel.Children, childSel)
	}

	return sel, nil
}

// predicate resolves a field argument to a bind parameter and records the
// WHERE predicate `alias.property = $n`. The argument name must be a scalar
// property of node (validated against the allowlist); the value is bound, never
// interpolated (SPEC.md §6.2).
func (b *builder) predicate(t *sdl.Target, alias string, arg *ast.Argument) error {
	def := findField(t.Fields, arg.Name)
	if def == nil {
		return fmt.Errorf("compiler: %s has no field %q to filter on", t.TypeName, arg.Name)
	}
	if !def.IsScalarColumn() {
		return fmt.Errorf("compiler: argument %s.%s must be a scalar property (relationship filters are not supported)",
			t.TypeName, arg.Name)
	}
	val, err := b.value(arg.Value)
	if err != nil {
		return err
	}
	b.args = append(b.args, val)
	b.preds = append(b.preds, fmt.Sprintf("%s.%s = $%d", alias, pgident.Quote(def.Name), len(b.args)))
	return nil
}

// value resolves a GraphQL argument value to its Go bind value. Variables are
// looked up in the vars map; literals are converted by kind.
func (b *builder) value(v *ast.Value) (any, error) {
	if v == nil {
		return nil, fmt.Errorf("compiler: missing argument value")
	}
	switch v.Kind {
	case ast.Variable:
		if val, ok := b.vars[v.Raw]; ok {
			return val, nil
		}
		if def := b.varDefs[v.Raw]; def != nil && def.DefaultValue != nil {
			return b.value(def.DefaultValue)
		}
		return nil, fmt.Errorf("compiler: no value supplied for variable $%s", v.Raw)
	case ast.IntValue:
		n, err := strconv.ParseInt(v.Raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("compiler: invalid Int literal %q: %w", v.Raw, err)
		}
		return n, nil
	case ast.FloatValue:
		f, err := strconv.ParseFloat(v.Raw, 64)
		if err != nil {
			return nil, fmt.Errorf("compiler: invalid Float literal %q: %w", v.Raw, err)
		}
		return f, nil
	case ast.StringValue, ast.BlockValue, ast.EnumValue:
		return v.Raw, nil
	case ast.BooleanValue:
		return v.Raw == "true", nil
	case ast.NullValue:
		return nil, nil
	default:
		return nil, fmt.Errorf("compiler: unsupported argument value of kind %v", v.Kind)
	}
}

// writeVertex appends a vertex pattern `(alias IS label)` to the MATCH path.
// An interface matched by alternation contributes several labels, which are
// joined with `|` into one label expression: `(v0 IS bot|person)`.
func (b *builder) writeVertex(alias string, labels []string) {
	quoted := make([]string, len(labels))
	for i, l := range labels {
		quoted[i] = pgident.Quote(l)
	}
	fmt.Fprintf(&b.match, "(%s IS %s)", alias, strings.Join(quoted, "|"))
}

// writeEdge appends a single directed edge pattern to the MATCH path. An OUT
// relationship (declaring type is the source) points right; an IN relationship
// (declaring type is the destination) points left (SPEC.md §7 → M3). The edge
// carries an explicit variable (e0, e1, …) matching the form proven against
// postgres:19beta2 in the M0 harness.
func (b *builder) writeEdge(rel *sdl.Relationship) {
	edge := "e" + strconv.Itoa(b.edgeN)
	b.edgeN++
	label := pgident.Quote(rel.Type)
	if rel.Direction == sdl.In {
		fmt.Fprintf(&b.match, " <-[%s IS %s]- ", edge, label)
	} else {
		fmt.Fprintf(&b.match, " -[%s IS %s]-> ", edge, label)
	}
}

// addColumn records one projected column: its COLUMNS entry and SELECT output
// name.
func (b *builder) addColumn(alias, property, out string) {
	b.columns = append(b.columns, fmt.Sprintf("%s.%s AS %s", alias, pgident.Quote(property), out))
	b.outs = append(b.outs, out)
}

// isomorphismGuards returns the `vi.id <> vj.id` predicates that exclude a
// pattern from binding one row to two positions.
//
// PostgreSQL does not enforce isomorphism (SPEC.md §2.2), so without these a
// self-follow satisfies `(a)-[follows]->(b)` with a = b, and a three-hop chain
// happily walks back to the vertex it started from. A guard is emitted for each
// pair of positions whose tables intersect — the only pairs that *can* bind the
// same row; positions over disjoint tables need none.
func (b *builder) isomorphismGuards() []string {
	var out []string
	id := pgident.Quote("id")
	for i := range b.verts {
		for j := i + 1; j < len(b.verts); j++ {
			if !overlaps(b.verts[i].tables, b.verts[j].tables) {
				continue
			}
			out = append(out, fmt.Sprintf("%s.%s <> %s.%s", b.verts[i].alias, id, b.verts[j].alias, id))
		}
	}
	return out
}

// overlaps reports whether two sorted table-name sets share a member.
func overlaps(a, c []string) bool {
	for _, x := range a {
		for _, y := range c {
			if x == y {
				return true
			}
		}
	}
	return false
}

// render assembles the final GRAPH_TABLE statement. Argument predicates come
// first so the emitted $n order matches the args slice; the isomorphism guards,
// which bind nothing, follow.
func (b *builder) render(graphName string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "SELECT %s\nFROM GRAPH_TABLE (%s\n  MATCH %s\n",
		strings.Join(b.outs, ", "), pgident.Quote(graphName), b.match.String())
	if where := append(append([]string{}, b.preds...), b.isomorphismGuards()...); len(where) > 0 {
		fmt.Fprintf(&sb, "  WHERE %s\n", strings.Join(where, " AND "))
	}
	fmt.Fprintf(&sb, "  COLUMNS (%s)\n)", strings.Join(b.columns, ", "))
	return sb.String()
}

func (b *builder) newAlias() string {
	a := "v" + strconv.Itoa(b.aliasN)
	b.aliasN++
	return a
}

// fieldSelections filters a selection set down to concrete fields.
func fieldSelections(set ast.SelectionSet) []*ast.Field {
	var fields []*ast.Field
	for _, sel := range set {
		if f, ok := sel.(*ast.Field); ok {
			fields = append(fields, f)
		}
	}
	return fields
}

// responseKey returns a field's alias, falling back to its name.
func responseKey(f *ast.Field) string {
	if f.Alias != "" {
		return f.Alias
	}
	return f.Name
}

func findField(fields []*sdl.Field, name string) *sdl.Field {
	for _, f := range fields {
		if f.Name == name {
			return f
		}
	}
	return nil
}
