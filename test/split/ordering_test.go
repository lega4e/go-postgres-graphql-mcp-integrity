package split_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/migrate"
)

// salariedPersonSDL is personSDL plus one more scalar. The generator turns it
// into a column *and* a graph property, so dropping it from the SDL changes
// both halves — which is what makes the apply order observable.
const salariedPersonSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  salary: Int
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// indexedPersonSDL is personSDL with an index on a field it already had. An
// index is a table change and nothing else: the CREATE PROPERTY GRAPH statement
// comes out identical, so the tables half gains a generation the graph half
// does not.
const indexedPersonSDL = `type Person @node(label: "person") {
  id: ID!
  name: String! @index
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// indexedSalariedPersonSDL is indexedPersonSDL with one more scalar: a
// generation that changes both halves, arriving after a generation that changed
// only the tables.
const indexedSalariedPersonSDL = `type Person @node(label: "person") {
  id: ID!
  name: String! @index
  salary: Int
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

const ada = "11111111-1111-1111-1111-111111111111"

// matchOnePersonName is the smallest query that proves the property graph is
// live over the tables.
const matchOnePersonName = `
	SELECT name FROM GRAPH_TABLE (app_graph
		MATCH (p IS person)
		COLUMNS (p.name AS name))`

// buildCLI compiles cmd/gopgql into a temporary directory.
//
// The ordering under test is the CLI's, so the suite drives the real binary.
// Re-implementing the walk in the test would prove only that the test agrees
// with itself.
func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gopgql")
	out, err := exec.Command("go", "build", "-o", bin,
		"github.com/lega4e/gopgql/cmd/gopgql").CombinedOutput()
	require.NoError(t, err, "build gopgql: %s", out)
	return bin
}

// gopgql runs the CLI and returns its combined output.
func gopgql(t *testing.T, bin string, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	t.Logf("$ gopgql %s\n%s", strings.Join(args, " "), out)
	return string(out), err
}

// writeSDL drops a schema file into dir and returns its path.
func writeSDL(t *testing.T, dir, name, source string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(source), 0o600), "write %s", name)
	return path
}

// readMigration returns the contents of the migration at version in dir.
func readMigration(t *testing.T, dir string, version int64) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, fmt.Sprintf("%04d_*.sql", version)))
	require.NoError(t, err, "glob %s", dir)
	require.Len(t, matches, 1, "exactly one migration at version %04d in %s", version, dir)
	data, err := os.ReadFile(matches[0]) //nolint:gosec // a path this test just generated
	require.NoError(t, err, "read %s", matches[0])
	return string(data)
}

// versionsOf is migrate.Versions with the error already asserted away.
func versionsOf(t *testing.T, dir string) []int64 {
	t.Helper()
	vs, err := migrate.Versions(dir)
	require.NoError(t, err, "versions of %s", dir)
	return vs
}

