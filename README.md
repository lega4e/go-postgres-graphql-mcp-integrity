# gopgql

Go library that treats a single annotated GraphQL SDL document as the source of
truth for both halves of a PostgreSQL 19 [SQL/PGQ](https://www.postgresql.org/)
property-graph application: it generates the DDL (vertex/edge tables and the
`CREATE PROPERTY GRAPH` mapping, as `goose` migrations) and compiles GraphQL
queries to `GRAPH_TABLE ... MATCH ... COLUMNS` statements.

See [`SPEC.md`](./SPEC.md) for the full design and milestone plan.

## Status

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
`sdl`+`generator`+`migrate`+`compiler` in the browser as compiled Go — paste two
SDL revisions and a query, see the initial migration, the folded-and-diffed
**delta migration**, and the emitted `GRAPH_TABLE`. Its *Depth limit* tab
compiles a four-hop query and surfaces the typed `*DepthExceededError`; its
*Interfaces* tab shows the shared `LABEL` clauses in the generated
`CREATE PROPERTY GRAPH` and both interface mappings in the compiled pattern.

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
make docs
cd docs && npm run preview
```
