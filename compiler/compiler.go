// Package compiler turns a GraphQL operation into a SQL/PGQ GRAPH_TABLE query
// plus ordered bind parameters. Compilation is pure — it never contacts a
// database (SPEC.md §6.1) — so it compiles to WASM.
//
// M1 handles a single root field with no nesting: it resolves the root field to
// a @node type, projects the requested scalar fields, and emits one GRAPH_TABLE
// with a single vertex pattern and an enumerated COLUMNS list (SPEC.md §7 → M1).
// Nested selections, field arguments and query variables are rejected with a
// clear error rather than silently dropped (SPEC.md §10: no silent fallbacks);
// they arrive from M3 onward.
package compiler

import (
	"fmt"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/internal/pgident"
	"github.com/lega4e/gopgql/sdl"
)

// Compiler compiles GraphQL operations against a fixed SDL document.
type Compiler struct {
	doc       *sdl.Document
	graphName string
}

// Option configures a Compiler.
type Option func(*Compiler)

// WithGraphName sets the property-graph name the compiled queries target. It
// must match the name the generator used (DefaultGraphName by default).
func WithGraphName(name string) Option {
	return func(c *Compiler) { c.graphName = name }
}

// New returns a Compiler for the given SDL document.
func New(doc *sdl.Document, opts ...Option) *Compiler {
	c := &Compiler{doc: doc, graphName: generator.DefaultGraphName}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ProjectedField is one column of the response: the GraphQL response key and
// the graph property it reads.
type ProjectedField struct {
	// ResponseKey is the GraphQL alias (or field name) — the key in the JSON
	// response and the output column name.
	ResponseKey string
	// Property is the vertex property / column name read from the graph.
	Property string
}

// Projection describes the shape of a compiled query's result: the root field
// under which rows nest, and the projected fields in selection order.
type Projection struct {
	// ResponseKey is the root field's response key (alias or name).
	ResponseKey string
	// Fields are the projected scalar fields in selection order.
	Fields []ProjectedField
}

// Compiled is the result of compiling an operation.
type Compiled struct {
	// SQL is the GRAPH_TABLE query.
	SQL string
	// Args are the ordered bind parameters (empty in M1).
	Args []any
	// Projection describes how to shape the returned rows into the GraphQL
	// response.
	Projection Projection
}

// Compile matches the SPEC.md §6.1 contract: it returns the SQL and ordered
// bind parameters for an operation.
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
	if len(operation.VariableDefinitions) > 0 || len(vars) > 0 {
		return nil, fmt.Errorf("compiler: query variables are not supported until M3")
	}

	roots := fieldSelections(operation.SelectionSet)
	if len(roots) != 1 {
		return nil, fmt.Errorf("compiler: exactly one root field is supported in M1, got %d", len(roots))
	}
	root := roots[0]

	node := c.doc.NodeByTable(root.Name)
	if node == nil {
		return nil, fmt.Errorf("compiler: unknown root field %q; no @node maps to table %q", root.Name, root.Name)
	}
	if len(root.Arguments) > 0 {
		return nil, fmt.Errorf("compiler: field arguments are not supported until M3")
	}

	proj, err := c.projection(node, root)
	if err != nil {
		return nil, err
	}

	sql := c.render(node, proj)
	return &Compiled{SQL: sql, Args: nil, Projection: proj}, nil
}

// projection resolves the root field's selection set into an ordered list of
// scalar properties, validating each against the node's allowlist.
func (c *Compiler) projection(node *sdl.Node, root *ast.Field) (Projection, error) {
	proj := Projection{ResponseKey: responseKey(root)}
	if len(root.SelectionSet) == 0 {
		return proj, fmt.Errorf("compiler: root field %q must select at least one field", root.Name)
	}
	for _, sel := range root.SelectionSet {
		f, ok := sel.(*ast.Field)
		if !ok {
			return proj, fmt.Errorf("compiler: fragments are not supported until a later milestone")
		}
		if len(f.SelectionSet) > 0 {
			return proj, fmt.Errorf("compiler: nested selection %q.%q requires traversal, which arrives in M3",
				node.TypeName, f.Name)
		}
		if len(f.Arguments) > 0 {
			return proj, fmt.Errorf("compiler: field arguments are not supported until M3")
		}
		field := findField(node, f.Name)
		if field == nil {
			return proj, fmt.Errorf("compiler: %s has no field %q", node.TypeName, f.Name)
		}
		if !field.IsScalarColumn() {
			return proj, fmt.Errorf("compiler: %s.%s is not a scalar property (relationships are not supported in M1)",
				node.TypeName, f.Name)
		}
		proj.Fields = append(proj.Fields, ProjectedField{ResponseKey: responseKey(f), Property: field.Name})
	}
	return proj, nil
}

// render emits the GRAPH_TABLE SQL for a single vertex projection.
func (c *Compiler) render(node *sdl.Node, proj Projection) string {
	const alias = "v"
	cols := make([]string, len(proj.Fields))
	outs := make([]string, len(proj.Fields))
	for i, f := range proj.Fields {
		out := pgident.Quote(f.ResponseKey)
		cols[i] = fmt.Sprintf("%s.%s AS %s", alias, pgident.Quote(f.Property), out)
		outs[i] = out
	}
	return fmt.Sprintf(
		"SELECT %s\nFROM GRAPH_TABLE (%s\n  MATCH (%s IS %s)\n  COLUMNS (%s)\n)",
		strings.Join(outs, ", "),
		pgident.Quote(c.graphName),
		alias,
		pgident.Quote(node.Label),
		strings.Join(cols, ", "),
	)
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

func findField(node *sdl.Node, name string) *sdl.Field {
	for _, f := range node.Fields {
		if f.Name == name {
			return f
		}
	}
	return nil
}
