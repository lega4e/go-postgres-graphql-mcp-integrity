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

**A generation is not atomic.** goose runs each file in its own transaction, so a
`goose up` that stops between the teardown and the rebuild leaves a database whose
tables have moved and which has no property graph. Queries against the graph fail
loudly in the meantime rather than returning wrong rows, and `gopgql conform`
names the state directly: it exits 1 with `conform: property graph not found`
(`conform.ErrGraphNotFound`), which is the difference between "the graph is down"
and "the graph has drifted", reported as exit 2.

Recovering from it depends on *why* the apply stopped, and the two cases are not
the same:

- **The run was interrupted** — a crash, a `^C`, a killed pod. Whatever committed
  stays committed and nothing is wrong with the SQL, so re-running `gopgql
  migrate` (or `goose up`) continues from where it stopped and the graph comes
  back up.
- **The `_tables` migration itself failed** — a `NOT NULL` added over existing
  rows, a type change PostgreSQL refuses. Re-running fails identically, because
  the DDL is the problem and not the interruption. The `_graph_down` migration in
  front of it has already committed, so going forwards is blocked and replaying
  from zero hits the same statement. The way out is `goose down` one step, which
  runs the teardown's own `Down` and puts the graph back; then fix the SDL (or
  the data) and generate again.

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

`sdl`, `schema`, `generator`, `migrate`, and `compiler` have **no database dependency** and compile to WASM. `exec` imports `pgx`; `conform` reflects a live catalog and so imports it too; `mcp` and `cmd/gopgql-mcp` build on `exec`. None of those four are part of the WASM surface, and nothing on the WASM side may import them — the boundary is what makes the browser playground possible at all.

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

### 8.4 CI

Integration tests run on `ubuntu-latest` with Docker available; `postgres:19beta2` is pulled and cached. The godog suite is the merge gate. Beta image bumps are a deliberate, separate PR that re-baselines any affected scenarios.

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
