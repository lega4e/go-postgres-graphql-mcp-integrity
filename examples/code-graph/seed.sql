-- The gopgql MCP server, as a graph. A hand-curated snapshot of the packages
-- this example ships alongside, taken from the source at the commit that added
-- it: real paths, real line numbers, real call edges. It is a slice, not a
-- whole-repo index — enough to make the traversals honest.
--
-- Ids are fixed and readable: p… packages, f… files, s… symbols.

BEGIN;

INSERT INTO packages (id, name, path, purpose, "wasmSafe") VALUES
  ('a0000000-0000-0000-0000-000000000001', 'mcp',   'mcp',              'MCP server: GraphQL introspection over the SDL, plus a query tool.', false),
  ('a0000000-0000-0000-0000-000000000002', 'exec',  'exec',             'Compiled query → pgx execution → shaped response; opens the read-only pool.', false),
  ('a0000000-0000-0000-0000-000000000003', 'compiler', 'compiler',      'GraphQL operation → GRAPH_TABLE SQL + ordered bind parameters.', true),
  ('a0000000-0000-0000-0000-000000000004', 'shape', 'shape',            'Flat rows → the nested GraphQL response.', true),
  ('a0000000-0000-0000-0000-000000000005', 'sdl',   'sdl',              'Parse and validate SDL; the typed directive/mapping model.', true),
  ('a0000000-0000-0000-0000-000000000006', 'main',  'cmd/gopgql-mcp',   'The MCP server binary: one SDL, one DSN, stdio transport.', false),
  ('a0000000-0000-0000-0000-000000000007', 'main',  'cmd/gopgql',       'The schema CLI: generate migrations from SDL and apply them.', false),
  ('a0000000-0000-0000-0000-000000000008', 'generator', 'generator',    'SDL model → schema model → DDL (CREATE …, property graph).', true),
  ('a0000000-0000-0000-0000-000000000009', 'migrate', 'migrate',        'Emit goose migrations; fold prior migrations and diff deltas.', true)
ON CONFLICT DO NOTHING;

INSERT INTO files (id, path, lines) VALUES
  ('b0000000-0000-0000-0000-000000000001', 'mcp/server.go', 399),
  ('b0000000-0000-0000-0000-000000000002', 'mcp/query.go', 370),
  ('b0000000-0000-0000-0000-000000000003', 'mcp/introspection.go', 568),
  ('b0000000-0000-0000-0000-000000000004', 'exec/exec.go', 152),
  ('b0000000-0000-0000-0000-000000000005', 'shape/shape.go', 71),
  ('b0000000-0000-0000-0000-000000000006', 'compiler/compiler.go', 680),
  ('b0000000-0000-0000-0000-000000000007', 'cmd/gopgql-mcp/main.go', 98),
  ('b0000000-0000-0000-0000-000000000008', 'cmd/gopgql/main.go', 173)
ON CONFLICT DO NOTHING;

INSERT INTO contains (source_id, target_id) VALUES
  ('a0000000-0000-0000-0000-000000000001', 'b0000000-0000-0000-0000-000000000001'),
  ('a0000000-0000-0000-0000-000000000001', 'b0000000-0000-0000-0000-000000000002'),
  ('a0000000-0000-0000-0000-000000000001', 'b0000000-0000-0000-0000-000000000003'),
  ('a0000000-0000-0000-0000-000000000002', 'b0000000-0000-0000-0000-000000000004'),
  ('a0000000-0000-0000-0000-000000000004', 'b0000000-0000-0000-0000-000000000005'),
  ('a0000000-0000-0000-0000-000000000003', 'b0000000-0000-0000-0000-000000000006'),
  ('a0000000-0000-0000-0000-000000000006', 'b0000000-0000-0000-0000-000000000007'),
  ('a0000000-0000-0000-0000-000000000007', 'b0000000-0000-0000-0000-000000000008')
ON CONFLICT DO NOTHING;

