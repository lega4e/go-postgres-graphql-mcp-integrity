# gopgql — Specification

**Version:** 1.0
**Status:** Draft, pre-implementation
**Target:** PostgreSQL 19 (SQL/PGQ), Go 1.23+

-----

## 1. Purpose

`gopgql` is a standalone Go library that treats a single annotated GraphQL SDL document as the source of truth for both halves of a property-graph application:

1. **Schema generation** — the SDL compiles to PostgreSQL DDL: vertex tables, edge tables, indexes, and the `CREATE PROPERTY GRAPH` statement that maps them, emitted as versioned `goose` migrations.
1. **Query compilation** — GraphQL queries against that same SDL compile to SQL/PGQ `GRAPH_TABLE ... MATCH ... COLUMNS` statements, executed over `pgx`.

Because one document drives both, the query mapping and the physical schema cannot drift by construction. A conformance check against the `pg_propgraph_*` catalogs guards the remaining gap (out-of-band database changes).

`gopgql` is a library, not a server. It exposes compilation and generation as pure functions; execution is a thin optional layer over `pgx`.

### 1.1 Non-goals

- Not a GraphQL server. No HTTP layer, no subscriptions, no resolver runtime.
- Not a mutation engine. SQL/PGQ property graphs are read-only views; `gopgql` compiles queries only.
- Not a general graph database abstraction. It targets PostgreSQL SQL/PGQ exclusively.
- Not a migration runner. It emits `goose`-format files; applying them is `goose`’s job.

-----

## 2. Context and constraints

### 2.1 SQL/PGQ status

SQL/PGQ was committed to PostgreSQL core in March 2026 and first shipped publicly in **PostgreSQL 19 Beta 1** (June 4, 2026); **Beta 2** followed July 16, 2026 with several SQL/PGQ fixes. GA is expected around September/October 2026. It is built into core — no extension install.

`gopgql` pins `postgres:19beta2` for all integration testing and re-baselines on each beta/RC bump.

### 2.2 Hard limitations of PG19 SQL/PGQ

These are absolute and shape the entire design:

- **Read-only.** A property graph is a view-like object; no mutation through it.
- **Fixed-depth only.** No quantified/variable-length paths (`*`, `+`, `{m,n}`), no shortest path, no path variables.
- **Multi-pattern `MATCH` parses but does not execute.** Comma-separated path patterns must be split into separate `GRAPH_TABLE` calls joined on projected IDs.
- **KEY columns are not queryable properties** unless also listed in `PROPERTIES`.
- **Same label across multiple tables** requires identical property count, name, and type.
- **No `*` in `COLUMNS`** — every projected column must be enumerated.
- **Edge isomorphism is not enforced** — self-matches must be excluded explicitly.
- **Rewritten to relational joins.** `GRAPH_TABLE` offers no performance advantage over equivalent hand-written joins; it is a syntactic layer. All ordinary indexing rules apply.

### 2.3 Prior art

No tool exists that generates SQL/PGQ property-graph DDL from GraphQL SDL, and no tool compiles GraphQL to `GRAPH_TABLE`. `gopgql` is first in both directions. The directive vocabulary borrows from the Neo4j GraphQL Library (`@node`, `@relationship(type:, direction:)`) and Dgraph (`@hasInverse`); the escape-hatch pattern borrows from `graphql-to-sql`’s `@sql`. No third-party code is vendored — `graphql-to-sql` is GPL-3.0 and MySQL-only.

-----

## 3. Locked decisions

|# |Decision             |Choice                                                                                                                                            |
|--|---------------------|--------------------------------------------------------------------------------------------------------------------------------------------------|
|1 |Mapping source       |Directive-driven SDL, schema-first. No live DB needed to build a schema.                                                                          |
|2 |Deliverables         |Both a schema generator and a query compiler, from one SDL.                                                                                       |
|3 |Transitive traversal |**Reject beyond configured depth.** Every emitted query is pure `GRAPH_TABLE`. No `WITH RECURSIVE` emitter.                                       |
|4 |Result shaping       |Go-side regrouping first; SQL-side `json_agg` added in a later milestone and benchmarked against it.                                              |
|5 |Migration application|Versioned migrations.                                                                                                                             |
|6 |Migration content    |**Delta migrations.** Prior state is folded from `gopgql`’s own earlier migration files — no sidecar state artifact, no database at generate time.|
|7 |Migration format     |`goose` single-file, `-- +goose Up` / `-- +goose Down`.                                                                                           |
|8 |SDL expressiveness   |**Full** as destination (`@check`, `@default`, `@key(fields:)`), reached incrementally — minimal in M1, widening each milestone.                  |
|9 |Test harness         |**godog** BDD over `testcontainers-go`, against real `postgres:19beta2`.                                                                          |
|10|GitHub Pages         |Docs site with embedded WASM playground. Branch mode + PR previews.                                                                               |

### 3.0 Split migrations

No migration ever contains both table DDL and property-graph DDL. One edit of the
SDL emits a **run of consecutive single-purpose migrations** into one directory,
recorded in the one `goose_db_version` table:

```
0001_init_tables.sql             CREATE TABLE …
0002_init_graph.sql              CREATE PROPERTY GRAPH …
0003_add_email_graph_down.sql    DROP PROPERTY GRAPH IF EXISTS …
0004_add_email_tables.sql        ALTER TABLE …
0005_add_email_graph.sql         CREATE PROPERTY GRAPH …
```

One slug per generation, shared by every file in it, plus a suffix saying what
each file does. The suffix is for humans: nothing is recorded inside a migration
and nothing is read back out of one — what a migration does is what its SQL does.
Each file's `Down` is the plain inverse of its own `Up`, so rolling a generation
back means undoing its migrations newest first.

This makes two things representable that were not:

