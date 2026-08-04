// Package playground is the thin driver behind the WASM playground. It runs the
// real gopgql pipeline — sdl parse/validate, generator, migrate, compiler and
// shape — end to end on an editable SDL document, GraphQL query and variables,
// with no JavaScript re-implementation.
//
// Everything it returns is *generated* from the inputs: the goose migration, the
// compiled GRAPH_TABLE SQL with its ordered bind parameters, and — given rows
// somebody else executed — the nested GraphQL response those rows shape into.
//
// It never fabricates query *results*. Rows come from a real PostgreSQL, which
// the browser now has: the playground runs the pinned wasm build of the fork in
// a Web Worker (SPEC.md §8.6) and hands the flat result set back here to be
// shaped. That last step is what makes the playground show gopgql's whole job
// rather than two thirds of it — a GraphQL query in, a GraphQL response out.
//
// Shaping needs no database. `shape` imports `compiler` and `fmt` and nothing
// else, so it sits on the database-free side of the WASM boundary alongside
// sdl, schema, generator, migrate and compiler (SPEC.md §4.1). Only `exec`,
// which owns the pgx connection, is excluded — and executing is precisely the
// step the worker does instead.
//
// It is a normal Go package so it is unit-testable on the host and reused
// verbatim by the js/wasm entry point in cmd/wasm.
package playground

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/sdl"
	"github.com/lega4e/gopgql/shape"
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

// ExampleSeed is the fixture ExampleSDL's generated tables are filled with
// before a compiled query runs against them. A GRAPH_TABLE query over an empty
// database returns zero rows, which demonstrates nothing; these rows are what
// make the Traversal and Multi-pattern results readable.
//
// It is chosen for three default queries at once, all of which read this one
// schema:
//
//   - the chain Alice → Bob → Carol → Dave → Erin is five distinct people, so
//     ExampleQuery's three hops and ExampleDeepQuery's four both find a path
//     whose vertices are all different — which is what the compiler's
//     isomorphism guards require. A shorter chain would satisfy the first and
//     silently return nothing for the second;
//   - the closing edge Dave → Alice gives Alice an incoming follow, which is
//     what ExampleMultiPatternQuery's `followedBy` branch reads. It closes a
//     cycle without shortening the chain: Alice still has exactly one outgoing
//     edge, so the traversal queries still match exactly one path.
//
// The ids are literal so the fixture is deterministic: the generated table
// defaults `id` to gen_random_uuid(), and edges have to name the vertices they
// join.
const ExampleSeed = `INSERT INTO persons (id, name, email) VALUES
  ('a0000000-0000-4000-8000-000000000001', 'Alice', 'alice@example.com'),
  ('a0000000-0000-4000-8000-000000000002', 'Bob',   'bob@example.com'),
  ('a0000000-0000-4000-8000-000000000003', 'Carol', 'carol@example.com'),
  ('a0000000-0000-4000-8000-000000000004', 'Dave',  NULL),
  ('a0000000-0000-4000-8000-000000000005', 'Erin',  'erin@example.com');

INSERT INTO follows (source_id, target_id) VALUES
  ('a0000000-0000-4000-8000-000000000001', 'a0000000-0000-4000-8000-000000000002'),
  ('a0000000-0000-4000-8000-000000000002', 'a0000000-0000-4000-8000-000000000003'),
  ('a0000000-0000-4000-8000-000000000003', 'a0000000-0000-4000-8000-000000000004'),
  ('a0000000-0000-4000-8000-000000000004', 'a0000000-0000-4000-8000-000000000005'),
  ('a0000000-0000-4000-8000-000000000004', 'a0000000-0000-4000-8000-000000000001');`

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

// ExampleDirectivesSeed fills ExampleDirectivesSDL's generated table. It is
// written against the *physical* column names, so `title` is inserted as
// `name` — the same rename the compiled query projects, which is the whole
// point of the scenario.
//
// One row matches ExampleDirectivesVars' "Chain"; the others are there so the
// filter is visibly doing something.
const ExampleDirectivesSeed = `INSERT INTO products (id, sku, name, price, category, vendor) VALUES
  ('b0000000-0000-4000-8000-000000000001', 'CHN-11S', 'Chain',    24.99, 'drivetrain',  'Shimano'),
  ('b0000000-0000-4000-8000-000000000002', 'CST-11S', 'Cassette', 59.50, 'drivetrain',  'Shimano'),
  ('b0000000-0000-4000-8000-000000000003', 'BTL-750', 'Bottle',    8.00, 'accessories', NULL);`

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

