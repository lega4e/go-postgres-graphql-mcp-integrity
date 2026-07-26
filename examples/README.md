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
.mcp.json           what an agent needs to connect
```

## Run one

```sh
cd examples/docs-graph
docker compose up -d --build      # postgres, migrate, seed
docker compose --profile mcp build # build the server image once
```

`init` runs `gopgql migrate --sdl schema.graphql`, which generates the initial
migration from the SDL and applies it with goose — tables, indexes and the
`CREATE PROPERTY GRAPH`. It is idempotent: goose skips versions it has already
applied, so re-running the stack is a no-op.

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

## Why the server is not started by `up`

MCP over stdio means one server process per client, spawned by the client on
its stdin/stdout. So the `mcp` service sits behind a compose profile and is
started on demand by `mcp.sh`, which is what `.mcp.json` runs:

```sh
exec docker compose run --rm -T --no-deps mcp
```

`--no-deps` matters: without it every spawn re-evaluates the
postgres → init → seed chain, which pushes the MCP handshake past two minutes.
Bring the stack up yourself first; the launcher then attaches in ~2 seconds.

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