- a property graph over tables gopgql does not manage;
- an SDL that describes **part** of a database — the slice surfaced as a graph.
  With the tables half off, absence from the SDL says nothing about the database,
  and nothing it does not mention is ever dropped or altered. With the tables
  half on, gopgql manages those tables and a column absent from the SDL is
  removed.

#### The order is the numbering

Two ordering constraints come from PostgreSQL, not from gopgql: tables must exist
before the graph that references them, and a live property graph must come down
before the columns it exposes can change. Both are satisfied **structurally**. A
generation is emitted graph-teardown → tables → graph-build, numbered
consecutively, so a `CREATE PROPERTY GRAPH` is always immediately preceded by the
table DDL of its *own* generation and always preceded by the drop of the graph the
generation before it built.

`gopgql migrate` is therefore goose's ordinary forward apply over the directory —
no interleaving, no version walk, no ordering logic in gopgql at all. `goose up`
against an empty database replays the whole history correctly because there is no
other order in which it could apply the files. Nothing has to remember the
constraint, and there is no code that could forget it.

**A generation is not atomic.** goose runs each file in its own transaction, so an
interrupted `goose up` can stop between the teardown and the rebuild, leaving a
database whose tables have moved and which has no property graph. Re-running
`gopgql migrate` (or `goose up`) continues from where it stopped; queries against
the graph fail loudly in the meantime rather than returning wrong rows.
Deployments that read the graph should finish migrating before serving.

#### Turning a half off

`--no-tables` and `--no-graph` scope **generation**, never application: applying
is always the whole directory in version order, so no flag can cause part of an
applied history to be skipped. Both together is an error — it asks for nothing.

**The flags scope a directory's first generation; after that the directory's own
history decides, and a flag that contradicts it is an error.** `--no-graph`
against a history that contains a `CREATE PROPERTY GRAPH`, or `--no-tables`
against a history that created tables, is refused at generate time with nothing
written. The evidence is the SQL in the directory, folded the way generation folds
it anyway, so there is nothing recorded that could drift out of agreement with it.

Turning a half off is not a way to delete anything. To drop the property graph,
generate from a desired schema that declares no graph: that generation emits the
`_graph_down` teardown and no rebuild, so the drop is recorded in the history and
reviewable in the diff.

There is **no other layout**: no detection of, and no fallback to, the original
single combined migration or the per-half subdirectories an earlier version of
this design used.

### 3.1 Rationale notes

**On rejecting deep traversal (3).** PG19 cannot express variable-length paths. The alternatives were a `WITH RECURSIVE` emitter (a second, structurally different code path over raw edge tables) or silent truncation. Rejecting keeps every emitted query a pure `GRAPH_TABLE`, keeps the library honest about PostgreSQL’s actual ceiling, and leaves a clean upgrade path when native quantified paths land.

**On folding migrations (6).** Generating a migration requires knowing the target state; therefore the resulting state is known by construction at generation time. Since `gopgql` generates every migration itself, those files are a small canonical statement set — folding them in memory reconstructs current state without a database or a separate snapshot file. This is only viable because `gopgql` owns generation end to end; tools that must tolerate hand-written migrations (Atlas, Prisma) need shadow-database replay instead.

The assumption this rests on: **no one hand-edits a generated migration or alters the database out of band.** The conformance check (M7) is the guard.

-----

## 4. Architecture

```
SDL (annotated GraphQL)
  │
  ├─→ [sdl] parse + validate ────────────→ *ast.Schema + directive model
  │                                             │
  │                                  ┌──────────┴──────────┐
  │                                  ▼                     ▼
  │                          [generator]              [compiler]
  │                          desired schema           GraphQL query
  │                                │                       │
  │                                ▼                       ▼
  │                          [migrate]               GRAPH_TABLE SQL
  │                          fold prior              + bind params
  │                          migrations,                   │
  │                          diff, emit                    ▼
  │                                │                 [exec] pgx
  │                                ▼                       │
  │                          NNNN_name.sql                 ▼
  │                          (goose)                 [shape] nested JSON
```

### 4.1 Packages

Single Go module, `github.com/<owner>/gopgql`:

|Package     |Responsibility                                                                                         |
|------------|-------------------------------------------------------------------------------------------------------|
|`sdl`       |Parse and validate SDL via `vektah/gqlparser/v2`; read directives into a typed mapping model.          |
|`schema`    |The in-memory schema model (tables, columns, indexes, graph elements). Shared by generator and migrate.|
|`generator` |Model → DDL statements (`CREATE TABLE`, indexes, `CREATE PROPERTY GRAPH`).                             |
|`migrate`   |Fold prior goose migrations → model; diff against desired; emit next migration.                        |
|`compiler`  |GraphQL operation → `GRAPH_TABLE` SQL + ordered bind params.                                           |
|`shape`     |Flat rows → nested GraphQL response (Go-side; SQL-side added M8).                                      |
|`exec`      |Thin `pgx` execution helper: compiled query → rows → shaped response; opens the read-only pool.        |
|`conform`   |Reflect the live property graph out of `pg_propgraph_*`; diff it against the generated model; report typed findings.|
|`mcp`       |Model Context Protocol server: GraphQL introspection over the SDL, plus a query tool over `exec`.      |
|`cmd/gopgql`|CLI: `generate` (SDL → migration), `migrate` (generate + apply) and `conform` (drift check). `compile` is not implemented yet.|
|`cmd/gopgql-mcp`|The MCP server binary: one SDL, one DSN, stdio or streamable-HTTP transport.                       |

`sdl`, `schema`, `generator`, `migrate`, `compiler` and `shape` have **no database dependency** and compile to WASM. `exec` imports `pgx`; `conform` reflects a live catalog and so imports it too; `mcp` and `cmd/gopgql-mcp` build on `exec`. None of those four are part of the WASM surface, and nothing on the WASM side may import them — the boundary is what makes the browser playground possible at all.

