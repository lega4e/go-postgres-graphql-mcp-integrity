# gopgql

Go library that treats a single annotated GraphQL SDL document as the source of
truth for both halves of a PostgreSQL 19 [SQL/PGQ](https://www.postgresql.org/)
property-graph application: it generates the DDL (vertex/edge tables and the
`CREATE PROPERTY GRAPH` mapping, as `goose` migrations) and compiles GraphQL
queries to `GRAPH_TABLE ... MATCH ... COLUMNS` statements.

See [`SPEC.md`](./SPEC.md) for the full design and milestone plan.

## Status

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
**delta migration**, and the emitted `GRAPH_TABLE`.

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

Run the harness without the container (compile check only):

```sh
GOPGQL_SKIP_INTEGRATION=1 go test ./...
```

## Playground locally

```sh
make docs
cd docs && npm run preview
```
