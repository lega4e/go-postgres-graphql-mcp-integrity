# gopgql

Go library that treats a single annotated GraphQL SDL document as the source of
truth for both halves of a PostgreSQL 19 [SQL/PGQ](https://www.postgresql.org/)
property-graph application: it generates the DDL (vertex/edge tables and the
`CREATE PROPERTY GRAPH` mapping, as `goose` migrations) and compiles GraphQL
queries to `GRAPH_TABLE ... MATCH ... COLUMNS` statements.

See [`SPEC.md`](./SPEC.md) for the full design and milestone plan.

## Status

**M0 — Harness.** This milestone proves the test infrastructure end to end: a
[godog](https://github.com/cucumber/godog) BDD suite boots a real
`postgres:19beta2` container via
[testcontainers-go](https://github.com/testcontainers/testcontainers-go),
applies a hand-written schema and `CREATE PROPERTY GRAPH`, seeds rows, executes
`GRAPH_TABLE` queries, and asserts on the returned data. Per-scenario reset uses
the container's snapshot/restore.

An early **WASM playground** (`cmd/wasm` + `docs/`) shows the M0-preview
translation of SDL to SQL/PGQ running entirely in the browser as compiled Go.

## Layout

| Path             | Purpose                                                            |
|------------------|-------------------------------------------------------------------|
| `demo/`          | M0-preview SDL → SQL/PGQ translation (pure Go, no DB, WASM-safe).  |
| `cmd/wasm/`      | WebAssembly entry point exposing `demo` to the playground.        |
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
