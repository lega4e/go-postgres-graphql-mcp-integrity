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
// M7 adds the constraint directives (SPEC.md §7 → M7): @default and @check
// carry raw SQL through to the DDL, @key declares a natural key *alongside* the
// surrogate id rather than replacing it, and @renamedFrom is a hint the differ
// needs because a rename can never be inferred from a disappeared name and an
// appeared one. This package parses and validates them; emitting and diffing
// them belong to generator and migrate.
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
	// Schema is the PostgreSQL schema qualifying Table (@relationship(schema:));
	// empty means the identifier resolves through search_path, exactly as every
	// gopgql identifier did before schema qualification existed.
	Schema string
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
	// Default is the column default from @default(value:); empty means none.
	// It is raw SQL, emitted verbatim (design D6) — `'unknown'`, `now()`, `0`.
	Default string
	// Check is the column constraint expression from @check(expr:); empty means
	// none. Also raw SQL: gopgql does not parse it, so an invalid expression is
	// PostgreSQL's error at migration time, not a parse error here.
	Check string
	// RenamedFrom is the field's previous name from @renamedFrom(name:); empty
	// means none. It is a hint to the differ, never an inference (design D2).
	RenamedFrom string
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

// PriorColumnNames derives the physical column names this field may have had
// before the rename its @renamedFrom declares, most likely first. It is nil when
// the field declares no rename.
//
// @renamedFrom(name:) states the previous *GraphQL field name* (see
// Document.validateRenameHints), and the differ needs a *column* name, so the
// two have to be bridged somewhere; sdl is where, because sdl owns the rule that
// a field with no @column(name:) maps to a column of its own name. That rule run
// backwards gives exactly one candidate: the hint itself.
//
// A field that kept its column while its GraphQL name changed therefore yields a
// candidate that is still present in the desired schema, and the differ declines
// to rename anything — correctly, because nothing physical moved.
func (f *Field) PriorColumnNames() []string {
	if f.RenamedFrom == "" || !f.IsScalarColumn() {
		return nil
	}
	return []string{f.RenamedFrom}
}

