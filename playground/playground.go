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

// ExampleMultiPatternQuery selects two relationships at one level, the shape
// that would need comma-separated path patterns — which PG19 parses but does not
// execute. It compiles to the M5 workaround instead: one GRAPH_TABLE per branch,
// LEFT JOINed on the projected root id (SPEC.md §6.2, §7 → M5).
const ExampleMultiPatternQuery = `{ persons(name: $n) { name follows { name } followedBy { name } } }`

// ExampleDirectivesSDL exercises the M6 mapping directives at once: a renamed
// column, an overridden type, a database-enforced unique, and two indexes — one
// named with an explicit access method, one bare so the generator derives the
// name (SPEC.md §5, §7 → M6).
const ExampleDirectivesSDL = `type Product @node(label: "product") {
  id: ID!
  sku: String! @unique
  title: String! @column(name: "name")
  price: Float! @column(type: "numeric(10,2)")
  category: String! @index(name: "products_category_idx", using: "btree")
  vendor: String @index
}`

// ExampleDirectivesQuery reads the renamed column: the GraphQL field stays
// `title`, while the graph exposes — and the compiler projects — `name`.
const ExampleDirectivesQuery = `{ products(title: $t) { title price category } }`

// ExampleDirectivesVars binds ExampleDirectivesQuery's variable.
const ExampleDirectivesVars = `{ "t": "Chain" }`

// ExampleConstraintsSDL exercises the whole M7 constraint surface at once
// (SPEC.md §5, §7 → M7): a column default, a column check, a table-level check
// spanning two columns, and a two-column natural key.
//
// The natural key sits *alongside* the surrogate `id`, which stays the physical
// identity edges reference (design D1) — the generated DDL keeps
// `id uuid PRIMARY KEY` and adds `CONSTRAINT employees_key UNIQUE (tenant,
// email)`, and the property graph lists the key's columns in a `KEY (...)`
// clause so a MATCH can select a vertex by its data.
//
// The check expressions name `left_at` and `joined_at` — the *physical* columns
// — rather than the GraphQL fields `leftAt` and `joinedAt`. They are raw SQL
// emitted verbatim (design D6), so they live in the column namespace that
// @column(name:) defines, not in the schema's.
const ExampleConstraintsSDL = `type Employee @node(label: "employee")
              @key(fields: ["tenant", "email"])
              @check(expr: "left_at IS NULL OR left_at >= joined_at") {
  id: ID!
  tenant: String!
  email: String!
  role: String! @default(value: "'member'")
                @check(expr: "role IN ('member', 'admin')")
  joinedAt: DateTime! @column(name: "joined_at") @default(value: "now()")
  leftAt: DateTime @column(name: "left_at")
  reportsTo: [Employee!]! @relationship(type: "reports_to", direction: OUT)
}`

// ExampleConstraintsQuery selects an employee by the two columns of its natural
// key. It is the milestone's exit criterion made visible: the key's columns are
// graph properties, so filtering on them compiles to predicates inside the
// MATCH rather than to anything the surrogate id has to mediate.
const ExampleConstraintsQuery = `{ employees(tenant: $t, email: $e) { role joinedAt } }`

// ExampleConstraintsVars binds ExampleConstraintsQuery's two variables.
const ExampleConstraintsVars = `{ "t": "acme", "e": "ada@acme.example" }`

// RevisedConstraintsSDL renames one field of ExampleConstraintsSDL — `email`
// becomes `workEmail`, mapped to the column `work_email` — and declares the
// rename with @renamedFrom. Diffing the two produces a migration that *moves*
// the column instead of dropping one and adding another, so the rows in it
// survive.
//
// The hint carries the previous **GraphQL** name, not the previous column name;
// the differ derives the candidate physical names from it and accepts one only
// when the folded prior state actually holds it (design D2). Nothing here is
// inferred: without the hint, the same edit is a drop and an add, because a
// differ cannot tell those apart from a rename and guessing wrong loses the
// data either way.
const RevisedConstraintsSDL = `type Employee @node(label: "employee")
              @key(fields: ["tenant", "workEmail"])
              @check(expr: "left_at IS NULL OR left_at >= joined_at") {
  id: ID!
  tenant: String!
  workEmail: String! @column(name: "work_email") @renamedFrom(name: "email")
  role: String! @default(value: "'member'")
                @check(expr: "role IN ('member', 'admin')")
  joinedAt: DateTime! @column(name: "joined_at") @default(value: "now()")
  leftAt: DateTime @column(name: "left_at")
  reportsTo: [Employee!]! @relationship(type: "reports_to", direction: OUT)
}`

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
// migrations generated from it — both halves, as gopgql writes them:
// migrations/tables/0001_init.sql and migrations/graph/0001_init.sql.
//
// They are returned as one annotated document because the playground has one
// output pane, but they are two files that are generated and applied
// separately, tables first (gopgql#38).
func Migration(sdlSrc string) (string, error) {
	doc, err := sdl.Parse(sdlSrc)
	if err != nil {
		return "", err
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		return "", err
	}
	return "-- migrations/tables/0001_init.sql\n" +
		migrate.InitTables(m) +
		"\n-- migrations/graph/0001_init.sql\n" +
		"-- Applied after the tables above: the graph references them.\n" +
		migrate.InitGraph(m), nil
}

// Schema parses and validates the SDL and returns the PostgreSQL DDL generated
// from it: the vertex and edge tables, their indexes, and the
// CREATE PROPERTY GRAPH that maps them.
//
// It is the same model Migration renders into a goose file, without the goose
// framing — the schema a compiled query runs against, so the playground can show
// the two side by side.
func Schema(sdlSrc string) (string, error) {
	doc, err := sdl.Parse(sdlSrc)
	if err != nil {
		return "", err
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		return "", err
	}
	return generator.DDL(m), nil
}

