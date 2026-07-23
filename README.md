# gopgql

Go library that treats a single annotated GraphQL SDL document as the source of
truth for both halves of a PostgreSQL 19 [SQL/PGQ](https://www.postgresql.org/)
property-graph application: it generates the DDL (vertex/edge tables and the
`CREATE PROPERTY GRAPH` mapping, as `goose` migrations) and compiles GraphQL
queries to `GRAPH_TABLE ... MATCH ... COLUMNS` statements.

See [`SPEC.md`](./SPEC.md) for the full design and milestone plan.

## Status

**M1 — Minimal generator + single-vertex compiler.** The first milestone with
real library code. From one annotated SDL document (`@node`, `@relationship`,
`@hasInverse`, `@ignore`; surrogate `uuid` keys; the default scalar mapping),
gopgql:

- **`sdl`** parses and validates via
  [`vektah/gqlparser/v2`](https://github.com/vektah/gqlparser) and builds a typed
  mapping model;
- **`generator`** emits vertex tables, edge tables, the mandatory
  destination-key indexes, and `CREATE PROPERTY GRAPH` — satisfying all five
  [`SPEC.md` §5.3](./SPEC.md) invariants;
- **`migrate`** writes `0001_init.sql` in `goose` format;
- **`compiler`** compiles a single-root-field GraphQL query to a `GRAPH_TABLE`
  with one vertex pattern and an enumerated `COLUMNS` projection;
- **`shape`** regroups the flat rows into the nested GraphQL response.

The [godog](https://github.com/cucumber/godog) suite boots a real
`postgres:19beta2` container via
[testcontainers-go](https://github.com/testcontainers/testcontainers-go),
applies the **generated** migration with `goose`, seeds rows, runs the
**compiler-produced** SQL, and asserts on the returned nested JSON. It also
records the [`SPEC.md` §6.3](./SPEC.md) spike: a bind parameter (`$1`) works
inside a `GRAPH_TABLE` `WHERE`.

The **WASM playground** (`cmd/wasm` + `docs/`) runs the real
`sdl`+`generator`+`compiler` in the browser as compiled Go — paste SDL and a
query, see the generated migration and the emitted `GRAPH_TABLE`.

## Layout

| Path             | Purpose                                                            |
|------------------|-------------------------------------------------------------------|
| `sdl/`           | Parse + validate SDL; typed directive/mapping model.              |
| `schema/`        | In-memory physical schema model (tables, indexes, graph).         |
| `generator/`     | SDL model → schema model → DDL (`CREATE ...`, property graph).    |
| `migrate/`       | Emit `goose` migrations (`0001_init.sql`).                        |
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