// ExampleConstraintsSeed fills ExampleConstraintsSDL's generated tables with
// two employees of one tenant and the edge between them. The first row is the
// one ExampleConstraintsVars selects by natural key.
//
// `role` is supplied on both rows and is inside the generated CHECK; `left_at`
// is left out entirely, so the table-level check comparing it to `joined_at`
// holds. Nothing here supplies a value the column defaults would have provided
// — `joined_at` is given explicitly only so the rendered result is stable
// rather than "whenever you pressed Run".
const ExampleConstraintsSeed = `INSERT INTO employees (id, tenant, email, role, joined_at) VALUES
  ('c0000000-0000-4000-8000-000000000001', 'acme', 'ada@acme.example',   'admin',  '2024-01-15T09:00:00Z'),
  ('c0000000-0000-4000-8000-000000000002', 'acme', 'grace@acme.example', 'member', '2024-03-02T09:00:00Z');

INSERT INTO reports_to (source_id, target_id) VALUES
  ('c0000000-0000-4000-8000-000000000002', 'c0000000-0000-4000-8000-000000000001');`

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

// ExampleInterfaceSeed fills ExampleInterfaceSDL's four generated tables. Both
// implementors of Actor are populated, and both edge tables carry a row, so
// ExampleInterfaceQuery — which matches the shared `actor` label and traverses
// `follows` — returns one row from each: a person following a person, and a bot
// following a person. A seed covering only `persons` would return rows too, and
// would hide the fact that the interface spans two tables.
const ExampleInterfaceSeed = `INSERT INTO persons (id, name, email) VALUES
  ('d0000000-0000-4000-8000-000000000001', 'Alice', 'alice@example.com'),
  ('d0000000-0000-4000-8000-000000000002', 'Bob',   'bob@example.com');

INSERT INTO bots (id, name, vendor) VALUES
  ('d0000000-0000-4000-8000-000000000003', 'Buildbot', 'acme');

INSERT INTO follows (source_id, target_id) VALUES
  ('d0000000-0000-4000-8000-000000000001', 'd0000000-0000-4000-8000-000000000002');

INSERT INTO bot_follows (source_id, target_id) VALUES
  ('d0000000-0000-4000-8000-000000000003', 'd0000000-0000-4000-8000-000000000001');`

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

// Migration parses and validates the SDL and returns the goose migrations a
// first generation emits from it: the tables, then the property graph over them.
//
// They are returned as one annotated document because the playground has one
// output pane, but they are separate files, applied in the order shown — no
// migration mixes table DDL with property-graph DDL (gopgql#38, design D2).
func Migration(sdlSrc string) (string, error) {
	doc, err := sdl.Parse(sdlSrc)
	if err != nil {
		return "", err
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		return "", err
	}
	return renderSequence(migrate.Plan(nil, m, "init", 1, migrate.Halves{})), nil
}

// renderSequence annotates a planned generation as one document: each file under
// the name gopgql would write it as, in the order goose applies them.
func renderSequence(planned []migrate.Migration) string {
	var b strings.Builder
	for i, m := range planned {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "-- migrations/%s\n", m.Filename())
		b.WriteString(m.Content())
	}
	return b.String()
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

