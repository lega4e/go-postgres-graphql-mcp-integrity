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
docker-compose.yml  postgres + generate + migrate + seed + mcp
.mcp.json           the URL an agent connects to
```

## Run one

```sh
cd examples/docs-graph
docker compose up -d --build      # postgres, generate, migrate, seed, the server
```

Generating and applying are **two** services, because they are two jobs — and
because what generation produces is **two** migration histories, not one:

```sh
docker compose logs generate
# two migration histories, in the order they are applied:
# /migrations/tables:     ← CREATE TABLE, CREATE INDEX
# 0001_init.sql
# /migrations/graph:      ← CREATE PROPERTY GRAPH
# 0001_init.sql

docker compose logs migrate
# gopgql: applied /migrations/tables up to 0001
# gopgql: applied /migrations/graph up to 0001
```

`generate` waits for nothing: the next migration is derived from the SDL and the
migrations already on disk, so it needs no database at all. `migrate` needs one,
and applies the two halves **in lockstep, one generation at a time** —
`tables/0001`, `graph/0001`, `tables/0002`, `graph/0002`, … Each
`CREATE PROPERTY GRAPH` therefore lands on the tables of its own generation,
rather than a historical graph definition being replayed against a schema that
has since moved on. `seed` waits for `migrate` to finish, so it never runs
against a half-applied schema.

The migrations live on a named volume, so the history accumulates. Edit
`schema.graphql`, `docker compose up -d` again, and whichever half actually
changed gains a `0002` delta. The other simply has no file at `0002` — the two
share one version counter, so a number is a generation of the SDL rather than a
count of that half's own files. Re-running with nothing to do applies nothing
and leaves the property graph up. `docker compose down -v` discards the
migrations along with the database.

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
