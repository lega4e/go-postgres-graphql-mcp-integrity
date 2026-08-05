// Package seed_test is the contract between an example SDL in the playground
// package and the seed fixture that sits beside it.
//
// The playground's Run button is four steps in a browser: the DDL
// playground.Schema generates, then the tab's data SQL — the seed, and whatever
// the reader edited it into — then the compiled GRAPH_TABLE query with its bind
// values, then playground.Shape regrouping the flat rows into the nested
// GraphQL response. Nothing in a unit test can tell whether that sequence
// actually works: a seed naming a column the generator renamed, a row that
// fails to satisfy a query's isomorphism guards, or a projection that no longer
// lines up with the result set is indistinguishable from a correct one until a
// real PostgreSQL runs it.
//
// So each test here runs exactly that sequence against a real postgres:19beta2
// container and asserts on the response it produced. A seed that drifts from
// the SDL it belongs to fails here, in CI, rather than in a reader's browser.
//
// This is deliberately *not* a test of PGlite. It proves the fixtures are
// correct against the PostgreSQL they were written for; that the same sequence
// runs on the pinned wasm build is proven in the browser suite under
// docs/e2e, which is the only place it can be.
package seed_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lega4e/gopgql/playground"
)

// baseDSN points at the container's bootstrap database; each test creates its
// own database through it.
var baseDSN string

// TestMain boots one postgres:19beta2 container for the package. There is no
// skip path when Docker is absent (SPEC.md §10): a suite that quietly passes
// without a database would assert nothing at all here, since every claim it
// makes is a claim about what PostgreSQL did.
func TestMain(m *testing.M) { os.Exit(runSuite(m)) }

// runSuite exists so the container's cleanup runs before os.Exit, which a defer
// in TestMain would not.
func runSuite(m *testing.M) int {
	ctx := context.Background()

	pgc, err := postgres.Run(ctx, "postgres:19beta2",
		postgres.WithDatabase("gopgql"),
		postgres.WithUsername("gopgql"),
		postgres.WithPassword("gopgql"),
		tc.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(3*time.Minute),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres:19beta2 container: %v\n", err)
		return 1
	}
	defer func() {
		if err := pgc.Terminate(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "terminate container: %v\n", err)
		}
	}()

	baseDSN, err = pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		return 1
	}
	return m.Run()
}

var (
	dbUnsafe = regexp.MustCompile(`[^a-z0-9]+`)
	dbSeq    atomic.Int64
)

// freshConn hands the test its own empty database, so no scenario's rows can
// make another scenario's assertion accidentally true. Two example schemas both
// generate a `persons` table, which is exactly how that would happen.
func freshConn(t *testing.T) (context.Context, *pgx.Conn) {
	t.Helper()
	ctx := t.Context()

	slug := strings.Trim(dbUnsafe.ReplaceAllString(strings.ToLower(t.Name()), "_"), "_")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	name := fmt.Sprintf("%s_%d", slug, dbSeq.Add(1))

	admin, err := pgx.Connect(ctx, baseDSN)
	require.NoError(t, err, "connect to the bootstrap database")
	_, err = admin.Exec(ctx, `CREATE DATABASE "`+name+`"`)
	require.NoError(t, err, "create database %s", name)
	require.NoError(t, admin.Close(ctx))

	conn, err := pgx.Connect(ctx, replaceDBName(baseDSN, name))
	require.NoError(t, err, "connect to %s", name)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return ctx, conn
}

// replaceDBName swaps the database out of the container's DSN. The container
// module hands back postgres://user:pass@host:port/gopgql?sslmode=disable, so
// the path segment is the only part that changes.
func replaceDBName(dsn, name string) string {
	head, tail, ok := strings.Cut(dsn, "?")
	if !ok {
		head, tail = dsn, ""
	}
	head = head[:strings.LastIndex(head, "/")+1] + name
	if tail == "" {
		return head
	}
	return head + "?" + tail
}

// scenario is one playground tab's inputs: the SDL, its seed, the query, and
// the variables the page binds. It is the whole of what a Run submits.
type scenario struct {
	name  string
	sdl   string
	seed  string
	query string
	vars  map[string]any
	// maxDepth overrides the compiler's default ceiling, for the one tab whose
	// point is that the ceiling is movable. Zero means "use the default".
	maxDepth int
}