// Compiled is the output of Compile: the GRAPH_TABLE SQL, its ordered bind
// values, and a human-readable rendering of them. All three are pure functions
// of the inputs — no database is consulted (SPEC.md §6.1).
type Compiled struct {
	// SQL is the compiled GRAPH_TABLE query, including any $n placeholders.
	SQL string
	// Args are the ordered bind values the placeholders refer to: $1 is
	// Args[0], $2 is Args[1], and so on. They are compiler.Compiled.Args
	// carried through unconverted — same name, same values — so a caller that
	// intends to *execute* the query has something to bind. It is nil when the
	// query carries no variables, and nil on every error path.
	Args []any
	// Params renders Args for a reader, e.g. "$1 = Alice", or a note when the
	// query carries none. It is the display form of Args, not a second fact:
	// the two always describe the same values.
	Params string
	// Projection describes how the flat rows the SQL returns regroup into the
	// nested GraphQL response. It is compiler.Compiled.Projection carried
	// through unconverted, and it is what Shape consumes: a caller that has
	// already compiled does not have to compile a second time to shape.
	//
	// It is the zero Projection on every error path.
	Projection compiler.Projection
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
	// CompileQuery rather than Compile: the two emit the same SQL and the same
	// bind values, and the former also hands back the projection the response
	// shaper needs. Compile is the SPEC.md §6.1 two-value contract and is left
	// alone; there is nothing to gain here by discarding a value and then
	// recompiling to get it back.
	cq, err := compiler.New(doc, compiler.WithMaxDepth(maxDepth)).CompileQuery(query, vars)
	if err != nil {
		return Compiled{}, err
	}
	return Compiled{
		SQL:        cq.SQL,
		Args:       cq.Args,
		Params:     renderParams(cq.Args),
		Projection: cq.Projection,
	}, nil
}

// Result is one execution's flat result set on its way back from the database:
// the output column names, and one positional value list per row.
//
// It is positional rather than a list of objects because that is the honest
// shape of what PostgreSQL returned. The compiler gives every projected column
// a unique name, but a result read as objects would silently collapse two
// columns that ever did share one — and the playground's whole claim is that
// what it shows is what happened.
//
// The JSON tags are the wire form the page posts across the WASM boundary
// (design D5): the Go module and the PostgreSQL module have separate linear
// memories, so the rows cross as text, exactly as the bind values do on the way
// out.
type Result struct {
	// Columns are the output column names, in result order.
	Columns []string `json:"columns"`
	// Rows are the values, one list per row, positionally aligned to Columns.
	Rows [][]any `json:"rows"`
}

// Shape regroups a compiled query's flat rows into the nested GraphQL response
// — the last step of gopgql's job, and the one the playground used to stop
// short of.
//
// proj comes from Compiled.Projection. The rows are whatever executed the SQL:
// pgx in the integration suites, PGlite in the browser. Shape does not care
// which, and touches no database itself.
//
// A row whose length disagrees with Columns is refused rather than padded. It
// would mean the result set and the column list came from different executions,
// and a response shaped from that would be wrong in a way no reader could see.
//
// shape.Rows normalises every leaf on the way through and can fail doing it —
// a non-finite Float has no JSON form (design D3, D5) — so that error is
// returned rather than swallowed.
func Shape(proj compiler.Projection, res Result) (map[string]any, error) {
	if proj.Root == nil {
		return nil, errors.New("playground: no projection to shape the rows with")
	}
	flat := make([]map[string]any, 0, len(res.Rows))
	for i, row := range res.Rows {
		if len(row) != len(res.Columns) {
			return nil, fmt.Errorf(
				"playground: result row %d has %d values but the result declares %d columns",
				i+1, len(row), len(res.Columns))
		}
		obj := make(map[string]any, len(res.Columns))
		for j, col := range res.Columns {
			obj[col] = row[j]
		}
		flat = append(flat, obj)
	}
	return shape.Rows(proj, flat)
}

// ShapeSQLSide reads the response an SQL-side-shaped statement returned.
//
// That statement's result is one row of one `response` column, already cast to
// text by the compiler, holding the whole nested response as JSON. Nothing here
// re-groups anything: PostgreSQL did the grouping, and this decodes what it
// produced back into the same Go value the Go-side path builds, so the one
// encoder writes both (design D3).
//
// The strictness is deliberate and mirrors exec.queryShaped. A result with a
// different column count or row count did not come from an SQL-side statement,
// and guessing which value in it was the response would be exactly the kind of
// quiet reinterpretation the milestone rules out.
func ShapeSQLSide(proj compiler.Projection, res Result) (map[string]any, error) {
	if proj.Root == nil {
		return nil, errors.New("playground: no projection to decode the response with")
	}
	if len(res.Columns) != 1 {
		return nil, fmt.Errorf(
			"playground: an SQL-side shaped query returns one column, got %d", len(res.Columns))
	}
	if len(res.Rows) != 1 {
		return nil, fmt.Errorf(
			"playground: an SQL-side shaped query returns one row, got %d", len(res.Rows))
	}
	if len(res.Rows[0]) != 1 {
		return nil, fmt.Errorf(
			"playground: the result row has %d values but the result declares 1 column",
			len(res.Rows[0]))
	}
	// The column is text and is read as text. Decoding it as generic JSON first
	// would turn 19.90 into float64 19.9 and lose, on this path only, a digit
	// the Go-side path keeps — the same trap exec.queryShaped names.
	response, ok := res.Rows[0][0].(string)
	if !ok {
		return nil, fmt.Errorf(
			"playground: the response column is %T, not the text the compiler casts it to",
			res.Rows[0][0])
	}
	return shape.Decode(proj, response)
}