`shape` is on the WASM side, and the distinction is worth stating because it is easy to get backwards. Shaping is often described together with executing — `exec` does both, in that order — which makes it sound as though shaping needs a database. It does not: `shape` imports `compiler` and `fmt`, takes a projection and a slice of rows, and returns a nested response. *Executing* needs `pgx`. So the browser can do everything except the middle step, and the playground does exactly that — it compiles here, executes in a Web Worker running the pinned wasm PostgreSQL (§8.6), and shapes the rows that come back here. Placing `shape` on the database side would have cost the playground the one thing that makes it a demonstration of gopgql rather than of a SQL generator.

`conform` reflects **into `schema.Schema`**, the same model `generator` builds from SDL, so a drift check is a comparison of two values of one type rather than a model-versus-database special case. That is also why it can sit outside the WASM surface without splitting the model: the shared type is on the database-free side, and only the reflection is not. The playground therefore shows the SDL half of the comparison — the generated `CREATE PROPERTY GRAPH` — beside a recorded report, and says which is which.

### 4.2 Dependencies

|Purpose               |Package                                                           |
|----------------------|------------------------------------------------------------------|
|GraphQL parse/validate|`github.com/vektah/gqlparser/v2`                                  |
|PostgreSQL driver     |`github.com/jackc/pgx/v5`                                         |
|Migration runner      |`github.com/pressly/goose/v3`                                     |
|BDD                   |`github.com/cucumber/godog`                                       |
|Containers            |`github.com/testcontainers/testcontainers-go` + `modules/postgres`|
|MCP server/client     |`github.com/modelcontextprotocol/go-sdk`                          |

-----

## 5. SDL directive reference

Directives are introduced progressively (§7). This is the full destination surface.

```graphql
# --- M1: graph structure (widened to INTERFACE in M4) ---
directive @node(label: String!, table: String) on OBJECT | INTERFACE
directive @relationship(
  type: String!          # edge label
  direction: OUT | IN    # relative to the declaring type
  table: String          # edge table name; derived from type if omitted
) on FIELD_DEFINITION
directive @hasInverse(field: String!) on FIELD_DEFINITION
directive @ignore on FIELD_DEFINITION      # present in GraphQL, absent from DB

# --- M6: moderate expressiveness ---
directive @column(name: String, type: String) on FIELD_DEFINITION
directive @index(name: String, using: String) on FIELD_DEFINITION | OBJECT
directive @unique on FIELD_DEFINITION

# --- M7: full expressiveness (implemented) ---
directive @default(value: String!) on FIELD_DEFINITION
directive @check(expr: String!) on FIELD_DEFINITION | OBJECT
directive @key(fields: [String!]!) on OBJECT     # natural key, alongside id
directive @renamedFrom(name: String!) on OBJECT | FIELD_DEFINITION  # rename hint
```

Every directive above is implemented. The four added in M7 carry semantics worth stating precisely, because each is easy to read as something slightly different:

- **`@default(value:)`** and **`@check(expr:)`** are **raw SQL, emitted verbatim** — `@default(value: "now()")` reaches the DDL as `DEFAULT now()`. gopgql never parses them; an invalid expression is PostgreSQL's error when the migration is applied, which is the right place and the right message. They therefore live in the *column* namespace: a check on a field mapped with `@column(name: "left_at")` writes `left_at`, not the GraphQL field name. This is a deliberate escape hatch on the same footing as `graphql-to-sql`'s `@sql` (§2.3), and it is defensible only because whoever writes the SDL already owns the schema. It would be indefensible for user input.
- **`@check`** may appear once per field (column-level) and any number of times on a type (table-level, for a constraint spanning columns). Every check is emitted **named** — `<table>_<column>_check`, `<table>_check_<n>` — because a later delta drops a constraint by name, and an anonymous one would first have to be looked up in a live database.
- **`@key(fields:)`** is a **natural key alongside the surrogate `id`, not a replacement for it.** The table keeps `id uuid PRIMARY KEY`, gains `CONSTRAINT <table>_key UNIQUE (…)` over the named columns, and lists those columns in the property graph's `KEY (...)` clause so a `MATCH` can select a vertex by its data. Edge tables continue to reference `id`. The surrogate key is load-bearing in the compiler (three projection sites) and in `shape`'s parent dedup; making a natural key *the* identity is a different, larger change and is recorded in §9 rather than smuggled into M7.
- **`@renamedFrom(name:)`** is a **hint, never an inference.** A differ that sees one name disappear and another appear cannot tell a rename from a drop-and-add, and guessing wrong destroys the rows one way or loses them the other — so nothing is inferred, and without the hint the old drop-and-add behaviour stands. Its value is the previous **GraphQL** name (a type name on `OBJECT`, a field name on `FIELD_DEFINITION`); the generator derives candidate *physical* names from it, and the differ accepts one only when the folded prior state actually holds it. A hint naming something the prior state does not have emits nothing — which is what lets the same SDL, hint included, keep generating cleanly after its rename has landed. A hint naming something the SDL *still declares* is an error: that is two objects, not one moved one.

### 5.1 Default scalar mapping

|GraphQL                   |PostgreSQL        |
|--------------------------|------------------|
|`Int`                     |`integer`         |
|`Float`                   |`double precision`|
|`String`                  |`text`            |
|`Boolean`                 |`boolean`         |
|`ID`                      |`uuid`            |
|`DateTime` (custom scalar)|`timestamptz`     |
|`JSON` (custom scalar)    |`jsonb`           |
|`T!`                      |`NOT NULL`        |
|`[T!]!` of scalar         |`T[]`             |

