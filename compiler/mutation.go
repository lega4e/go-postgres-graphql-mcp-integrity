package compiler

import (
	"fmt"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	"github.com/lega4e/gopgql/internal/pgident"
	"github.com/lega4e/gopgql/sdl"
)

// A mutation operation compiles to a plain function call — `SELECT
// dbos.enqueue_workflow(agent_digest => $1)` — and to nothing else. There is no
// graph name in it, no MATCH and no GRAPH_TABLE, which is why SPEC.md §2.2's
// "no mutation through a property graph" survives this whole feature untouched:
// the two never meet. What is compiled here is an ordinary call to an ordinary
// function over ordinary tables, named by the SDL and owned by the database.

// CompiledCall is the result of compiling a mutation operation: the statement,
// its ordered bind parameters, and how its result is to be read.
//
// It is deliberately not a *Compiled. A call has no projection and no shaping —
// there is no pattern, no level, and no fan-out to regroup — so sharing the type
// would buy one field and cost every consumer an `if` on which half is real.
type CompiledCall struct {
	// SQL is the function call.
	SQL string
	// Args are the ordered bind parameters ($1, $2, … in emission order).
	Args []any
	// Returns is the declared return shape, which decides how the statement is
	// run and how its result is read (see exec.Call).
	Returns sdl.FunctionReturn
	// ReturnTypeName and ReturnNonNull are the mutation field's GraphQL result
	// type, carried for a consumer that has to name a static Go type for it.
	// Nothing in the execution path reads them: exec.Call is told the shape by
	// Returns and hands the value back as it came.
	ReturnTypeName string
	ReturnNonNull  bool
	// ResponseKey is the mutation field's alias (or name) — the key the caller
	// puts the result under in a GraphQL response. gopgql builds no response
	// envelope of its own (SPEC.md §1.1); this is what a consumer needs to.
	ResponseKey string
	// Schema and Function name the function called, so an error can say what
	// failed without the caller re-deriving it from the SQL.
	Schema   string
	Function string
}

// CompileMutation compiles a mutation operation into a function call and its
// ordered bind parameters. Like [Compiler.CompileQuery] it is pure: it contacts
// no database, and the declaration in the SDL is the only thing it reads.
//
// Arguments map to the function's parameters **by name**, using PostgreSQL's
// named notation, so the order they are written in the operation has no effect
// on the call.
func (c *Compiler) CompileMutation(op string, vars map[string]any) (*CompiledCall, error) {
	doc, gqlErr := parser.ParseQuery(&ast.Source{Name: "operation", Input: op})
	if gqlErr != nil {
		return nil, fmt.Errorf("compiler: parse operation: %w", gqlErr)
	}
	if len(doc.Operations) != 1 {
		return nil, fmt.Errorf("compiler: exactly one operation is supported, got %d", len(doc.Operations))
	}
	operation := doc.Operations[0]
	if operation.Operation != ast.Mutation {
		return nil, fmt.Errorf("compiler: CompileMutation compiles mutation operations; "+
			"got a %s — compile it with CompileQuery", operation.Operation)
	}

	roots := fieldSelections(operation.SelectionSet)
	if len(roots) != 1 {
		return nil, fmt.Errorf("compiler: exactly one root field is supported, got %d", len(roots))
	}
	root := roots[0]

	m := c.doc.MutationByField(root.Name)
	if m == nil {
		callable := c.doc.MutationFields()
		if len(callable) == 0 {
			return nil, fmt.Errorf("compiler: unknown mutation field %q; this schema declares no Mutation type",
				root.Name)
		}
		return nil, fmt.Errorf("compiler: unknown mutation field %q; callable mutation fields are %s",
			root.Name, strings.Join(callable, ", "))
	}
	if len(root.SelectionSet) > 0 {
		return nil, fmt.Errorf("compiler: %s returns a scalar and cannot have a subselection", root.Name)
	}

	varDefs := map[string]*ast.VariableDefinition{}
	for _, vd := range operation.VariableDefinitions {
		varDefs[vd.Variable] = vd
	}
	// A call has no projection and nothing to shape, so the shaping strategy is
	// irrelevant here: the builder is borrowed only for its variable resolution
	// and its $-ordered bind list, which is what keeps a mutation's argument
	// rules identical to a query's. GoSide is the neutral value, not a choice.
	b := newBuilder(c.doc, vars, varDefs, c.maxDepth, GoSide)

	args, err := b.callArgs(m, root)
	if err != nil {
		return nil, err
	}

	return &CompiledCall{
		SQL:            fmt.Sprintf("SELECT %s(%s)", m.QualifiedName(), strings.Join(args, ", ")),
		Args:           b.args,
		Returns:        m.Returns,
		ReturnTypeName: m.TypeName,
		ReturnNonNull:  m.NonNull,
		ResponseKey:    responseKey(root),
		Schema:         m.Schema,
		Function:       m.Function,
	}, nil
}

// callArgs renders the named-notation argument list, in the SDL's declaration
// order, and binds each value as a parameter.
//
// Which arguments appear is decided by the **operation document**, not by the
// request: an argument the document does not pass and that declares no GraphQL
// default is left out of the call entirely, so the function's own SQL DEFAULT
// applies. gopgql never emits the DEFAULT keyword and never invents a value.
//
// That the document decides is forced rather than chosen. A generated client
// bakes each operation's SQL as a const, so an argument list that varied per
// request would need one statement per subset of the arguments — or compilation
// at run time, which is the thing generating a client exists to avoid.
//
// The trap this leaves is worth naming, because no compiler can catch it:
// **NULL is not DEFAULT**. Passing an argument explicitly as null, or as an
// unset nullable variable, sends NULL — the parameter is present and its value
// is NULL, and a function whose declared default is anything else never sees
// that default. Omitting the argument from the document is the only way to
// reach it.
func (b *builder) callArgs(m *sdl.Mutation, field *ast.Field) ([]string, error) {
	for _, arg := range field.Arguments {
		if m.ArgByName(arg.Name) == nil {
			return nil, fmt.Errorf("compiler: %s has no argument %q", m.Name, arg.Name)
		}
	}

	out := make([]string, 0, len(m.Args))
	for _, decl := range m.Args {
		var value *ast.Value
		if passed := field.Arguments.ForName(decl.Name); passed != nil {
			value = passed.Value
		} else if decl.Default != nil {
			// A GraphQL default declared in the SDL is applied here, explicitly.
			// Nothing else applies it: CompileQuery/CompileMutation parse the
			// operation and never run gqlparser's validator, so a default left
			// to it would silently reach nothing at all.
			value = decl.Default
		} else {
			// Absent from the document and with no GraphQL default: the
			// parameter is not mentioned, so the function's own DEFAULT applies.
			continue
		}
		if decl.NonNull && value.Kind == ast.NullValue {
			return nil, fmt.Errorf("compiler: %s(%s:) is non-null and cannot be passed as null", m.Name, decl.Name)
		}
		v, err := b.value(value)
		if err != nil {
			return nil, err
		}
		b.args = append(b.args, v)
		out = append(out, fmt.Sprintf("%s => $%d", pgident.Quote(decl.Param), len(b.args)))
	}
	return out, nil
}
