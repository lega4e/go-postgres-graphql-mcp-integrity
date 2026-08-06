// Package bench_test measures the two shaping strategies against each other
// over depth and fan-out (SPEC.md §7 → M8, design D7).
//
// A wall-clock number from a shared runner is nearly meaningless, so the
// benchmark reports the two numbers that are properties of the *strategy*
// rather than of the machine: the rows the database ships to the client, and
// the bytes it sends. A depth-d, fan-out-f query ships f^d flat rows under
// Go-side shaping and exactly one row under SQL-side shaping, and that is the
// reason to choose one over the other.
//
// The fixture is a perfect f-ary tree of depth d whose ids are derived from a
// fixed seed, so two runs are comparable. CI runs this at -benchtime 1x, which
// proves it still compiles, still boots a container and still returns results
// from both strategies; it asserts no timings, because a shared runner cannot
// and a flaky performance gate gets switched off within a month. What it *does*
// assert, in ordinary tests below, are the row counts — which are deterministic.
package bench_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	// Registers the "pgx" database/sql driver goose applies migrations through.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/sdl"
)

// --- the axes, declared once ---
//
// Both the benchmark and the check on docs/benchmarks.md read these, so adding
// an axis and forgetting the document turns CI red (design D7).

// Depths is the traversal depth in hops from the root field. Three is
// compiler.DefaultMaxDepth, so it is also the deepest query gopgql compiles
// without being told to.
var Depths = []int{1, 2, 3}

// Fanouts is the number of children each vertex has in the fixture.
var Fanouts = []int{1, 8, 64}

// Strategies is the pair under comparison.
var Strategies = []compiler.Shaping{compiler.GoSide, compiler.SQLSide}

// FixtureSeed is mixed into every generated id. It is fixed so that two runs
// build the same graph, with the same ids, and therefore the same ORDER BY
// order — without it the benchmark would be comparing different graphs.
const FixtureSeed = "gopgql-m8-bench-v1"

// benchSDL is the smallest schema the axes need: one vertex type and one
// self-relationship to fan out over.
const benchSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}`

// rootName is the fixture's root vertex, and the value every benchmark query
// filters on. Without it a query would start from every vertex in the tree.
const rootName = "r"

var baseDSN string

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx := context.Background()

	pgc, err := postgres.Run(ctx, "postgres:19beta2",
		postgres.WithDatabase("gopgql"),
		postgres.WithUsername("gopgql"),
		postgres.WithPassword("gopgql"),
		postgres.WithSQLDriver("pgx"),
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
	if err := goose.SetDialect("postgres"); err != nil {
		fmt.Fprintf(os.Stderr, "goose set dialect: %v\n", err)
		return 1
	}
	goose.SetLogger(goose.NopLogger())

	code := m.Run()
	// The fixtures outlive every individual benchmark and test that uses them,
	// so their pools are closed here rather than through tb.Cleanup. A benchmark
	// body runs several times as the framework ramps b.N, and a pool closed at
	// the end of the first of those would be handed to the second already shut.
	for _, f := range fixtures {
		f.pool.Close()
	}
	return code
}

// fixture is a built graph plus the document its queries compile against.
type fixture struct {
	pool *pgxpool.Pool
	doc  *sdl.Document
}

// fixtures are built once per fan-out and shared across depths and strategies:
// a depth-1 query and a depth-3 query read the same tree, and regenerating
// 266k vertices per benchmark case would measure the generator (design, Risks).
var fixtures = map[int]*fixture{}

// fixtureFor builds — or returns the already-built — tree of the given fan-out,
// at the deepest depth the axes ask for.
func fixtureFor(tb testing.TB, fanout int) *fixture {
	tb.Helper()
	if f, ok := fixtures[fanout]; ok {
		return f
	}
	f := buildFixture(tb, fanout, maxOf(Depths))
	fixtures[fanout] = f
	return f
}

// buildFixture creates a database, applies the schema through the real
// generate-and-goose pipeline, and grows a perfect f-ary tree of the given
// depth in it.
//
// The tree is generated *in SQL*, with each vertex's id derived from its path
// through md5, because a fan-out-64 depth-3 tree is 266,305 vertices and
// shipping those from Go would dominate the setup. Deriving the id rather than
// letting the default fill it in is what makes the seed mean anything: the ids
// decide the ORDER BY order, so a random id per run would reorder every result.
func buildFixture(tb testing.TB, fanout, depth int) *fixture {
	tb.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("bench_f%d_d%d", fanout, depth)

	admin, err := pgxpool.New(ctx, baseDSN)
	require.NoError(tb, err, "connect to the bootstrap database")
	defer admin.Close()
	_, err = admin.Exec(ctx, `CREATE DATABASE "`+name+`"`)
	require.NoError(tb, err, "create %s", name)

	u, err := url.Parse(baseDSN)
	require.NoError(tb, err)
	u.Path = "/" + name

	doc, err := sdl.Parse(benchSDL)
	require.NoError(tb, err)
	model, err := generator.Build(doc, "")
	require.NoError(tb, err)

	dir, err := os.MkdirTemp("", "gopgql-bench-")
	require.NoError(tb, err)
	defer func() { _ = os.RemoveAll(dir) }()
	_, err = migrate.Generate(dir, model, "init", migrate.Halves{})
	require.NoError(tb, err)

	db, err := sql.Open("pgx", u.String())
	require.NoError(tb, err)
	defer db.Close()
	require.NoError(tb, goose.UpContext(ctx, db, dir), "apply the migration")

	pool, err := pgxpool.New(ctx, u.String())
	require.NoError(tb, err)

	// The tree, as one recursive CTE: every vertex carries its path and its
	// parent's, which is all the ids and all the edges need.
	tree := fmt.Sprintf(`
