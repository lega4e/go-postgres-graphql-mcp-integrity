// Package seed_test is the contract between an example SDL in the playground
// package and the seed fixture that sits beside it.
//
// The playground's Run button executes three things in a browser: the DDL
// playground.Schema generates, then the scenario's seed, then the compiled
// GRAPH_TABLE query with its bind values. Nothing in a unit test can tell
// whether that sequence actually works — a seed naming a column the generator
// renamed, or a row that fails to satisfy a query's isomorphism guards, is
// indistinguishable from a correct one until a real PostgreSQL runs it.
//
// So each test here runs exactly that sequence against a real postgres:19beta2
// container and asserts the query returned rows. A seed that drifts from the
// SDL it belongs to fails here, in CI, rather than in a reader's browser.
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
// compiled query — for every runnable tab, and requires rows. Zero rows is a
// legitimate outcome for an edited schema, but never for the defaults: a
// playground whose Run button demonstrates an empty table demonstrates nothing.
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

			rows, err := conn.Query(ctx, compiled.SQL, compiled.Args...)
			require.NoError(t, err, "execute:\n%s\nwith %v", compiled.SQL, compiled.Args)
			defer rows.Close()

			n := 0
			for rows.Next() {
				n++
			}
			require.NoError(t, rows.Err(), "read rows")
			assert.Positive(t, n,
				"the default inputs must return rows; seed and query have drifted apart")
		})
	}
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
