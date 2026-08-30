package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/formatter"
	"github.com/vektah/gqlparser/v2/parser"

	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/sdl"
)

// The result formats the query tool accepts.
const (
	FormatJSON     = "json"
	FormatMarkdown = "markdown"
)

const (
	// typenameField is the introspection meta-field answerable on any object.
	typenameField = "__typename"
	// keyAlias is the response key of the surrogate selection injected when a
	// level would otherwise select nothing — `{ Persons { __typename } }` strips
	// down to an empty selection set, which the compiler rejects. The column is
	// removed from the response before it is returned.
	keyAlias = "_gopgql_key"
)

// typenameSite records a `__typename` selection removed before compilation: the
// response-key path to the level it was selected at, the key it must reappear
// under, and the type name to write there. drop marks the surrogate selection
// injected for an otherwise-empty level, which is deleted rather than written.
type typenameSite struct {
	path     []string
	key      string
	typeName string
	drop     bool
}

// Query compiles a GraphQL operation, executes it against the connected
// database, and renders the response in the requested format.
//
// Introspection is answered from the loaded schema: an operation selecting only
// meta-fields never reaches the database (design D2). Everything else compiles
// to SQL with its values bound as parameters, executes, and is shaped by the
// same code path the library uses.
//
// This is the span the query tool is measured by, and the parent of the
// exec.Query span underneath it: the difference between the two is what parsing,
// compiling and shaping cost, which is the only way to tell a slow database from
// a slow compiler. The result parameters are named because the deferred closer
// observes the error *variable*; the work is delegated to an unexported method
// so that none of its many `return` statements can bypass that assignment and
// record a failure as a success.
//
// The operation text is not an attribute. It is unbounded, and its literals are
// the caller's data.
func (s *Server) Query(ctx context.Context, query string, vars map[string]any, format string) (out string, err error) {
	ctx, end := instr.Start(ctx, "Query", attrFormat.String(defaultFormat(format, FormatJSON)))
	defer func() { end(err) }()

	out, err = s.query(ctx, query, vars, format)
	return out, err
}

func (s *Server) query(ctx context.Context, query string, vars map[string]any, format string) (string, error) {
	switch format {
	case "", FormatJSON:
		format = FormatJSON
	case FormatMarkdown:
	default:
		return "", fmt.Errorf("gopgql/mcp: unknown format %q; supported formats are %q and %q", format, FormatJSON, FormatMarkdown)
	}

	doc, gqlErr := parser.ParseQuery(&ast.Source{Name: "operation", Input: query})
	if gqlErr != nil {
		return "", fmt.Errorf("gopgql/mcp: parse operation: %w", gqlErr)
	}
	if len(doc.Operations) != 1 {
		return "", fmt.Errorf("gopgql/mcp: exactly one operation is supported, got %d", len(doc.Operations))
	}
	op := doc.Operations[0]
	if op.Operation != ast.Query {
		// Not because a mutation cannot be compiled — it can, since M11 — but
		// because running one needs a connection the caller owns and this
		// server has only the read-only pool it opened itself (see the package
		// doc).
		return "", fmt.Errorf("gopgql/mcp: only query operations are supported by this server")
	}

	if isIntrospection(op.SelectionSet, doc.Fragments) {
		if format == FormatMarkdown {
			return "", fmt.Errorf("gopgql/mcp: an introspection result is nested and a markdown table cannot represent it; use the %q format", FormatJSON)
		}
		data, err := s.intro.execute(op, doc.Fragments, vars)
		if err != nil {
			return "", fmt.Errorf("gopgql/mcp: introspection: %w", err)
		}
		return renderJSON(data)
	}

	roots := rootFields(op.SelectionSet)
	if len(roots) != 1 {
		return "", fmt.Errorf("gopgql/mcp: exactly one root field is supported, got %d", len(roots))
	}
	root := roots[0]
	target := s.doc.RootTarget(root.Name)
	if target == nil {
		return "", fmt.Errorf("gopgql/mcp: unknown root field %q; queryable root fields are %s",
			root.Name, strings.Join(s.doc.RootFields(), ", "))
	}

	// A markdown table has no way to express nesting, so a nested selection is
	// refused here — before anything is compiled, and so before any statement
	// could reach the database (design D2a).
	if format == FormatMarkdown {
		if nesting := s.nestedField(root, target); nesting != "" {
			return "", fmt.Errorf("gopgql/mcp: %q selects the relationship %q, and a markdown table cannot represent nested results; use the %q format",
				responseKey(root), nesting, FormatJSON)
		}
	}
	columns := selectedColumns(root)

	var sites []typenameSite
	if err := s.stripTypenames(root, target, nil, &sites); err != nil {
		return "", err
	}

	cq, err := s.comp.CompileQuery(formatOperation(doc), vars)
	if err != nil {
		return "", err
	}
	response, err := exec.Query(ctx, s.db, cq)
	if err != nil {
		return "", err
	}
	applyTypenames(response, sites)

	if format == FormatMarkdown {
		return renderMarkdown(response, responseKey(root), columns), nil
	}
	return renderJSON(response)
}

