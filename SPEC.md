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
|`exec`      |Optional thin `pgx` execution helper.                                                                  |
|`cmd/gopgql`|CLI: `generate`, `migrate diff`, `compile`.                                                            |

`sdl`, `schema`, `generator`, `migrate`, and `compiler` have **no database dependency** and compile to WASM. Only `exec` imports `pgx`.

### 4.2 Dependencies

|Purpose               |Package                                                           |
|----------------------|------------------------------------------------------------------|
|GraphQL parse/validate|`github.com/vektah/gqlparser/v2`                                  |
|PostgreSQL driver     |`github.com/jackc/pgx/v5`                                         |
|Migration runner      |`github.com/pressly/goose/v3`                                     |
|BDD                   |`github.com/cucumber/godog`                                       |
|Containers            |`github.com/testcontainers/testcontainers-go` + `modules/postgres`|

-----

## 5. SDL directive reference

Directives are introduced progressively (§7). This is the full destination surface.

```graphql
# --- M1: graph structure ---
directive @node(label: String!, table: String) on OBJECT
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

# --- M7: full expressiveness ---
directive @default(value: String!) on FIELD_DEFINITION
directive @check(expr: String!) on FIELD_DEFINITION | OBJECT
directive @key(fields: [String!]!) on OBJECT     # composite / natural key
directive @renamedFrom(name: String!) on OBJECT | FIELD_DEFINITION  # rename hint
```

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
1. When one label spans multiple tables, property lists are aligned by count, name, and type — with `col AS name` renames emitted as needed.

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

### 6.3 Open spike — bind parameters inside `GRAPH_TABLE`

No published PG19 example places `$1` inside `MATCH`/`WHERE`; all inline literals. It is architecturally expected to work (the inline `WHERE` rewrites to an ordinary qual), but **must be verified empirically in M1**.

Fallback if unsupported: project the needed columns out of `GRAPH_TABLE` and apply the parameterized predicate in the **outer** `WHERE`. Still one query. The compiler carries a strategy flag so this can be switched without touching the AST walk.

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

- `@default`, `@check`, `@key(fields:)` composite/natural keys, `@renamedFrom` rename hints.
- Composite keys flow into `KEY (...)` clauses and multi-column `SOURCE KEY`/`DESTINATION KEY`.
- **Conformance check:** reflect `pg_propgraph_element`, `pg_propgraph_label`, `pg_propgraph_property` and compare to the SDL directive model; report structured drift.

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
1. **Rename hint ergonomics** — `@renamedFrom` is proposed; an alternative is an explicit rename manifest consumed by `migrate diff`.
1. **Table naming convention** — pluralisation rules for deriving table names from type names, and whether `@node(table:)` is required or optional.
1. **Goose embedding** — whether `gopgql` embeds `goose` as a library for a `migrate up` convenience command, or only emits files.
1. **Default `MaxDepth`** — proposed 3; needs validation against the join cost of a 3-hop pattern (7-way join).

-----

## 10. Principles

- **Single source of truth.** The SDL defines the schema and the queries. The migration directory records history. Nothing else holds schema state.
- **Real infrastructure in tests.** Every milestone proves itself against a real PostgreSQL 19, never against a mock or a string comparison alone.
- **No silent fallbacks.** Depth limits reject rather than truncate. Unsupported constructs error at compile time, not at runtime.
- **Purity where possible.** Compilation and generation contact no database, which is what makes the WASM playground possible.
- **One scenario, one commit.** Milestones advance scenario by scenario.
