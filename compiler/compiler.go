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
// M5 lifts the one-relationship-per-level restriction (SPEC.md §7 → M5). PG19
// parses comma-separated path patterns in a single MATCH but does not execute
// them (SPEC.md §2.2), so a level selecting several relationships is split into
// **separate GRAPH_TABLE calls joined on projected IDs**:
//
//   - The chain up to the branching level stays one GRAPH_TABLE — the spine.
//   - Each relationship branching off it becomes its own GRAPH_TABLE whose
//     pattern re-binds the branch-point vertex by label and projects its id.
//   - The outer query LEFT JOINs each branch onto the spine on that id, so a
//     parent with no match on one branch keeps its other branches and shapes to
//     an empty list rather than disappearing.
//   - Isomorphism guards that would have spanned the split (a branch walking back
//     to an ancestor) move to the join's ON clause, expressed over the projected
//     id columns.
//
// Branching may itself nest: a branch that branches again splits further, each
// sub-branch joining on its own branch point's id.
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
	hops := "hops"
	if e.Depth == 1 {
		hops = "hop"
	}
	return fmt.Sprintf(
		"compiler: %s.%s at %s is %d %s from the root, past MaxDepth %d; "+
			"SQL/PGQ has no variable-length paths, so gopgql rejects rather than truncating",
		e.TypeName, e.Field, strings.Join(e.Path, "."), e.Depth, hops, e.MaxDepth)
}

// Compiler compiles GraphQL operations against a fixed SDL document.
type Compiler struct {
	doc       *sdl.Document
	graphName string
	maxDepth  int
	shaping   Shaping
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
	c := &Compiler{
		doc:       doc,
		graphName: generator.DefaultGraphName,
		maxDepth:  DefaultMaxDepth,
		shaping:   DefaultShaping,
	}
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
	// GraphQLType is the named GraphQL scalar the SDL declares for this field
	// (Int, DateTime, …), with list and non-null wrappers stripped.
	GraphQLType string
	// ColumnType is the @column(type:) override, or empty when the field takes
	// the default scalar mapping (SPEC.md §5.1).
	ColumnType string
	// List is true when the field is a list type, e.g. [Int!]!.
	List bool
	// NonNull is true when the field's GraphQL type is non-null. Nothing in the
	// query path reads it: it is here for a consumer that has to name a *static*
	// Go type for this position, where a nullable column has to become a pointer
	// so a NULL stays distinguishable from a zero value (SPEC.md §7 → M14).
	NonNull bool
	// Scalar is the canonical response form this field's value takes. shape
	// normalises a leaf through it so a pgx-scanned value and a value decoded
	// out of the database's own JSON reach the same Go representation
	// (design D5). Only the compiler knows it, which is why it is recorded here.
	Scalar ScalarKind
}