// TestEachGraphGenerationLandsOnItsOwnTables is the review comment on gopgql#38
// turned into an assertion: the halves are applied pairwise by generation —
// tables/0001, graph/0001, tables/0002, graph/0002 — not all of one and then
// all of the other.
//
// The difference is invisible on a first migration and fatal on a second. A
// graph migration names the columns of *its own* generation, so replaying
// graph/0001 once the tables have moved on to 0002 runs a historical graph
// definition against a current schema: it exposes a column 0002 dropped, and
// PostgreSQL refuses it. This test builds exactly that schema, so it fails
// under a global phase order and passes under the pairwise one.
func TestEachGraphGenerationLandsOnItsOwnTables(t *testing.T) {
	ctx := context.Background()
	pool, dsn := freshDB(t)
	bin := buildCLI(t)

	root := t.TempDir()
	dir := filepath.Join(root, "migrations")
	tablesDir := filepath.Join(dir, migrate.TablesDir)
	graphDir := filepath.Join(dir, migrate.GraphDir)

	// Generation one: salary is a column and a graph property.
	out, err := gopgql(t, bin, "generate",
		"--sdl", writeSDL(t, root, "v1.graphql", salariedPersonSDL), "--dir", dir)
	require.NoError(t, err, "generate generation one:\n%s", out)

	// Generation two: it is neither.
	out, err = gopgql(t, bin, "generate",
		"--sdl", writeSDL(t, root, "v2.graphql", personSDL), "--dir", dir)
	require.NoError(t, err, "generate generation two:\n%s", out)

	require.Equal(t, []int64{1, 2}, versionsOf(t, tablesDir), "two table generations")
	require.Equal(t, []int64{1, 2}, versionsOf(t, graphDir), "two graph generations")

	// The trap, stated rather than described: the first graph migration exposes
	// a column the second tables migration removes.
	require.Contains(t, readMigration(t, graphDir, 1), "PROPERTIES (id, name, salary)",
		"graph/0001 must expose the column generation two drops")
	require.Contains(t, readMigration(t, tablesDir, 2), "DROP COLUMN",
		"tables/0002 must drop it")

	out, err = gopgql(t, bin, "migrate", "--dsn", dsn, "--dir", dir)
	require.NoError(t, err,
		"applying every tables migration before any graph migration runs graph/0001 "+
			"against a schema that no longer has salary:\n%s", out)

	// The column generation two dropped is gone …
	var columns int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'persons' AND column_name = 'salary'`).Scan(&columns))
	assert.Zero(t, columns, "generation two drops the salary column")

	// … and the graph generation two describes is live over the tables.
	_, err = pool.Exec(ctx, `INSERT INTO persons (id, name) VALUES ($1, 'Ada')`, ada)
	require.NoError(t, err, "seed")
	var name string
	require.NoError(t, pool.QueryRow(ctx, matchOnePersonName).Scan(&name),
		"the property graph of the last generation must be live")
	assert.Equal(t, "Ada", name)

	// And a second run changes nothing. The graph only comes down when there is
	// table work pending behind it, so an already-migrated database keeps it.
	out, err = gopgql(t, bin, "migrate", "--dsn", dsn, "--dir", dir)
	require.NoError(t, err, "second migrate:\n%s", out)
	require.NoError(t, pool.QueryRow(ctx, matchOnePersonName).Scan(&name),
		"re-running migrate must leave the property graph up")
	assert.Equal(t, "Ada", name)
}

// TestTablesHalfMayOutrunTheGraphHalf covers the halves being different
// lengths. A change the graph statement does not mention — here an index —
// gives the tables half a generation the graph half has no counterpart for.
// The graph still has to come down for the tables to move, and nothing in the
// graph directory follows to put it back, so the applier rebuilds the graph the
// graph half still describes.
func TestTablesHalfMayOutrunTheGraphHalf(t *testing.T) {
	ctx := context.Background()
	pool, dsn := freshDB(t)
	bin := buildCLI(t)

	root := t.TempDir()
	dir := filepath.Join(root, "migrations")
	tablesDir := filepath.Join(dir, migrate.TablesDir)
	graphDir := filepath.Join(dir, migrate.GraphDir)

	out, err := gopgql(t, bin, "generate",
		"--sdl", writeSDL(t, root, "v1.graphql", personSDL), "--dir", dir)
	require.NoError(t, err, "generate generation one:\n%s", out)
	out, err = gopgql(t, bin, "generate",
		"--sdl", writeSDL(t, root, "v2.graphql", indexedPersonSDL), "--dir", dir)
	require.NoError(t, err, "generate generation two:\n%s", out)

	require.Equal(t, []int64{1, 2}, versionsOf(t, tablesDir), "an index is a table change")
	require.Equal(t, []int64{1}, versionsOf(t, graphDir), "an index changes no graph property")

	out, err = gopgql(t, bin, "migrate", "--dsn", dsn, "--dir", dir)
	require.NoError(t, err, "migrate uneven halves:\n%s", out)

	var indexes int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes
		WHERE tablename = 'persons' AND indexdef LIKE '%(name)'`).Scan(&indexes))
	assert.Equal(t, 1, indexes, "generation two adds the index")

	_, err = pool.Exec(ctx, `INSERT INTO persons (id, name) VALUES ($1, 'Ada')`, ada)
	require.NoError(t, err, "seed")
	var name string
	require.NoError(t, pool.QueryRow(ctx, matchOnePersonName).Scan(&name),
		"a tables generation with no graph counterpart must not leave the graph dropped")
	assert.Equal(t, "Ada", name)
}

// TestVersionNumbersAreGenerationsNotFileCounts guards the assumption the
// lockstep walk rests on: that tables/000N and graph/000N are the same edit of
// the SDL.
//
// They only are if the halves share a version counter. Numbered by their own
// file counts they drift, and the drift is silent until it is fatal: a
// generation that changes only the tables (here, an index) advances one half
// alone, so the *next* graph migration would be numbered 0002 while the tables
// migration it was generated against is numbered 0003. The applier, walking by
// version, would then apply that graph migration one generation early —
// against tables that do not yet have the column it exposes.
func TestVersionNumbersAreGenerationsNotFileCounts(t *testing.T) {
	ctx := context.Background()
	pool, dsn := freshDB(t)
	bin := buildCLI(t)

	root := t.TempDir()
	dir := filepath.Join(root, "migrations")
	tablesDir := filepath.Join(dir, migrate.TablesDir)
	graphDir := filepath.Join(dir, migrate.GraphDir)

	for i, source := range []string{personSDL, indexedPersonSDL, indexedSalariedPersonSDL} {
		out, err := gopgql(t, bin, "generate",
			"--sdl", writeSDL(t, root, fmt.Sprintf("v%d.graphql", i+1), source), "--dir", dir)
		require.NoError(t, err, "generate generation %d:\n%s", i+1, out)
	}

	// Generation two was an index: a table change with no graph counterpart.
	// Generation three changes both — and must be numbered three in both halves.
	require.Equal(t, []int64{1, 2, 3}, versionsOf(t, tablesDir))
	require.Equal(t, []int64{1, 3}, versionsOf(t, graphDir),
		"the graph half skips the generation it had nothing to say about, "+
			"rather than renumbering the next one down to 0002")
	require.Contains(t, readMigration(t, graphDir, 3), "salary",
		"graph/0003 exposes the column tables/0003 adds")

	out, err := gopgql(t, bin, "migrate", "--dsn", dsn, "--dir", dir)
	require.NoError(t, err, "migrate a history with a skipped generation:\n%s", out)

	_, err = pool.Exec(ctx, `INSERT INTO persons (id, name, salary) VALUES ($1, 'Ada', 120)`, ada)
	require.NoError(t, err, "seed")
	var salary int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT salary FROM GRAPH_TABLE (app_graph
			MATCH (p IS person)
			COLUMNS (p.salary AS salary))`).Scan(&salary),
		"the last generation's graph must expose the last generation's column")
	assert.Equal(t, 120, salary)
}