// scenarios mirrors the runnable tabs on the page. Traversal and Multi-pattern
// share ExampleSDL and therefore ExampleSeed, which is why that seed has to
// satisfy both queries at once — a three-hop chain from Alice *and* an incoming
// follow to her.
var scenarios = []scenario{
	{
		name:  "traversal",
		sdl:   playground.ExampleSDL,
		seed:  playground.ExampleSeed,
		query: playground.ExampleQuery,
		vars:  map[string]any{"n": "Alice"},
	},
	{
		name:  "multipattern",
		sdl:   playground.ExampleSDL,
		seed:  playground.ExampleSeed,
		query: playground.ExampleMultiPatternQuery,
		vars:  map[string]any{"n": "Alice"},
	},
	{
		name:  "directives",
		sdl:   playground.ExampleDirectivesSDL,
		seed:  playground.ExampleDirectivesSeed,
		query: playground.ExampleDirectivesQuery,
		vars:  map[string]any{"t": "Chain"},
	},
	{
		name:  "constraints",
		sdl:   playground.ExampleConstraintsSDL,
		seed:  playground.ExampleConstraintsSeed,
		query: playground.ExampleConstraintsQuery,
		vars:  map[string]any{"t": "acme", "e": "ada@acme.example"},
	},
	{
		name:  "interfaces",
		sdl:   playground.ExampleInterfaceSDL,
		seed:  playground.ExampleInterfaceSeed,
		query: playground.ExampleInterfaceQuery,
		vars:  nil,
	},
	{
		// The Shaping tab reads ExampleSDL but deliberately not ExampleSeed. Its
		// query fans out on two branches at once, and the chain above gives each
		// branch exactly one edge — a 1×1 cross-product, which is the single
		// case where the two strategies' result sets look the same and the tab
		// demonstrates nothing.
		name:  "shaping",
		sdl:   playground.ExampleSDL,
		seed:  playground.ExampleShapingSeed,
		query: playground.ExampleShapingQuery,
		vars:  map[string]any{"n": "Alice"},
	},
	{
		// The Depth tab refuses its query at the default ceiling, which is the
		// point of the tab — but a reader who raises the ceiling gets a query,
		// and it has to find something. Four hops from Alice with five distinct
		// vertices is what ExampleSeed's chain is long enough for; a shorter
		// chain would return nothing here and nowhere else, which is exactly
		// the kind of gap that only shows up in a browser.
		name:     "depth, ceiling raised",
		sdl:      playground.ExampleSDL,
		seed:     playground.ExampleSeed,
		query:    playground.ExampleDeepQuery,
		vars:     map[string]any{"n": "Alice"},
		maxDepth: 4,
	},
}

// TestSeededScenarioReturnsRows runs the page's own sequence — schema, seed,
// compiled query, shaped response — for every runnable tab, and requires rows.
// Zero rows is a legitimate outcome for an edited schema, but never for the
// defaults: a playground whose Run button demonstrates an empty table
// demonstrates nothing.
//
// The shaping step is asserted here for the same reason the rows are. A
// projection that named a column the query no longer projects would shape into
// an object of nulls — a response that looks like a response and carries
// nothing — and nothing before the browser would notice.
func TestSeededScenarioReturnsRows(t *testing.T) {
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			ctx, conn := freshConn(t)

			ddl, err := playground.Schema(s.sdl)
			require.NoError(t, err, "Schema")
			// pgx sends a statement-free Exec over the simple protocol, which
			// is what allows the whole multi-statement document in one call —
			// the same way the page hands it to PGlite.
			_, err = conn.Exec(ctx, ddl)
			require.NoError(t, err, "apply the generated DDL:\n%s", ddl)

			_, err = conn.Exec(ctx, s.seed)
			require.NoError(t, err, "apply the seed:\n%s", s.seed)

			depth := s.maxDepth
			if depth == 0 {
				depth = playground.MaxDepth()
			}
			compiled, err := playground.CompileWithMaxDepth(s.sdl, s.query, s.vars, depth)
			require.NoError(t, err, "Compile")

			res := query(ctx, t, conn, compiled)
			assert.NotEmpty(t, res.Rows,
				"the default inputs must return rows; seed and query have drifted apart")

			shaped, err := playground.Shape(compiled.Projection, res)
			require.NoError(t, err, "Shape")
			assert.NotEmpty(t, rootList(t, shaped),
				"rows came back but the response is empty; the projection and the "+
					"result set have drifted apart")
		})
	}
}

// query runs a compiled query and returns its result set in the positional form
// the page posts back across the WASM boundary — the same shape, built the same
// way, so what is shaped here is what would be shaped in a browser.
func query(ctx context.Context, t *testing.T, conn *pgx.Conn, compiled playground.Compiled) playground.Result {
	t.Helper()

	rows, err := conn.Query(ctx, compiled.SQL, compiled.Args...)
	require.NoError(t, err, "execute:\n%s\nwith %v", compiled.SQL, compiled.Args)
	defer rows.Close()

	res := playground.Result{}
	for _, fd := range rows.FieldDescriptions() {
		res.Columns = append(res.Columns, fd.Name)
	}
	for rows.Next() {
		vals, err := rows.Values()
		require.NoError(t, err, "read row")
		res.Rows = append(res.Rows, vals)
	}
	require.NoError(t, rows.Err(), "read rows")
	return res
}

