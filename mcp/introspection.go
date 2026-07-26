package mcp

import (
	"fmt"
	"sort"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/lega4e/gopgql/sdl"
)

// introspector answers the GraphQL introspection system — `__schema`,
// `__type(name:)` and `__typename` — over a loaded SDL document.
//
// gopgql has no GraphQL server to forward an introspection query to: the
// document *is* the schema, and the compiler validates against the same mapping
// model, so the introspection result is derived from it rather than from a
// hand-written summary. What a client sees is the response the GraphQL
// specification defines, not a gopgql dialect (design D2).
//
// Two adjustments make the result describe what is actually queryable:
//
//   - The Query root type is synthesized. gopgql schemas declare no `type
//     Query`; the root fields are derived from the @node tables and the
//     interfaces (sdl.Document.RootFields), and each takes one equality filter
//     argument per scalar property.
//   - @ignore fields are dropped from mapped types. They exist in the SDL but
//     map to no column, so a query selecting one would not compile.
type introspector struct {
	doc       *sdl.Document
	sdlSource string

	// types holds every named type reported by introspection, including the
	// synthesized Query, the built-in scalars, and the introspection meta-types
	// gqlparser's prelude declares.
	types map[string]*ast.Definition
	// names is types' keys, sorted, so __schema.types is deterministic.
	names []string
}

const queryTypeName = "Query"

// newIntrospector builds the introspection view of a document. sdlSource is the
// verbatim SDL the server was started with, returned by the introspect tool's
// SDL format.
func newIntrospector(doc *sdl.Document, sdlSource string) *introspector {
	in := &introspector{
		doc:       doc,
		sdlSource: sdlSource,
		types:     map[string]*ast.Definition{},
	}

	for name, def := range doc.Raw.Types {
		in.types[name] = def
	}
	// Mapped types are re-derived so introspection reports exactly the fields a
	// query may select.
	for _, n := range doc.Nodes {
		if def := doc.Raw.Types[n.TypeName]; def != nil {
			in.types[n.TypeName] = in.queryableFields(def, n.Fields)
		}
	}
	for _, iface := range doc.Interfaces {
		if def := doc.Raw.Types[iface.TypeName]; def != nil {
			in.types[iface.TypeName] = in.queryableFields(def, iface.Fields)
		}
	}
	in.types[queryTypeName] = in.queryType()

	in.names = make([]string, 0, len(in.types))
	for name := range in.types {
		in.names = append(in.names, name)
	}
	sort.Strings(in.names)
	return in
}

// queryType synthesizes the Query root: one field per queryable root field,
// returning a non-null list of non-null objects, with an equality filter
// argument for each of the target's scalar properties.
func (in *introspector) queryType() *ast.Definition {
	q := &ast.Definition{
		Kind:        ast.Object,
		Name:        queryTypeName,
		Description: "The queryable roots of the mapped graph. Every root field returns the rows of one mapped type; arguments filter it by equality on a scalar property.",
	}
	for _, name := range in.doc.RootFields() {
		t := in.doc.RootTarget(name)
		if t == nil {
			continue
		}
		q.Fields = append(q.Fields, &ast.FieldDefinition{
			Name:        name,
			Description: fmt.Sprintf("Every %s.", t.TypeName),
			Type:        ast.NonNullListType(ast.NonNullNamedType(t.TypeName, nil), nil),
			Arguments:   filterArguments(t),
		})
	}
	return q
}

// queryableFields copies a mapped type's definition, keeping only the fields a
// query may select and attaching filter arguments to the relationships.
//
// The physical mapping — the table, the label, the column a field reads — stays
// out of the result: an agent queries fields, not columns.
func (in *introspector) queryableFields(def *ast.Definition, fields []*sdl.Field) *ast.Definition {
	byName := make(map[string]*sdl.Field, len(fields))
	for _, f := range fields {
		byName[f.Name] = f
	}

	cp := *def
	cp.Fields = nil
	for _, fd := range def.Fields {
		if strings.HasPrefix(fd.Name, "__") {
			continue
		}
		f, ok := byName[fd.Name]
		if !ok || f.Ignore {
			continue // GraphQL-only: no column, no relationship, not queryable
		}
		out := *fd
		if f.Rel != nil {
			if child := in.doc.TargetForType(f.TypeName); child != nil {
				out.Arguments = filterArguments(child)
			}
		}
		cp.Fields = append(cp.Fields, &out)
	}
	return &cp
}

