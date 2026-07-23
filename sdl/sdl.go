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
}

// IsScalarColumn reports whether the field maps to a physical column: it is not
// ignored and is not a relationship.
func (f *Field) IsScalarColumn() bool {
	return !f.Ignore && f.Rel == nil
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

// Document is the typed mapping model built from an SDL source: the set of
// @node types with their resolved labels, table names and relationships.
type Document struct {
	// Nodes are the @node types in a stable (type-name) order.
	Nodes []*Node

	byType  map[string]*Node
	byTable map[string]*Node
	// Raw is the validated gqlparser schema, retained for callers that need
	// the underlying AST (SPEC.md §4: "*ast.Schema + directive model").
	Raw *ast.Schema
}

// NodeByType returns the node for a GraphQL type name, or nil.
func (d *Document) NodeByType(name string) *Node { return d.byType[name] }

// NodeByTable returns the node whose physical table has the given name, or nil.
// Query root fields resolve to nodes by table name (SPEC.md §7 → M1).
func (d *Document) NodeByTable(name string) *Node { return d.byTable[name] }

// prelude declares the gopgql directive vocabulary and custom scalars so
// gqlparser recognises and validates them. It is the M1 subset of SPEC.md §5.
const prelude = `directive @node(label: String!, table: String) on OBJECT
directive @relationship(type: String!, direction: RelDirection, table: String) on FIELD_DEFINITION
directive @hasInverse(field: String!) on FIELD_DEFINITION
directive @ignore on FIELD_DEFINITION
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
		Raw:     schema,
	}

	for _, def := range schema.Types {
		if def.Kind != ast.Object || def.BuiltIn {
			continue
		}
		nodeDir := def.Directives.ForName("node")
		if nodeDir == nil {
			continue
		}
		doc.Nodes = append(doc.Nodes, buildNode(def, nodeDir))
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

	if err := doc.validate(); err != nil {
		return nil, err
	}
	return doc, nil
}

func buildNode(def *ast.Definition, nodeDir *ast.Directive) *Node {
	// label is required by the directive definition, so gqlparser guarantees a
	// non-empty value here.
	label := argString(nodeDir, "label")
	table := argString(nodeDir, "table")
	if table == "" {
		table = pluralize(label)
	}
	n := &Node{TypeName: def.Name, Label: label, Table: table}

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
		n.Fields = append(n.Fields, f)
	}
	return n
}

// validate enforces gopgql's M1 semantic rules beyond GraphQL well-formedness.
func (d *Document) validate() error {
	for _, n := range d.Nodes {
		if pgident.NeedsQuote(n.Table) {
			// Allowed, but the table name will be quoted in DDL; nothing to do.
			_ = n.Table
		}
		if err := validateKey(n); err != nil {
			return err
		}
		for _, f := range n.Fields {
			if f.Rel == nil {
				continue
			}
			if f.Rel.Type == "" {
				return fmt.Errorf("sdl: %s.%s: @relationship requires a non-empty type", n.TypeName, f.Name)
			}
			if !f.List {
				return fmt.Errorf("sdl: %s.%s: @relationship fields must be list types (e.g. [%s!]!)",
					n.TypeName, f.Name, f.TypeName)
			}
			if d.NodeByType(f.TypeName) == nil {
				return fmt.Errorf("sdl: %s.%s: relationship targets %q, which is not a @node type",
					n.TypeName, f.Name, f.TypeName)
			}
			if f.Rel.HasInverse != "" {
				if err := validateInverse(d, n, f); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validateKey requires the M1 surrogate key: `id: ID!`.
func validateKey(n *Node) error {
	for _, f := range n.Fields {
		if f.Name != "id" {
			continue
		}
		if f.TypeName == "ID" && f.NonNull && !f.List {
			return nil
		}
		return fmt.Errorf("sdl: %s.id must be `ID!` (M1 supports surrogate uuid keys only)", n.TypeName)
	}
	return fmt.Errorf("sdl: type %s (@node) must declare a surrogate key field `id: ID!`", n.TypeName)
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