// Selection is one vertex level of the projected result tree: its scalar fields,
// the hidden column that uniquely identifies a row at this level (used by shape
// to deduplicate parents), and any nested relationship level.
type Selection struct {
	// ResponseKey is this level's key in the JSON response — the root field's
	// response key at the top, or a relationship field's response key when
	// nested.
	ResponseKey string
	// TypeName is the GraphQL type bound at this level — a @node type or an
	// interface. Carried for the same reason as ProjectedField.TypeName.
	TypeName string
	// Alias is the vertex's SQL alias in the MATCH pattern (v0, v1, …).
	Alias string
	// KeyColumns are the SQL output-column names under which this level's
	// identifying values appear in each flat row, in identity order. shape
	// groups rows by the whole tuple.
	//
	// It is a slice because a @readonly type may identify a row by a declared
	// @key(fields:) rather than by a surrogate `id` — a table gopgql does not
	// own may have no `id` at all (SPEC.md §7 → M13). For every type that has
	// one it is a single column named as it always was, and the emitted SQL is
	// unchanged.
	KeyColumns []string
	// Fields are the projected scalar fields in selection order.
	Fields []ProjectedField
	// Children are nested relationship levels, in selection order. More than one
	// means the level branches: each child was compiled into its own GRAPH_TABLE
	// joined on this level's KeyColumns (SPEC.md §7 → M5).
	Children []*Selection

	// frag is the query fragment this level's columns are projected by. It is
	// unexported because it is an emission detail: the SQL-side renderer needs
	// it to know which GRAPH_TABLE a level reads from, and nothing outside this
	// package does.
	frag *fragment
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
	// Shaping is the strategy this query was compiled under, recorded so exec
	// can dispatch on it without being told again (design D1). Under SQLSide the
	// SQL returns a single `response` column the database has already
	// assembled; under GoSide it returns one flat column per projected field.
	Shaping Shaping
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
	if operation.Operation == ast.Mutation {
		// The reason this used to give — "SQL/PGQ graphs are read-only" — is
		// still true of the *graph* and is no longer the reason: a mutation is
		// compiled, just not here, because it is a function call rather than a
		// pattern (SPEC.md §7 → M11).
		return nil, fmt.Errorf("compiler: CompileQuery compiles query operations; " +
			"compile a mutation with CompileMutation")
	}
	if operation.Operation != ast.Query {
		return nil, fmt.Errorf("compiler: only query operations are supported, got %s", operation.Operation)
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

	b := newBuilder(c.doc, vars, varDefs, c.maxDepth, c.shaping)
	rootSel, err := b.vertex(target, root, 0, nil)
	if err != nil {
		return nil, err
	}

	var sql string
	if c.shaping == SQLSide {
		sql = b.renderShaped(c.graphName, rootSel)
	} else {
		sql = b.render(c.graphName)
	}
	return &Compiled{
		SQL:        sql,
		Args:       b.args,
		Projection: Projection{Root: rootSel},
		Shaping:    c.shaping,
	}, nil
}

// builder walks a root field's selection tree, accumulating one or more query
// fragments. A fragment is a single GRAPH_TABLE call; a second one appears only
// where a level selects more than one relationship and the pattern has to be
// split (SPEC.md §7 → M5).
//
// Alias, edge and bind-parameter numbering is global across fragments, so every
// projected column name is unique in the outer query and `$n` order follows
// fragment order.
type builder struct {
	doc      *sdl.Document
	vars     map[string]any
	varDefs  map[string]*ast.VariableDefinition
	maxDepth int
	shaping  Shaping

	aliasN int
	edgeN  int
	relN   int   // derived-table aliases in the SQL-side statement
	args   []any // bind parameters in $-order

	frags []*fragment // fragment 0 is the spine; the rest are branches
	cur   *fragment   // the fragment currently being written
	path  []vertexPos // ancestor positions on the walk, root first

	// order is every level's key columns in walk order — outermost level first —
	// each tagged with the fragment projecting it. It is what the flat query's
	// ORDER BY is built from, and the order is total because every key
	// identifies a level's row uniquely (design D4).
	order []orderKey
}

// orderKey is one level's key columns and the fragment that projects them.
type orderKey struct {
	frag *fragment
	cols []string
}

// outCol is one column the outer query exposes: the name it is projected under
// and, where the value needs one, a cast applied outside the GRAPH_TABLE call.
type outCol struct {
	// name is the output-column name, which is also the key the flat row
	// carries and the name the projection refers to.
	name string
	// cast is a PostgreSQL type the column is cast to in the outer SELECT, or
	// empty for none. A JSON-typed column is cast to text under **both**
	// strategies: left to the driver's own JSON decode, `19.90` inside a jsonb
	// document comes back as float64 19.9 on the Go-side path only (design D5).
	cast string
}

// selectExpr renders the column for an outer SELECT list, qualified by the
// fragment alias when the query joins more than one fragment.
func (o outCol) selectExpr(qualifier string) string {
	ref := o.name
	if qualifier != "" {
		ref = qualifier + "." + o.name
	}
	if o.cast == "" {
		return ref
	}
	return fmt.Sprintf("%s::%s AS %s", ref, o.cast, o.name)
}

// fragment is one GRAPH_TABLE call plus how it attaches to its parent fragment.
type fragment struct {
	name    string          // outer-query alias: q0, q1, …
	match   strings.Builder // the MATCH path
	columns []string        // COLUMNS entries, e.g. "v0.name AS v0_c0"
	outs    []outCol        // columns the outer SELECT exposes (projection-visible)
	preds   []string        // WHERE predicates inside this GRAPH_TABLE
	verts   []vertexPos     // this fragment's vertex positions, for its own guards

	// Branch wiring — empty on the spine.
	parent     *fragment // fragment holding the branch point
	joinKeys   []string  // this fragment's projected branch-point identity columns
	parentKeys []string  // the parent fragment's key columns it joins against
	onGuards   []string  // isomorphism guards that span the split, for the ON clause
}

// vertexPos is one vertex position in an emitted pattern: its alias, the tables a
// row bound there can come from, the fragment it lives in, the columns that
// identify a row bound there, and the output columns they are projected as.
type vertexPos struct {
	alias    string
	tables   []string
	frag     *fragment
	identity []string // the vertex's identity columns, as the graph exposes them
	keys     []string // the output columns those are projected as
}

func newBuilder(doc *sdl.Document, vars map[string]any, varDefs map[string]*ast.VariableDefinition, maxDepth int, shaping Shaping) *builder {
	spine := &fragment{name: "q0"}
	return &builder{
		doc:      doc,
		vars:     vars,
		varDefs:  varDefs,
		maxDepth: maxDepth,
		shaping:  shaping,
		frags:    []*fragment{spine},
		cur:      spine,
	}
}

// relSelection is one nested relationship selected at a level: the queried field
// and its SDL definition.
type relSelection struct {
	field *ast.Field
	def   *sdl.Field
}

// vertex walks one vertex level: it writes the vertex pattern, projects the
// hidden key column and the requested scalar fields, turns field arguments into
// bind-parameter predicates, and recurses into the nested relationships.
//
// One nested relationship extends the current pattern. Several split it: the
// first continues the current fragment only if it is alone, otherwise every
// relationship becomes its own GRAPH_TABLE joined on this level's id (SPEC.md
// §6.2, §7 → M5).
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
	path = append(append([]string{}, path...), responseKey(field))

	sel := &Selection{ResponseKey: responseKey(field), TypeName: t.TypeName, Alias: alias, frag: b.cur}

	// Hidden key columns: every level projects its identity so shape can regroup
	// rows and deduplicate parents across the fan-out. That is `id` for every
	// type gopgql owns, and a declared @key for a @readonly type without one.
	keyCols := make([]string, len(t.Identity))
	for i, col := range t.Identity {
		// A single-column identity keeps the name it always had, so the emitted
		// SQL for every pre-M13 schema is byte-identical.
		keyCols[i] = alias + "_k"
		if len(t.Identity) > 1 {
			keyCols[i] = fmt.Sprintf("%s_k%d", alias, i)
		}
		b.addColumn(alias, col, keyCols[i], "")
	}
	sel.KeyColumns = keyCols

	// Every level's key joins the flat query's ORDER BY, outermost level first,
	// so the Go-side and SQL-side strategies deliver lists in the same order
	// (design D4).
	b.order = append(b.order, orderKey{frag: b.cur, cols: keyCols})

	pos := vertexPos{alias: alias, tables: t.Tables, frag: b.cur, identity: t.Identity, keys: keyCols}
	b.addPosition(pos)
	b.path = append(b.path, pos)
	defer func() { b.path = b.path[:len(b.path)-1] }()

	// Field arguments become predicates on this vertex, bound as parameters.
	for _, arg := range field.Arguments {
		if err := b.predicate(t, alias, arg); err != nil {
			return nil, err
		}
	}

	var rels []relSelection
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
			kind := classifyScalar(def.TypeName, def.ColumnType)
			// A scalar with no canonical response form is refused under SQL-side
			// shaping and accepted under Go-side, which makes no cross-strategy
			// promise about it (design D5).
			if kind == ScalarUnknown && b.shaping == SQLSide {
				return nil, &UnshapeableScalarError{
					TypeName:    t.TypeName,
					Field:       f.Name,
					GraphQLType: def.TypeName,
					ColumnType:  def.ColumnType,
				}
			}
			col := fmt.Sprintf("%s_c%d", alias, len(sel.Fields))
			// An embedded JSON document is projected ::text so the driver never
			// runs its own JSON decode over it — that decode is lossy, and it
			// would run on the Go-side path only (design D5).
			cast := ""
			if kind == ScalarJSON {
				cast = "text"
			}
			// The graph exposes the *column*, which @column(name:) may have
			// renamed away from the GraphQL field name (SPEC.md §7 → M6).
			b.addColumn(alias, def.ColumnName(), col, cast)
			sel.Fields = append(sel.Fields, ProjectedField{
				ResponseKey: responseKey(f),
				Property:    def.ColumnName(),
				Column:      col,
				GraphQLType: def.TypeName,
				ColumnType:  def.ColumnType,
				List:        def.List,
				NonNull:     def.NonNull,
				Scalar:      kind,
			})
		case def.Rel != nil:
			rels = append(rels, relSelection{field: f, def: def})
		default:
			// @ignore fields have no column and no relationship.
			return nil, fmt.Errorf("compiler: %s.%s is not queryable (it maps to no column or relationship)",
				t.TypeName, f.Name)
		}
	}

	// The depth ceiling applies to every branch, and is checked before any
	// fragment is created so a rejection still emits no SQL at all.
	for _, rel := range rels {
		if depth+1 > b.maxDepth {
			return nil, &DepthExceededError{
				MaxDepth: b.maxDepth,
				Depth:    depth + 1,
				Path:     append(append([]string{}, path...), responseKey(rel.field)),
				TypeName: t.TypeName,
				Field:    rel.field.Name,
			}
		}
	}

	split := len(rels) > 1
	for _, rel := range rels {
		child := b.doc.TargetForType(rel.def.TypeName)
		if child == nil {
			return nil, fmt.Errorf("compiler: %s.%s targets %q, which is not a @node type",
				t.TypeName, rel.field.Name, rel.def.TypeName)
		}

		here := b.cur
		if split {
			// Comma-separated patterns parse but do not execute on PG19, so this
			// relationship gets its own GRAPH_TABLE, re-binding the branch-point
			// vertex by label and joining on its projected id.
			b.cur = b.branch(t, pos)
		}
		b.writeEdge(rel.def.Rel)
		childSel, err := b.vertex(child, rel.field, depth+1, path)
		if err != nil {
			return nil, err
		}
		b.cur = here
		sel.Children = append(sel.Children, childSel)
	}

	return sel, nil
}