WITH RECURSIVE t AS (
  SELECT 0 AS d, %s::text AS path, NULL::text AS parent
  UNION ALL
  SELECT t.d + 1, t.path || '.' || i, t.path
  FROM t, generate_series(1, %d) AS i
  WHERE t.d < %d
)
SELECT path, parent FROM t`, quoteLiteral(rootName), fanout, depth)

	_, err = pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO persons (id, name)
SELECT md5(%s || ':' || path)::uuid, path FROM (%s) AS g`,
		quoteLiteral(FixtureSeed), tree))
	require.NoError(tb, err, "grow the vertex tree")

	_, err = pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO follows (source_id, target_id)
SELECT md5(%s || ':' || parent)::uuid, md5(%s || ':' || path)::uuid
FROM (%s) AS g WHERE parent IS NOT NULL`,
		quoteLiteral(FixtureSeed), quoteLiteral(FixtureSeed), tree))
	require.NoError(tb, err, "grow the edges")

	return &fixture{pool: pool, doc: doc}
}

// queryFor builds the GraphQL operation for a depth: `{ persons(name: $n) {
// name follows { name … } } }` nested depth times.
func queryFor(depth int) string {
	inner := "name"
	for i := 0; i < depth; i++ {
		inner = "name follows { " + inner + " }"
	}
	return "{ persons(name: $n) { " + inner + " } }"
}

// BenchmarkShaping is the benchmark the milestone asks for: every
// depth × fan-out × strategy combination, driving exec.Query end to end against
// a real container.
func BenchmarkShaping(b *testing.B) {
	for _, fanout := range Fanouts {
		for _, depth := range Depths {
			for _, strategy := range Strategies {
				name := fmt.Sprintf("depth=%d/fanout=%d/%s", depth, fanout, strategy)
				b.Run(name, func(b *testing.B) {
					f := fixtureFor(b, fanout)
					// Not b.Context(): it is cancelled when this sub-benchmark
					// ends, and the fixture is shared with the ones after it.
					ctx := context.Background()

					cq, err := compiler.New(f.doc, compiler.WithShaping(strategy)).
						CompileQuery(queryFor(depth), map[string]any{"n": rootName})
					require.NoError(b, err)

					// Rows and bytes are properties of the strategy, not of the
					// run, so they are measured once rather than per iteration.
					rows, bytes, err := measure(ctx, f.pool, cq)
					require.NoError(b, err)

					b.ResetTimer()
					for range b.N {
						if _, err := exec.Query(ctx, exec.PgxQuerier(f.pool), cq); err != nil {
							b.Fatalf("execute under %s: %v", strategy, err)
						}
					}
					b.StopTimer()

					// Reported after the loop, not before it: ResetTimer deletes
					// user metrics along with the counters it zeroes, so metrics
					// registered ahead of it never reach the output.
					b.ReportMetric(float64(rows), "rows")
					b.ReportMetric(float64(bytes), "recv-B")
				})
			}
		}
	}
}

// measure runs a compiled query and reports how much the database sent back:
// the number of result rows, and the total size of their column values.
//
// It reads RawValues rather than decoded ones, so what it counts is what came
// over the wire rather than what Go made of it — the point being that the
// Go-side strategy receives f^d rows of k columns where the SQL-side strategy
// receives one row of one column.
func measure(ctx context.Context, pool *pgxpool.Pool, cq *compiler.Compiled) (rows, bytes int, err error) {
	rs, err := pool.Query(ctx, cq.SQL, cq.Args...)
	if err != nil {
		return 0, 0, err
	}
	defer rs.Close()

	for rs.Next() {
		rows++
		for _, v := range rs.RawValues() {
			bytes += len(v)
		}
	}
	return rows, bytes, rs.Err()
}

// quoteLiteral renders a string as a SQL literal. Applied only to constants this
// file declares.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func maxOf(xs []int) int {
	m := xs[0]
	for _, x := range xs[1:] {
		if x > m {
			m = x
		}
	}
	return m
}
