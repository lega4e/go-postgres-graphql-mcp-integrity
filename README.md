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
`INSERT`, `UPDATE` or `DELETE` and watch the response change. Writes there are
plain SQL rather than GraphQL mutations because a SQL/PGQ property graph is a
read-only view — nothing gopgql compiles writes *through the graph*, and
`CompileQuery` emits `GRAPH_TABLE` and nothing else.

gopgql does have a write path, and it is a different one: a mutation field
carrying [`@function`](#calling-a-function) compiles to a plain call to a
PL/pgSQL function the database already owns, over ordinary tables. It runs only
through a connection you hand in.

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
the environment. It is repeatable here too, and takes a directory: the server
loads whatever documents it names as one schema.

Two tools:

| Tool | What it does |
|------|--------------|
| `introspect` | Standard GraphQL introspection over the loaded schema. No arguments gives the overview; `type: "Person"` drills into one type; `full: true` returns the complete result; `format: "sdl"` returns the document. |
| `query` | Compiles a GraphQL query, executes it, and returns the nested response. `variables` are bound as SQL parameters, never interpolated. `format: "markdown"` renders a table instead of JSON. |

The server never migrates and never writes. It exposes no mutation tool and its
introspection reports a null `mutationType` — deliberately, and not because
mutations cannot exist: the server holds a pool it opened itself, with
`default_transaction_read_only=on`, and no handle from a caller. A `@function`
call needs a connection somebody else is responsible for, and an agent able to
enqueue work is a different authorization question from an agent able to read.

## Calling a function

A `Mutation` field carrying `@function` maps to a PL/pgSQL function the database
already owns. gopgql derives no writes of its own — there is no generated
`createPerson` and no inferred input type — it calls what the SDL names.

```graphql
type Mutation {
  startAgentRun(
    agentDigest: String! @column(name: "agent_digest")
    userId: String!      @column(name: "user_id")
    queue: String = "agent"
    priority: Int
  ): String! @function(schema: "dbos", name: "enqueue_workflow")

  sendMessage(destination: String!): Boolean!
    @function(schema: "dbos", name: "send_message", returns: VOID)
}
```

```
SELECT dbos.enqueue_workflow(agent_digest => $1, user_id => $2, queue => $3)
```

Arguments map to parameters **by name**, so writing them in another order
compiles to the same call. An argument your operation does not pass is left out
of the call, so the function's own `DEFAULT` applies — passing it as `null` is a
different thing, and sends `NULL`. `returns: VOID` is declared rather than
guessed, because `Boolean!` cannot tell a function that returned `false` from
one that returned nothing.

An error the function raises arrives as an `*exec.FunctionError` carrying its
SQLSTATE:

```go
var fnErr *exec.FunctionError
if errors.As(err, &fnErr) && fnErr.SQLSTATE == "P0001" { … }
```

## Tables you do not own

`@readonly` marks a type gopgql **surfaces but does not own**: it appears in the
property graph and is queryable like any other, and gopgql emits no `CREATE
TABLE`, no `ALTER`, no `DROP` and no index for it, ever. `@node(schema:)` names
the PostgreSQL schema it lives in.

```graphql
type Stream @node(label: "stream", table: "streams", schema: "dbos") @readonly {
  id: ID!
  topic: String!
  seq: Int! @column(name: "offset")   # quoted correctly; `offset` is reserved
}
```

It is the per-type grain of `--no-tables`, and the two compose. Whether gopgql
owns a table cannot change in a delta — both directions are refused at generate
time, because neither has a migration that could be written for it.

`@node(schema:)` applies to any type, owned or not; gopgql emits no `CREATE
SCHEMA` either way, so the schema has to already exist.

A table you do not own may have no `id` at all, and gopgql cannot add one to it.
Declare `@key(fields:)` and those columns become the type's identity — what the
compiler projects, groups the response by, and compares between positions:

```graphql
type Step @node(label: "step", table: "operation_outputs", schema: "dbos") @readonly
  @key(fields: ["workflowUuid", "functionId"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  functionId: Int! @column(name: "function_id")
  output: String
}
```

For a type gopgql *owns* nothing changes: `id` stays the identity, and
`@key(fields:)` stays a uniqueness constraint alongside it.

An edge can be mapped onto an existing table too, which is how one table serves
as both a row and the join that reaches it:

```graphql
steps: [Step!]! @relationship(
  type: "has_step", direction: OUT
  table: "operation_outputs", schema: "dbos"
  sourceKey: ["workflow_uuid"], destKey: ["workflow_uuid", "function_id"]
)
```

Without `sourceKey:`/`destKey:`, a relationship touching a `@readonly` type is
refused rather than mis-generated: gopgql would otherwise have to create an edge
table referencing a table it does not own.

## A typed Go client

`gopgql generate client` reads the SDL and a directory of **named** GraphQL
operation documents, compiles every operation there and then, and writes a Go
package.

```
gopgql generate client --sdl schema.graphql --operations ops/ --out internal/gen
```

If you generated the migrations with `--graph`, pass the **same** name here —
the compiled SQL names the graph, and a mismatch is only discovered at query
time. See the [CLI reference](#cli-reference).

```go
func (c *Client) AppendEvent(ctx context.Context, h exec.Handle, in AppendEventInput) error
func (c *Client) ListPeople(ctx context.Context, h exec.Handle, in ListPeopleInput) ([]ListPeoplePerson, error)
```

The handle is the second parameter of every method, and the client opens no
connection and holds no pool — so an operation runs in whatever transaction you
are already in, and commits with it.

`exec.Handle` names no driver type. Adapt whichever handle you hold:

```go
exec.Pgx(pool)      // *pgxpool.Pool, *pgx.Conn or pgx.Tx
exec.Portable(tx)   // anything driver-agnostic, e.g. a dbos.Tx
```

`exec.PgxQuerier` and `exec.PortableQuerier` are the read-only halves, for a
caller that only runs queries.

`exec.Portable` is what lets a transaction opened by another framework run
gopgql's statements. Its type parameters are inferred, so a DBOS transaction
goes through unannotated and gopgql names no DBOS type:

```go
dbos.RunAsTransaction(ctx, ds, func(ctx context.Context, tx dbos.Tx) (T, error) {
    return client.AppendEvent(ctx, exec.Portable(tx), in)
})
```

The SQL is a `const` and results are assigned field by field by generated code;
nothing is compiled, parsed or reflected over at run time. A query that cannot
compile — an unknown root field, a selection past the depth ceiling — fails the
generate command, never a request. Generation is deterministic and needs no
database, so `go generate ./... && git diff --exit-code` is a usable CI gate.

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
gopgql generate --sdl <path> [--sdl <path>…] --dir <dir> [--name <suffix>]
                [--graph <name>] [--json-type <type>] [--no-tables] [--no-graph]
gopgql generate client --sdl <path> [--sdl <path>…] --operations <dir> --out <dir>
                [--package <name>] [--graph <name>]
gopgql migrate  --dsn <url> [--sdl <path>…] [--dir <dir>] [--name <suffix>]
                [--graph <name>] [--json-type <type>] [--no-tables] [--no-graph]
gopgql conform  --sdl <path> [--sdl <path>…] --dsn <url> [--graph <name>]
gopgql version
```

`--sdl`, `--dsn` and `--dir` fall back to `GOPGQL_SDL`, `GOPGQL_DSN` and
`GOPGQL_MIGRATIONS`; a flag wins over the environment. `--dir` defaults to
`migrations`.

### A schema in several files

**`--sdl` is repeatable, and it also takes a directory** whose `*.graphql` files
are read in sorted order (not recursively). Every document is parsed as one
schema:

```sh
gopgql generate --sdl schema/00-dbos.graphql --sdl schema/10-app.graphql --dir migrations
gopgql generate --sdl schema/ --dir migrations            # the same thing
```

The reason to want this is that **a property graph can only span two PostgreSQL
schemas if one schema describes both** — and the boundary between what a service
owns and migrates and what it only reads is usually the thing you least want to
lose. Splitting the SDL along that boundary keeps it visible without giving up
the graph that crosses it.

Splitting is purely editorial: generating from several files produces
byte-identical output to generating from those files concatenated, so there is
never a reason to concatenate them yourself. The files are merged before anything
is resolved, so a type may be referenced before the file declaring it is read —
the split follows ownership, not dependency order. What each file keeps is its
name, so a parse or validation error points at the file you have to open.

`GOPGQL_SDL` carries one path or several, separated the way your platform
separates a path list (`:` on Unix, `;` on Windows).

### `--json-type`

The `JSON` scalar maps to `jsonb`. `--json-type json` moves that default for the
whole schema, and `@column(type:)` still wins on any column that carries one:

```sh
gopgql generate --sdl schema.graphql --json-type json --dir migrations
```

`jsonb` is the default because it is what can be indexed and queried. What it
costs is byte-identical round trip — it sorts object keys, drops insignificant
whitespace and keeps only the last of a duplicated key — so a schema on a
round-trip path (a signed payload, a document whose hash is checked) wants
`json`. Setting it globally is the point: per column, the annotation that was
forgotten is invisible until a stored value has more than one key.

Changing it on a schema that is already deployed generates a real migration —
`ALTER TABLE … ALTER COLUMN … TYPE json USING …::json`, which keeps the column's
rows. It cannot un-normalise what `jsonb` already stored, though: only documents
written after the migration round-trip. `GOPGQL_JSON_TYPE` is the environment
equivalent.

**`--graph` has to match across commands.** It names the property graph, and it
defaults to `app_graph` everywhere it appears. `generate` bakes that name into
`CREATE PROPERTY GRAPH`; `generate client` bakes it into every `GRAPH_TABLE` it
compiles. Give it to one and not the other and nothing complains — the client
compiles cleanly against a graph that does not exist, and the first thing to
notice is PostgreSQL, at query time, in your process. Neither command can see
the other's output, so pass the same `--graph` to both (or to neither) and check
it with `gopgql conform`, which compares the SDL against the graph a live
database actually holds.

`--no-tables` generates the property graph over tables someone else owns —
gopgql then never reads, diffs or emits anything about a table, so absence from
the SDL is not evidence of absence from the database. `--no-graph` is the
mirror. Both scope a directory's *first* generation; after that its own history
decides, and a flag contradicting it is refused. With the tables half **on**,
gopgql is managing those tables, and a column absent from the SDL is a column it
will remove.

## Telemetry

Both binaries are instrumented with OpenTelemetry through
[goga](https://github.com/lega4e/goga)'s `telemetry` module: traces and
metrics for `gopgql generate`, `migrate` (the whole goose apply) and `conform`,
for the MCP server's `introspect` and `query` tools, and for the query and
connection paths underneath them — so a slow answer can be split into compiling,
querying and shaping rather than guessed at.

**Nothing is exported unless you ask for it.** These are short-lived processes —
an init container, a server on an agent's stdio — and a default that posted to a
collector nobody runs would print export failures next to the command's real
output. If the environment carries no `OTEL_` variable, every exporter is
`none`: the spans are still opened, they simply go nowhere. Set the standard
variables and they arrive:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 gopgql migrate --sdl schema.graphql
OTEL_TRACES_EXPORTER=console gopgql conform --sdl schema.graphql
```

From there the [OpenTelemetry environment
variables](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/)
mean exactly what the specification says they mean. `service.name` is the binary
(`gopgql` or `gopgql-mcp`) and `service.version` is its build version, the same
one `--version` prints.

The WASM playground has none of this: `cmd/wasm` reaches neither the MCP server
nor the pgx path, so the module the docs site ships carries no OpenTelemetry
code at all.

### Where the connection comes from

The read-only pool `exec.OpenReadOnly` returns is built by goga's
`database/pgxdb`, and `gopgql migrate`'s goose connection by goga's `database`.
Both arrive instrumented at the driver, so an individual `SELECT` or a single
applied migration is a span of its own underneath the operation that asked for
it — a trace of a `query` tool call reads

```
mcp.Query → exec.Query → query SELECT → prepare SELECT
```

with the pool's own statistics (acquired, idle, waiting) exported as metrics
beside them.

**What you hold is still pgx's.** `OpenReadOnly` returns a `*pgxpool.Pool`, and
`exec.Handle` still accepts a caller's own pool, connection or transaction:
goga's database module deliberately has no portable handle to move onto, so
`CopyFrom`, `SendBatch`, `LISTEN`/`NOTIFY` and pgx's native types stay directly
available and nothing needs unwrapping. What changed is where the pool is
constructed, not what flows through it.

One consequence worth knowing: `otelpgx` traces a statement only when the
context already carries a recording span. Everything gopgql runs is inside one —
`exec.Query`, `exec.Rows` and `OpenReadOnly` each open their own — but a query
you issue on the pool yourself, from a `context.Background()`, records nothing.
Wrap it in a span and it appears.

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