// branch starts a new fragment for one relationship of a branching level. Its
// pattern begins by re-binding the branch-point vertex — same labels, a fresh
// alias — and projects that vertex's id as the join key, which the outer query
// matches against the branch point's own key column.
func (b *builder) branch(t *sdl.Target, at vertexPos) *fragment {
	f := &fragment{
		name:       "q" + strconv.Itoa(len(b.frags)),
		parent:     at.frag,
		parentKeys: at.keys,
	}
	b.frags = append(b.frags, f)

	alias := b.newAlias()
	prev := b.cur
	b.cur = f
	b.writeVertex(alias, t.Labels)
	// The join keys are projected but not exposed to the outer SELECT: they
	// repeat the branch point's identity, which the parent fragment already
	// carries.
	for i, col := range t.Identity {
		name := alias + "_j"
		if len(t.Identity) > 1 {
			name = fmt.Sprintf("%s_j%d", alias, i)
		}
		f.joinKeys = append(f.joinKeys, name)
		f.columns = append(f.columns, fmt.Sprintf("%s.%s AS %s", alias, pgident.Quote(col), name))
	}
	b.cur = prev

	// The re-bound vertex is the branch point, so it needs no guard against it;
	// its own descendants are guarded within this fragment as usual.
	f.verts = append(f.verts, vertexPos{
		alias: alias, tables: t.Tables, frag: f, identity: t.Identity, keys: f.joinKeys,
	})
	return f
}