Overridable per field via `@column(type:)` from M6.

### 5.2 Worked example

```graphql
type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}
```

Generates:

```sql
CREATE TABLE persons (
    id    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name  text NOT NULL,
    email text
);

CREATE TABLE follows (
    source_id uuid NOT NULL REFERENCES persons (id),
    target_id uuid NOT NULL REFERENCES persons (id),
    PRIMARY KEY (source_id, target_id)
);
CREATE INDEX follows_target_idx ON follows (target_id);   -- reverse traversal

CREATE PROPERTY GRAPH app_graph
  VERTEX TABLES (
    persons LABEL person PROPERTIES (id, name, email)      -- key re-listed
  )
  EDGE TABLES (
    follows SOURCE KEY (source_id) REFERENCES persons (id)
            DESTINATION KEY (target_id) REFERENCES persons (id)
            LABEL follows PROPERTIES (source_id, target_id)
  );
```

Note `@hasInverse` pairs the two fields onto **one** physical edge table.

### 5.3 Generator invariants

The generator must guarantee, and the test suite must assert:

1. Every `KEY` / `SOURCE KEY` / `DESTINATION KEY` column also appears in that element’s `PROPERTIES` list.
1. Every edge table has an index on its destination key column.
1. Labels and identifiers colliding with SQL keywords are double-quoted.
1. Vertex and edge table aliases are unique within a graph; self-referential edges get an explicit `AS` alias.
1. When one label spans multiple tables, property lists are aligned by count, name, and type — with `col AS name` renames emitted as needed. (Reached in M4; interface fields carry identical names and types across implementors by GraphQL's own rules, so no rename is needed for that case.)

-----

## 6. Compiler behaviour

### 6.1 Compilation contract

```go
func (c *Compiler) Compile(op string, vars map[string]any) (sql string, args []any, err error)
```

Pure: no database contact. Mirrors `neo4j-graphql-java`’s `(cypher, params)` shape.

### 6.2 Rules

- **One `GRAPH_TABLE` per root field.** Nested selections extend the `MATCH` pattern, they do not spawn additional queries — N+1 is avoided by construction.
- **Values are bind parameters.** Never interpolated. Ordered `[]any` returned alongside the SQL.
- **Identifiers are never parameters.** Graph names, labels, and property names are validated against the loaded schema (allowlist) and quoted via `pgx.Identifier{...}.Sanitize()`.
- **Depth limit.** Selection nesting beyond `MaxDepth` (default 3) returns a typed `*DepthExceededError` at compile time.
- **Multi-pattern workaround.** Where a query would need comma-separated patterns, emit separate `GRAPH_TABLE` calls joined on projected IDs in the outer query.
- **Isomorphism guards.** PostgreSQL enforces neither vertex nor edge isomorphism (§2.2), so the compiler emits `vi.id <> vj.id` for every pair of vertex positions whose underlying tables intersect — the only pairs that can bind the same row. Positions over disjoint tables get none.

### 6.4 Interfaces (resolved in M4)

A GraphQL interface implemented by `@node` types makes their tables one queryable position. Two mappings, chosen by whether the interface itself carries `@node`:

|Interface                   |Graph mapping                                                                                                        |Emitted pattern       |
|----------------------------|---------------------------------------------------------------------------------------------------------------------|----------------------|
|`interface A @node(label:)`|**Shared label** — every implementor's vertex table carries that label with an aligned property list (§5.3 invariant 5).|`(v0 IS actor)`       |
|`interface A` (unlabelled)  |**Label alternation** over the implementors' own labels.                                                              |`(v0 IS bot|person)`  |

Both were verified against `postgres:19beta2` in M4:

- A vertex table may carry several `LABEL ... PROPERTIES (...)` clauses, and one label may span several tables. PostgreSQL rejects a graph whose tables disagree about a label's properties (`mismatching number of properties` / `mismatching property names`) and further requires **one type per property name across the whole graph**. The generator checks both, turning a migration-time database error into a generate-time one.
- Only the properties exposed under the matched label are readable: `v.email` under `(v IS actor)` errors with `property "email" for element variable "v" not found`. The compiler therefore projects only the interface's own fields at an interface position.
- The same edge label may span several edge tables, so one `-[e IS follows]->` traverses them all — which is what lets an interface position sit at either end of a hop.

Relationships *targeting* an interface would need one edge table per implementor joined by a comma-separated pattern; they are rejected at parse time pointing at M5, not silently mis-generated.

### 6.3 Spike — bind parameters inside `GRAPH_TABLE` (resolved in M1)

No published PG19 example places `$1` inside `MATCH`/`WHERE`; all inline literals. It is architecturally expected to work (the inline `WHERE` rewrites to an ordinary qual), but had to be verified empirically.

**Outcome (M1, `postgres:19beta2`): supported.** A bind parameter placed in the graph-pattern `WHERE` of a `GRAPH_TABLE` executes correctly:

```sql
SELECT name FROM GRAPH_TABLE (app_graph
  MATCH (v IS person)
  WHERE v.name = $1
  COLUMNS (v.name AS name));
```

filtered to the single matching row when bound with `Alice`. This is asserted by the godog scenario *“Bind parameters work inside `GRAPH_TABLE`”* in `test/m1/features/m1_generate_compile.feature`, which runs against the real container in CI. The SQL/PGQ pattern `WHERE` is rewritten to an ordinary relational qual, so a `ParamRef` there is planned exactly like any other parameterized predicate.

**Consequence for the compiler.** The `inline` strategy — emitting predicates as bind parameters directly inside the graph pattern — is viable and is the default from M3 (arguments/predicates). The fallback below is retained only as a strategy flag; it is not needed on PG19beta2.

Fallback if a future PG version regresses: project the needed columns out of `GRAPH_TABLE` and apply the parameterized predicate in the **outer** `WHERE`. Still one query. The compiler carries a strategy flag so this can be switched without touching the AST walk.

-----

## 7. Milestones

Every milestone ends with **godog scenarios that execute real SQL against a real `postgres:19beta2` container and assert on returned data.** Asserting on generated SQL text alone never satisfies a milestone’s exit criteria; golden-file SQL assertions are permitted only as an *additional* inner-loop check.

Each milestone is a separate commit series; each scenario is its own commit.

-----

### M0 — Harness

Prove the test infrastructure before any compiler exists.

- `testcontainers-go` boots `postgres:19beta2`; `postgres.WithInitScripts` loads fixture DDL; `WithSQLDriver("pgx")` enables Snapshot/Restore.
- Shared container across the suite; per-scenario reset via `Restore` (pools drained first — open connections to the target DB break restore).
- godog wiring: `TestFeatures` entrypoint under `go test`, scenario context carrying the pool.

**Exit:** A feature applies hand-written `CREATE TABLE` + `CREATE PROPERTY GRAPH`, seeds rows, executes a hand-written `GRAPH_TABLE` query, and asserts the returned rows. Confirms PG19, SQL/PGQ availability, and reset semantics.

-----

### M1 — Minimal generator + single-vertex compiler

**SDL surface:** `@node`, `@relationship`, `@hasInverse`, `@ignore`. Surrogate `uuid` keys only. Default scalar mapping.

- `sdl`: parse, validate, build mapping model.
- `generator`: vertex tables, edge tables, mandatory indexes, `CREATE PROPERTY GRAPH` — satisfying all five §5.3 invariants.
- `migrate`: emit `0001_init.sql` in goose format (no diffing yet).
- `compiler`: single root field, no nesting → `GRAPH_TABLE` with one vertex pattern and a `COLUMNS` projection.
- **Spike:** verify whether `$1` works inside `MATCH`/`WHERE`; record the outcome in the spec.

**Exit:** Scenario — given an SDL, generate `0001`, apply via goose, seed rows, compile and execute `{ persons { name } }`, assert the nested JSON response.

-----

### M2 — Migration fold and delta generation

The generator stops being one-shot.

- `migrate`: fold prior goose migration files into an in-memory `schema` model (interpreter over gopgql’s own canonical statement set — not general DDL).
- Diff folded state against desired state; emit `ALTER TABLE ADD/DROP COLUMN`, `CREATE/DROP INDEX`, and `DROP` + `CREATE PROPERTY GRAPH` (graphs are metadata, always recreated).
- `-- +goose Down` sections emitted as inverses.
- Fold correctness is itself asserted by applying folded output to a container and comparing to a direct apply.

**Exit:** Scenario — apply `0001`; add a field to the SDL; generate `0002`; apply; assert the new column exists and queries against both old and new fields return correct data. Second scenario removes a field and asserts the column is dropped.

-----

### M3 — One-hop traversal, arguments, Go-side shaping

Parser complicates: nesting and arguments.

- `compiler`: nested selection → `MATCH` with one edge; `direction: IN` emits `<-[]-`.
- Field arguments → predicates, as bind parameters (or outer `WHERE` per the M1 spike outcome).
- GraphQL variables → ordered `$n` placeholders.
- `shape`: flat rows → nested response; parent-row dedup across one-to-many fan-out.

**Exit:** Scenario — `{ persons(name: $n) { follows { name } } }` with a variable, executed against seeded data, asserting correct nesting and no duplicate parents.

-----

### M4 — Multi-hop, depth limit, label alternation

- Multi-hop `MATCH` chains; `@key`-free interfaces mapped by shared labels across tables with aligned properties.
- Label alternation (`IS a|b`) for GraphQL interfaces/unions.
- `MaxDepth` config; `*DepthExceededError` beyond it.
- Edge isomorphism guards (`WHERE a.id <> b.id`) where patterns could self-match.

**Exit:** Scenario — a 3-hop query returning correct rows; a scenario asserting a 4-hop query is rejected at compile time with the typed error and never reaches the database; a scenario over an interface spanning two tables.

-----

### M5 — Multi-pattern workaround and joins

- Detect selections requiring comma-separated patterns; emit split `GRAPH_TABLE` calls joined on projected IDs.
- `GRAPH_TABLE` output joined with ordinary tables.

**Exit:** Scenario — a query whose shape needs two patterns returns correct results via the join workaround, verified against an equivalent hand-written query.

-----

### M6 — SDL widening: moderate

- `@column(name:, type:)`, `@index(name:, using:)`, `@unique`.
- Differ extended to index and constraint add/drop.

**Exit:** Scenarios — a `numeric(10,2)` column via `@column(type:)` round-trips exact values; a `@unique` violation is rejected by the database; an `@index` appears in `pg_indexes` and is used (asserted via `EXPLAIN`).

-----

### M7 — SDL widening: full, plus conformance

- `@default`, `@check`, `@key(fields:)` natural keys, `@renamedFrom` rename hints.
- Composite keys flow into `KEY (...)` clauses. **Narrowed during implementation:** `SOURCE KEY`/`DESTINATION KEY` stay single-column on the surrogate `id`. A natural key is a uniqueness constraint *alongside* `id`, not a replacement for it (§5), so edge tables keep referencing `id` and the compiler's three `id` projection sites are untouched. Making a natural key the physical identity is §9's open question, not this milestone's.
- **`internal/ddl` first.** Emitting `ALTER TABLE … RENAME` without teaching the fold parser to read it back would corrupt the *next* delta: prior state would be reconstructed as though the rename never happened, and the differ would drop the renamed column. The parser, the AST and the fold visitor learn `RENAME TO`, `RENAME COLUMN … TO` and `ADD`/`DROP CONSTRAINT` before anything emits one.
- **Conformance check:** reflect `pg_propgraph_element`, `pg_propgraph_label`, `pg_propgraph_property` and compare to the SDL directive model; report structured drift. Findings are typed (`MissingElement`, `UnexpectedElement`, `MissingProperty`, `UnexpectedProperty`, `LabelMismatch`) so a caller branches on a kind rather than parsing English. Scope is the graph and only the graph: defaults, `CHECK` and `UNIQUE` constraints, indexes, column types and any table the graph does not expose are in other catalogs and are **not** compared, so an empty report means the graph mapping matches the SDL — not that the tables underneath it do. `gopgql conform` exits `0` when it conforms, `2` when it ran and found drift, and `1` when it could not run at all; the last two are separate because an unreachable database and a drifted one demand different responses.

**Exit:** Scenarios — a `@check` constraint rejects invalid data; a composite-key vertex is matchable by `MATCH`; `@renamedFrom` produces `ALTER TABLE ... RENAME` rather than drop+add, with data preserved; the conformance check detects deliberately injected out-of-band drift and passes on a clean database.

-----

### M8 — SQL-side shaping and benchmark

- Second shaping strategy: `jsonb_build_object` / `json_agg` producing the nested response in-database.
- Strategy selectable; both must produce byte-identical responses.
- Benchmark both across depth and fan-out; record results in the docs.

**Exit:** Scenario — every prior milestone’s query scenarios re-run under SQL-side shaping and produce identical responses. Benchmark output committed.

-----

### M9 — Docs site, WASM playground, Pages deployment

- Docs: guide, directive reference, worked SDL → DDL → `GRAPH_TABLE` examples generated from the godog fixtures so docs cannot drift.
- Playground: `sdl` + `generator` + `compiler` compiled to WASM; paste SDL and a query, see emitted migration SQL and `GRAPH_TABLE` output client-side. No backend.
- Deployment per §8.

**Exit:** Pages site live from the `gh-pages` branch; a PR preview deploys to its subpath and the playground functions there.

-----

## 8. Deployment

### 8.1 GitHub Pages — branch mode

Publishing uses **branch mode** via `peaceiris/actions-gh-pages`, deploying built output to the `gh-pages` branch. Pages is configured to serve from `gh-pages` / root.

```yaml
name: deploy-docs
on:
  push:
    branches: [main]
permissions:
  contents: write
jobs:
  build-deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.23' }
      - name: Build WASM playground
        run: GOOS=js GOARCH=wasm go build -o docs/public/gopgql.wasm ./cmd/wasm
      - uses: actions/setup-node@v4
        with: { node-version: '20' }
      - run: npm ci && npm run build
        working-directory: docs
      - uses: peaceiris/actions-gh-pages@v4
        with:
          github_token: ${{ secrets.GITHUB_TOKEN }}
          publish_dir: ./docs/dist
```

### 8.2 PR previews

PR previews use `rossjrw/pr-preview-action` with `action: auto`, deploying each PR to a subpath of the same `gh-pages` branch.

```yaml
name: pr-preview
on:
  pull_request:
    types: [opened, reopened, synchronize, closed]
concurrency: preview-${{ github.ref }}
permissions:
  contents: write
  pull-requests: write
jobs:
  preview:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.23' }
      - if: github.event.action != 'closed'
        run: GOOS=js GOARCH=wasm go build -o docs/public/gopgql.wasm ./cmd/wasm
      - uses: actions/setup-node@v4
        with: { node-version: '20' }
      - if: github.event.action != 'closed'
        run: npm ci && npm run build
        working-directory: docs
      - uses: rossjrw/pr-preview-action@v1
        with:
          source-dir: ./docs/dist
          action: auto
```

### 8.3 Deployment constraints

- **Vite `base` must be `'./'`** — relative paths, required for PR preview subpath compatibility.
- **No Git LFS** for any asset served by Pages; LFS objects are not served.
- The WASM binary is a build artifact, never committed.
- **No COOP/COEP headers, and none are needed** (§8.6). GitHub Pages cannot set response headers anyway, which is why the PostgreSQL runtime the playground executes against has to be a single-threaded build.

### 8.4 CI

Integration tests run on `ubuntu-latest` with Docker available; `postgres:19beta2` is pulled and cached. The godog suite is the merge gate. Beta image bumps are a deliberate, separate PR that re-baselines any affected scenarios.

### 8.5 Binary and container distribution

Pages (§8.1–§8.2) publishes the docs site. The binaries and the container image are published by **goreleaser** (v2 schema, `.goreleaser.yaml`), driven from a single tag push.

**Two binaries ship, and only two.** `cmd/gopgql` is the schema CLI (`generate`, `migrate`, `conform`) and `cmd/gopgql-mcp` is the MCP server. `cmd/wasm` is deliberately **excluded**: it is a `GOOS=js GOARCH=wasm` build target for the docs playground, staged by `scripts/build-wasm.sh` and consumed by the site (§8.3), and a `gopgql.wasm` in a release archive would be an artifact nobody could execute.

- **Platform matrix** — `linux` and `darwin` × `amd64` and `arm64`. `CGO_ENABLED=0` and `-trimpath`: the result is a static binary with no toolchain paths baked into it, which is what makes the same artifact usable in a scratch-adjacent container and on a developer's machine. Archives are **one `tar.gz` per platform carrying both binaries**, not one per binary: they are not independently useful, since `gopgql-mcp` serves the same schema `gopgql migrate` applies and the examples run the pair together.
- **Version stamping** — `-ldflags "-s -w -X main.version=… -X main.commit=… -X main.date=…"`. The version is a release-time input, never derived at runtime from build metadata; a `go build` from a checkout reports `dev`/`none`/`unknown`, which is the honest answer for an untagged binary. `mod_timestamp` is the commit time rather than the build time, so re-releasing the same commit produces the same bytes.
- **Images** — `ghcr.io/lega4e/gopgql`, multi-arch (`linux/amd64`, `linux/arm64`) via a single `dockers_v2` entry: one `docker buildx build --platform=…` builds both architectures and publishes the manifest directly, replacing the deprecated per-architecture `dockers` + `docker_manifests` pair. This is why `Dockerfile.release` copies from `$TARGETPLATFORM/` — with one build covering every platform, the context cannot hold a single flat pair of binaries. Tags are `{{.Version}}`, `{{.Major}}.{{.Minor}}` and `latest`, so a consumer can pin exactly, follow a minor series, or track the newest release. **The floating tags never move to a prerelease:** `dockers_v2` has no `skip_push`, so the guard is in the tag templates — `{{.Major}}.{{.Minor}}` and `latest` are wrapped in `{{ if not .Prerelease }}`, render empty for a `v1.2.0-rc.1` tag, and empty tags are dropped. An rc therefore publishes `1.2.0-rc.1` and nothing else.

**The release image is a separate Dockerfile.** `Dockerfile.release` builds it; the root `Dockerfile` is left exactly as it is, because all three `examples/*/docker-compose.yml` build it with `context: ../..` and repointing it would break them. The release image's contract is:

- `alpine:3.21` plus `postgresql17-client` — `psql` is present because the examples' seed steps use it.
- Both binaries at `/usr/local/bin/`.
- **No `ENTRYPOINT` and no `CMD`.** The command is always supplied by the caller. That is what keeps the image a drop-in for the examples' compose files, each of which runs a different one of the two binaries with its own arguments; an `ENTRYPOINT` would force every one of them to override it.

**The trigger is a semver tag push** — `v[0-9]+.[0-9]+.[0-9]+*`, spelled out rather than `v*` so a non-release tag cannot publish. `permissions: contents: write` (the release and its archives) and `packages: write` (GHCR) are the whole grant, and the workflow's own `GITHUB_TOKEN` authenticates to `ghcr.io`: **no PAT and no repository secret** is required. Checkout is `fetch-depth: 0`, since goreleaser derives the version from the tag and the changelog from the commits since the previous one. `docker/setup-qemu-action` and `docker/setup-buildx-action` precede the release step because the arm64 image is built under emulation on an amd64 runner.

**The test suite does not re-run at release time.** It requires Docker and a real `postgres:19beta2`, has no skip path (§10), and costs up to 20 minutes; running it again on a commit `ci.yml` already proved green on `main` would double the release's runtime to re-test the same tree. The gate is therefore ordering — green CI on `main`, then the tag. What *is* checked on every PR is the release configuration itself: a `release-config` job runs `goreleaser check` and a snapshot build with `--skip=publish,docker,announce`, so a broken `.goreleaser.yaml` fails on the PR rather than after a tag is already pushed and a retag is the only fix. The multi-arch image build is exercised only on a tag.

### 8.6 In-browser execution — the pinned PostgreSQL runtime

The playground does not only generate. Each tab that compiles a query — Traversal, Multi-pattern, Directives, Constraints, Depth limit, Interfaces — carries a **Run** control that executes what it generated against a real PostgreSQL 19 with SQL/PGQ, compiled to WebAssembly and running in the reader's own tab. Generate is unchanged: still pure, instant and offline.

**The round trip is complete.** A Run is four steps, not three: compile the GraphQL query to `GRAPH_TABLE` (Go, main thread), execute it (PostgreSQL, worker), hand the flat rows back to `shape` (Go, main thread), and render the **nested GraphQL response** it returns. That last step is the one that makes the page a demonstration of gopgql: a SQL generator would stop at the rows, and rows are not what a GraphQL client asked for. The response leads the result panel; the flat rows sit under it in a disclosure, because the fan-out they show is exactly what shaping collapses and seeing both is how the parent dedup becomes legible. `cmd/wasm` exports `gopgqlShape` for this; it re-derives the projection by recompiling, since a `compiler.Projection` is a Go value and cannot cross into JavaScript.

What is rendered is the response *payload* — the root field and its objects — which is what `exec.Query` returns to a Go caller. It is deliberately **not** wrapped in a GraphQL `{"data": …}` envelope: gopgql is a library that compiles and shapes, not a server that answers requests, and an envelope it does not produce would be the one fabricated panel on a page whose entire claim is that nothing is.

**Changing the data is plain SQL, by design.** Every runnable tab carries an editable **Data** pane, pre-filled with the `INSERT`s its example schema needs and applied between the generated DDL and the query, so a reader can `INSERT`, `UPDATE` or `DELETE` and watch the next Run answer differently. It is SQL and not a GraphQL mutation because §2 is a hard constraint, not an omission: a SQL/PGQ property graph is a read-only view, `compiler.CompileQuery` refuses any operation that is not a `query`, and the MCP server's introspection reports a null `mutationType`. gopgql has no write path to expose, so the playground exposes the database's. A failed statement in that pane does not abort the run — the query still executes and reports what it honestly found — because the pane is the reader's to break.

**Where the runtime comes from.** `docs/package.json` depends on an npm package tarball published as a **release asset of `lega4e/postgres-pglite`**, by URL:

| | |
| --- | --- |
| Release tag | `pglite-wasm-19beta2.1` |
| Package | `@electric-sql/pglite@0.5.4-pg19beta2` |
| PostgreSQL | 19beta2 |
| Fork ref built from | `REL_19_BETA2-pglite` @ `edf1a2c0d7477ef0a458861bf3e55a31ff5dc917` |
| Toolchain | emscripten 3.1.74, linked `-sUSE_PTHREADS=0` |
| `pglite.wasm` | 9,379,167 B (3,126,403 B gzipped), sha256 `37f1ffbe…` |
| `pglite.data` | 5,434,026 B (~1.5 MB gzipped), sha256 `03aad8eb…` |

The package name is unchanged from upstream, so `import { PGlite } from '@electric-sql/pglite'` is a drop-in. The raw `pglite.wasm` and `pglite.data` published beside it are **not loadable on their own** — PGlite's emscripten glue size-checks the filesystem bundle against a value fixed at link time — which is why the tarball, carrying the matching JS runtime and `initdb` module, is what gets pinned.

Pinning is by URL plus `package-lock.json`'s integrity hash: `npm ci` reproduces exactly those bytes on every machine and every CI run, or fails. No registry credential, no vendored binary in git, no build step to reproduce. Moving to a newer build is a pin bump plus `npm install` in one reviewable commit — which is what `postgres-pglite#10`'s re-pin to `REL_19_0` at GA will be.

**The build is beta.** PostgreSQL 19beta2 is a beta and this pin will be replaced at GA. What this build has been exercised for is property-graph DDL, `GRAPH_TABLE` evaluation with bind parameters, and ordinary DDL/DML. What it has **not** been exercised for, and nothing should be built on: extension loading, `pg_dump`, the socket server, and any form of persistence.

**Lazy, and enforced.** The module specifier appears in exactly one place — a dynamic `import()` inside `docs/src/pglite-worker.js` — and the worker itself is not constructed until the first Run. A reader who never presses Run downloads exactly what the site cost before execution existed. `docs/scripts/check-lazy-runtime.mjs` runs as npm's `postbuild` hook, walks Vite's static import graph from every entry, and **fails the build** if anything reachable that way references `pglite.wasm` or `pglite.data`. Laziness that regresses silently on a bundler upgrade is worth nothing.

**In memory only.** `new PGlite()` with no `dataDir`. No IndexedDB, no OPFS, no persistence of any kind, and no cross-tab sharing — `@electric-sql/pglite/worker` is deliberately *not* used, because its leader election exists to share a *persisted* database between tabs, which is the opposite of what this is. Each Run builds a fresh database, applies the generated DDL, applies the tab's data SQL, executes the compiled query with its bind values, and discards the database. A reader's edits therefore survive as long as the text in the pane and no longer, which is the same lifetime as everything else on the page.

**The two WebAssembly modules never meet.** `gopgql.wasm` stays on the main thread; PGlite runs in a dedicated Web Worker. They have separate linear memories and nothing is shared between them: what crosses is text, plain arrays and plain values, structured-cloned by `postMessage`.

**Previews keep parity with production.** No asset is withheld from a preview, because the runtime's bytes come from an immutable pinned tarball and are byte-identical on every build — so `gh-pages` stores one git blob for them however many previews reference it. Contrast `gopgql.wasm`, rebuilt from Go source on every commit, which adds a fresh blob per deploy. The cost that actually matters is the reader's download, and the lazy-load contract already bounds that to people who asked for it.

**How it is proven.** `test/seed` runs the schema → data → query → **shape** sequence for every runnable tab against a real `postgres:19beta2` container, so neither a fixture nor a projection can drift from the SDL beside it. `docs/e2e` then runs the same sequence in a real Chromium, in a Web Worker, on the pinned wasm build, served from a preview-shaped subpath by a server that sets no isolation headers — and asserts the nested response comes back with the values the seed put in. It also asserts the write path end to end: an `UPDATE` typed into the Data pane changes the response the next Run returns. Both suites are merge gates. Nothing here is inferred from the fork branch a build came from or from symbols in the binary.

-----

## 9. Open decisions

Carried forward, to be resolved before or during the milestone that needs them:

1. **Module layout** — proposed as a single module with the §4.1 packages; separate modules for `exec` (to keep `pgx` out of WASM consumers’ dependency graph) is an alternative.
1. **Rename hint ergonomics** — **resolved in M7: `@renamedFrom`.** The hint lives in the SDL rather than in a manifest, so the rename travels with the declaration it describes and cannot be lost from a separate file; it names the previous *GraphQL* name and the differ resolves candidate physical names against the folded prior state (§5). A manifest remains the better answer only if renames ever need to be expressed for objects the SDL does not declare, which has not come up.
1. **Natural keys as the physical identity** — opened by M7. `@key(fields:)` is currently a uniqueness constraint alongside the surrogate `id`. Making it the identity instead means the compiler's three `id` projection sites, `shape`'s parent dedup, and every edge table's references all become multi-column, and would be a milestone of its own.
1. **Table naming convention** — pluralisation rules for deriving table names from type names, and whether `@node(table:)` is required or optional.
1. **Goose embedding** — whether `gopgql` embeds `goose` as a library for a `migrate up` convenience command, or only emits files.
1. **Default `MaxDepth`** — **resolved in M4: 3.** A 3-hop pattern rewrites to a 7-way join, which the M4 suite executes against `postgres:19beta2` well inside the scenario budget. The ceiling is per-`Compiler` configuration (`compiler.WithMaxDepth`), so a deployment can lower it without a code change; 4 hops remains rejected by default.

-----

## 10. Principles

- **Single source of truth.** The SDL defines the schema and the queries. The migration directory records history. Nothing else holds schema state.
- **Real infrastructure in tests.** Every milestone proves itself against a real PostgreSQL 19, never against a mock or a string comparison alone.
- **No silent fallbacks.** Depth limits reject rather than truncate. Unsupported constructs error at compile time, not at runtime.
- **Purity where possible.** Compilation and generation contact no database, which is what makes the WASM playground possible.
- **One scenario, one commit.** Milestones advance scenario by scenario.