// isIntrospection reports whether an operation selects only introspection
// meta-fields, and is therefore answerable from the loaded schema alone.
func isIntrospection(set ast.SelectionSet, frags ast.FragmentDefinitionList) bool {
	names := selectedNames(set, frags, map[string]bool{})
	if len(names) == 0 {
		return false
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "__") {
			return false
		}
	}
	return true
}

// selectedNames lists the field names a selection set reaches, following
// fragments. seen guards against a fragment cycle.
func selectedNames(set ast.SelectionSet, frags ast.FragmentDefinitionList, seen map[string]bool) []string {
	var names []string
	for _, sel := range set {
		switch s := sel.(type) {
		case *ast.Field:
			names = append(names, s.Name)
		case *ast.InlineFragment:
			names = append(names, selectedNames(s.SelectionSet, frags, seen)...)
		case *ast.FragmentSpread:
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			for _, f := range frags {
				if f.Name == s.Name {
					names = append(names, selectedNames(f.SelectionSet, frags, seen)...)
				}
			}
		}
	}
	return names
}

// rootFields returns the plain fields selected at the root. Fragments at the
// root of a data query are left for the compiler to reject.
func rootFields(set ast.SelectionSet) []*ast.Field {
	var out []*ast.Field
	for _, sel := range set {
		if f, ok := sel.(*ast.Field); ok {
			out = append(out, f)
		}
	}
	return out
}

// nestedField returns the response key of the first relationship selected under
// field, or "" when the selection is flat.
func (s *Server) nestedField(field *ast.Field, target *sdl.Target) string {
	for _, sel := range field.SelectionSet {
		f, ok := sel.(*ast.Field)
		if !ok {
			continue
		}
		if def := findField(target.Fields, f.Name); def != nil && def.Rel != nil {
			return responseKey(f)
		}
	}
	return ""
}

// selectedColumns lists a level's response keys in selection order — the column
// order a markdown table uses.
func selectedColumns(field *ast.Field) []string {
	var cols []string
	for _, sel := range field.SelectionSet {
		if f, ok := sel.(*ast.Field); ok {
			cols = append(cols, responseKey(f))
		}
	}
	return cols
}