// addPosition records a vertex position for the isomorphism guards. Positions in
// the same fragment are guarded inside its WHERE; a position whose ancestor sits
// in another fragment is guarded in the join's ON clause, over the projected id
// columns — the guard has to survive the split (SPEC.md §2.2).
func (b *builder) addPosition(pos vertexPos) {
	b.cur.verts = append(b.cur.verts, pos)
	for _, anc := range b.path {
		if anc.frag == pos.frag || !overlaps(anc.tables, pos.tables) {
			continue
		}
		// The branch point itself is re-bound as this fragment's first position
		// and joined on its identity, so the fragment's own guards already cover
		// it.
		if anc.frag == pos.frag.parent && sameCols(anc.keys, pos.frag.parentKeys) {
			continue
		}
		pos.frag.onGuards = append(pos.frag.onGuards,
			distinctGuard(pos.frag.name, pos.keys, anc.frag.name, anc.keys))
	}
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
	b.cur.preds = append(b.cur.preds, fmt.Sprintf("%s.%s = $%d", alias, pgident.Quote(def.ColumnName()), len(b.args)))
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
		def := b.varDefs[v.Raw]
		if def != nil && def.DefaultValue != nil {
			return b.value(def.DefaultValue)
		}
		// An unset variable declared nullable binds NULL, which is what GraphQL
		// says an omitted nullable variable means. Failing instead — as this did
		// before M11 — makes every optional argument unusable: there would be no
		// way to write an operation whose argument the caller may leave out.
		//
		// NULL is a *value*, and it is not the same thing as leaving the
		// argument out of the operation document, which is what reaches a
		// function's SQL DEFAULT (see callArgs).
		if def != nil && !def.Type.NonNull {
			return nil, nil
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
	fmt.Fprintf(&b.cur.match, "(%s IS %s)", alias, strings.Join(quoted, "|"))
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
		fmt.Fprintf(&b.cur.match, " <-[%s IS %s]- ", edge, label)
	} else {
		fmt.Fprintf(&b.cur.match, " -[%s IS %s]-> ", edge, label)
	}
}