// rootList unwraps a shaped response to the root field's objects. A shaped
// response always has exactly one key — the root field's response key — because
// the compiler accepts exactly one root field.
func rootList(t *testing.T, shaped map[string]any) []any {
	t.Helper()
	require.Len(t, shaped, 1, "a response carries exactly one root field")
	for _, v := range shaped {
		list, ok := v.([]any)
		require.True(t, ok, "the root field's value is a list of objects")
		return list
	}
	return nil
}

// TestReadersDataChangesReachTheResponse is the page's Data pane, proven
// against a real PostgreSQL.
//
// Every runnable tab lets a reader edit the SQL that runs between the generated
// schema and the query — that is the page's write path, because a SQL/PGQ
// property graph is a read-only view and nothing gopgql compiles writes
// *through the graph* (SPEC.md §2.2). gopgql does compile writes since M11, but
// only a `@function` call the SDL declares, and the playground's tabs declare
// none: the reader's edit is meant to be arbitrary SQL. What has to hold is that
// a write reaches the *response* — not the table, which is PostgreSQL's
// business, but the shaped GraphQL document the page renders, which is the only
// thing the reader sees.
//
// An UPDATE through the vertex table and an INSERT into the edge table are both
// exercised, because they land in different places in the response — one
// changes a property, the other changes how many children a parent has.
func TestReadersDataChangesReachTheResponse(t *testing.T) {
	ctx, conn := freshConn(t)

	ddl, err := playground.Schema(playground.ExampleSDL)
	require.NoError(t, err, "Schema")
	_, err = conn.Exec(ctx, ddl)
	require.NoError(t, err, "apply the generated DDL")
	_, err = conn.Exec(ctx, playground.ExampleSeed)
	require.NoError(t, err, "apply the seed")

	// One hop, so the assertion is about Alice and who she follows and nothing
	// deeper.
	const oneHop = `{ persons(name: $n) { name follows { name } } }`
	vars := map[string]any{"n": "Alice"}

	compiled, err := playground.Compile(playground.ExampleSDL, oneHop, vars)
	require.NoError(t, err, "Compile")

	before, err := playground.Shape(compiled.Projection, query(ctx, t, conn, compiled))
	require.NoError(t, err, "Shape")
	assert.Equal(t, []string{"Bob"}, followedNames(t, before),
		"the seed gives Alice exactly one outgoing follow")

	// What a reader types into the Data pane: rename a vertex, and add an edge.
	_, err = conn.Exec(ctx, `
UPDATE persons SET name = 'Robert'
 WHERE id = 'a0000000-0000-4000-8000-000000000002';
INSERT INTO follows (source_id, target_id) VALUES
  ('a0000000-0000-4000-8000-000000000001', 'a0000000-0000-4000-8000-000000000003');`)
	require.NoError(t, err, "apply the reader's data changes")

	after, err := playground.Shape(compiled.Projection, query(ctx, t, conn, compiled))
	require.NoError(t, err, "Shape")
	assert.Equal(t, []string{"Robert", "Carol"}, followedNames(t, after),
		"the response reflects both the UPDATE and the INSERT")

	// And still one Alice: the second edge fans the result out to two rows, and
	// shaping is what collapses them back to one parent.
	assert.Len(t, rootList(t, after), 1)
}

// followedNames reads the `name` of every person nested under the first (and
// only) root object.
func followedNames(t *testing.T, shaped map[string]any) []string {
	t.Helper()

	list := rootList(t, shaped)
	require.Len(t, list, 1, "the query filters to one person")
	person, ok := list[0].(map[string]any)
	require.True(t, ok)
	follows, ok := person["follows"].([]any)
	require.True(t, ok, "the relationship field is a list")

	names := make([]string, 0, len(follows))
	for _, f := range follows {
		obj, ok := f.(map[string]any)
		require.True(t, ok)
		name, ok := obj["name"].(string)
		require.True(t, ok, "name is a text property")
		names = append(names, name)
	}
	return names
}