INSERT INTO imports (source_id, target_id) VALUES
  ('a0000000-0000-0000-0000-000000000001', 'a0000000-0000-0000-0000-000000000002'),
  ('a0000000-0000-0000-0000-000000000001', 'a0000000-0000-0000-0000-000000000003'),
  ('a0000000-0000-0000-0000-000000000001', 'a0000000-0000-0000-0000-000000000005'),
  ('a0000000-0000-0000-0000-000000000002', 'a0000000-0000-0000-0000-000000000003'),
  ('a0000000-0000-0000-0000-000000000002', 'a0000000-0000-0000-0000-000000000004'),
  ('a0000000-0000-0000-0000-000000000003', 'a0000000-0000-0000-0000-000000000005'),
  ('a0000000-0000-0000-0000-000000000004', 'a0000000-0000-0000-0000-000000000003'),
  ('a0000000-0000-0000-0000-000000000006', 'a0000000-0000-0000-0000-000000000001'),
  ('a0000000-0000-0000-0000-000000000006', 'a0000000-0000-0000-0000-000000000002'),
  ('a0000000-0000-0000-0000-000000000006', 'a0000000-0000-0000-0000-000000000005'),
  ('a0000000-0000-0000-0000-000000000007', 'a0000000-0000-0000-0000-000000000008'),
  ('a0000000-0000-0000-0000-000000000007', 'a0000000-0000-0000-0000-000000000009'),
  ('a0000000-0000-0000-0000-000000000007', 'a0000000-0000-0000-0000-000000000005'),
  ('a0000000-0000-0000-0000-000000000008', 'a0000000-0000-0000-0000-000000000005'),
  ('a0000000-0000-0000-0000-000000000009', 'a0000000-0000-0000-0000-000000000008')
ON CONFLICT DO NOTHING;

INSERT INTO symbols (id, name, kind, signature, doc, line) VALUES
  ('c0000000-0000-0000-0000-000000000001', 'New', 'func',
   'New(doc *sdl.Document, sdlSource string, db exec.Querier, opts ...Option) *Server',
   'Builds a server over a parsed SDL document and a database handle, and registers both tools.', 79),
  ('c0000000-0000-0000-0000-000000000002', 'Server.register', 'method',
   '(s *Server) register()',
   'Declares both tools with the input schemas and descriptions a client needs to call them without guessing.', 141),
  ('c0000000-0000-0000-0000-000000000003', 'Server.handleQuery', 'method',
   '(s *Server) handleQuery(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error)',
   'The query tool''s MCP handler: decodes the arguments and reports failures as tool errors.', 216),
  ('c0000000-0000-0000-0000-000000000004', 'Server.Introspect', 'method',
   '(s *Server) Introspect(typeName string, full bool, format string) (string, error)',
   'Answers the introspect tool by issuing one of four standard introspection queries. Never touches the database.', 234),
  ('c0000000-0000-0000-0000-000000000005', 'Server.Query', 'method',
   '(s *Server) Query(ctx context.Context, query string, vars map[string]any, format string) (string, error)',
   'Compiles a GraphQL operation, executes it, and renders the response. Introspection is answered from the schema.', 52),
  ('c0000000-0000-0000-0000-000000000006', 'newIntrospector', 'func',
   'newIntrospector(doc *sdl.Document, sdlSource string) *introspector',
   'Builds the introspection view of a document: the synthesized Query root, and mapped types minus their @ignore fields.', 47),
  ('c0000000-0000-0000-0000-000000000007', 'introspector.execute', 'method',
   '(in *introspector) execute(op *ast.OperationDefinition, frags ast.FragmentDefinitionList, vars map[string]any) (map[string]any, error)',
   'Runs an introspection operation: aliases, arguments, variables, fragment definitions and inline fragments.', 404),
  ('c0000000-0000-0000-0000-000000000008', 'Query', 'func',
   'Query(ctx context.Context, db Querier, cq *compiler.Compiled) (map[string]any, error)',
   'Executes a compiled query and returns the nested GraphQL response.', 36),
  ('c0000000-0000-0000-0000-000000000009', 'Rows', 'func',
   'Rows(ctx context.Context, db Querier, sql string, args ...any) ([]map[string]any, error)',
   'Executes a statement and returns its rows as column-name maps — the flat form the shaper consumes.', 51),
  ('c0000000-0000-0000-0000-000000000010', 'scan', 'func',
   'scan(rows pgx.Rows) ([]map[string]any, error)',
   'Drains a result set into one map per row, keyed by output column name.', 100),
  ('c0000000-0000-0000-0000-000000000011', 'OpenReadOnly', 'func',
   'OpenReadOnly(ctx context.Context, dsn string, opts ...PoolOption) (*pgxpool.Pool, error)',
   'Opens a pool whose every session starts with default_transaction_read_only=on, and pings it.', 75),
  ('c0000000-0000-0000-0000-000000000012', 'shape.Rows', 'func',
   'Rows(proj compiler.Projection, rows []map[string]any) map[string]any',
   'Regroups flat result rows into the nested GraphQL response.', 25),
  ('c0000000-0000-0000-0000-000000000013', 'group', 'func',
   'group(sel *compiler.Selection, rows []map[string]any) []any',
   'Buckets rows by a level''s key column so every parent appears exactly once with its children nested beneath it.', 35),
  ('c0000000-0000-0000-0000-000000000014', 'Compiler.CompileQuery', 'method',
   '(c *Compiler) CompileQuery(op string, vars map[string]any) (*Compiled, error)',
   'Compiles an operation and returns the SQL, bind parameters and the projection used to shape the result.', 210),
  ('c0000000-0000-0000-0000-000000000015', 'run', 'func',
   'run(argv []string) error',
   'The MCP binary''s entry point: parse the SDL, open the read-only pool, serve MCP over stdio.', 44)