// filterArguments builds the equality-filter arguments a selection of a target
// accepts: one per scalar property, nullable, of the property's own type.
func filterArguments(t *sdl.Target) ast.ArgumentDefinitionList {
	var args ast.ArgumentDefinitionList
	for _, f := range t.Fields {
		if !f.IsScalarColumn() {
			continue
		}
		args = append(args, &ast.ArgumentDefinition{
			Name:        f.Name,
			Description: fmt.Sprintf("Keep only rows whose %s equals this value.", f.Name),
			Type:        ast.NamedType(f.TypeName, nil),
		})
	}
	return args
}

// ---------------------------------------------------------------------------
// The introspection value model.
//
// Introspection is cyclic: a __Type has fields, whose type is a __Type. Values
// are therefore built lazily — an object is a set of named resolvers, and a
// resolver runs only when a selection asks for it, so the graph is walked to
// exactly the depth the query selects and no further.
// ---------------------------------------------------------------------------

// introField resolves one field of an introspection object. args carries the
// field's arguments, already coerced.
type introField func(args map[string]any) any

// introObject is one introspection object: a __Schema, __Type, __Field,
// __InputValue, __EnumValue or __Directive. typeName answers __typename.
type introObject struct {
	typeName string
	fields   map[string]introField
}

func constant(v any) introField { return func(map[string]any) any { return v } }

// schemaValue is the __Schema root object.
func (in *introspector) schemaValue() *introObject {
	return &introObject{typeName: "__Schema", fields: map[string]introField{
		"description": constant(nil),
		"types": func(map[string]any) any {
			list := make([]any, 0, len(in.names))
			for _, name := range in.names {
				list = append(list, in.typeValue(name))
			}
			return list
		},
		"queryType":        func(map[string]any) any { return in.typeValue(queryTypeName) },
		"mutationType":     constant(nil),
		"subscriptionType": constant(nil),
		"directives": func(map[string]any) any {
			names := make([]string, 0, len(in.doc.Raw.Directives))
			for name := range in.doc.Raw.Directives {
				names = append(names, name)
			}
			sort.Strings(names)
			list := make([]any, 0, len(names))
			for _, name := range names {
				list = append(list, in.directiveValue(in.doc.Raw.Directives[name]))
			}
			return list
		},
	}}
}

// typeValue is the __Type for a named type, or a typed nil when the schema does
// not declare it — which the specification renders as null rather than an error.
func (in *introspector) typeValue(name string) *introObject {
	def := in.types[name]
	if def == nil {
		return nil
	}
	return &introObject{typeName: "__Type", fields: map[string]introField{
		"kind":           constant(typeKind(def.Kind)),
		"name":           constant(def.Name),
		"description":    constant(nilIfEmpty(def.Description)),
		"specifiedByURL": constant(nil),
		"ofType":         constant(nil),
		"fields": func(args map[string]any) any {
			if def.Kind != ast.Object && def.Kind != ast.Interface {
				return nil
			}
			list := make([]any, 0, len(def.Fields))
			for _, fd := range def.Fields {
				if strings.HasPrefix(fd.Name, "__") {
					continue
				}
				if isDeprecated(fd.Directives) && !includeDeprecated(args) {
					continue
				}
				list = append(list, in.fieldValue(fd))
			}
			return list
		},
		"interfaces": func(map[string]any) any {
			if def.Kind != ast.Object && def.Kind != ast.Interface {
				return nil
			}
			list := make([]any, 0, len(def.Interfaces))
			for _, name := range def.Interfaces {
				list = append(list, in.typeValue(name))
			}
			return list
		},
		"possibleTypes": func(map[string]any) any {
			if def.Kind != ast.Interface && def.Kind != ast.Union {
				return nil
			}
			possible := in.doc.Raw.PossibleTypes[def.Name]
			list := make([]any, 0, len(possible))
			for _, pt := range possible {
				list = append(list, in.typeValue(pt.Name))
			}
			return list
		},
		"enumValues": func(args map[string]any) any {
			if def.Kind != ast.Enum {
				return nil
			}
			list := make([]any, 0, len(def.EnumValues))
			for _, ev := range def.EnumValues {
				if isDeprecated(ev.Directives) && !includeDeprecated(args) {
					continue
				}
				list = append(list, &introObject{typeName: "__EnumValue", fields: map[string]introField{
					"name":              constant(ev.Name),
					"description":       constant(nilIfEmpty(ev.Description)),
					"isDeprecated":      constant(isDeprecated(ev.Directives)),
					"deprecationReason": constant(deprecationReason(ev.Directives)),
				}})
			}
			return list
		},
		"inputFields": func(args map[string]any) any {
			if def.Kind != ast.InputObject {
				return nil
			}
			list := make([]any, 0, len(def.Fields))
			for _, fd := range def.Fields {
				if isDeprecated(fd.Directives) && !includeDeprecated(args) {
					continue
				}
				list = append(list, in.inputValue(fd.Name, fd.Description, fd.Type, fd.DefaultValue, fd.Directives))
			}
			return list
		},
	}}
}