// ShapeJSON compiles the query and shapes res into the nested GraphQL response,
// rendered as indented JSON.
//
// It recompiles rather than taking a projection because it is the WASM entry
// point's shape: a projection is a Go value and cannot cross into JavaScript,
// so the page sends back the same three inputs it compiled with and the
// projection is re-derived here. Compilation is pure and touches no database,
// so re-deriving it costs nothing and cannot disagree with the first pass.
//
// What comes back is the response *payload* — the root field and its objects —
// which is exactly what exec.Query returns to a Go caller. It is deliberately
// not wrapped in a GraphQL `{"data": …}` envelope: gopgql is a library that
// compiles and shapes, not a server that answers requests, and inventing an
// envelope it does not produce would be the one fabricated panel on a page
// whose whole claim is that nothing is.
func ShapeJSON(sdlSrc, query string, vars map[string]any, maxDepth int, res Result) (string, error) {
	compiled, err := CompileWithMaxDepth(sdlSrc, query, vars, maxDepth)
	if err != nil {
		return "", err
	}
	shaped, err := Shape(compiled.Projection, res)
	if err != nil {
		return "", err
	}
	out, err := indent(shaped)
	if err != nil {
		return "", err
	}
	return out, nil
}

// MaxDepth reports the compiler's default traversal-depth ceiling: what the
// playground starts at, and what it falls back to when no ceiling is given.
func MaxDepth() int { return compiler.DefaultMaxDepth }

// Shaped is the output of CompileWithShaping: what one shaping strategy makes
// of a query (SPEC.md §7 → M8, design D8).
type Shaped struct {
	// Strategy names the strategy, as compiler.Shaping renders it.
	Strategy string
	// SQL is the statement that strategy emits.
	SQL string
	// Args are the ordered bind values the statement's placeholders refer to,
	// carried through from compiler.Compiled.Args exactly as Compiled.Args is.
	// They are identical under both strategies, and they are here because the
	// caller executes the statement: a playground that could only show the SQL
	// had no use for them.
	Args []any
	// Params renders Args for a reader. They are identical under both
	// strategies — the MATCH pattern and its predicates do not change, only the
	// projection around them does.
	Params string
	// ResultShape describes the result set the statement asks the database for:
	// k flat columns assembled in Go, or one column assembled in PostgreSQL.
	// It is derived from the projection, with no database consulted, and it is
	// the point of the whole milestone made visible.
	ResultShape string
	// Projection describes how the statement's result becomes the nested
	// response. It is the *same* projection under both strategies — that is
	// what makes the two comparable at all — and it is what shapes the rows
	// under Go-side shaping and decodes the JSON under SQL-side.
	Projection compiler.Projection
}

// CompileWithShaping compiles a query under one shaping strategy and returns
// the SQL it emits together with the bind values to run it with.
//
// It is what the playground's shaping toggle runs, and it is honest work for a
// browser to do: choosing a strategy changes what the *compiler* emits, and the
// compiler is pure (SPEC.md §6.1).
//
// It used to stop there, because the playground had no database and the panel
// could only show two SQL texts (design D8, as first written). gopgql#31 has
// since merged and put a real PostgreSQL in the page, so both statements can be
// executed and their responses compared — which is what ShapeParity does with
// what this returns.
func CompileWithShaping(sdlSrc, query string, vars map[string]any, sqlSide bool) (Shaped, error) {
	strategy := compiler.GoSide
	if sqlSide {
		strategy = compiler.SQLSide
	}

	doc, err := sdl.Parse(sdlSrc)
	if err != nil {
		return Shaped{}, err
	}
	cq, err := compiler.New(doc, compiler.WithShaping(strategy)).CompileQuery(query, vars)
	if err != nil {
		return Shaped{}, err
	}
	return Shaped{
		Strategy:    strategy.String(),
		SQL:         cq.SQL,
		Args:        cq.Args,
		Params:      renderParams(cq.Args),
		ResultShape: resultShape(cq),
		Projection:  cq.Projection,
	}, nil
}