ON CONFLICT DO NOTHING;

INSERT INTO declares (source_id, target_id) VALUES
  ('b0000000-0000-0000-0000-000000000001', 'c0000000-0000-0000-0000-000000000001'),
  ('b0000000-0000-0000-0000-000000000001', 'c0000000-0000-0000-0000-000000000002'),
  ('b0000000-0000-0000-0000-000000000001', 'c0000000-0000-0000-0000-000000000003'),
  ('b0000000-0000-0000-0000-000000000001', 'c0000000-0000-0000-0000-000000000004'),
  ('b0000000-0000-0000-0000-000000000002', 'c0000000-0000-0000-0000-000000000005'),
  ('b0000000-0000-0000-0000-000000000003', 'c0000000-0000-0000-0000-000000000006'),
  ('b0000000-0000-0000-0000-000000000003', 'c0000000-0000-0000-0000-000000000007'),
  ('b0000000-0000-0000-0000-000000000004', 'c0000000-0000-0000-0000-000000000008'),
  ('b0000000-0000-0000-0000-000000000004', 'c0000000-0000-0000-0000-000000000009'),
  ('b0000000-0000-0000-0000-000000000004', 'c0000000-0000-0000-0000-000000000010'),
  ('b0000000-0000-0000-0000-000000000004', 'c0000000-0000-0000-0000-000000000011'),
  ('b0000000-0000-0000-0000-000000000005', 'c0000000-0000-0000-0000-000000000012'),
  ('b0000000-0000-0000-0000-000000000005', 'c0000000-0000-0000-0000-000000000013'),
  ('b0000000-0000-0000-0000-000000000006', 'c0000000-0000-0000-0000-000000000014'),
  ('b0000000-0000-0000-0000-000000000007', 'c0000000-0000-0000-0000-000000000015')
ON CONFLICT DO NOTHING;

-- The call graph. `Server.Query` is the hub: it is what both tools go through,
-- and it is two hops from there to the only code that touches the database.
INSERT INTO calls (source_id, target_id) VALUES
  ('c0000000-0000-0000-0000-000000000015', 'c0000000-0000-0000-0000-000000000001'),  -- run → mcp.New
  ('c0000000-0000-0000-0000-000000000015', 'c0000000-0000-0000-0000-000000000011'),  -- run → OpenReadOnly
  ('c0000000-0000-0000-0000-000000000001', 'c0000000-0000-0000-0000-000000000002'),  -- New → register
  ('c0000000-0000-0000-0000-000000000001', 'c0000000-0000-0000-0000-000000000006'),  -- New → newIntrospector
  ('c0000000-0000-0000-0000-000000000003', 'c0000000-0000-0000-0000-000000000005'),  -- handleQuery → Server.Query
  ('c0000000-0000-0000-0000-000000000004', 'c0000000-0000-0000-0000-000000000005'),  -- Introspect → Server.Query
  ('c0000000-0000-0000-0000-000000000005', 'c0000000-0000-0000-0000-000000000007'),  -- Server.Query → introspector.execute
  ('c0000000-0000-0000-0000-000000000005', 'c0000000-0000-0000-0000-000000000014'),  -- Server.Query → CompileQuery
  ('c0000000-0000-0000-0000-000000000005', 'c0000000-0000-0000-0000-000000000008'),  -- Server.Query → exec.Query
  ('c0000000-0000-0000-0000-000000000008', 'c0000000-0000-0000-0000-000000000009'),  -- exec.Query → exec.Rows
  ('c0000000-0000-0000-0000-000000000008', 'c0000000-0000-0000-0000-000000000012'),  -- exec.Query → shape.Rows
  ('c0000000-0000-0000-0000-000000000009', 'c0000000-0000-0000-0000-000000000010'),  -- exec.Rows → scan
  ('c0000000-0000-0000-0000-000000000012', 'c0000000-0000-0000-0000-000000000013')   -- shape.Rows → group
ON CONFLICT DO NOTHING;

COMMIT;
