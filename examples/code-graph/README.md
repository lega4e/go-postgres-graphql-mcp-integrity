# code-graph — this repository, as a graph

The MCP server querying itself. Packages contain files, files declare symbols,
symbols call symbols, packages import packages:

```text
Package ──contains──▶ File ──declares──▶ Symbol ──calls──▶ Symbol
   │
   └──imports──▶ Package
```

The seed is a **hand-curated snapshot** of the packages this example ships
alongside — real paths, real line numbers, real call edges, taken from the
source at the commit that added it. It is a slice of the repository, not a
whole-repo index, and it does not update itself.

## Run it

```sh
docker compose up -d --build          # postgres, migrate, seed, and the server
claude --mcp-config .mcp.json         # the server is at http://localhost:8766/mcp
```

## Questions worth asking it

- *"What does `Server.Query` call?"* — the hub both tools go through.
- *"Which packages does `mcp` import, and are they wasm-safe?"* — `wasmSafe` is
  the property that decides where new code may live (SPEC.md §4.1), so this is
  a design question answered from the graph.
- *"What imports `exec`?"* — `importedBy`, i.e. what a change to the execution
  helper would disturb.

A worked exchange:

```graphql
{ packages(name: "mcp") { name wasmSafe imports { name wasmSafe } } }
```
```json
{"packages": [{
  "name": "mcp", "wasmSafe": false,
  "imports": [
    {"name": "exec", "wasmSafe": false},
    {"name": "compiler", "wasmSafe": true},
    {"name": "sdl", "wasmSafe": true}
  ]}]}
```

## The inner-join gotcha, in the place it bites

`MATCH` is an inner join, so a two-level traversal returns only paths that
exist all the way down:

```graphql
{ symbols(name: "Server.Query") { name calls { name } } }
# → introspector.execute, Compiler.CompileQuery, Query
```
```graphql
{ symbols(name: "Server.Query") { name calls { name calls { name } } } }
# → Query only
```

`introspector.execute` and `Compiler.CompileQuery` call nothing in this seed,
so at depth two they drop out entirely rather than coming back with an empty
`calls` list. That is SQL/PGQ's semantics rather than a server bug — but it is
not what a GraphQL client expects, so ask for the depth you want.
