# gopgql

One annotated GraphQL SDL document is the source of truth for both halves of a
PostgreSQL 19 [SQL/PGQ](https://www.postgresql.org/) property-graph application.
gopgql generates the DDL — vertex and edge tables plus the
`CREATE PROPERTY GRAPH` mapping, as [goose](https://github.com/pressly/goose)
migrations — and compiles GraphQL queries to `GRAPH_TABLE ... MATCH ... COLUMNS`
statements.

Two binaries ship: **`gopgql`**, the schema CLI (`generate`, `migrate`,
`conform`), and **`gopgql-mcp`**, an MCP server that serves one schema and one
database to an AI agent.

Property graphs are a PostgreSQL **19** feature; nothing here runs on 18.

See [`SPEC.md`](./SPEC.md) for the design, the milestones and the SQL/PGQ
limitations gopgql works around.

## Try it in the browser

**[gopgql.garutyunov.com](https://gopgql.garutyunov.com/)** runs the real
`sdl` + `generator` + `migrate` + `compiler` + `shape` packages as WebAssembly.
Edit an SDL document and a GraphQL query on the left and the generated DDL,
migrations and compiled SQL update on the right — traversals, migration deltas,
the depth limit, interfaces and every mapping directive, each as an editable
scenario.

Every query tab also **runs** what it generated. **Run in PostgreSQL** starts a
real PostgreSQL 19 with SQL/PGQ — a wasm build of the fork, in a Web Worker, in
memory only — applies the generated DDL and the tab's data, executes the
compiled `GRAPH_TABLE` query with its bind values, and hands the flat rows back
to `shape`, which regroups them into the **nested GraphQL response** the panel
leads with. That is the whole round trip: a GraphQL query in, a GraphQL response
out, with the rows one disclosure underneath as the evidence. Nothing is fetched
until the button is pressed, and nothing is sent anywhere
([`SPEC.md` §8.6](./SPEC.md)).

Each of those tabs carries an editable **Data** pane, pre-filled with the
`INSERT`s its schema needs and applied just before the query, so you can
`INSERT`, `UPDATE` or `DELETE` and watch the response change. Writes are plain
SQL rather than GraphQL mutations because a SQL/PGQ property graph is a
read-only view: gopgql compiles queries against it and never writes through it,
and `CompileQuery` refuses any operation that is not a `query`.

## What it looks like

```graphql
# schema.graphql
type Person @node(label: "person") {
  id: ID!
  name: String! @unique
  email: String

  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followers: [Person!]! @relationship(type: "follows", direction: IN)
}
```

`gopgql generate --sdl schema.graphql --dir migrations --name people` writes the
migrations that schema calls for. Tables and the property graph over them are
never mixed in one file, and the files are numbered in the order they must run:

```sql
-- migrations/0001_people_tables.sql   -- +goose Up
CREATE TABLE persons (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL UNIQUE,
    email text
);
CREATE TABLE follows (
    source_id uuid NOT NULL REFERENCES persons (id),
    target_id uuid NOT NULL REFERENCES persons (id),
    PRIMARY KEY (source_id, target_id)
);
CREATE INDEX follows_target_idx ON follows (target_id);

-- migrations/0002_people_graph.sql    -- +goose Up
CREATE PROPERTY GRAPH app_graph
  VERTEX TABLES (
    persons LABEL person PROPERTIES (id, name, email)
  )
  EDGE TABLES (
    follows SOURCE KEY (source_id) REFERENCES persons (id)
            DESTINATION KEY (target_id) REFERENCES persons (id)
            LABEL follows PROPERTIES (source_id, target_id)
  );
```

Run it again after widening the SDL and gopgql folds the migrations already in
the directory, diffs them against the schema, and writes only the delta — no
database and no sidecar state file.

A query against that schema:

```graphql
query Followed($name: String!) {
  persons(name: $name) {
    name
    follows { name email }
  }
}
```

compiles to one statement, with variables bound as parameters rather than
interpolated:

```sql
SELECT v0_k, v0_c0, v1_k, v1_c0, v1_c1
FROM GRAPH_TABLE (app_graph
  MATCH (v0 IS person) -[e0 IS follows]-> (v1 IS person)
  WHERE v0.name = $1 AND v0.id <> v1.id
  COLUMNS (v0.id AS v0_k, v0.name AS v0_c0, v1.id AS v1_k, v1.name AS v1_c0, v1.email AS v1_c1)
)
```

Nesting extends the same pattern instead of issuing another query, so there is
no N+1; the flat rows are regrouped into the nested GraphQL response on the Go
side. `v0.id <> v1.id` is there because PostgreSQL does not enforce isomorphism
— without it a self-follow would satisfy the pattern.

## Install

Prebuilt binaries for linux and macOS, amd64 and arm64, are on the
[Releases page](https://github.com/lega4e/gopgql/releases) — one `tar.gz`
per platform carrying **both** binaries. They are static, so there is nothing
else to install.

From source, with a Go toolchain:

```sh
go install github.com/lega4e/gopgql/cmd/gopgql@latest
go install github.com/lega4e/gopgql/cmd/gopgql-mcp@latest
```

Or as a container image, published to GHCR on every release:

```sh
docker run --rm ghcr.io/lega4e/gopgql:latest gopgql version
```

The image is multi-arch, carries both binaries in `/usr/local/bin/`, and has no
`ENTRYPOINT` and no `CMD` — the command is always given explicitly, which is
what makes it a drop-in for a compose file.

## Run it with Docker Compose

Postgres, a one-shot migration, and the MCP server:

```yaml
# docker-compose.yml
name: gopgql

x-dsn: &dsn postgres://gopgql:gopgql@postgres:5432/gopgql?sslmode=disable

services:
  postgres:
    image: postgres:19beta2
    environment:
      POSTGRES_DB: gopgql
      POSTGRES_USER: gopgql
      POSTGRES_PASSWORD: gopgql
    ports: ["127.0.0.1:55432:5432"]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U gopgql -d gopgql"]
      interval: 2s
      retries: 30

  migrate:
    image: ghcr.io/lega4e/gopgql:latest
    depends_on:
      postgres: {condition: service_healthy}
    environment:
      GOPGQL_DSN: *dsn
    volumes: ["./schema.graphql:/app/schema.graphql:ro"]
    command: ["gopgql", "migrate", "--sdl", "/app/schema.graphql", "--dir", "/tmp/migrations"]

  mcp:
    image: ghcr.io/lega4e/gopgql:latest
    depends_on:
      migrate: {condition: service_completed_successfully}
    environment:
      GOPGQL_DSN: *dsn
      GOPGQL_TRANSPORT: http
      GOPGQL_ADDR: ":8080"
    ports: ["127.0.0.1:8765:8080"]
    volumes: ["./schema.graphql:/app/schema.graphql:ro"]
    command: ["gopgql-mcp", "--sdl", "/app/schema.graphql"]
```

```sh
docker compose up -d
```

`gopgql migrate` generates the migrations the SDL calls for and applies them
with goose, in one step. The `--dir` above is ephemeral, so the whole history is
regenerated from the SDL each run and re-applied from zero; goose skips the
versions it has already applied, so restarting the stack is a no-op.

Point `psql` at `postgres://gopgql:gopgql@localhost:55432/gopgql` to see what
the server sees.

### Run an example

Three complete stacks live in [`examples/`](./examples), each with an SDL
document, a seeded corpus, a compose file and a `.mcp.json`:

| Example | The graph | Ports (MCP / Postgres) |
|---|---|---|
| [`docs-graph/`](./examples/docs-graph) | A documentation index in [PageIndex](https://github.com/VectifyAI/PageIndex)'s shape — documents, a tree of sections, and citations *between* documents | 8765 / 55432 |
| [`code-graph/`](./examples/code-graph) | This repository's own source: packages, files, symbols, calls, imports | 8766 / 55433 |
| [`slack-graph/`](./examples/slack-graph) | Team chat: channels, members, threads, messages, mentions | 8767 / 55434 |

```sh
cd examples/docs-graph
docker compose up -d --build    # postgres, migrate, seed, and the server
claude                          # picks up ./.mcp.json
```

These build the image from the checkout rather than pulling it, so they always
run the code you have. Ask the agent to `introspect` first, then to `query`:

```graphql
{ sections(title: "Root Cause") { id title cites { title } } }
```
```json
{"sections": [{
  "id": "50000000-0000-0000-0000-000000000012",
  "title": "Root Cause",
  "cites": [
    {"title": "Ledger Write Path"},
    {"title": "Retries and Backoff"},
    {"title": "Alert: Ledger Lock Wait High"}
  ]}]}
```

Two of those three citations are in other documents, which a tree-shaped index
could not answer. See [`examples/README.md`](./examples/README.md) for the rest,
including the one thing worth knowing before writing a deep query: `MATCH` is an
**inner join**, so a node with no edge at the next level drops out of the result
rather than coming back with an empty list.

## Connect an AI agent (MCP)

The examples run the server over HTTP, so it outlives its clients and several
agents can share one process — `.mcp.json` is just a URL. For a server an agent
spawns itself, the binary speaks stdio, which is its default:

```sh
claude mcp add gopgql --env GOPGQL_DSN="$GOPGQL_DSN" -- gopgql-mcp --sdl schema.graphql
```

Pass the DSN through the environment rather than `--dsn`: an MCP configuration
file sits on disk and command-line arguments are visible to every process on the
machine. `--sdl` falls back to `GOPGQL_SDL` the same way, and a flag wins over
the environment.

Two tools:

| Tool | What it does |
|------|--------------|
| `introspect` | Standard GraphQL introspection over the loaded schema. No arguments gives the overview; `type: "Person"` drills into one type; `full: true` returns the complete result; `format: "sdl"` returns the document. |
| `query` | Compiles a GraphQL query, executes it, and returns the nested response. `variables` are bound as SQL parameters, never interpolated. `format: "markdown"` renders a table instead of JSON. |

The server never migrates and never writes: the compiler emits nothing but a
`SELECT`, there is no mutation tool, and the pool opens with
`default_transaction_read_only=on`, so a write is refused by the database
itself.

## Choose how the response is shaped

gopgql has two ways to turn a match into the nested GraphQL response, and you
pick one when you build the compiler:

```go
// The default: project flat columns and regroup them in Go.
c := compiler.New(doc)

// Or: let PostgreSQL assemble the response with json_build_object / json_agg
// and return it as a single `response` column.
c := compiler.New(doc, compiler.WithShaping(compiler.SQLSide))
```

It is a *compiler* option rather than a runtime switch because the two emit
different SQL. `exec.Query` reads the strategy off the compiled query and
dispatches, so its signature is the same either way and every existing caller
keeps working. The `MATCH` pattern, its predicates and its bind parameters are
identical under both — only the projection around them changes.

A depth-*d*, fan-out-*f* query ships *f^d* rows to the client under Go-side
shaping and exactly one row under SQL-side shaping. Where a query branches, the
flat statement `LEFT JOIN`s the branches and a parent with *m* and *n* children
yields *m×n* rows, while the SQL-side statement aggregates each branch to an
array before the join. Measurements are in
[`docs/benchmarks.md`](docs/benchmarks.md); the default is Go-side, and the
numbers are what a change to that would have to argue from.

**Both produce byte-identical responses** — where that means something specific,
and worth reading before relying on it:

> The response is the `map[string]any` returned by `exec.Query`. Its canonical
> encoding is `shape.Encode`, which is `encoding/json` over that value. Two
> strategies produce byte-identical responses when `shape.Encode` of each
> returns equal bytes.

It does **not** mean the bytes PostgreSQL sends equal the bytes gopgql writes.
They do not: `json_build_object` emits `{"k" : v}` in argument order, and
`jsonb_build_object` additionally sorts keys by length-then-bytes and drops
duplicates. That divergence stops where the SQL-side path decodes the database's
JSON into the same Go value the Go-side path builds — one encoder writes both,
so identity holds by construction rather than by a passing test. `test/parity`
re-runs every query scenario from every earlier milestone under both strategies
against a real `postgres:19beta2` and compares the encoded bytes, list order
included.

One consequence worth knowing: under SQL-side shaping a projected scalar gopgql
has no canonical form for — a `@column(type: "interval")`, say — is a typed
`*compiler.UnshapeableScalarError` at compile time. Go-side shaping keeps
accepting it, because it makes no cross-strategy promise about it.

## Check the database still matches

Everything above reasons from the SDL alone, which is sound only while nobody
alters the database out of band. `gopgql conform` is the check on that
assumption — it reads the property graph back out of a live database and reports
how it differs:

```sh
gopgql conform --sdl schema.graphql --dsn "$GOPGQL_DSN"
```

The answer is the exit status, so it gates a pipeline with nothing parsing its
output: `0` the graph matches, `2` it has drifted, `1` the check could not run
at all — an unreachable database and a drifted one call for completely different
next moves. Findings print one per line, naming which side said what:

```
KIND             ELEMENT    PROPERTY  SDL            DATABASE
MissingElement   companies  -         company        -
LabelMismatch    persons    -         actor, person  human
MissingProperty  persons    email     email          -
```

It compares the property graph — which elements exist, the labels they carry and
the properties they expose. Column defaults, `CHECK` and `UNIQUE` constraints,
indexes and column types live in other catalogs and are **not** compared, so a
clean report means the graph mapping matches the SDL, not that the tables
underneath it do.

## CLI reference

```
gopgql generate --sdl <file> --dir <dir> [--name <suffix>] [--graph <name>]
                [--no-tables] [--no-graph]
gopgql migrate  --dsn <url> [--sdl <file>] [--dir <dir>] [--name <suffix>] [--graph <name>]
                [--no-tables] [--no-graph]
gopgql conform  --sdl <file> --dsn <url> [--graph <name>]
gopgql version
```

`--sdl`, `--dsn` and `--dir` fall back to `GOPGQL_SDL`, `GOPGQL_DSN` and
`GOPGQL_MIGRATIONS`; a flag wins over the environment. `--dir` defaults to
`migrations`.

`--no-tables` generates the property graph over tables someone else owns —
gopgql then never reads, diffs or emits anything about a table, so absence from
the SDL is not evidence of absence from the database. `--no-graph` is the
mirror. Both scope a directory's *first* generation; after that its own history
decides, and a flag contradicting it is refused. With the tables half **on**,
gopgql is managing those tables, and a column absent from the SDL is a column it
will remove.

## Developing

```sh
make build        # go build ./...
make test         # integration suite (needs Docker + postgres:19beta2)
make bench        # shaping benchmark; regenerates docs/benchmarks.md
make lint         # golangci-lint
make docs         # build the WASM playground + docs site into docs/dist
```

The suites under `./test/...` boot a real `postgres:19beta2` container and have
no skip path. Everything else runs without Docker:

```sh
go build ./... && go vet ./...
go test ./compiler/... ./shape/... ./sdl/... ./generator/... ./migrate/... \
        ./playground/... ./internal/... ./conform/... ./cmd/gopgql/...
```

The playground served at the demo link is `docs/`:

```sh
cd docs && npm install && npm run dev
```

`npm run dev` and `npm run build` rebuild `gopgql.wasm` first, so the page is
never served against a stale module.
