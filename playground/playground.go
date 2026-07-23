// Package playground is the thin driver behind the WASM playground. It runs the
// real gopgql pipeline — sdl parse/validate, generator, migrate and compiler —
// end to end on a pasted SDL document and GraphQL query, with no JavaScript
// re-implementation and no database (SPEC.md §7 → M1/M2 demo criteria).
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

// RevisedExampleSDL is the M2 demo's second SDL revision: the worked example
// widened with a nullable `age` field. The playground diffs it against
// ExampleSDL to show the generated delta migration.
const RevisedExampleSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  age: Int
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// RevisedExampleQuery projects the new field to show the delta is queryable.
const RevisedExampleQuery = `{ persons { name age } }`

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

// DeltaResult is the output of RunDelta: the initial migration for the prior
// SDL, the delta migration between the two revisions, whether the revision
// changed anything, and the query compiled against the revised SDL.
type DeltaResult struct {
	// Init is the 0001_init.sql goose file for the prior (old) SDL.
	Init string
	// Delta is the generated delta goose file between old and new, or a note
	// when the revision changes nothing.
	Delta string
	// Changed reports whether the revision produced a delta.
	Changed bool
	// SQL is the GRAPH_TABLE query compiled from query against the revised SDL.
	SQL string
}

// RunDelta demonstrates M2 end to end with no database (SPEC.md §7 → M2 demo
// criterion): it generates the initial migration for oldSDL, folds that
// migration back into a schema — running the real fold interpreter — diffs it
// against the schema built from newSDL, and renders the delta migration. The
// query is compiled against the revised SDL so the new field is visibly
// queryable.
func RunDelta(oldSDL, newSDL, query string) (DeltaResult, error) {
	oldDoc, err := sdl.Parse(oldSDL)
	if err != nil {
		return DeltaResult{}, err
	}
	oldModel, err := generator.Build(oldDoc, "")
	if err != nil {
		return DeltaResult{}, err
	}
	initFile := migrate.Init(oldModel)

	// Fold the just-generated init back into a model, exactly as `migrate diff`
	// folds prior migration files — proving the fold works in the browser too.
	prior, err := migrate.FoldContent([]string{initFile})
	if err != nil {
		return DeltaResult{}, err
	}

	newDoc, err := sdl.Parse(newSDL)
	if err != nil {
		return DeltaResult{}, err
	}
	newModel, err := generator.Build(newDoc, "")
	if err != nil {
		return DeltaResult{}, err
	}

	res := DeltaResult{Init: initFile}
	up, down, changed := migrate.Delta(prior, newModel)
	res.Changed = changed
	if changed {
		res.Delta = "-- +goose Up\n" + up + "\n-- +goose Down\n" + down
	} else {
		res.Delta = "-- no schema change between the two SDL revisions"
	}

	if query != "" {
		sql, _, err := compiler.New(newDoc).Compile(query, nil)
		if err != nil {
			return DeltaResult{}, err
		}
		res.SQL = sql
	}
	return res, nil
}
