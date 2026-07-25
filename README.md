# gopgql

Go library that treats a single annotated GraphQL SDL document as the source of
truth for both halves of a PostgreSQL 19 [SQL/PGQ](https://www.postgresql.org/)
property-graph application: it generates the DDL (vertex/edge tables and the
`CREATE PROPERTY GRAPH` mapping, as `goose` migrations) and compiles GraphQL
queries to `GRAPH_TABLE ... MATCH ... COLUMNS` statements.

See [`SPEC.md`](./SPEC.md) for the full design and milestone plan.

## Status

**M6 — SDL widening: moderate.** The SDL stops being structure-only
([`SPEC.md` §5](./SPEC.md), [§7](./SPEC.md) → M6):

- **`@column(name:)`** renames the physical column — and with it the property the
  graph exposes, so the compiler projects and filters on the column while the
  GraphQL field keeps its own name.
- **`@column(type:)`** overrides the default scalar mapping
  ([`SPEC.md` §5.1](./SPEC.md)): `price: Float! @column(type: "numeric(10,2)")`
  reaches the database as `numeric(10,2)` and round-trips exact values.
- **`@unique`** puts the constraint in the database, so a duplicate is rejected
  by PostgreSQL (SQLSTATE 23505) rather than by anything gopgql checks.
- **`@index(name:, using:)`** adds a secondary index, with the name derived from
  the table and column when it is omitted.
- **The differ follows.** A constraint or index added to an SDL becomes an
  `ALTER TABLE … ADD CONSTRAINT` / `CREATE INDEX` in the next delta, an index
  whose definition moved is dropped and recreated (PostgreSQL has no `ALTER` for
  either), and every Down section is the exact inverse.

The M6 godog scenarios read the column type back from the catalog, assert the
stored value as text so exactness is the database's word and not the driver's,
force a unique violation and check its SQLSTATE, and prove the index is both in
`pg_indexes` **and** chosen by the planner via `EXPLAIN` over 2 020 seeded rows.
A further scenario applies a delta that adds a constraint and an index, then
rolls it back and asserts both are gone. The playground's **Directives** tab
shows the DDL and compiled query for an editable SDL using all four directives.

**M5 — Multi-pattern workaround and joins.** A level may now select more than
one relationship ([`SPEC.md` §7](./SPEC.md) → M5). That shape would need
comma-separated path patterns in a single `MATCH`, which PG19 parses but does not
execute ([`SPEC.md` §2.2](./SPEC.md)), so the compiler splits it instead:

- **One `GRAPH_TABLE` per branch, joined on projected ids.** The chain up to the
  branching level stays a single call — the spine; each relationship hanging off
  it becomes its own call whose pattern re-binds the branch-point vertex by label
  and projects its id; the outer query `LEFT JOIN`s them on that id.
- **A missing branch no longer deletes the parent.** Joining left means a person
  with followers but nothing followed keeps both keys and shapes to an empty
  list, instead of vanishing the way an inner join would.
- **Guards survive the split.** An isomorphism guard between two positions that
  ended up in different calls moves to the join's `ON` clause, expressed over the
  projected ids — so a branch still cannot walk back to a vertex an ancestor
  bound.
- **Unbranched queries are untouched.** A single chain still compiles to exactly
  one `GRAPH_TABLE` with no join at all.

The M5 godog scenarios execute the split SQL against the container and assert on
the returned rows, and — the milestone's exit criterion — re-run an **equivalent
hand-written join** over the same data and require an identical response, so the
workaround is proven correct rather than merely runnable. A further scenario
joins `GRAPH_TABLE` output with an ordinary relational table. The playground's
**Multi-pattern** tab shows the emitted split for an editable branching query.

**M4 — Multi-hop, depth limit, label alternation.** The compiler stops being
one-hop ([`SPEC.md` §7](./SPEC.md) → M4):

- **Multi-hop `MATCH` chains.** Nested selections keep extending one pattern, so
  a three-hop query is still a single `GRAPH_TABLE` — N+1 is avoided by
  construction ([`SPEC.md` §6.2](./SPEC.md)).
- **A depth ceiling that rejects rather than truncates.** A selection nested past
  `MaxDepth` (default 3, configurable with `compiler.WithMaxDepth`) fails
  compilation with a typed `*compiler.DepthExceededError`. SQL/PGQ has no
  variable-length paths, so there is nothing honest to emit — and because the
  failure is at compile time, no statement ever reaches the database
  ([`SPEC.md` §3](./SPEC.md) decision 3).
- **Interfaces spanning several tables.** A GraphQL interface makes its
  implementors' tables one queryable position. Carrying `@node(label:)` it
  becomes a *shared label* every implementor's vertex table exposes with an
  aligned property list — `(v0 IS actor)`; without it, the compiler emits *label
  alternation* over the implementors' own labels — `(v0 IS bot|person)`.
- **Edge-isomorphism guards.** PostgreSQL does not enforce isomorphism
  ([`SPEC.md` §2.2](./SPEC.md)), so a pattern will bind one row to two positions:
  a self-follow satisfies `(a)-[follows]->(b)`, and a three-hop chain walks back
  to where it started. Wherever two positions could bind the same row — decided
  by whether their tables intersect — the compiler emits `vi.id <> vj.id`.

The M4 godog scenarios execute a three-hop query against the container and
assert on the returned rows; assert that a four-hop selection fails with the
typed error and that a pgx query tracer counted **zero** statements afterwards;
and traverse an interface spanning two tables, with the self-match exclusion
verified against seeded data that contains both a self-loop and a cycle.

This builds on **M3** (nesting and arguments): a nested relationship extends the
`MATCH` with an edge (`direction: IN` emits `<-[]-`), field arguments and GraphQL
variables become ordered `$n` bind parameters, and **`shape`** regroups the flat
rows into the nested response with no duplicate parents across the fan-out.

**M2 — Migration fold and delta generation.** The generator stops being
one-shot. gopgql now folds its own earlier migrations back into a schema model
and emits a delta migration against a widened SDL — no database and no sidecar
state file ([`SPEC.md` §3](./SPEC.md) decision 6):

- **`migrate.Fold`** interprets gopgql's own canonical `goose` statement set
  (`0001`, `0002`, …) — `CREATE TABLE`, `ALTER TABLE ADD/DROP COLUMN`,
  `CREATE/DROP INDEX`, `CREATE/DROP PROPERTY GRAPH` — back into a `schema.Schema`.
  It is an interpreter over gopgql's own DDL, not a general DDL parser. The
  reading follows the shape every established Go SQL parser
  ([vitess](https://github.com/vitessio/vitess),
  [xwb1989/sqlparser](https://github.com/xwb1989/sqlparser),
  [pg_query_go](https://github.com/pganalyze/pg_query_go)) uses — a lexer feeds a
  recursive-descent parser that builds a typed AST — implemented in the small
  `internal/ddl` package; `Fold` is then a plain visitor over those statement
  nodes, so a new statement is a new node plus a production rather than another
  string-surgery special case.
- **`migrate.Delta`** diffs the folded prior state against the desired state and
  renders the next migration: `ALTER TABLE ADD/DROP COLUMN`, `CREATE/DROP INDEX`,
  `CREATE/DROP TABLE`, and a `DROP` + `CREATE PROPERTY GRAPH` (graphs are
  metadata, always recreated), with a `-- +goose Down` section that is the exact
  inverse.
- **`migrate.Generate`** ties them together over a migration directory: fold what
  is there, diff, and write `NNNN_<name>.sql`.

This builds on **M1** (from `@node`, `@relationship`, `@hasInverse`, `@ignore`;
surrogate `uuid` keys; the default scalar mapping): **`sdl`** parses/validates
via [`vektah/gqlparser/v2`](https://github.com/vektah/gqlparser); **`generator`**
emits vertex/edge tables, destination-key indexes and `CREATE PROPERTY GRAPH`
(all five [`SPEC.md` §5.3](./SPEC.md) invariants); **`compiler`** compiles a
GraphQL query to a `GRAPH_TABLE`; **`shape`** regroups flat rows into the nested
response.

The [godog](https://github.com/cucumber/godog) suite boots a real
`postgres:19beta2` container via
[testcontainers-go](https://github.com/testcontainers/testcontainers-go). The M2
scenarios apply a generated `0001`, generate and apply a `0002` delta from a
widened SDL, and assert on **returned data** for both the pre-existing and the
new field; a drop scenario asserts (via `information_schema`) that the column is
gone; and a fold-correctness scenario applies the folded output and a direct
apply of the same final schema and asserts the resulting schemas are identical.

The **WASM playground** (`cmd/wasm` + `docs/`) runs the real
`sdl`+`generator`+`migrate`+`compiler` in the browser as compiled Go. Each tab is
one complete, editable scenario — schema and query in on the left, **Generate**
in the middle, generated database schema and compiled query on the right:

| Tab           | Scenario                                                                                             |
|---------------|------------------------------------------------------------------------------------------------------|
| *Traversal*   | The three-hop exit query: one `GRAPH_TABLE`, bind parameters, isomorphism guards.                     |
| *Directives*  | The M6 mapping directives in the generated DDL: a renamed column, an overridden type, a UNIQUE, and two indexes. |
| *Multi-pattern* | A branching query split into one `GRAPH_TABLE` per branch, `LEFT JOIN`ed on projected ids; the status line counts the calls. |
| *Depth limit* | A four-hop query refused with the typed `*DepthExceededError`; move the `MaxDepth` control and watch it flip. |
| *Interfaces*  | The shared `LABEL` clauses in the generated graph, and both interface mappings in the compiled pattern.|
| *Migration*   | Two stacked scenarios: the initial `0001_init.sql`, then a revised schema folded and diffed to a delta.|

## Layout

| Path             | Purpose                                                            |
|------------------|-------------------------------------------------------------------|
| `sdl/`           | Parse + validate SDL; typed directive/mapping model.              |
| `schema/`        | In-memory physical schema model (tables, indexes, graph).         |
| `generator/`     | SDL model → schema model → DDL (`CREATE ...`, property graph).    |
| `migrate/`       | Emit `goose` migrations; fold prior migrations + diff deltas.     |
| `internal/ddl/`  | Lexer + recursive-descent parser + AST for gopgql's own DDL.       |
| `compiler/`      | GraphQL operation → `GRAPH_TABLE` SQL + ordered bind params.      |
| `shape/`         | Flat rows → nested GraphQL response.                              |
| `playground/`    | Pure driver wiring the pipeline for the WASM playground.          |
| `cmd/wasm/`      | WebAssembly entry point exposing `playground` to the docs site.  |
| `docs/`          | Vite site with the browser playground.                            |
| `test/`          | godog feature + testcontainers harness (`postgres:19beta2`).      |
| `scripts/`       | `build-wasm.sh` — builds `gopgql.wasm` + stages `wasm_exec.js`.   |

## Developing

```sh
make build        # go build ./...
make vet          # go vet ./...
make test         # integration suite (needs Docker + postgres:19beta2)
make lint         # golangci-lint
make vuln         # govulncheck
make docs         # build the WASM playground + docs site into docs/dist
```

Compile the packages and run the unit tests without booting a container
(the integration suites under `./test/...` always require Docker and have no
skip path — SPEC.md §10):

```sh
go build ./... && go vet ./...
go test ./compiler/... ./shape/... ./sdl/... ./generator/... ./migrate/... ./playground/... ./internal/...
```

## Playground locally

```sh
cd docs && npm install && npm run dev
```

`npm run dev` and `npm run build` stage `gopgql.wasm` first (a `pre*` hook
running `scripts/build-wasm.sh`), so the page is never served against a stale
module — it is a build artefact and is never committed (`SPEC.md` §8.3). The
page also refuses to start if the module it loads is older than the code
calling it, rather than silently ignoring arguments it does not understand.