// PriorTableNames derives the physical table names this type may have had before
// the rename its @renamedFrom declares, most likely first. It is nil when the
// type declares no rename.
//
// The GraphQL → physical bridge is looser here than for a field, because a
// type's table is pluralize(@node(label:)) and the label is independent of the
// type name. Two candidates cover what an author can reasonably have meant:
//
//   - the table a type of that name conventionally produced — the pluralized,
//     lowercased prior type name;
//   - the hint read literally, for a schema whose @node(table:) was explicit and
//     whose author wrote the physical name.
//
// Offering both is safe precisely because a rename is resolved against the prior
// state (design D2): a candidate that names nothing that is actually there emits
// nothing, so a wrong guess costs a no-op rather than a wrong rename.
func (n *Node) PriorTableNames() []string {
	if n.RenamedFrom == "" {
		return nil
	}
	out := []string{pluralize(strings.ToLower(n.RenamedFrom))}
	if n.RenamedFrom != out[0] {
		out = append(out, n.RenamedFrom)
	}
	return out
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
	// Schema is the PostgreSQL schema qualifying Table (@node(schema:)); empty
	// means the identifier resolves through search_path, which is what every
	// gopgql identifier did before schema qualification existed and is still
	// what an SDL that declares none gets.
	Schema string
	// ReadOnly is true when the type carries @readonly: gopgql surfaces the
	// table in the property graph and the read model, and emits no DDL and no
	// migration for it, ever. Somebody else creates and migrates that table
	// (SPEC.md §7 → M12).
	//
	// It constrains *DDL emission*, not query access — a @readonly type is as
	// queryable as any other. The word is the consumer's; it reads as though it
	// meant the other thing.
	ReadOnly bool
	// Fields are the type's fields in declaration order.
	Fields []*Field
	// Checks are the table-level constraint expressions from @check(expr:) on
	// the type, in declaration order — the form that spans more than one
	// column. Raw SQL, like Field.Check.
	Checks []string
	// NaturalKey names the fields of a @key(fields:), in the declared order: a
	// uniqueness constraint over existing scalar properties, *alongside* the
	// surrogate id, which stays the physical identity edges reference
	// (design D1). Nil means the type declares no natural key.
	NaturalKey []string
	// RenamedFrom is the type's previous name from @renamedFrom(name:); empty
	// means none.
	RenamedFrom string
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
	// Mutations are the Mutation type's fields, in declaration order — one per
	// PL/pgSQL function the SDL names with @function (SPEC.md §7 → M11). Nil
	// when the SDL declares no Mutation type, which is every schema written
	// before M11 and every schema that only reads.
	Mutations []*Mutation

	byType     map[string]*Node
	byTable    map[string]*Node
	byIface    map[string]*Interface
	byMutation map[string]*Mutation
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

// MutationByField returns the mutation for a Mutation root-field name, or nil.
func (d *Document) MutationByField(name string) *Mutation { return d.byMutation[name] }

// MutationFields lists every mutation root-field name, sorted. Like
// [Document.RootFields] it exists so a compile error can say what *is* callable.
func (d *Document) MutationFields() []string {
	out := make([]string, 0, len(d.byMutation))
	for name := range d.byMutation {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

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
// gqlparser recognises and validates them. It is SPEC.md §5 through M7.
const prelude = `directive @node(label: String!, table: String, schema: String) on OBJECT | INTERFACE
directive @relationship(type: String!, direction: RelDirection, table: String, schema: String) on FIELD_DEFINITION
directive @hasInverse(field: String!) on FIELD_DEFINITION
directive @ignore on FIELD_DEFINITION
directive @column(name: String, type: String) on FIELD_DEFINITION | ARGUMENT_DEFINITION
directive @index(name: String, using: String) on FIELD_DEFINITION | OBJECT
directive @unique on FIELD_DEFINITION
directive @default(value: String!) on FIELD_DEFINITION
directive @check(expr: String!) on FIELD_DEFINITION | OBJECT
directive @key(fields: [String!]!) on OBJECT
directive @renamedFrom(name: String!) on OBJECT | FIELD_DEFINITION
directive @readonly on OBJECT
directive @function(schema: String!, name: String!, returns: FunctionReturn = SCALAR) on FIELD_DEFINITION
enum RelDirection { OUT IN }
enum FunctionReturn { SCALAR VOID }
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
		byType:     map[string]*Node{},
		byTable:    map[string]*Node{},
		byIface:    map[string]*Interface{},
		byMutation: map[string]*Mutation{},
		targets:    map[string]*Target{},
		roots:      map[string]*Target{},
		Raw:        schema,
	}

	var ifaceDefs []*ast.Definition
	for _, def := range schema.Types {
		if def.BuiltIn {
			continue
		}
		switch def.Kind {
		case ast.Object:
			if nodeDir := def.Directives.ForName("node"); nodeDir != nil {
				n, err := buildNode(def, nodeDir)
				if err != nil {
					return nil, err
				}
				doc.Nodes = append(doc.Nodes, n)
			}
		case ast.Interface:
			ifaceDefs = append(ifaceDefs, def)
		}
	}

	if err := doc.buildMutations(schema); err != nil {
		return nil, err
	}

	// A schema has to describe *something*. Before M11 that could only be a
	// graph, so the check named @node alone; a document that declares nothing but
	// a callable command surface is now equally complete.
	if len(doc.Nodes) == 0 && len(doc.Mutations) == 0 {
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

		fields, err := buildFields(def)
		if err != nil {
			return err
		}
		iface := &Interface{TypeName: def.Name, Fields: fields, Implementors: impls}
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

func buildNode(def *ast.Definition, nodeDir *ast.Directive) (*Node, error) {
	// label is required by the directive definition, so gqlparser guarantees a
	// non-empty value here.
	label := argString(nodeDir, "label")
	table := argString(nodeDir, "table")
	if table == "" {
		table = pluralize(label)
	}
	fields, err := buildFields(def)
	if err != nil {
		return nil, err
	}
	n := &Node{
		TypeName: def.Name, Label: label, Table: table,
		Schema:   argString(nodeDir, "schema"),
		ReadOnly: def.Directives.ForName("readonly") != nil,
		Fields:   fields,
	}

	// A type may carry several @check directives — one constraint per
	// expression, each named separately in the DDL so a later delta can drop it
	// (design D6) — but only one @key and one @renamedFrom, because the model
	// holds one of each.
	for _, dir := range def.Directives.ForNames("check") {
		expr, err := requiredArg(dir, "expr", def.Name)
		if err != nil {
			return nil, err
		}
		n.Checks = append(n.Checks, expr)
	}
	if keys := def.Directives.ForNames("key"); len(keys) > 0 {
		if err := atMostOne(keys, "@key", def.Name); err != nil {
			return nil, err
		}
		n.NaturalKey = argStringList(keys[0], "fields")
	}
	if renames := def.Directives.ForNames("renamedFrom"); len(renames) > 0 {
		if err := atMostOne(renames, "@renamedFrom", def.Name); err != nil {
			return nil, err
		}
		if n.RenamedFrom, err = requiredArg(renames[0], "name", def.Name); err != nil {
			return nil, err
		}
	}
	return n, nil
}

// buildFields reads a type's (or interface's) fields into the mapping model,
// skipping GraphQL introspection meta-fields.
func buildFields(def *ast.Definition) ([]*Field, error) {
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
		// A field holds one default, one check and one rename hint. gqlparser
		// already rejects a repeated directive at a field location, so unlike
		// the type-level directives these need no cardinality guard of their
		// own; two column checks are expressed as one `a AND b`, or moved to a
		// type-level @check.
		where := def.Name + "." + fd.Name
		for _, spec := range []struct {
			name string
			arg  string
			into *string
		}{
			{"default", "value", &f.Default},
			{"check", "expr", &f.Check},
			{"renamedFrom", "name", &f.RenamedFrom},
		} {
			dir := fd.Directives.ForName(spec.name)
			if dir == nil {
				continue
			}
			v, err := requiredArg(dir, spec.arg, where)
			if err != nil {
				return nil, err
			}
			*spec.into = v
		}
		if relDir := fd.Directives.ForName("relationship"); relDir != nil {
			rel := &Relationship{
				Type:      argString(relDir, "type"),
				Direction: Out,
				Table:     argString(relDir, "table"),
				Schema:    argString(relDir, "schema"),
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
	return fields, nil
}

// atMostOne rejects a repeated directive on a type definition. gqlparser
// enforces single use at field locations but not at definitions, so without
// this a second @key or @renamedFrom would be read and silently discarded — a
// declared constraint vanishing without a word.
func atMostOne(dirs []*ast.Directive, name, where string) error {
	if len(dirs) > 1 {
		return fmt.Errorf("sdl: %s carries %d %s directives; only one is allowed", where, len(dirs), name)
	}
	return nil
}

// requiredArg returns a directive argument the schema declares as non-null, and
// rejects an empty one. GraphQL requires the argument to be *present*; it has no
// opinion about `""`, and an empty expression, default or previous name would
// each become nonsense the generator emits verbatim.
//
// The value itself is returned unchanged: an expression and a default are raw
// SQL, so trimming them is not this package's call.
func requiredArg(dir *ast.Directive, arg, where string) (string, error) {
	v := argString(dir, arg)
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("sdl: %s: @%s(%s:) is empty", where, dir.Name, arg)
	}
	return v, nil
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
		if err := validateNaturalKey(n); err != nil {
			return err
		}
		if err := d.validateRenameHints(n); err != nil {
			return err
		}
		if err := d.validateUnmanaged(n); err != nil {
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

// validateMappingDirectives checks the column directives sit on fields that
// actually map to a column, and that they do not contradict the surrogate key.
// A directive on a relationship or an @ignore field is a mistake with no
// possible effect, so it is rejected rather than ignored (SPEC.md §10).
//
// @renamedFrom is deliberately absent from this rule: it describes the prior
// name of a declaration, not a property of a column, so it is meaningful on
// every field.
func validateMappingDirectives(typeName string, f *Field) error {
	has := f.Column != "" || f.ColumnType != "" || f.Unique || f.Index != nil ||
		f.Default != "" || f.Check != ""
	if !has {
		return nil
	}
	if !f.IsScalarColumn() {
		what := "a relationship"
		if f.Ignore {
			what = "@ignore"
		}
		return fmt.Errorf("sdl: %s.%s carries @column/@index/@unique/@default/@check, but it is %s and maps to no column",
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
		if f.Default != "" {
			return fmt.Errorf("sdl: %s.id already defaults to a generated uuid; drop the @default", typeName)
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

// validateNaturalKey checks that @key(fields:) names stored scalar columns of
// the declaring type. The natural key becomes a UNIQUE constraint over those
// columns and their names are listed in the property graph's KEY clause
// (design D1), so a name that maps to no column has nothing to constrain — a
// relationship lives on an edge table and an @ignore field is not in the
// database at all. Catching it here is the difference between a parse error
// naming the field and a PostgreSQL error naming a column nobody wrote.
func validateNaturalKey(n *Node) error {
	if n.NaturalKey == nil {
		return nil
	}
	if len(n.NaturalKey) == 0 {
		return fmt.Errorf("sdl: %s: @key(fields:) is empty; a natural key must name at least one field", n.TypeName)
	}
	seen := map[string]bool{}
	for _, name := range n.NaturalKey {
		if seen[name] {
			return fmt.Errorf("sdl: %s: @key names field %q twice", n.TypeName, name)
		}
		seen[name] = true

		var f *Field
		for _, cand := range n.Fields {
			if cand.Name == name {
				f = cand
				break
			}
		}
		if f == nil {
			return fmt.Errorf("sdl: %s: @key names field %q, which %s does not declare", n.TypeName, name, n.TypeName)
		}
		if !f.IsScalarColumn() {
			what := "a relationship"
			if f.Ignore {
				what = "@ignore"
			}
			return fmt.Errorf("sdl: %s.%s: @key names it, but it is %s and maps to no column on %s",
				n.TypeName, name, what, n.Table)
		}
	}
	return nil
}

// validateRenameHints enforces the one thing a rename hint can be wrong about
// (design D2). @renamedFrom is a claim about the *prior* state — "this used to
// be called X" — so an SDL that still declares X is describing two objects, not
// one renamed one, and the differ would be asked to rename something into a
// name that is already taken.
//
// The converse is deliberately not an error: a hint naming something absent
// from the prior state is a no-op. The hint stays in the SDL after the rename
// has been applied, and that same SDL has to keep generating cleanly — every
// later delta re-reads it. Rejecting an unmatched hint would make a schema stop
// parsing the moment its own migration landed.
//
// Both namespaces are checked, because a rename is declared in GraphQL names
// but applied to physical ones: naming a still-declared field is a
// contradiction, and so is naming a column another field still maps to.
func (d *Document) validateRenameHints(n *Node) error {
	if from := n.RenamedFrom; from != "" {
		if d.byType[from] != nil {
			return fmt.Errorf("sdl: %s: @renamedFrom(name: %q), but the SDL still declares the type %s; "+
				"that is two types, not a rename", n.TypeName, from, from)
		}
		if d.byIface[from] != nil {
			return fmt.Errorf("sdl: %s: @renamedFrom(name: %q), but the SDL still declares the interface %s; "+
				"that is two types, not a rename", n.TypeName, from, from)
		}
		if other := d.byTable[from]; other != nil && other != n {
			return fmt.Errorf("sdl: %s: @renamedFrom(name: %q), but %s still maps to table %q; "+
				"that is two tables, not a rename", n.TypeName, from, other.TypeName, from)
		}
	}

	for _, f := range n.Fields {
		from := f.RenamedFrom
		if from == "" {
			continue
		}
		for _, other := range n.Fields {
			if other.Name == from {
				return fmt.Errorf("sdl: %s.%s: @renamedFrom(name: %q), but %s still declares the field %s; "+
					"that is two fields, not a rename", n.TypeName, f.Name, from, n.TypeName, from)
			}
			// A field keeping its own column while its GraphQL name changes is
			// a rename with nothing to rename physically, so it is exempt.
			if other != f && other.IsScalarColumn() && other.ColumnName() == from {
				return fmt.Errorf("sdl: %s.%s: @renamedFrom(name: %q), but %s.%s still maps to column %q; "+
					"that is two columns, not a rename", n.TypeName, f.Name, from, n.TypeName, other.Name, from)
			}
		}
	}
	return nil
}

// validateKey requires the surrogate key `id: ID!`. Interfaces need it too: the
// compiler projects it as every level's hidden key column and compares it
// between vertex positions to exclude self-matches.
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

// argStringList returns the elements of a list-valued directive argument. A
// list value carries no Raw of its own, so the elements come from its children;
// GraphQL also coerces a bare value to a one-element list, which is why a
// non-list value is returned as one element rather than as nothing. The result
// is non-nil whenever the argument is present, so an empty list stays
// distinguishable from an absent directive.
func argStringList(dir *ast.Directive, name string) []string {
	a := dir.Arguments.ForName(name)
	if a == nil || a.Value == nil {
		return nil
	}
	if a.Value.Kind != ast.ListValue {
		return []string{a.Value.Raw}
	}
	out := make([]string, 0, len(a.Value.Children))
	for _, child := range a.Value.Children {
		out = append(out, child.Value.Raw)
	}
	return out
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
