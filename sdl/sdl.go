// Package sdl parses and validates an annotated GraphQL SDL document and reads
// its directives into a typed mapping model.
//
// It is the front end of the gopgql pipeline (SPEC.md §4): the generator turns
// the model into DDL and the compiler turns GraphQL queries into GRAPH_TABLE
// SQL, both reading the same model so the physical schema and the query mapping
// cannot drift.
//
// M1 supports the graph-structure directive surface (SPEC.md §7 → M1):
// @node, @relationship, @hasInverse and @ignore, with surrogate uuid keys and
// the default scalar mapping (§5.1). Parsing and validation are delegated to
// vektah/gqlparser/v2; this package layers gopgql's directive semantics on top.
//
// M4 adds GraphQL interfaces (SPEC.md §7 → M4). An interface makes the tables of
// every implementing @node type addressable as one queryable position, mapped
// one of two ways depending on whether the interface itself carries @node:
//
//   - @node(label:) — a *shared label*. Every implementor's vertex table carries
//     that label with an aligned property list, and a pattern matches it with a
//     single label, `(v IS actor)`.
//   - no @node — *label alternation*. A pattern matches the implementors' own
//     labels, `(v IS bot|person)`.
//
// Both are exposed to the compiler through Target, which also reports the
// tables a vertex bound at that position can come from — what decides where two
// positions in one pattern could bind the same row and so need an
// edge-isomorphism guard (SPEC.md §2.2).
//
// It has no database dependency and compiles to WASM.
package sdl

import (
	"fmt"
	"sort"
	"strings"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/lega4e/gopgql/internal/pgident"
)

// Direction is the orientation of a @relationship relative to the declaring
// type.
type Direction string

const (
	// Out is a relationship pointing away from the declaring type: the
	// declaring type is the edge source.
	Out Direction = "OUT"
	// In is a relationship pointing toward the declaring type: the declaring
	// type is the edge destination (the reverse view of an Out edge).
	In Direction = "IN"
)

// Relationship is the @relationship directive on a field.
type Relationship struct {
	// Type is the edge label (@relationship(type:)).
	Type string
	// Direction is OUT or IN; defaults to OUT when omitted.
	Direction Direction
	// Table is the explicit edge table name (@relationship(table:)); empty
	// means "derive from Type".
	Table string
	// HasInverse names the paired field when @hasInverse is present.
	HasInverse string
}

// Field is one member of a @node type.
type Field struct {
	// Name is the GraphQL field name.
	Name string
	// TypeName is the underlying named type (e.g. "String", "Person"), with
	// any list and non-null wrappers stripped.
	TypeName string
	// NonNull is true when the outermost type is non-null (ends in "!").
	NonNull bool
	// List is true when the field is a list type (e.g. [T!]!).
	List bool
	// Ignore is true when the field carries @ignore: present in GraphQL,
	// absent from the database.
	Ignore bool
	// Rel is the relationship metadata, or nil for a plain scalar column.
	Rel *Relationship
	// Column is the physical column name from @column(name:); empty means the
	// field name is used (SPEC.md §7 → M6).
	Column string
	// ColumnType is the PostgreSQL type from @column(type:), overriding the
	// default scalar mapping (SPEC.md §5.1); empty means the default.
	ColumnType string
	// Unique is true when the field carries @unique: the database rejects a
	// duplicate value.
	Unique bool
	// Index is the secondary index requested with @index, or nil.
	Index *IndexSpec
}

// IndexSpec is a per-field @index request: an optional explicit name and access
// method (`USING`), both defaulted by the generator when omitted.
type IndexSpec struct {
	// Name is the index name from @index(name:); empty means a derived one.
	Name string
	// Using is the access method from @index(using:) — btree, hash, gin, … —
	// empty means the database default.
	Using string
}

// IsScalarColumn reports whether the field maps to a physical column: it is not
// ignored and is not a relationship.
func (f *Field) IsScalarColumn() bool {
	return !f.Ignore && f.Rel == nil
}

// ColumnName is the physical column a scalar field maps to: its @column(name:)
// override, or the field name. The graph exposes the column under this name too,
// so the compiler projects properties by it (SPEC.md §5.3 invariant 1).
func (f *Field) ColumnName() string {
	if f.Column != "" {
		return f.Column
	}
	return f.Name
}