// addColumn records one projected column on the current fragment: its COLUMNS
// entry and the name the outer SELECT exposes it under. cast, when non-empty,
// is applied outside the GRAPH_TABLE call rather than inside COLUMNS, which
// takes plain property references.
func (b *builder) addColumn(alias, property, out, cast string) {
	b.cur.columns = append(b.cur.columns, fmt.Sprintf("%s.%s AS %s", alias, pgident.Quote(property), out))
	b.cur.outs = append(b.cur.outs, outCol{name: out, cast: cast})
}

// isomorphismGuards returns the `vi.id <> vj.id` predicates that exclude a
// pattern from binding one row to two positions.
//
// PostgreSQL does not enforce isomorphism (SPEC.md §2.2), so without these a
// self-follow satisfies `(a)-[follows]->(b)` with a = b, and a three-hop chain
// happily walks back to the vertex it started from. A guard is emitted for each
// pair of positions whose tables intersect — the only pairs that *can* bind the
// same row; positions over disjoint tables need none.
func (f *fragment) isomorphismGuards() []string {
	var out []string
	for i := range f.verts {
		for j := i + 1; j < len(f.verts); j++ {
			if !overlaps(f.verts[i].tables, f.verts[j].tables) {
				continue
			}
			a, b := f.verts[i], f.verts[j]
			cols := make([]string, len(a.identity))
			for k, c := range a.identity {
				cols[k] = pgident.Quote(c)
			}
			out = append(out, distinctGuard(a.alias, cols, b.alias, cols))
		}
	}
	return out
}