// typeRef renders a type reference: the ofType wrapper chain the specification
// requires for non-null and list types, terminating in a named type.
func (in *introspector) typeRef(t *ast.Type) *introObject {
	if t == nil {
		return nil
	}
	if t.NonNull {
		inner := *t
		inner.NonNull = false
		return wrapperType("NON_NULL", in.typeRef(&inner))
	}
	if t.Elem != nil {
		return wrapperType("LIST", in.typeRef(t.Elem))
	}
	return in.typeValue(t.NamedType)
}

// wrapperType is the unnamed __Type of a NON_NULL or LIST wrapper: every field
// but kind and ofType is null on it.
func wrapperType(kind string, of *introObject) *introObject {
	return &introObject{typeName: "__Type", fields: map[string]introField{
		"kind":           constant(kind),
		"name":           constant(nil),
		"description":    constant(nil),
		"specifiedByURL": constant(nil),
		"fields":         constant(nil),
		"interfaces":     constant(nil),
		"possibleTypes":  constant(nil),
		"enumValues":     constant(nil),
		"inputFields":    constant(nil),
		"ofType":         constant(of),
	}}
}

func (in *introspector) fieldValue(fd *ast.FieldDefinition) *introObject {
	return &introObject{typeName: "__Field", fields: map[string]introField{
		"name":        constant(fd.Name),
		"description": constant(nilIfEmpty(fd.Description)),
		"args": func(args map[string]any) any {
			list := make([]any, 0, len(fd.Arguments))
			for _, ad := range fd.Arguments {
				if isDeprecated(ad.Directives) && !includeDeprecated(args) {
					continue
				}
				list = append(list, in.inputValue(ad.Name, ad.Description, ad.Type, ad.DefaultValue, ad.Directives))
			}
			return list
		},
		"type":              func(map[string]any) any { return in.typeRef(fd.Type) },
		"isDeprecated":      constant(isDeprecated(fd.Directives)),
		"deprecationReason": constant(deprecationReason(fd.Directives)),
	}}
}

func (in *introspector) inputValue(name, description string, t *ast.Type, def *ast.Value, dirs ast.DirectiveList) *introObject {
	var defaultValue any
	if def != nil {
		defaultValue = def.String()
	}
	return &introObject{typeName: "__InputValue", fields: map[string]introField{
		"name":              constant(name),
		"description":       constant(nilIfEmpty(description)),
		"type":              func(map[string]any) any { return in.typeRef(t) },
		"defaultValue":      constant(defaultValue),
		"isDeprecated":      constant(isDeprecated(dirs)),
		"deprecationReason": constant(deprecationReason(dirs)),
	}}
}

func (in *introspector) directiveValue(d *ast.DirectiveDefinition) *introObject {
	locations := make([]any, 0, len(d.Locations))
	for _, loc := range d.Locations {
		locations = append(locations, string(loc))
	}
	return &introObject{typeName: "__Directive", fields: map[string]introField{
		"name":         constant(d.Name),
		"description":  constant(nilIfEmpty(d.Description)),
		"locations":    constant(locations),
		"isRepeatable": constant(d.IsRepeatable),
		"args": func(args map[string]any) any {
			list := make([]any, 0, len(d.Arguments))
			for _, ad := range d.Arguments {
				if isDeprecated(ad.Directives) && !includeDeprecated(args) {
					continue
				}
				list = append(list, in.inputValue(ad.Name, ad.Description, ad.Type, ad.DefaultValue, ad.Directives))
			}
			return list
		},
	}}
}

// rootObject is the object an introspection operation selects against: the
// meta-fields the specification adds to the query root.
func (in *introspector) rootObject() *introObject {
	return &introObject{typeName: queryTypeName, fields: map[string]introField{
		"__schema": func(map[string]any) any { return in.schemaValue() },
		"__type": func(args map[string]any) any {
			name, _ := args["name"].(string)
			return in.typeValue(name)
		},
	}}
}

// ---------------------------------------------------------------------------
// The executor.
// ---------------------------------------------------------------------------

// execState carries what a selection walk needs beyond the object itself.
type execState struct {
	frags map[string]*ast.FragmentDefinition
	vars  map[string]any
}

