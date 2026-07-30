# gopgql

Go library that treats a single annotated GraphQL SDL document as the source of
truth for both halves of a PostgreSQL 19 [SQL/PGQ](https://www.postgresql.org/)
property-graph application: it generates the DDL (vertex/edge tables and the
`CREATE PROPERTY GRAPH` mapping, as `goose` migrations) and compiles GraphQL
queries to `GRAPH_TABLE ... MATCH ... COLUMNS` statements.

See [`SPEC.md`](./SPEC.md) for the full design and milestone plan.

## Status

**M7 — SDL widening: full, plus conformance.** The SDL can now describe a
*schema*, not just a graph ([`SPEC.md` §5](./SPEC.md), [§7](./SPEC.md) → M7):

- **`@default(value:)`** is the column's DDL default, emitted **verbatim**:
  `joinedAt: DateTime! @default(value: "now()")` reaches the database as
  `DEFAULT now()`, so a row inserted without the column gets its value from
  PostgreSQL and not from anything gopgql supplies.
- **`@check(expr:)`** is a constraint the database enforces — on a field for a
  column-level check, on a type for one spanning columns. Every check is emitted
  under a name gopgql derives itself (`<table>_<column>_check`,
  `<table>_check_<n>`), because a later delta drops a constraint *by name* and an
  anonymous one would first have to be looked up in a live database.
- **Both are raw SQL, deliberately.** gopgql never parses an expression or a
  default: an invalid one is PostgreSQL's error at migration time, which is the
  right place and the right message. That also means they name **columns**, not
  GraphQL fields — a check on a field mapped with `@column(name: "left_at")`
  writes `left_at`. It is an escape hatch, defensible because whoever writes the
  SDL already owns the schema, and indefensible for user input.
- **`@key(fields:)`** is a natural key **alongside** the surrogate `id`, not a
  replacement for it. The table keeps `id uuid PRIMARY KEY`, gains
  `CONSTRAINT <table>_key UNIQUE (…)`, and the key's columns are listed in the
  property graph's `KEY (...)` clause — so a `MATCH` can select a vertex by its
  data while edge tables go on referencing `id`. Making a natural key *the*
  identity would rewrite the compiler's three `id` projection sites, `shape`'s
  parent dedup and every edge reference; it is recorded as an open question
  ([`SPEC.md` §9](./SPEC.md)) rather than smuggled into this milestone.
- **`@renamedFrom(name:)`** is a **hint, never an inference.** A differ that sees
  one name disappear and another appear cannot tell a rename from a genuine
  drop-and-add, and guessing wrong destroys the rows one way or loses them the
  other — so nothing is inferred, and without the hint the old behaviour stands.
  The hint carries the previous **GraphQL** name (a type name on a type, a field
  name on a field); gopgql derives the candidate physical names from it and
  accepts one only when the folded prior state actually holds it. A hint naming
  something the prior state does not have emits nothing, which is what lets the
  same SDL keep generating cleanly after its own rename has landed; a hint naming
  something the SDL *still declares* is an error, because that is two objects and
  not one moved one.
- **The fold learned to read a rename back — the invisible half.** Emitting
  `ALTER TABLE … RENAME` alone would be worse than not emitting it: prior state
  is reconstructed by re-parsing gopgql's own migrations, so the *next* delta
  would be computed as though the rename never happened and would drop the
  renamed column. `internal/ddl` grew `RENAME TO`, `RENAME COLUMN … TO` and
  `ADD`/`DROP CONSTRAINT` first, and `migrate.Fold` grew the visitor with them.
- **`gopgql conform`** closes the assumption the whole migration story rests on
  — that nobody alters the database out of band. It reflects the live property
  graph and reports **typed** findings, and its exit status is its answer
  ([below](#check-the-database-still-matches)).

The M7 suite runs each of the issue's acceptance criteria against a real
`postgres:19beta2` container, every test on its own freshly created database: a
`@check` rejects a violating row with SQLSTATE 23514 under the derived
constraint name; a natural-key vertex is **matched by its key columns**, on a
graph PostgreSQL accepts with the natural key as the vertex `KEY` while its
edges still reference the surrogate `id`; a duplicate natural key is refused by
the database; a rename is applied as `ALTER TABLE … RENAME` with the seeded rows
still present afterwards; folding the emitted migrations back across a rename
reconstructs the same schema as a direct apply; conformance is quiet on a clean
database and names the finding when a property is dropped from the graph behind
gopgql's back; and a row inserted without a column gets its declared default.
The playground's **Constraints** and **Conformance** tabs show the generated
DDL, the rename delta and the report's structure.

**M6 — SDL widening: moderate.** The SDL stops being structure-only
([`SPEC.md` §5](./SPEC.md), [§7](./SPEC.md) → M6):

- **`@column(name:)`** renames the physical column — and with it the property the
  graph exposes, so the compiler projects and filters on the column while the
  GraphQL field keeps its own name.
- **`@column(type:)`** overrides the default scalar mapping
  ([`SPEC.md` §5.1](./SPEC.md)): `price: Float! @column(type: "numeric(10,2)")`
  reaches the database as `numeric(10,2)` and round-trips exact values.
- **`@unique`** puts the constraint in the database, so a duplicate is rejected
  by PostgreSQL (SQLSTATE 23505) rather than by anything gopgql checks.
- **`@index(name:, using:)`** adds a secondary index, with the name derived from
  the table and column when it is omitted.
- **The differ follows.** A constraint or index added to an SDL becomes an
  `ALTER TABLE … ADD CONSTRAINT` / `CREATE INDEX` in the next delta, an index
  whose definition moved is dropped and recreated (PostgreSQL has no `ALTER` for
  either), and every Down section is the exact inverse.

The M6 godog scenarios read the column type back from the catalog, assert the
stored value as text so exactness is the database's word and not the driver's,
force a unique violation and check its SQLSTATE, and prove the index is both in
`pg_indexes` **and** chosen by the planner via `EXPLAIN` over 2 020 seeded rows.
A further scenario applies a delta that adds a constraint and an index, then
rolls it back and asserts both are gone. The playground's **Directives** tab
shows the DDL and compiled query for an editable SDL using all four directives.

**M5 — Multi-pattern workaround and joins.** A level may now select more than
one relationship ([`SPEC.md` §7](./SPEC.md) → M5). That shape would need
comma-separated path patterns in a single `MATCH`, which PG19 parses but does not
execute ([`SPEC.md` §2.2](./SPEC.md)), so the compiler splits it instead:

- **One `GRAPH_TABLE` per branch, joined on projected ids.** The chain up to the
  branching level stays a single call — the spine; each relationship hanging off
  it becomes its own call whose pattern re-binds the branch-point vertex by label
  and projects its id; the outer query `LEFT JOIN`s them on that id.
- **A missing branch no longer deletes the parent.** Joining left means a person
  with followers but nothing followed keeps both keys and shapes to an empty
  list, instead of vanishing the way an inner join would.
- **Guards survive the split.** An isomorphism guard between two positions that
  ended up in different calls moves to the join's `ON` clause, expressed over the
  projected ids — so a branch still cannot walk back to a vertex an ancestor
  bound.
- **Unbranched queries are untouched.** A single chain still compiles to exactly
  one `GRAPH_TABLE` with no join at all.

The M5 godog scenarios execute the split SQL against the container and assert on
the returned rows, and — the milestone's exit criterion — re-run an **equivalent
hand-written join** over the same data and require an identical response, so the
workaround is proven correct rather than merely runnable. A further scenario
joins `GRAPH_TABLE` output with an ordinary relational table. The playground's
**Multi-pattern** tab shows the emitted split for an editable branching query.

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
- **`migrate.Plan` / `migrate.Generate`** tie them together over a migration
  directory: fold what is there, work out which single-purpose migrations the
  change calls for, and write them numbered consecutively.

### No migration mixes tables with the graph

`gopgql generate --dir migrations` writes into `migrations/` itself. One edit of
the SDL emits a run of consecutive migrations, each doing exactly one thing:

```
migrations/0001_init_tables.sql             CREATE TABLE, CREATE INDEX
migrations/0002_init_graph.sql              CREATE PROPERTY GRAPH
migrations/0003_add_email_graph_down.sql    DROP PROPERTY GRAPH IF EXISTS
migrations/0004_add_email_tables.sql        ALTER TABLE
migrations/0005_add_email_graph.sql         CREATE PROPERTY GRAPH
```

One slug per generation, shared by all of its files, and a suffix saying what each
file does. The suffix is for humans — nothing is recorded inside a migration and
nothing is read back out of one. Each file's `Down` is the plain inverse of its own
`Up`, so `goose down` three times walks that generation back out in exactly
reverse order: new graph dropped, tables reverted, previous graph restored.

Either half can be turned off with `--no-tables` or `--no-graph`.

Two reasons this matters more than tidiness:

**Someone else may own the tables.** A database managed by Atlas, Flyway or a
DBA can still have a property graph over it: generate with `--no-tables` and
gopgql supplies the `CREATE PROPERTY GRAPH` and nothing else.

**The SDL may describe only part of a database.** A database can hold far more
than a service needs to expose, and the SDL is then the source of truth for the
slice that is surfaced as a graph — a read-only projection. With the tables half
off, **absence from the SDL is not evidence of absence from the database**:
gopgql never drops or alters a table or column it was not told about.

The asymmetry is worth stating plainly, because it is the one way to get this
wrong: with the tables half **on**, gopgql *is* managing those tables, and a
column absent from the SDL is a column it will remove.

#### `gopgql migrate` is a plain forward apply

Two ordering constraints come from PostgreSQL, not from gopgql:

1. **Tables before the graph that references them.** Creating a property graph
   over tables that do not exist is refused.
2. **The graph down before the columns it exposes change.** PostgreSQL will not
   drop or retype a column a live property graph exposes.

Both are satisfied by *where the files are and what they are numbered*. A
generation is emitted teardown → tables → build, so a `CREATE PROPERTY GRAPH` is
always immediately preceded by the table DDL of its own generation, and always
preceded by the drop of the graph the generation before it built.

So `gopgql migrate` is `goose up` over the one directory against the one
`goose_db_version` table. It does not interleave anything, does not walk versions,
and does not decide that any migration should be skipped — and `goose up` from an
empty database is correct by construction, because there is no other order in which
the files could be applied. Applying them with goose directly, or with any tool that
reads a goose directory, works exactly the same.

One caveat, documented rather than solved: **a generation is not atomic.** goose
runs each file in its own transaction, so an apply that stops between the teardown
and the rebuild leaves a database whose tables have moved and which has no property
graph. Queries against the graph fail loudly until it is back rather than returning
wrong rows, and `gopgql conform` reports the state directly — it exits 1 with
`property graph not found`.

Getting out of it depends on why the apply stopped:

- **Interrupted** — a crash, a `^C`, a killed pod. Re-running `gopgql migrate`
  continues from where it stopped.
- **The `_tables` migration failed** — a `NOT NULL` added over existing rows, a
  type change PostgreSQL refuses. Re-running fails the same way, because the DDL
  is the problem; and the graph teardown in front of it has already committed, so
  replaying from zero is no better. Take the step back with `goose down`, which
  restores the graph from the teardown's own `Down`, then fix the SDL and
  generate again.

#### The flags scope generation, and only the first one

`--no-tables` / `--no-graph` change what is **generated**; what is **applied** is
always the whole directory in version order. Both together is an error — it asks
for nothing.

They also scope a directory's **first** generation. After that the directory's own
history decides which halves it manages, and a flag that contradicts it is an
error: `--no-graph` against a history containing a `CREATE PROPERTY GRAPH`, or
`--no-tables` against a history that created tables, is refused at generate time
with nothing written. There is nothing recorded that could drift — the evidence is
the SQL in the directory, folded the way generation folds it anyway.

Turning a half off is not a way to delete anything. **To drop the property graph,
generate from a desired schema that declares no graph**: that generation emits the
`_graph_down` teardown and no rebuild, so the drop is recorded in the history and
reviewable in the diff.

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
`sdl`+`generator`+`migrate`+`compiler` in the browser as compiled Go. Each tab is
one complete, editable scenario — schema and query in on the left, **Generate**
in the middle, generated database schema and compiled query on the right. Every
pane is a CodeMirror 6 editor: GraphQL on the inputs, JSON on the variables, and
SQL on the generated output — the stock PostgreSQL dialect **extended** with
PostgreSQL 19's graph vocabulary (`GRAPH_TABLE`, `MATCH`, `COLUMNS`,
`CREATE PROPERTY GRAPH` …), which no published grammar covers yet:

| Tab           | Scenario                                                                                             |
|---------------|------------------------------------------------------------------------------------------------------|
| *Traversal*   | The three-hop exit query: one `GRAPH_TABLE`, bind parameters, isomorphism guards.                     |
| *Directives*  | The M6 mapping directives in the generated DDL: a renamed column, an overridden type, a UNIQUE, and two indexes. |
| *Constraints* | Two stacked scenarios: the M7 constraint surface — defaults, named checks, a natural key beside the surrogate `id` — with a query selecting a vertex **by that key**; then the same schema revised with `@renamedFrom`, whose delta *moves* the column. Delete the hint and watch the delta become `DROP COLUMN` + `ADD COLUMN`. |
| *Multi-pattern* | A branching query split into one `GRAPH_TABLE` per branch, `LEFT JOIN`ed on projected ids; the status line counts the calls. |
| *Depth limit* | A four-hop query refused with the typed `*DepthExceededError`; move the `MaxDepth` control and watch it flip. |
| *Interfaces*  | The shared `LABEL` clauses in the generated graph, and both interface mappings in the compiled pattern.|
| *Migration*   | Two stacked scenarios: the initial `0001_init.sql`, then a revised schema folded and diffed to a delta.|
| *Conformance* | The graph mapping `conform` compares — generated here, and visibly *not* containing the defaults, checks and unique the same schema declares — beside a **fixture** report showing all five finding kinds. A browser has no database, so this is the one output on the page that is not generated, and the panel says so. |

## Connect an AI agent (MCP)

`cmd/gopgql-mcp` serves one SDL schema and one database over the
[Model Context Protocol](https://modelcontextprotocol.io), so an agent can
discover what is queryable and query it:

```sh
go build -o gopgql-mcp ./cmd/gopgql-mcp
claude mcp add gopgql --env GOPGQL_DSN="$GOPGQL_DSN" -- ./gopgql-mcp --sdl schema.graphql
```

Pass the DSN through the environment, not `--dsn`: an MCP configuration file is
stored on disk and command-line arguments are visible to every process on the
machine, so a password in either is a password leaked. `--dsn` exists for a
local database that has no secret worth protecting. `--sdl` likewise falls back
to `GOPGQL_SDL`, and a flag wins over the environment.

The schema is parsed and validated, and the pool is pinged, **before** the
server starts serving, so a broken schema or an unreachable database is an exit
code rather than a tool that fails on every call.

Three runnable examples live in [`examples/`](./examples) — a PageIndex-style
documentation graph, this repository's own source graph, and a team-chat graph.
Each is a `docker compose` stack (Postgres + `gopgql migrate` + seed + the
server on the HTTP transport) with a `.mcp.json`, so
`cd examples/docs-graph && docker compose up -d --build && claude` is the whole
setup.

Two tools:

| Tool | What it does |
|------|--------------|
| `introspect` | Runs a standard GraphQL introspection query over the loaded schema. No arguments gives the overview — every root field, and every type by name and kind, without the types' field definitions. `type: "Person"` drills into one type; `full: true` returns the complete introspection result; `format: "sdl"` returns the document. |
| `query` | Compiles a GraphQL query, executes it, and returns the nested response. `variables` are bound as SQL parameters, never interpolated. `format: "markdown"` renders a table instead of JSON, and is refused for a selection that nests, because a table cannot represent it. |

Discovery is **standard GraphQL introspection**, not a gopgql dialect: the
`__schema`, `__type(name:)` and `__typename` meta-fields are answered through
the `query` tool too, served from the loaded schema without touching the
database. Both tool descriptions say how, so an agent that has only the tool
list can still reach a valid data query.

The server never migrates and never writes. The compiler emits nothing but a
`SELECT` over `GRAPH_TABLE`, there is no migration or mutation tool, and the
pool opens with `default_transaction_read_only=on` — so a write is refused by
the database itself. A read-only database role is recommended, not required.

## Check the database still matches

`gopgql conform` reads the property graph back out of a live database and
reports how it differs from the SDL:

```sh
gopgql conform --sdl schema.graphql --dsn "$GOPGQL_DSN"
```

Nothing else in gopgql can notice that the database stopped agreeing with the
SDL. Prior state is reconstructed by folding gopgql's own migrations in memory
([`SPEC.md` §3.1](./SPEC.md)), which is sound only while nobody alters the
database out of band — the generator, the differ and the compiler would go on
agreeing with each other indefinitely while the database quietly diverged.
`conform` is the check on that assumption.

The answer is the **exit status**, so it gates a pipeline with no wrapper
interpreting its output:

| Status | Meaning |
|--------|---------|
| `0` | The graph matches the SDL. |
| `1` | The check did not run: a schema that would not parse, a database it could not reach, a property graph it could not find. |
| `2` | The check ran, and the database has drifted. |

`1` and `2` are separate on purpose. "The database is unreachable" and "the
database has drifted" call for completely different next moves, and a CI step
should not have to parse English to tell them apart — the same reason the
findings are structured rather than prose. Every failure that is *not* a
verdict says `conformance check did not run` before anything else.

Findings print one per line, each carrying the kind a reader acts on, with
`SDL` and `DATABASE` naming which side said what and `-` meaning nothing there:

```
$ gopgql conform --sdl schema.graphql --dsn "$GOPGQL_DSN"; echo "exit $?"
KIND             ELEMENT    PROPERTY  SDL            DATABASE
MissingElement   companies  -         company        -
LabelMismatch    persons    -         actor, person  human
MissingProperty  persons    email     email          -

gopgql: compared elements, labels and properties only; defaults, constraints and indexes are not covered.
gopgql: property graph "app_graph" has drifted from schema.graphql: 3 findings
exit 2
```

The findings and the coverage note go to stdout; the one-line summary rides out
on stderr as the process's error, so redirecting the report to a file still
leaves the terminal saying what happened. The note is printed on *both*
outcomes, not only on drift — "conforms" is the sentence most likely to be
over-read, and a caveat that appears only when something is already wrong is a
caveat nobody sees.

`--sdl` and `--dsn` fall back to `GOPGQL_SDL` and `GOPGQL_DSN` as they do for
every other subcommand, and a flag wins over the environment. `--graph` names
the property graph when it is not the generator's default.

The check covers the **property graph**: which elements exist, the labels they
carry, and the properties they expose — the whole of what
`pg_propgraph_element`, `pg_propgraph_label` and `pg_propgraph_property`
record. Column defaults, `CHECK` and `UNIQUE` constraints, indexes and column
types live in other catalogs and are **not** compared. So an empty report means
the graph mapping matches the SDL; it does not mean the tables underneath it
do. Overstating that would be worse than the gap, because an operator would
stop looking.

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
| `exec/`          | Compiled query → `pgx` execution → shaped response; read-only pool.|
| `conform/`       | Reflect the live property graph; report drift as typed findings.  |
| `mcp/`           | MCP server: GraphQL introspection over the SDL + a query tool.     |
| `cmd/gopgql-mcp/`| The MCP server binary (stdio or HTTP).                             |
| `cmd/gopgql/`    | Schema CLI: generate migrations, apply them, check conformance.    |
| `examples/`      | Three runnable graphs (docs, code, chat) as docker compose stacks.|
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
go test ./compiler/... ./shape/... ./sdl/... ./generator/... ./migrate/... ./playground/... ./internal/... ./conform/... ./cmd/gopgql/...
```

## Playground locally

```sh
cd docs && npm install && npm run dev
```

`npm run dev` and `npm run build` stage `gopgql.wasm` first (a `pre*` hook
running `scripts/build-wasm.sh`), so the page is never served against a stale
module — it is a build artefact and is never committed (`SPEC.md` §8.3). The
page also refuses to start if the module it loads is older than the code
calling it, rather than silently ignoring arguments it does not understand.