// distinctGuard renders "these two positions are not the same row" over an
// identity of any width.
//
// A single column keeps `<>`, which is what every schema with a surrogate `id`
// emitted before M13 and what its golden files still expect. A wider identity
// becomes a **disjunction of IS DISTINCT FROM**, one per component, and the
// NULL-safety is the whole reason for that shape: `(a,b) <> (c,d)` evaluates to
// NULL as soon as one component is NULL, and a WHERE that is NULL excludes the
// row. A @key column is only UNIQUE, never NOT NULL, so a plain `<>` over a
// nullable key column would silently *drop* rows rather than admit them — the
// failure this guard exists to prevent, inverted.
//
// A surrogate `id` is a NOT NULL primary key, so the single-column form is
// exactly as safe as it has always been.
func distinctGuard(leftAlias string, left []string, rightAlias string, right []string) string {
	if len(left) == 1 {
		return fmt.Sprintf("%s.%s <> %s.%s", leftAlias, left[0], rightAlias, right[0])
	}
	parts := make([]string, len(left))
	for i := range left {
		parts[i] = fmt.Sprintf("%s.%s IS DISTINCT FROM %s.%s",
			leftAlias, left[i], rightAlias, right[i])
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// sameCols reports whether two column lists are equal, order included.
func sameCols(a, b []string) bool {
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

// render assembles the final flat statement — the Go-side strategy's SQL. A
// single fragment renders as the bare GRAPH_TABLE query M1–M4 always emitted;
// several render as the spine LEFT JOINed with each branch on the projected ids
// (SPEC.md §6.2 → multi-pattern workaround).
//
// Both forms close with an ORDER BY over every level's key column, outermost
// level first. The order is arbitrary — the keys are uuids — but it is a *total*
// one, and it is the same order the SQL-side strategy's json_agg carries, which
// is what byte-identity needs (design D4).
func (b *builder) render(graphName string) string {
	if len(b.frags) == 1 {
		f := b.frags[0]
		return fmt.Sprintf("SELECT %s\nFROM %s%s",
			strings.Join(selectList(f.outs, ""), ", "), f.graphTable(graphName, ""), b.orderBy(false))
	}

	var outs []string
	for _, f := range b.frags {
		outs = append(outs, selectList(f.outs, f.name)...)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "SELECT %s\nFROM %s AS %s",
		strings.Join(outs, ", "), b.frags[0].graphTable(graphName, "  "), b.frags[0].name)
	for _, f := range b.frags[1:] {
		on := branchJoinOn(f)
		fmt.Fprintf(&sb, "\nLEFT JOIN %s AS %s ON %s",
			f.graphTable(graphName, "  "), f.name, strings.Join(on, " AND "))
	}
	sb.WriteString(b.orderBy(true))
	return sb.String()
}

// branchJoinOn renders a branch fragment's ON clause: the branch-point identity
// equated component by component, then any guard that spans the split.
//
// Equality rather than IS NOT DISTINCT FROM is deliberate. A NULL component
// means the row has no identity at that position, and a row with no identity
// must not join — shape skips such a row for the same reason, so the two
// strategies agree about it.
func branchJoinOn(f *fragment) []string {
	on := make([]string, 0, len(f.joinKeys)+len(f.onGuards))
	for i := range f.joinKeys {
		on = append(on, fmt.Sprintf("%s.%s = %s.%s",
			f.name, f.joinKeys[i], f.parent.name, f.parentKeys[i]))
	}
	return append(on, f.onGuards...)
}

// orderBy renders the flat query's ORDER BY clause over every level's key
// column. qualified selects the multi-fragment form, where a column has to name
// the fragment it came from.
func (b *builder) orderBy(qualified bool) string {
	if len(b.order) == 0 {
		return ""
	}
	var cols []string
	for _, k := range b.order {
		for _, c := range k.cols {
			if qualified {
				cols = append(cols, k.frag.name+"."+c)
			} else {
				cols = append(cols, c)
			}
		}
	}
	return "\nORDER BY " + strings.Join(cols, ", ")
}

// selectList renders a fragment's exposed columns for an outer SELECT list,
// qualified by the fragment alias when the query joins more than one.
func selectList(outs []outCol, qualifier string) []string {
	list := make([]string, len(outs))
	for i, o := range outs {
		list[i] = o.selectExpr(qualifier)
	}
	return list
}

// graphTable renders one fragment as a GRAPH_TABLE call. Argument predicates come
// first so the emitted $n order matches the args slice; the isomorphism guards,
// which bind nothing, follow. indent prefixes the inner lines when the call is
// nested inside an outer query.
func (f *fragment) graphTable(graphName, indent string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "GRAPH_TABLE (%s\n%s  MATCH %s\n", pgident.Quote(graphName), indent, f.match.String())
	if where := append(append([]string{}, f.preds...), f.isomorphismGuards()...); len(where) > 0 {
		fmt.Fprintf(&sb, "%s  WHERE %s\n", indent, strings.Join(where, " AND "))
	}
	fmt.Fprintf(&sb, "%s  COLUMNS (%s)\n%s)", indent, strings.Join(f.columns, ", "), indent)
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
