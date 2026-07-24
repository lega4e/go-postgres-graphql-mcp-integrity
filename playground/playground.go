// Package playground is the thin driver behind the WASM playground. It runs the
// real gopgql pipeline — sdl parse/validate, generator, migrate and compiler —
// end to end on an editable SDL document, GraphQL query and variables, with no
// JavaScript re-implementation and no database.
//
// Everything it returns is *generated* from the inputs: the goose migration and
// the compiled GRAPH_TABLE SQL with its ordered bind parameters. It never
// fabricates query *results* — shaping a response requires rows from PostgreSQL,
// which the browser has no access to (SPEC.md §4: only sdl/generator/migrate/
// compiler are database-free and compile to WASM; exec/shape need a real DB).
//
// It is a normal Go package so it is unit-testable on the host and reused
// verbatim by the js/wasm entry point in cmd/wasm.
package playground

import (
	"errors"
	"fmt"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/sdl"
)

// ExampleSDL is the worked example from SPEC.md §5.2, loaded as the playground's
// initial schema.
const ExampleSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// ExampleQuery is the M4 exit query (SPEC.md §7 → M4): a three-hop traversal
// filtered by a bound variable, compiled to a single GRAPH_TABLE. It is
// editable in the playground.
const ExampleQuery = `{ persons(name: $n) { name follows { name follows { name follows { name } } } } }`

// ExampleDeepQuery is one hop past the default MaxDepth. It compiles to a typed
// *compiler.DepthExceededError rather than a truncated pattern: SQL/PGQ has no
// variable-length paths, so gopgql rejects (SPEC.md §3, decision 3).
const ExampleDeepQuery = `{ persons(name: $n) { follows { follows { follows { follows { name } } } } } }`

// ExampleInterfaceSDL maps two vertex tables under one interface twice over
// (SPEC.md §7 → M4). Actor carries @node, so persons and bots both expose the
// shared `actor` label; Profile does not, so it is matched by label alternation
// over the implementors' own labels.
const ExampleInterfaceSDL = `interface Actor @node(label: "actor") {
  id: ID!
  name: String!
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}

interface Profile {
  id: ID!
  name: String!
}

type Person implements Actor & Profile @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}

type Bot implements Actor & Profile @node(label: "bot") {
  id: ID!
  name: String!
  vendor: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT, table: "bot_follows")
}`

// ExampleInterfaceQuery traverses from the shared-label interface into a
// concrete type. Because an actor may be a person, the two positions could bind
// the same row, so the compiler emits the isomorphism guard (SPEC.md §2.2).
const ExampleInterfaceQuery = `{ actors { name follows { name } } }`

// ExampleVars is the initial variables document (JSON) bound to ExampleQuery.
const ExampleVars = `{ "n": "Alice" }`

// RevisedExampleSDL widens the worked example with a nullable `age` field. The
// Delta view diffs the schema against it to show the generated delta migration.
const RevisedExampleSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  age: Int
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// Migration parses and validates the SDL and returns the initial goose
// migration (0001_init.sql) generated from it.
func Migration(sdlSrc string) (string, error) {
	doc, err := sdl.Parse(sdlSrc)
	if err != nil {
		return "", err
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		return "", err
	}
	return migrate.Init(m), nil
}

// Compiled is the output of Compile: the GRAPH_TABLE SQL and a human-readable
// rendering of its ordered bind parameters. Both are pure functions of the
// inputs — no database is consulted (SPEC.md §6.1).
type Compiled struct {
	// SQL is the compiled GRAPH_TABLE query, including any $n placeholders.
	SQL string
	// Params renders the ordered bind parameters, e.g. "$1 = Alice", or a note
	// when the query carries none.
	Params string
}

// Compile parses the SDL and compiles the GraphQL query against it, resolving
// any variables from vars. It returns the emitted SQL and the ordered bind
// parameters — proving values travel as parameters, never interpolated
// (SPEC.md §6.2). It never executes the query.
func Compile(sdlSrc, query string, vars map[string]any) (Compiled, error) {
	doc, err := sdl.Parse(sdlSrc)
	if err != nil {
		return Compiled{}, err
	}
	sql, args, err := compiler.New(doc).Compile(query, vars)
	if err != nil {
		return Compiled{}, err
	}
	return Compiled{SQL: sql, Params: renderParams(args)}, nil
}

// MaxDepth reports the compiler's default traversal-depth ceiling, so the
// playground can name it when a query is rejected for exceeding it.
func MaxDepth() int { return compiler.DefaultMaxDepth }

// DepthExceeded classifies a Compile error: it reports whether the compiler
// refused the query for nesting past its depth ceiling, and what that ceiling
// was. It is what lets the playground present a depth rejection as the designed
// outcome it is, rather than as a generic error (SPEC.md §10: depth limits
// reject rather than truncate).
func DepthExceeded(err error) (limit int, ok bool) {
	var depthErr *compiler.DepthExceededError
	if errors.As(err, &depthErr) {
		return depthErr.MaxDepth, true
	}
	return 0, false
}

// Delta generates the delta migration between two SDL revisions. It builds the
// initial migration for oldSDL, folds it back into a schema — running the real
// fold interpreter, exactly as `migrate diff` does — and diffs it against the
// schema built from newSDL (SPEC.md §7 → M2). changed reports whether the
// revision produced any schema change.
func Delta(oldSDL, newSDL string) (delta string, changed bool, err error) {
	oldDoc, err := sdl.Parse(oldSDL)
	if err != nil {
		return "", false, err
	}
	oldModel, err := generator.Build(oldDoc, "")
	if err != nil {
		return "", false, err
	}

	prior, err := migrate.FoldContent([]string{migrate.Init(oldModel)})
	if err != nil {
		return "", false, err
	}

	newDoc, err := sdl.Parse(newSDL)
	if err != nil {
		return "", false, err
	}
	newModel, err := generator.Build(newDoc, "")
	if err != nil {
		return "", false, err
	}

	up, down, changed := migrate.Delta(prior, newModel)
	if !changed {
		return "-- no schema change between the two SDL revisions", false, nil
	}
	return "-- +goose Up\n" + up + "\n-- +goose Down\n" + down, true, nil
}

// renderParams formats ordered bind parameters as "$1 = v1, $2 = v2".
func renderParams(args []any) string {
	if len(args) == 0 {
		return "(no bind parameters)"
	}
	out := ""
	for i, a := range args {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("$%d = %v", i+1, a)
	}
	return out
}
