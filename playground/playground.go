// Package playground is the thin driver behind the WASM playground. It runs the
// real gopgql pipeline — sdl parse/validate, generator, migrate and compiler —
// end to end on a pasted SDL document and GraphQL query, with no JavaScript
// re-implementation and no database (SPEC.md §7 → M1 demo criterion).
//
// It is a normal Go package so it is unit-testable on the host and reused
// verbatim by the js/wasm entry point in cmd/wasm.
package playground

import (
	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/sdl"
)

// ExampleSDL is the worked example from SPEC.md §5.2, loaded as the playground's
// initial input.
const ExampleSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// ExampleQuery is the M1 exit query (SPEC.md §7).
const ExampleQuery = `{ persons { name } }`

// Result is the output of Run: the initial goose migration and the compiled
// GRAPH_TABLE query.
type Result struct {
	// Migration is the full 0001_init.sql goose file content.
	Migration string
	// SQL is the GRAPH_TABLE query compiled from the GraphQL query.
	SQL string
}

// Run parses and validates the SDL, generates the initial migration, and
// compiles the query — returning the artefacts or the first error encountered.
func Run(sdlSrc, query string) (Result, error) {
	doc, err := sdl.Parse(sdlSrc)
	if err != nil {
		return Result{}, err
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		return Result{}, err
	}
	res := Result{Migration: migrate.Init(m)}

	if query != "" {
		sql, _, err := compiler.New(doc).Compile(query, nil)
		if err != nil {
			return Result{}, err
		}
		res.SQL = sql
	}
	return res, nil
}