// Node is a GraphQL object type carrying @node.
type Node struct {
	// TypeName is the GraphQL type name (e.g. "Person").
	TypeName string
	// Label is the graph label (@node(label:)), defaulting to the lowercased
	// type name.
	Label string
	// Table is the physical table name (@node(table:)), defaulting to the
	// pluralized label.
	Table string
	// Fields are the type's fields in declaration order.
	Fields []*Field
}

// Interface is a GraphQL interface implemented by @node types. It maps the
// tables of all its implementors to one queryable position (SPEC.md §7 → M4).
type Interface struct {
	// TypeName is the GraphQL interface name (e.g. "Actor").
	TypeName string
	// Label is the shared graph label when the interface carries @node(label:) —
	// every implementor's vertex table exposes it. It is empty for an
	// unlabelled interface, which is matched by label alternation instead.
	Label string
	// RootField is this interface's query root-field name: the pluralized
	// label, or the pluralized lowercased type name when unlabelled.
	RootField string
	// Fields are the interface's fields in declaration order. Every implementor
	// declares each of them (GraphQL enforces it), so they are projectable and
	// traversable wherever the interface is matched.
	Fields []*Field
	// Implementors are the @node types implementing it, in stable (type-name)
	// order.
	Implementors []*Node
}

// Target is a queryable vertex position: a @node type or an interface. It
// carries exactly what the compiler needs to emit a vertex pattern.
type Target struct {
	// TypeName is the GraphQL type at this position.
	TypeName string
	// Labels are the alternatives of the label expression to emit: one label
	// for a @node type or a shared-label interface, several — joined with `|` —
	// for an interface matched by alternation. Sorted, so emitted SQL is
	// deterministic.
	Labels []string
	// Fields are the fields projectable and traversable at this position.
	Fields []*Field
	// Tables are the physical tables a vertex bound here can come from. Two
	// positions whose table sets intersect could bind the same row, which is
	// where an isomorphism guard is required (SPEC.md §2.2). Sorted.
	Tables []string
}

// Document is the typed mapping model built from an SDL source: the set of
// @node types with their resolved labels, table names and relationships, plus
// the interfaces spanning them.
type Document struct {
	// Nodes are the @node types in a stable (type-name) order.
	Nodes []*Node
	// Interfaces are the interfaces implemented by @node types, in a stable
	// (type-name) order.
	Interfaces []*Interface

	byType  map[string]*Node
	byTable map[string]*Node
	byIface map[string]*Interface
	// targets and roots resolve a GraphQL type name and a query root-field name
	// respectively to the vertex position they select.
	targets map[string]*Target
	roots   map[string]*Target
	// Raw is the validated gqlparser schema, retained for callers that need
	// the underlying AST (SPEC.md §4: "*ast.Schema + directive model").
	Raw *ast.Schema
}

// NodeByType returns the node for a GraphQL type name, or nil.
func (d *Document) NodeByType(name string) *Node { return d.byType[name] }

// NodeByTable returns the node whose physical table has the given name, or nil.
// Query root fields resolve to nodes by table name (SPEC.md §7 → M1).
func (d *Document) NodeByTable(name string) *Node { return d.byTable[name] }

// InterfaceByType returns the interface for a GraphQL type name, or nil.
func (d *Document) InterfaceByType(name string) *Interface { return d.byIface[name] }

// RootTarget resolves a query root-field name to the vertex position it
// selects, or nil when no @node table and no interface answers to that name.
func (d *Document) RootTarget(field string) *Target { return d.roots[field] }

// TargetForType resolves a GraphQL type name — a @node type or an interface —
// to the vertex position a selection of that type occupies, or nil.
func (d *Document) TargetForType(name string) *Target { return d.targets[name] }