// stripTypenames removes every `__typename` selection from the operation,
// recording where each one must reappear in the response.
//
// The compiler maps fields to columns and has no notion of a meta-field, so
// `__typename` is answered here, from the same document introspection is built
// from. A level whose whole selection was meta-fields is given a surrogate
// selection so it still compiles; that column is dropped from the response.
func (s *Server) stripTypenames(field *ast.Field, target *sdl.Target, path []string, sites *[]typenameSite) error {
	here := append(append([]string{}, path...), responseKey(field))

	var kept ast.SelectionSet
	for _, sel := range field.SelectionSet {
		f, ok := sel.(*ast.Field)
		if !ok {
			kept = append(kept, sel) // not a plain field; the compiler will reject it
			continue
		}
		if f.Name == typenameField {
			if s.doc.NodeByType(target.TypeName) == nil {
				return fmt.Errorf("gopgql/mcp: __typename cannot be resolved on %s: it is an interface, whose concrete type is only known per row",
					target.TypeName)
			}
			*sites = append(*sites, typenameSite{path: here, key: responseKey(f), typeName: target.TypeName})
			continue
		}
		kept = append(kept, f)

		def := findField(target.Fields, f.Name)
		if def == nil || def.Rel == nil {
			continue
		}
		child := s.doc.TargetForType(def.TypeName)
		if child == nil {
			continue
		}
		if err := s.stripTypenames(f, child, here, sites); err != nil {
			return err
		}
	}

	if len(kept) == 0 {
		// Every node carries a surrogate `id` key (sdl validates it), so this
		// always compiles; the column is removed again before the response is
		// returned.
		kept = ast.SelectionSet{&ast.Field{Name: "id", Alias: keyAlias}}
		*sites = append(*sites, typenameSite{path: here, key: keyAlias, drop: true})
	}
	field.SelectionSet = kept
	return nil
}

// applyTypenames writes the recorded `__typename` values into the shaped
// response and removes the surrogate selections.
func applyTypenames(response map[string]any, sites []typenameSite) {
	for _, site := range sites {
		site := site
		visitPath(response, site.path, func(obj map[string]any) {
			if site.drop {
				delete(obj, site.key)
				return
			}
			obj[site.key] = site.typeName
		})
	}
}

// visitPath calls fn on every object the response-key path reaches, descending
// through the lists the shaper produced at each level.
func visitPath(response map[string]any, path []string, fn func(map[string]any)) {
	if len(path) == 0 {
		return
	}
	value, ok := response[path[0]]
	if !ok {
		return
	}
	descend(value, path[1:], fn)
}

func descend(value any, rest []string, fn func(map[string]any)) {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			descend(item, rest, fn)
		}
	case map[string]any:
		if len(rest) == 0 {
			fn(v)
			return
		}
		if next, ok := v[rest[0]]; ok {
			descend(next, rest[1:], fn)
		}
	}
}

// formatOperation renders the (possibly rewritten) operation back to GraphQL
// for the compiler.
func formatOperation(doc *ast.QueryDocument) string {
	var buf bytes.Buffer
	formatter.NewFormatter(&buf).FormatQueryDocument(doc)
	return buf.String()
}

func renderJSON(v any) (string, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("gopgql/mcp: render result: %w", err)
	}
	return string(out), nil
}

// renderMarkdown renders a flat result set as a table. An empty result is a
// header with no rows, not an error: "no matching records" is an answer.
func renderMarkdown(response map[string]any, rootKey string, columns []string) string {
	var b strings.Builder
	b.WriteString("| " + strings.Join(columns, " | ") + " |\n")
	b.WriteString("|" + strings.Repeat(" --- |", len(columns)) + "\n")

	rows, _ := response[rootKey].([]any)
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		cells := make([]string, len(columns))
		for i, col := range columns {
			cells[i] = markdownCell(obj[col])
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
	return b.String()
}

// markdownCell renders one value so it cannot break out of its cell.
func markdownCell(v any) string {
	if v == nil {
		return ""
	}
	s := fmt.Sprintf("%v", v)
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// responseKey is the key a field appears under in the response: its alias when
// it has one, otherwise its name.
func responseKey(f *ast.Field) string {
	if f.Alias != "" {
		return f.Alias
	}
	return f.Name
}

// findField looks a field up by name in a target's field list.
func findField(fields []*sdl.Field, name string) *sdl.Field {
	for _, f := range fields {
		if f.Name == name {
			return f
		}
	}
	return nil
}