// TestShapingSeedShowsBothStrategiesAgreeing is the Shaping tab's Run button,
// proven against a real PostgreSQL: one database, the same query compiled under
// each strategy, and the two responses compared.
//
// It is what the panel now claims, and the two halves of the claim are asserted
// separately because they are separate facts. The *result sets* differ, and
// visibly — four flat rows against one row of one column — which is the reason
// the two strategies exist. The *responses* do not differ at all. A seed that
// drifted into a 1×1 fan-out would still pass the second assertion while making
// the tab pointless, so the first one is what keeps the fixture honest.
//
// test/parity is the milestone's proof across the whole catalogue; this is the
// narrower claim that the fixtures the page ships with demonstrate it.
func TestShapingSeedShowsBothStrategiesAgreeing(t *testing.T) {
	ctx, conn := freshConn(t)

	ddl, err := playground.Schema(playground.ExampleSDL)
	require.NoError(t, err, "Schema")
	_, err = conn.Exec(ctx, ddl)
	require.NoError(t, err, "apply the generated DDL")
	_, err = conn.Exec(ctx, playground.ExampleShapingSeed)
	require.NoError(t, err, "apply the shaping seed")

	vars := map[string]any{"n": "Alice"}
	goSide, err := playground.CompileWithShaping(
		playground.ExampleSDL, playground.ExampleShapingQuery, vars, false)
	require.NoError(t, err, "CompileWithShaping(Go-side)")
	sqlSide, err := playground.CompileWithShaping(
		playground.ExampleSDL, playground.ExampleShapingQuery, vars, true)
	require.NoError(t, err, "CompileWithShaping(SQL-side)")

	goRes := shapedQuery(ctx, t, conn, goSide)
	sqlRes := shapedQuery(ctx, t, conn, sqlSide)

	assert.Len(t, goRes.Rows, 4,
		"Alice follows two people and is followed by two others, so the Go-side "+
			"statement's LEFT JOIN of the branches is 2×2 rows; a seed that stopped "+
			"fanning out would leave the tab demonstrating nothing")
	assert.Len(t, sqlRes.Rows, 1, "the SQL-side statement returns one row")
	assert.Len(t, sqlRes.Columns, 1, "of one response column")

	parity, err := playground.ShapeParity(
		playground.ExampleSDL, playground.ExampleShapingQuery, vars, goRes, sqlRes)
	require.NoError(t, err, "ShapeParity")
	assert.True(t, parity.Identical,
		"the two strategies must shape into the same response\nGo-side:\n%s\nSQL-side:\n%s",
		parity.GoJSON, parity.SQLJSON)
	assert.JSONEq(t,
		`{"persons":[{"name":"Alice",`+
			`"follows":[{"name":"Bob"},{"name":"Carol"}],`+
			`"followedBy":[{"name":"Dave"},{"name":"Erin"}]}]}`,
		parity.GoJSON,
		"and it is the response the seed describes, not merely the same wrong one twice")
}

// shapedQuery runs a strategy-compiled statement and returns its result in the
// positional form the page posts back across the WASM boundary. It is `query`
// for a playground.Shaped rather than a playground.Compiled — the two carry the
// same SQL and Args, and keeping them apart avoids a conversion whose only
// purpose would be to satisfy a signature.
func shapedQuery(ctx context.Context, t *testing.T, conn *pgx.Conn, compiled playground.Shaped) playground.Result {
	t.Helper()
	return query(ctx, t, conn, playground.Compiled{SQL: compiled.SQL, Args: compiled.Args})
}

// TestSeedDependsOnPhysicalColumnNames pins the one way a seed silently goes
// wrong that the row count above would not catch on its own. The directives
// example renames `title` to `name` with @column(name:), so a seed written
// against the GraphQL field would fail to apply — and this is the assertion
// that says the failure is the point, not an accident of how the fixture reads.
func TestSeedDependsOnPhysicalColumnNames(t *testing.T) {
	ctx, conn := freshConn(t)

	ddl, err := playground.Schema(playground.ExampleDirectivesSDL)
	require.NoError(t, err, "Schema")
	_, err = conn.Exec(ctx, ddl)
	require.NoError(t, err, "apply the generated DDL")

	assert.Contains(t, playground.ExampleDirectivesSeed, "name",
		"the seed inserts the physical column")
	assert.NotContains(t, playground.ExampleDirectivesSeed, "title",
		"the GraphQL field name is not a column and must not appear")

	_, err = conn.Exec(ctx, playground.ExampleDirectivesSeed)
	require.NoError(t, err, "the seed must apply to the DDL beside it")

	// The renamed column is the one the compiled query filters on, so proving
	// the seed reached it proves the whole rename survives to a result.
	var title string
	require.NoError(t,
		conn.QueryRow(ctx, `SELECT name FROM products WHERE sku = 'CHN-11S'`).Scan(&title))
	assert.Equal(t, "Chain", title)
}