// resultShape describes the result set a compiled query asks for. Under Go-side
// shaping that is one column per projected field plus a hidden key column per
// level, one row per matched path; under SQL-side shaping it is a single
// `response` column of one row, whatever the fan-out.
func resultShape(cq *compiler.Compiled) string {
	if cq.Shaping == compiler.SQLSide {
		return "1 column (response), 1 row — PostgreSQL assembles the nested response"
	}
	cols, levels := countColumns(cq.Projection.Root)
	return fmt.Sprintf("%d columns across %d level(s), one row per matched path — Go assembles the nested response",
		cols, levels)
}

// countColumns totals a projection's output columns and levels. Every level
// contributes its scalar fields plus the hidden key column shape groups by.
func countColumns(sel *compiler.Selection) (cols, levels int) {
	cols, levels = len(sel.Fields)+1, 1
	for _, child := range sel.Children {
		c, l := countColumns(child)
		cols, levels = cols+c, levels+l
	}
	return cols, levels
}

// ExampleShapingQuery is the query the shaping tab compiles under both
// strategies. It fans out twice at one level, which is where the two strategies
// differ most visibly: the flat statement LEFT JOINs the branches and a parent
// with m and n children yields m×n rows, while the SQL-side statement aggregates
// each branch to an array before the join, so that cross-product never forms.
const ExampleShapingQuery = `{ persons(name: $n) { name follows { name } followedBy { name } } }`

// ExampleShapingSeed fills ExampleSDL's tables for the shaping tab.
//
// It is a separate seed from ExampleSeed on the same schema, because the two
// scenarios want opposite things from the data. ExampleSeed is a chain, so the
// traversal queries match exactly one path — Alice has exactly one outgoing
// edge, deliberately. The shaping tab wants the opposite: ExampleShapingQuery
// fans out twice at one level, and with one edge on each branch the
// cross-product the Go-side statement forms is 1×1, which looks like no
// cross-product at all.
//
// So Alice follows two people and is followed by two others, all four distinct
// from her and from each other — the compiler's isomorphism guard would drop a
// combination that reused a vertex. Go-side, that is 2×2 = 4 flat rows for one
// person; SQL-side it is 1 row. Both shape into the same response, which is the
// entire claim of the milestone and now the thing the panel demonstrates rather
// than asserts.
const ExampleShapingSeed = `INSERT INTO persons (id, name, email) VALUES
  ('e0000000-0000-4000-8000-000000000001', 'Alice', 'alice@example.com'),
  ('e0000000-0000-4000-8000-000000000002', 'Bob',   'bob@example.com'),
  ('e0000000-0000-4000-8000-000000000003', 'Carol', 'carol@example.com'),
  ('e0000000-0000-4000-8000-000000000004', 'Dave',  NULL),
  ('e0000000-0000-4000-8000-000000000005', 'Erin',  'erin@example.com');

INSERT INTO follows (source_id, target_id) VALUES
  ('e0000000-0000-4000-8000-000000000001', 'e0000000-0000-4000-8000-000000000002'),
  ('e0000000-0000-4000-8000-000000000001', 'e0000000-0000-4000-8000-000000000003'),
  ('e0000000-0000-4000-8000-000000000004', 'e0000000-0000-4000-8000-000000000001'),
  ('e0000000-0000-4000-8000-000000000005', 'e0000000-0000-4000-8000-000000000001');`

// Parity is the outcome of running one query under both shaping strategies:
// each response, and whether they are the same one.
type Parity struct {
	// GoJSON and SQLJSON are the two responses, indented for display.
	GoJSON, SQLJSON string
	// Identical reports whether the two canonical encodings are equal bytes.
	Identical bool
	// Bytes is the length of the canonical encoding. It is the Go-side one, but
	// when Identical it is equally the SQL-side one, and when they differ the
	// number is not the point.
	Bytes int
}

