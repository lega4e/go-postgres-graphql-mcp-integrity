package sdl

import (
	"fmt"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/lega4e/gopgql/internal/pgident"
)

// The @function directive maps a GraphQL mutation field to a PL/pgSQL function
// the database already owns (SPEC.md §7 → M11).
//
// It is the *whole* of gopgql's write surface, and the boundary is worth stating
// because "mutation directive" invites the wider reading. gopgql derives no
// writes from a @node type: there is no generated createPerson, no inferred
// input type, no update or delete. A mutation exists exactly where the SDL
// declares one and names a function somebody else wrote — the same footing as
// @default and @check, which SPEC.md §5 already calls a deliberate escape hatch
// defensible only because whoever writes the SDL owns the schema.
//
// Nothing here contacts a database, so the declaration has to carry everything
// the call needs: which function, and what shape it returns. Sniffing either at
// compile time would cost the package its WASM build and its purity.

// FunctionReturn is the return shape a @function declares.
type FunctionReturn string

const (
	// ReturnScalar is a function returning one value: read as a single row of a
	// single column and mapped back through the §5.1 scalar table.
	ReturnScalar FunctionReturn = "SCALAR"
	// ReturnVoid is a `RETURNS void` function: executed as a statement, with
	// success itself as the result.
	//
	// It is declared rather than inferred because `Boolean!` cannot tell a
	// function that returned `false` from one that returned nothing at all, and
	// with no database at compile time there is nowhere else the answer could
	// come from. Left to inference, a successful void call would report false.
	ReturnVoid FunctionReturn = "VOID"
)

// Argument is one argument of a mutation field: the GraphQL name the caller
// passes and the SQL parameter name it maps to.
type Argument struct {
	// Name is the GraphQL argument name.
	Name string
	// Param is the function's parameter name — @column(name:) on the argument,
	// or Name. Arguments map to parameters *by name*, never positionally, so
	// this is the whole of the mapping.
	Param string
	// TypeName is the underlying named type with list and non-null wrappers
	// stripped; NonNull and List describe those wrappers.
	TypeName string
	NonNull  bool
	List     bool
	// Default is the GraphQL default value the SDL declares for the argument, or
	// nil. It is applied by the compiler when the operation omits the argument,
	// so a declared GraphQL default always wins over the function's own SQL
	// DEFAULT — which is why an argument that should reach the SQL DEFAULT must
	// declare no GraphQL default at all.
	Default *ast.Value
}

// Mutation is one field of the Mutation type: the function it calls, the shape
// that function returns, and the arguments that become its named parameters.
type Mutation struct {
	// Name is the GraphQL field name, and the root field a mutation operation
	// selects.
	Name string
	// Schema and Function name the PL/pgSQL function (@function(schema:, name:)).
	Schema   string
	Function string
	// Returns is the declared return shape.
	Returns FunctionReturn
	// Args are the field's arguments in declaration order. Emission order
	// follows this list, not the order the operation happens to write them in,
	// so one SDL and one operation always compile to one statement.
	Args []*Argument
	// TypeName, NonNull and List describe the GraphQL result type.
	TypeName string
	NonNull  bool
	List     bool
}

// QualifiedName renders the function as it is called: schema.name, each part
// quoted only where it has to be.
func (m *Mutation) QualifiedName() string {
	return pgident.Quote(m.Schema) + "." + pgident.Quote(m.Function)
}

