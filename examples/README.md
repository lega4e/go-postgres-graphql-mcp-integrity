# Examples

Three graphs, each a self-contained `docker compose` stack you can point an
agent at:

| Example | The graph | What it is good at showing |
|---|---|---|
| [`docs-graph/`](./docs-graph) | A documentation index in [PageIndex](https://github.com/VectifyAI/PageIndex)'s shape — documents, a tree of sections, and citations **between** documents | The citation edges a per-document tree cannot express |
| [`code-graph/`](./code-graph) | This repository's own MCP server: packages, files, symbols, calls, imports | Call chains and blast radius — "what reaches the database?" |
| [`slack-graph/`](./slack-graph) | Team chat: channels, members, threads, messages, mentions, thread→thread references | How people get pulled across team boundaries |

Every example has the same four pieces, so once you have read one compose file
you have read all three:

```text
schema.graphql      the SDL — the whole mapping
seed.sql            the corpus
docker-compose.yml  postgres + init (gopgql migrate) + seed + mcp
.mcp.json           the URL an agent connects to
```

## Run one

```sh
cd examples/docs-graph
docker compose up -d --build      # postgres, migrate, seed, and the server
```

`init` runs `gopgql migrate --sdl schema.graphql`, which generates the migrations
the SDL calls for and applies them with goose. **One** step, because that is all
it takes — even though a generation is several files:

```text
/tmp/migrations/0001_schema_tables.sql   CREATE TABLE, CREATE INDEX
/tmp/migrations/0002_schema_graph.sql    CREATE PROPERTY GRAPH
```

No migration mixes table DDL with property-graph DDL, and the files are numbered
in the order they have to run in. A later edit of the SDL that changes a table
emits three — the graph taken down, the tables migrated, the graph rebuilt over
them — again numbered consecutively, because PostgreSQL will not alter a column a
live property graph exposes. So `gopgql migrate` is a plain forward apply in
version order: it does not interleave anything and does not skip anything, and
`goose up` over the directory does exactly the same thing.

It is idempotent: goose skips versions it has already applied, so re-running the
stack is a no-op and the property graph stays up. The `--dir` is ephemeral, so the
whole history is regenerated from the SDL each run and re-applied from zero — which
works only because the history is in one directory in chronological order.

One caveat: goose runs each file in its own transaction, so a generation is not
atomic. An interrupted apply can stop between the graph teardown and the rebuild;
re-running `gopgql migrate` continues from where it stopped.

Then point an agent at it. Each example ships a `.mcp.json`, so from inside the
example directory:

```sh
claude   # picks up ./.mcp.json
```

or explicitly, without a project config:

```sh
claude --mcp-config .mcp.json -p "introspect the schema, then find …"
```

Ask it to `introspect` first — the tool descriptions tell it how — and then to
`query`. It never needs the SDL file.

## The server is an ordinary container

The examples run `gopgql-mcp --transport http`, so the server is a long-lived
service like any other — several agents can connect to one process, and
`.mcp.json` is just a URL:

```json
{"mcpServers": {"gopgql-docs": {"type": "http", "url": "http://localhost:8765/mcp"}}}
```

| Example | MCP URL | Postgres |
|---|---|---|
| docs-graph | `http://localhost:8765/mcp` | `localhost:55432` |
| code-graph | `http://localhost:8766/mcp` | `localhost:55433` |
| slack-graph | `http://localhost:8767/mcp` | `localhost:55434` |

Both ports bind to loopback only. `/healthz` answers the compose healthcheck
without opening an MCP session, so `docker compose ps` tells you when the
server is actually ready.

The binary still speaks stdio — that is the default, and it is what
`claude mcp add gopgql -- gopgql-mcp --sdl …` uses for a server an agent owns
and spawns itself. `--transport http` is for a server that outlives its
clients, which is what a compose stack is.

## Poking at the database directly

Each example publishes Postgres on its own port (`55432` docs, `55433` code,
`55434` slack), so you can check what the agent is seeing:

```sh
psql postgres://gopgql:gopgql@localhost:55432/gopgql
```

## One thing to know before you write a deep query

`MATCH` is an **inner join**. A two-level traversal returns only the paths that
exist all the way down — a middle node with no edge at the next level drops out,
and takes its branch with it. In the code graph:

```graphql
{ symbols(name: "Server.Query") { name calls { name } } }
# → introspector.execute, Compiler.CompileQuery, Query   (all three)

{ symbols(name: "Server.Query") { name calls { name calls { name } } } }
# → Query only — the other two call nothing, so they vanish rather than
#   coming back with an empty `calls` list
```

That is SQL/PGQ's semantics, not a bug in the server, but it is not what a
GraphQL client expects, so ask for the depth you actually want.