// ShapeParity shapes both strategies' result sets and reports whether they
// produce the same response.
//
// It is test/parity's assertion, executed in the page instead of in CI: the
// same query, compiled twice, run against one database, shaped by each path,
// and compared. The comparison is shape.Encode of each — the definition
// byte-identity rests on (design D3) — not the indented text it also returns,
// which exists only to be read.
//
// What that comparison does *not* claim is that PostgreSQL's own bytes match
// gopgql's. They do not: json_build_object spaces its colons and emits keys in
// argument order, which encoding/json does neither of. The SQL-side JSON is
// decoded into the same Go value the Go-side path builds and re-encoded by the
// one encoder, and that is why the bytes agree. A caller showing this to a
// reader has to say so, or it implies a stronger guarantee than the milestone
// makes.
//
// Both legs are shaped from a projection re-derived here, and both re-derive it
// from the same SDL, query and variables. Compilation is pure, so neither can
// disagree with the compile that produced the SQL the caller ran.
func ShapeParity(sdlSrc, query string, vars map[string]any, goRes, sqlRes Result) (Parity, error) {
	goCompiled, err := CompileWithShaping(sdlSrc, query, vars, false)
	if err != nil {
		return Parity{}, err
	}
	sqlCompiled, err := CompileWithShaping(sdlSrc, query, vars, true)
	if err != nil {
		return Parity{}, err
	}

	goResp, err := Shape(goCompiled.Projection, goRes)
	if err != nil {
		// ST1005 exempts a proper noun, and "Go" is one: it is the language's
		// name, and "Go-side" is what this codebase calls the strategy
		// everywhere else. staticcheck cannot tell it from a capitalised
		// sentence, but lowercasing it here would leave "go-side" standing
		// next to the "SQL-side" staticcheck accepts as an acronym.
		//nolint:staticcheck // ST1005: "Go" is a proper noun.
		return Parity{}, fmt.Errorf("Go-side: %w", err)
	}
	sqlResp, err := ShapeSQLSide(sqlCompiled.Projection, sqlRes)
	if err != nil {
		return Parity{}, fmt.Errorf("SQL-side: %w", err)
	}

	goBytes, err := shape.Encode(goResp)
	if err != nil {
		//nolint:staticcheck // ST1005: proper noun, as above.
		return Parity{}, fmt.Errorf("Go-side: %w", err)
	}
	sqlBytes, err := shape.Encode(sqlResp)
	if err != nil {
		return Parity{}, fmt.Errorf("SQL-side: %w", err)
	}

	goJSON, err := indent(goResp)
	if err != nil {
		return Parity{}, err
	}
	sqlJSON, err := indent(sqlResp)
	if err != nil {
		return Parity{}, err
	}
	return Parity{
		GoJSON:    goJSON,
		SQLJSON:   sqlJSON,
		Identical: bytes.Equal(goBytes, sqlBytes),
		Bytes:     len(goBytes),
	}, nil
}

// indent renders a response for a reader. It is never what identity is judged
// on — shape.Encode is — so this is free to be as legible as it likes.
func indent(resp map[string]any) (string, error) {
	out, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return "", fmt.Errorf("playground: render the shaped response as JSON: %w", err)
	}
	return string(out), nil
}

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

// Delta generates the migrations a revision of the SDL emits. It plans the first
// generation for oldSDL, folds those migrations back into a schema — running the
// real fold interpreter, exactly as `gopgql generate` does — and plans the next
// generation against the schema built from newSDL (SPEC.md §7 → M2). changed
// reports whether the revision produced any schema change.
//
// The result is a run of consecutive files, numbered on from the history: the
// graph comes down, the tables move, the graph goes back up.
func Delta(oldSDL, newSDL string) (delta string, changed bool, err error) {
	oldDoc, err := sdl.Parse(oldSDL)
	if err != nil {
		return "", false, err
	}
	oldModel, err := generator.Build(oldDoc, "")
	if err != nil {
		return "", false, err
	}

	history := migrate.Plan(nil, oldModel, "init", 1, migrate.Halves{})
	contents := make([]string, len(history))
	for i, m := range history {
		contents[i] = m.Content()
	}
	prior, err := migrate.FoldContent(contents)
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

	planned := migrate.Plan(prior, newModel, "delta", len(history)+1, migrate.Halves{})
	if len(planned) == 0 {
		return "-- no schema change between the two SDL revisions", false, nil
	}
	return renderSequence(planned), true, nil
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