// ArgByName returns the declared argument with the given GraphQL name, or nil.
func (m *Mutation) ArgByName(name string) *Argument {
	for _, a := range m.Args {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// scalarReturns are the GraphQL types a SCALAR-returning @function may declare.
// They are exactly SPEC.md §5.1's scalars, because that table is what exec maps
// the returned value back through.
var scalarReturns = map[string]bool{
	"Int": true, "Float": true, "String": true, "Boolean": true,
	"ID": true, "DateTime": true, "JSON": true,
}

// buildMutations reads the Mutation type into the mapping model and rejects
// every @function that is not on one of its fields.
//
// The second half is not tidiness. gqlparser's directive definition permits
// @function on any FIELD_DEFINITION, so without this check a @function written
// on a @node type's field would parse, generate a column, and never be called —
// a declaration that quietly does nothing, which SPEC.md §10 forbids.
func (d *Document) buildMutations(schema *ast.Schema) error {
	root := schema.Mutation
	for _, def := range schema.Types {
		if def.BuiltIn || (root != nil && def.Name == root.Name) {
			continue
		}
		for _, fd := range def.Fields {
			if fd.Directives.ForName("function") == nil {
				continue
			}
			return fmt.Errorf("sdl: %s.%s carries @function, but only a field of the Mutation type maps to a "+
				"function call", def.Name, fd.Name)
		}
	}
	if root == nil {
		return nil
	}
	if root.Directives.ForName("node") != nil {
		return fmt.Errorf("sdl: %s is the mutation root and cannot also be a @node type; "+
			"a mutation calls a function, it does not map to a table", root.Name)
	}

	for _, fd := range root.Fields {
		if strings.HasPrefix(fd.Name, "__") {
			continue // introspection meta-fields
		}
		m, err := buildMutation(root.Name, fd)
		if err != nil {
			return err
		}
		if _, dup := d.byMutation[m.Name]; dup {
			return fmt.Errorf("sdl: %s declares the field %q twice", root.Name, m.Name)
		}
		d.Mutations = append(d.Mutations, m)
		d.byMutation[m.Name] = m
	}
	return nil
}

func buildMutation(rootName string, fd *ast.FieldDefinition) (*Mutation, error) {
	where := rootName + "." + fd.Name
	dir := fd.Directives.ForName("function")
	if dir == nil {
		// No default target and no inference: gopgql cannot guess which function
		// a field means, and a mutation it silently declined to map would be a
		// field that parses and never runs.
		return nil, fmt.Errorf("sdl: %s declares no @function; every field of the mutation root must name the "+
			"function it calls", where)
	}
	schemaName, err := requiredArg(dir, "schema", where)
	if err != nil {
		return nil, err
	}
	fnName, err := requiredArg(dir, "name", where)
	if err != nil {
		return nil, err
	}

	m := &Mutation{
		Name:     fd.Name,
		Schema:   schemaName,
		Function: fnName,
		Returns:  ReturnScalar,
		TypeName: namedType(fd.Type),
		NonNull:  fd.Type.NonNull,
		List:     fd.Type.Elem != nil,
	}
	// The directive declares SCALAR as its own default, so an absent argument
	// still arrives as "SCALAR"; reading it rather than assuming keeps the two
	// spellings from drifting.
	if r := argString(dir, "returns"); r != "" {
		m.Returns = FunctionReturn(r)
	}

	if err := validateReturn(where, m); err != nil {
		return nil, err
	}
	for _, ad := range fd.Arguments {
		a, err := buildArgument(where, ad)
		if err != nil {
			return nil, err
		}
		if m.ArgByName(a.Name) != nil {
			return nil, fmt.Errorf("sdl: %s declares the argument %q twice", where, a.Name)
		}
		m.Args = append(m.Args, a)
	}
	return m, nil
}

// validateReturn checks that the declared return shape and the GraphQL result
// type agree, and refuses the function shapes gopgql cannot call.
//
// A set-returning, output-parameter, variadic or polymorphic function all reach
// gopgql the same way — as a result that is not one value — and none of them can
// be read as one. They are refused here, by the shape of the *declaration*,
// because there is no database at compile time to ask about the function itself:
// a list or an object result type is the only evidence available, and a call
// that failed at run time would name a SQLSTATE rather than the declaration that
// caused it.
func validateReturn(where string, m *Mutation) error {
	switch m.Returns {
	case ReturnVoid:
		if m.TypeName != "Boolean" || !m.NonNull || m.List {
			return fmt.Errorf("sdl: %s declares @function(returns: VOID) but returns %s; a void function has no "+
				"value to map, so the field must be `Boolean!` and reports whether the call succeeded", where,
				renderType(m))
		}
		return nil
	case ReturnScalar:
		if m.List {
			return fmt.Errorf("sdl: %s returns %s; a set-returning function is not supported — gopgql reads a "+
				"scalar function as one row of one column", where, renderType(m))
		}
		if !scalarReturns[m.TypeName] {
			return fmt.Errorf("sdl: %s returns %s, which is not one of the scalars gopgql maps (SPEC.md §5.1); "+
				"a composite, output-parameter or polymorphic return is not supported", where, renderType(m))
		}
		return nil
	default:
		return fmt.Errorf("sdl: %s: unknown @function(returns: %s)", where, m.Returns)
	}
}

func buildArgument(where string, ad *ast.ArgumentDefinition) (*Argument, error) {
	a := &Argument{
		Name:     ad.Name,
		Param:    ad.Name,
		TypeName: namedType(ad.Type),
		NonNull:  ad.Type.NonNull,
		List:     ad.Type.Elem != nil,
		Default:  ad.DefaultValue,
	}
	if colDir := ad.Directives.ForName("column"); colDir != nil {
		if name := argString(colDir, "name"); name != "" {
			a.Param = name
		}
		if typ := argString(colDir, "type"); typ != "" {
			return nil, fmt.Errorf("sdl: %s(%s:): @column(type:) has no meaning on an argument; a function's "+
				"parameter types are the function's own", where, a.Name)
		}
	}

	// A camelCase GraphQL argument name is the ordinary GraphQL convention and
	// the ordinary PostgreSQL parameter is lower_snake_case, so the two disagree
	// by default. Named notation quotes the parameter, and `"agentDigest" => $1`
	// does not match a parameter named agent_digest: the call fails at run time
	// with 42883 and nothing in that error says a naming convention caused it.
	// Refusing it here costs one @column(name:) and names the argument.
	if pgident.NeedsQuote(a.Param) {
		return nil, fmt.Errorf("sdl: %s(%s:) maps to the parameter %q, which is not a plain lower-case "+
			"identifier; PostgreSQL parameter names are, so give the argument a @column(name:) naming the "+
			"parameter the function declares", where, a.Name, a.Param)
	}
	return a, nil
}

// renderType renders a mutation's GraphQL result type back for an error message.
func renderType(m *Mutation) string {
	out := m.TypeName
	if m.List {
		out = "[" + out + "]"
	}
	if m.NonNull {
		out += "!"
	}
	return out
}