// execute runs an introspection operation and returns its response data. It
// supports the whole of what a real client's IntrospectionQuery uses: aliases,
// arguments, variables, fragment definitions and inline fragments.
func (in *introspector) execute(op *ast.OperationDefinition, frags ast.FragmentDefinitionList, vars map[string]any) (map[string]any, error) {
	st := &execState{frags: map[string]*ast.FragmentDefinition{}, vars: vars}
	for _, f := range frags {
		st.frags[f.Name] = f
	}
	return in.resolveSet(in.rootObject(), op.SelectionSet, st)
}

func (in *introspector) resolveSet(obj *introObject, set ast.SelectionSet, st *execState) (map[string]any, error) {
	out := map[string]any{}
	if err := in.collect(obj, set, st, out); err != nil {
		return nil, err
	}
	return out, nil
}

// collect merges one selection set into out, flattening fragments whose type
// condition matches the object being resolved.
func (in *introspector) collect(obj *introObject, set ast.SelectionSet, st *execState, out map[string]any) error {
	for _, sel := range set {
		switch s := sel.(type) {
		case *ast.Field:
			key := s.Alias
			if key == "" {
				key = s.Name
			}
			if s.Name == typenameField {
				out[key] = obj.typeName
				continue
			}
			resolve, ok := obj.fields[s.Name]
			if !ok {
				return fmt.Errorf("cannot query field %q on type %q", s.Name, obj.typeName)
			}
			args, err := argumentValues(s.Arguments, st.vars)
			if err != nil {
				return err
			}
			value, err := in.resolveValue(resolve(args), s.SelectionSet, st)
			if err != nil {
				return err
			}
			out[key] = value
		case *ast.InlineFragment:
			if !matchesCondition(s.TypeCondition, obj) {
				continue
			}
			if err := in.collect(obj, s.SelectionSet, st, out); err != nil {
				return err
			}
		case *ast.FragmentSpread:
			frag, ok := st.frags[s.Name]
			if !ok {
				return fmt.Errorf("unknown fragment %q", s.Name)
			}
			if !matchesCondition(frag.TypeCondition, obj) {
				continue
			}
			if err := in.collect(obj, frag.SelectionSet, st, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveValue renders a resolved value against its sub-selection: objects are
// walked, lists are mapped, scalars are returned as they are.
func (in *introspector) resolveValue(v any, set ast.SelectionSet, st *execState) (any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case *introObject:
		if t == nil {
			return nil, nil
		}
		if len(set) == 0 {
			return nil, fmt.Errorf("a %s must have a subselection", t.typeName)
		}
		return in.resolveSet(t, set, st)
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			resolved, err := in.resolveValue(item, set, st)
			if err != nil {
				return nil, err
			}
			out = append(out, resolved)
		}
		return out, nil
	default:
		if len(set) > 0 {
			return nil, fmt.Errorf("a scalar cannot have a subselection")
		}
		return v, nil
	}
}

func matchesCondition(condition string, obj *introObject) bool {
	return condition == "" || condition == obj.typeName
}

// argumentValues coerces an argument list to Go values, resolving variables and
// defaults the way the specification does.
func argumentValues(args ast.ArgumentList, vars map[string]any) (map[string]any, error) {
	if len(args) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(args))
	for _, arg := range args {
		v, err := arg.Value.Value(vars)
		if err != nil {
			return nil, fmt.Errorf("argument %q: %w", arg.Name, err)
		}
		out[arg.Name] = v
	}
	return out, nil
}

// includeDeprecated reads the argument every deprecatable introspection field
// takes. Absent, it is false, and deprecated members are hidden.
func includeDeprecated(args map[string]any) bool {
	b, _ := args["includeDeprecated"].(bool)
	return b
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isDeprecated(dirs ast.DirectiveList) bool { return dirs.ForName("deprecated") != nil }

func deprecationReason(dirs ast.DirectiveList) any {
	d := dirs.ForName("deprecated")
	if d == nil {
		return nil
	}
	if arg := d.Arguments.ForName("reason"); arg != nil && arg.Value != nil {
		return arg.Value.Raw
	}
	return "No longer supported"
}

// typeKind maps gqlparser's definition kinds onto the __TypeKind enum.
func typeKind(k ast.DefinitionKind) string {
	switch k {
	case ast.Scalar:
		return "SCALAR"
	case ast.Object:
		return "OBJECT"
	case ast.Interface:
		return "INTERFACE"
	case ast.Union:
		return "UNION"
	case ast.Enum:
		return "ENUM"
	case ast.InputObject:
		return "INPUT_OBJECT"
	default:
		return strings.ToUpper(string(k))
	}
}