// GraphMapping parses and validates the SDL and returns just the
// CREATE PROPERTY GRAPH statement generated from it — the graph mapping,
// without the tables it is drawn over.
//
// It exists because that statement is exactly the surface `gopgql conform`
// compares: PostgreSQL records elements, labels and properties in
// pg_propgraph_element, pg_propgraph_label and pg_propgraph_property, and
// records nothing else about them. Showing the mapping on its own says what a
// conformance report can and cannot be about far better than a paragraph does —
// everything the surrounding DDL declares (defaults, CHECK and UNIQUE
// constraints, indexes, column types) is absent from it, and is equally absent
// from the check.
//
// The conform package itself needs a live connection and so sits on the pgx
// side of the WASM boundary (SPEC.md §4.1, design D5). This package must never
// import it; what the playground can honestly show is this half of the
// comparison, generated here and now, next to a recorded report.
func GraphMapping(sdlSrc string) (string, error) {
	doc, err := sdl.Parse(sdlSrc)
	if err != nil {
		return "", err
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		return "", err
	}
	return generator.GraphDDL(m), nil
}

// ExampleConformanceReport is a **fixture**: a recorded run of `gopgql conform`
// against a database that had drifted from ExampleConstraintsSDL. It is not
// generated, and the page says so — a browser has no database, so a live check
// is not something this playground can do (design D5). Presenting a fabricated
// report as a live one would be the one dishonest panel in a playground whose
// whole claim is that nothing is hardcoded.
//
// What it is for is the report's *structure*, which is the part a reader has to
// understand before they can use the check: five finding kinds, an element and
// an optional property, and the two sides named SDL and DATABASE with `-` for
// "nothing there". All five kinds appear once, which no single real drift is
// likely to produce.
//
// It is kept honest in two ways. The drift it describes is drift in
// ExampleConstraintsSDL's own graph — the same schema the Constraints tab
// generates, so a reader can check every element and property name against the
// mapping shown beside it, and TestConformanceReportMatchesTheSchema asserts
// that correspondence on every build. And the layout is the real one: the
// column order, the `-` convention and the closing coverage note are what
// cmd/gopgql prints, so what a reader learns here is what they will see.
//
// The exit status is part of the report. `2` means the check ran and found
// drift; `1` would mean it did not run at all — a schema that would not parse,
// a database it could not reach, a graph it could not find. Those demand
// different responses, which is why they are different numbers.
const ExampleConformanceReport = `$ gopgql conform --sdl schema.graphql --dsn "$GOPGQL_DSN"; echo "exit $?"
KIND                ELEMENT       PROPERTY      SDL         DATABASE
UnexpectedElement   audit_events  -             -           audit_event
LabelMismatch       employees     -             employee    staff
MissingProperty     employees     left_at       left_at     -
UnexpectedProperty  employees     legacy_email  -           legacy_email
MissingElement      reports_to    -             reports_to  -

gopgql: compared elements, labels and properties only; defaults, constraints and indexes are not covered.
gopgql: property graph "app_graph" has drifted from schema.graphql: 5 findings
exit 2`

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
// any variables from vars, at the compiler's default traversal-depth ceiling.
// It returns the emitted SQL and the ordered bind parameters — proving values
// travel as parameters, never interpolated (SPEC.md §6.2). It never executes the
// query.
func Compile(sdlSrc, query string, vars map[string]any) (Compiled, error) {
	return CompileWithMaxDepth(sdlSrc, query, vars, MaxDepth())
}

// CompileWithMaxDepth is Compile with an explicit traversal-depth ceiling, in
// hops from the root field. It exists because MaxDepth is per-Compiler
// configuration rather than a constant (SPEC.md §6.2): letting a reader move the
// limit and watch one query flip between compiling and being refused shows that
// better than any prose. A negative ceiling is clamped to zero, which permits no
// traversal at all.
func CompileWithMaxDepth(sdlSrc, query string, vars map[string]any, maxDepth int) (Compiled, error) {
	doc, err := sdl.Parse(sdlSrc)
	if err != nil {
		return Compiled{}, err
	}
	sql, args, err := compiler.New(doc, compiler.WithMaxDepth(maxDepth)).Compile(query, vars)
	if err != nil {
		return Compiled{}, err
	}
	return Compiled{SQL: sql, Params: renderParams(args)}, nil
}

// MaxDepth reports the compiler's default traversal-depth ceiling: what the
// playground starts at, and what it falls back to when no ceiling is given.
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

	prior, err := migrate.FoldContent([]string{migrate.InitTables(oldModel), migrate.InitGraph(oldModel)})
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

	tUp, tDown, tChanged := migrate.DeltaTables(prior, newModel)
	gUp, gDown, gChanged := migrate.DeltaGraph(prior, newModel)
	if !tChanged && !gChanged {
		return "-- no schema change between the two SDL revisions", false, nil
	}

	out := ""
	if tChanged {
		out += "-- migrations/tables/0002_delta.sql\n" +
			"-- +goose Up\n" + tUp + "\n-- +goose Down\n" + tDown + "\n"
	}
	if gChanged {
		if out != "" {
			out += "\n"
		}
		out += "-- migrations/graph/0002_delta.sql\n" +
			"-- +goose Up\n" + gUp + "\n-- +goose Down\n" + gDown + "\n"
	}
	return out, true, nil
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