// RootFields lists every queryable root-field name, sorted. It exists so a
// compile error can tell the caller what *is* queryable.
func (d *Document) RootFields() []string {
	out := make([]string, 0, len(d.roots))
	for name := range d.roots {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// prelude declares the gopgql directive vocabulary and custom scalars so
// gqlparser recognises and validates them. It is the M1 subset of SPEC.md §5.
const prelude = `directive @node(label: String!, table: String) on OBJECT | INTERFACE
directive @relationship(type: String!, direction: RelDirection, table: String) on FIELD_DEFINITION
directive @hasInverse(field: String!) on FIELD_DEFINITION
directive @ignore on FIELD_DEFINITION
directive @column(name: String, type: String) on FIELD_DEFINITION
directive @index(name: String, using: String) on FIELD_DEFINITION | OBJECT
directive @unique on FIELD_DEFINITION
enum RelDirection { OUT IN }
scalar DateTime
scalar JSON
`

// Parse parses and validates an SDL source and builds the mapping model.
//
// Validation covers both GraphQL well-formedness (via gqlparser) and gopgql's
// own rules: at least one @node type, a surrogate `id: ID!` key on every node,
// and consistent @relationship/@hasInverse pairing.
func Parse(src string) (*Document, error) {
	schema, err := gqlparser.LoadSchema(
		&ast.Source{Name: "gopgql/prelude", Input: prelude, BuiltIn: true},
		&ast.Source{Name: "schema.graphql", Input: src},
	)
	if err != nil {
		return nil, fmt.Errorf("sdl: %w", err)
	}

	doc := &Document{
		byType:  map[string]*Node{},
		byTable: map[string]*Node{},
		byIface: map[string]*Interface{},
		targets: map[string]*Target{},
		roots:   map[string]*Target{},
		Raw:     schema,
	}

	var ifaceDefs []*ast.Definition
	for _, def := range schema.Types {
		if def.BuiltIn {
			continue
		}
		switch def.Kind {
		case ast.Object:
			if nodeDir := def.Directives.ForName("node"); nodeDir != nil {
				doc.Nodes = append(doc.Nodes, buildNode(def, nodeDir))
			}
		case ast.Interface:
			ifaceDefs = append(ifaceDefs, def)
		}
	}

	if len(doc.Nodes) == 0 {
		return nil, fmt.Errorf("sdl: no `type ... @node(...)` definitions found; at least one is required")
	}

	// Stable order so generated migrations and queries are deterministic.
	sort.Slice(doc.Nodes, func(i, j int) bool { return doc.Nodes[i].TypeName < doc.Nodes[j].TypeName })

	for _, n := range doc.Nodes {
		if prev, dup := doc.byTable[n.Table]; dup {
			return nil, fmt.Errorf("sdl: types %q and %q both map to table %q; disambiguate with @node(table:)",
				prev.TypeName, n.TypeName, n.Table)
		}
		doc.byType[n.TypeName] = n
		doc.byTable[n.Table] = n
	}

	if err := doc.buildInterfaces(schema, ifaceDefs); err != nil {
		return nil, err
	}

	if err := doc.validate(); err != nil {
		return nil, err
	}
	if err := doc.buildTargets(); err != nil {
		return nil, err
	}
	return doc, nil
}

// buildInterfaces reads every interface an @node type implements into the
// mapping model. An interface implemented by no @node type is ignored — it is
// GraphQL-only, like an @ignore field.
func (d *Document) buildInterfaces(schema *ast.Schema, defs []*ast.Definition) error {
	implementors := map[string][]*Node{}
	for _, n := range d.Nodes {
		def := schema.Types[n.TypeName]
		for _, name := range def.Interfaces {
			implementors[name] = append(implementors[name], n)
		}
	}

	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	for _, def := range defs {
		impls := implementors[def.Name]
		if len(impls) == 0 {
			continue
		}
		// Every possible type of the interface must be a mapped @node; a plain
		// object implementing it would have no table to contribute.
		for _, pt := range schema.PossibleTypes[def.Name] {
			if d.byType[pt.Name] == nil {
				return fmt.Errorf("sdl: interface %s is implemented by %s, which is not a `type ... @node(...)`; "+
					"every implementor must map to a table", def.Name, pt.Name)
			}
		}

		iface := &Interface{TypeName: def.Name, Fields: buildFields(def), Implementors: impls}
		if nodeDir := def.Directives.ForName("node"); nodeDir != nil {
			iface.Label = argString(nodeDir, "label")
			iface.RootField = pluralize(iface.Label)
		} else {
			iface.RootField = pluralize(strings.ToLower(def.Name))
		}
		d.Interfaces = append(d.Interfaces, iface)
		d.byIface[iface.TypeName] = iface
	}
	return nil
}

// buildTargets resolves every queryable vertex position: one per @node type and
// one per interface, indexed by GraphQL type name and by query root-field name.
func (d *Document) buildTargets() error {
	claim := func(field, owner string, t *Target) error {
		if prev, dup := d.roots[field]; dup {
			return fmt.Errorf("sdl: %s and %s both answer to the root field %q; "+
				"disambiguate with @node(table:) or @node(label:)", prev.TypeName, owner, field)
		}
		d.roots[field] = t
		return nil
	}

	for _, n := range d.Nodes {
		t := &Target{TypeName: n.TypeName, Labels: []string{n.Label}, Fields: n.Fields, Tables: []string{n.Table}}
		d.targets[n.TypeName] = t
		if err := claim(n.Table, n.TypeName, t); err != nil {
			return err
		}
	}

	for _, iface := range d.Interfaces {
		t := &Target{TypeName: iface.TypeName, Fields: iface.Fields}
		for _, impl := range iface.Implementors {
			t.Tables = append(t.Tables, impl.Table)
			if iface.Label == "" {
				// Unlabelled: matched by alternation over the implementors'
				// own labels.
				t.Labels = append(t.Labels, impl.Label)
			}
		}
		if iface.Label != "" {
			t.Labels = []string{iface.Label}
		}
		sort.Strings(t.Labels)
		sort.Strings(t.Tables)
		d.targets[iface.TypeName] = t
		if err := claim(iface.RootField, iface.TypeName, t); err != nil {
			return err
		}
	}
	return nil
}

func buildNode(def *ast.Definition, nodeDir *ast.Directive) *Node {
	// label is required by the directive definition, so gqlparser guarantees a
	// non-empty value here.
	label := argString(nodeDir, "label")
	table := argString(nodeDir, "table")
	if table == "" {
		table = pluralize(label)
	}
	return &Node{TypeName: def.Name, Label: label, Table: table, Fields: buildFields(def)}
}

// buildFields reads a type's (or interface's) fields into the mapping model,
// skipping GraphQL introspection meta-fields.
func buildFields(def *ast.Definition) []*Field {
	var fields []*Field
	for _, fd := range def.Fields {
		if strings.HasPrefix(fd.Name, "__") {
			continue // introspection meta-fields
		}
		f := &Field{
			Name:     fd.Name,
			TypeName: namedType(fd.Type),
			NonNull:  fd.Type.NonNull,
			List:     fd.Type.Elem != nil,
		}
		if fd.Directives.ForName("ignore") != nil {
			f.Ignore = true
		}
		if colDir := fd.Directives.ForName("column"); colDir != nil {
			f.Column = argString(colDir, "name")
			f.ColumnType = argString(colDir, "type")
		}
		if fd.Directives.ForName("unique") != nil {
			f.Unique = true
		}
		if idxDir := fd.Directives.ForName("index"); idxDir != nil {
			f.Index = &IndexSpec{Name: argString(idxDir, "name"), Using: argString(idxDir, "using")}
		}
		if relDir := fd.Directives.ForName("relationship"); relDir != nil {
			rel := &Relationship{
				Type:      argString(relDir, "type"),
				Direction: Out,
				Table:     argString(relDir, "table"),
			}
			if d := argString(relDir, "direction"); d == string(In) {
				rel.Direction = In
			}
			if inv := fd.Directives.ForName("hasInverse"); inv != nil {
				rel.HasInverse = argString(inv, "field")
			}
			f.Rel = rel
		}
		fields = append(fields, f)
	}
	return fields
}

// validate enforces gopgql's semantic rules beyond GraphQL well-formedness.
func (d *Document) validate() error {
	for _, n := range d.Nodes {
		if pgident.NeedsQuote(n.Table) {
			// Allowed, but the table name will be quoted in DDL; nothing to do.
			_ = n.Table
		}
		if err := validateKey(n.TypeName, "type", n.Fields); err != nil {
			return err
		}
		for _, f := range n.Fields {
			if err := d.validateRelField(n.TypeName, f); err != nil {
				return err
			}
			if err := validateMappingDirectives(n.TypeName, f); err != nil {
				return err
			}
			if f.Rel != nil && f.Rel.HasInverse != "" {
				if err := validateInverse(d, n, f); err != nil {
					return err
				}
			}
		}
		if err := validateColumnNames(n); err != nil {
			return err
		}
	}

	for _, iface := range d.Interfaces {
		if err := validateKey(iface.TypeName, "interface", iface.Fields); err != nil {
			return err
		}
		for _, f := range iface.Fields {
			if err := d.validateRelField(iface.TypeName, f); err != nil {
				return err
			}
			if err := validateMappingDirectives(iface.TypeName, f); err != nil {
				return err
			}
			if err := validateImplementors(iface, f); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateRelField checks a @relationship field's shape: a non-empty edge type,
// a list type, and a @node target. Relationships targeting an interface would
// need one edge table per implementor joined by a comma-separated pattern,
// which is M5's multi-pattern workaround — so they are rejected here rather
// than silently mis-generated (SPEC.md §10).
func (d *Document) validateRelField(owner string, f *Field) error {
	if f.Rel == nil {
		return nil
	}
	if f.Rel.Type == "" {
		return fmt.Errorf("sdl: %s.%s: @relationship requires a non-empty type", owner, f.Name)
	}
	if !f.List {
		return fmt.Errorf("sdl: %s.%s: @relationship fields must be list types (e.g. [%s!]!)",
			owner, f.Name, f.TypeName)
	}
	if d.NodeByType(f.TypeName) != nil {
		return nil
	}
	if d.InterfaceByType(f.TypeName) != nil {
		return fmt.Errorf("sdl: %s.%s: relationship targets the interface %q; an interface-typed relationship "+
			"needs one edge table per implementor, which arrives in M5", owner, f.Name, f.TypeName)
	}
	return fmt.Errorf("sdl: %s.%s: relationship targets %q, which is not a @node type",
		owner, f.Name, f.TypeName)
}

// validateImplementors checks that every implementor maps an interface field the
// same way the interface does. GraphQL already guarantees each implementor
// *declares* the field with a compatible type, but directives are not inherited:
// a mismatched @relationship on an implementor would make one label expression
// stand for two different traversals.
func validateImplementors(iface *Interface, f *Field) error {
	for _, impl := range iface.Implementors {
		var got *Field
		for _, cand := range impl.Fields {
			if cand.Name == f.Name {
				got = cand
				break
			}
		}
		if got == nil {
			// GraphQL validation already rejects this; guard defensively so a
			// nil field can never reach the generator.
			return fmt.Errorf("sdl: %s implements %s but declares no field %q",
				impl.TypeName, iface.TypeName, f.Name)
		}
		switch {
		case f.Rel == nil && got.Rel == nil:
			// Both plain columns; GraphQL has already aligned their types.
		case f.Rel == nil || got.Rel == nil:
			return fmt.Errorf("sdl: %s.%s and %s.%s disagree: one is a @relationship and the other is a column",
				iface.TypeName, f.Name, impl.TypeName, f.Name)
		case f.Rel.Type != got.Rel.Type:
			return fmt.Errorf("sdl: %s.%s and %s.%s must share the same @relationship(type:) (%q vs %q)",
				iface.TypeName, f.Name, impl.TypeName, f.Name, f.Rel.Type, got.Rel.Type)
		case f.Rel.Direction != got.Rel.Direction:
			return fmt.Errorf("sdl: %s.%s and %s.%s must share the same @relationship(direction:) (%s vs %s)",
				iface.TypeName, f.Name, impl.TypeName, f.Name, f.Rel.Direction, got.Rel.Direction)
		}
		if f.Ignore != got.Ignore {
			return fmt.Errorf("sdl: %s.%s and %s.%s disagree about @ignore",
				iface.TypeName, f.Name, impl.TypeName, f.Name)
		}
	}
	return nil
}

// validateKey requires the surrogate key `id: ID!`. Interfaces need it too: the
// compiler projects it as every level's hidden key column and compares it
// between vertex positions to exclude self-matches.
// validateMappingDirectives checks the M6 column directives sit on fields that
// actually map to a column, and that they do not contradict the surrogate key.
// A directive on a relationship or an @ignore field is a mistake with no
// possible effect, so it is rejected rather than ignored (SPEC.md §10).
func validateMappingDirectives(typeName string, f *Field) error {
	has := f.Column != "" || f.ColumnType != "" || f.Unique || f.Index != nil
	if !has {
		return nil
	}
	if !f.IsScalarColumn() {
		what := "a relationship"
		if f.Ignore {
			what = "@ignore"
		}
		return fmt.Errorf("sdl: %s.%s carries @column/@index/@unique, but it is %s and maps to no column",
			typeName, f.Name, what)
	}
	if f.Name == "id" {
		if f.ColumnType != "" {
			return fmt.Errorf("sdl: %s.id cannot override its type: the surrogate key is a uuid primary key", typeName)
		}
		if f.Unique {
			return fmt.Errorf("sdl: %s.id is already unique as the primary key; drop the @unique", typeName)
		}
		if f.Index != nil {
			return fmt.Errorf("sdl: %s.id is already indexed by its primary key; drop the @index", typeName)
		}
	}
	if f.Column != "" && pgident.NeedsQuote(f.Column) {
		// Allowed — the column name is quoted wherever it is emitted.
		_ = f.Column
	}
	return nil
}

// validateColumnNames rejects two fields of one type mapping to the same
// physical column, which @column(name:) makes possible.
func validateColumnNames(n *Node) error {
	seen := map[string]string{}
	for _, f := range n.Fields {
		if !f.IsScalarColumn() {
			continue
		}
		col := f.ColumnName()
		if prev, dup := seen[col]; dup {
			return fmt.Errorf("sdl: %s.%s and %s.%s both map to column %q; disambiguate with @column(name:)",
				n.TypeName, prev, n.TypeName, f.Name, col)
		}
		seen[col] = f.Name
	}
	return nil
}

func validateKey(typeName, kind string, fields []*Field) error {
	for _, f := range fields {
		if f.Name != "id" {
			continue
		}
		if f.TypeName == "ID" && f.NonNull && !f.List {
			return nil
		}
		return fmt.Errorf("sdl: %s.id must be `ID!` (surrogate uuid keys only)", typeName)
	}
	return fmt.Errorf("sdl: %s %s must declare a surrogate key field `id: ID!`", kind, typeName)
}

// validateInverse checks that a @hasInverse field points at a real field on the
// related type whose relationship shares the same edge type and opposite
// direction — the pairing that folds two GraphQL fields onto one edge table
// (SPEC.md §5.2).
func validateInverse(d *Document, n *Node, f *Field) error {
	target := d.NodeByType(f.TypeName)
	var inv *Field
	for _, cand := range target.Fields {
		if cand.Name == f.Rel.HasInverse {
			inv = cand
			break
		}
	}
	if inv == nil {
		return fmt.Errorf("sdl: %s.%s: @hasInverse(field: %q) has no matching field on %s",
			n.TypeName, f.Name, f.Rel.HasInverse, target.TypeName)
	}
	if inv.Rel == nil || inv.Rel.Type != f.Rel.Type {
		return fmt.Errorf("sdl: %s.%s and %s.%s must share the same @relationship(type:) to pair via @hasInverse",
			n.TypeName, f.Name, target.TypeName, inv.Name)
	}
	if inv.Rel.Direction == f.Rel.Direction {
		return fmt.Errorf("sdl: %s.%s and %s.%s are paired by @hasInverse but share direction %s; they must be opposite",
			n.TypeName, f.Name, target.TypeName, inv.Name, f.Rel.Direction)
	}
	return nil
}

// argString returns the raw value of a directive argument, or "".
func argString(dir *ast.Directive, name string) string {
	if a := dir.Arguments.ForName(name); a != nil && a.Value != nil {
		return a.Value.Raw
	}
	return ""
}

// namedType strips list and non-null wrappers to the underlying named type.
func namedType(t *ast.Type) string {
	for t != nil {
		if t.NamedType != "" {
			return t.NamedType
		}
		t = t.Elem
	}
	return ""
}

// pluralize derives a table name from a label ("person" -> "persons"). It is a
// naive English pluralizer; @node(table:) overrides it (SPEC.md §9 open
// decision 3).
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
