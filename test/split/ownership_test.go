package split_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/migrate"
)

// The two defects #40 shipped and this branch fixes, proven against a real
// database rather than against the packages in isolation (gopgql#38).
//
// Both are here rather than in a unit test for the same reason: what makes them
// worth fixing is what they do to a *database*, and neither `migrate` nor
// `cmd/gopgql` can observe that. The ownership one emits SQL that a unit test
// would happily accept — it is well-formed, and it is only against a database
// carrying the tables it re-adds that it fails, and only *after* the graph
// teardown in front of it has committed. The env-var one is a parse, but its
// consequence is a migration directory that permanently manages the wrong half.

// foreignPersonTables creates, by hand, the tables personSDL describes — as
// Atlas, Flyway or a DBA would have. They are the precondition for a graph-only
// history: gopgql supplies the mapping and never the schema, which is exactly why
// its fold of that history has no columns in it.
func foreignPersonTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE persons (id uuid PRIMARY KEY, name text NOT NULL)`,
		`CREATE TABLE follows (
			source_id uuid NOT NULL REFERENCES persons (id),
			target_id uuid NOT NULL REFERENCES persons (id),
			PRIMARY KEY (source_id, target_id)
		)`,
		`INSERT INTO persons (id, name) VALUES ('` + ada + `', 'Ada')`,
	} {
		_, err := pool.Exec(context.Background(), stmt)
		require.NoError(t, err, "seed: %s", stmt)
	}
}

// graphIsLive reports whether the property graph answers a MATCH.
//
// It asks the graph rather than the catalogue because that is the question an
// operator has after a half-applied generation: not "is there a row in
// pg_propgraph_element" but "do my queries work".
func graphIsLive(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var n int
	return pool.QueryRow(context.Background(), `
		SELECT count(*) FROM GRAPH_TABLE (app_graph
			MATCH (p IS person) COLUMNS (p.id AS id))`).Scan(&n) == nil
}

// graphOnlyHistory builds a directory whose whole history is a property graph
// over tables it did not create, applies it, and returns the directory.
//
// This is the state both halves of the ownership defect start from, and the only
// state that produces it: a fold of these migrations knows the vertex tables by
// name and knows none of their columns.
func graphOnlyHistory(t *testing.T, bin, root, dsn string) string {
	t.Helper()
	dir := filepath.Join(root, "migrations")
	out, err := gopgql(t, bin, "generate", "--no-tables",
		"--sdl", writeSDL(t, root, "schema.graphql", personSDL), "--dir", dir)
	require.NoError(t, err, "generate the graph half:\n%s", out)
	require.Equal(t, []string{"0001_schema_graph.sql"}, migrationNames(t, dir),
		"a graph-only history is one file and says nothing about a table")
	require.NoError(t, applyDir(t, dsn, dir), "apply the graph half")
	return dir
}

// writeUnguardedGeneration writes into dir the generation migrate.Generate would
// have written if its ownership check did not fire.
//
// It is deliberately migrate's own code with one thing removed: Generate folds
// the directory, plans against the desired schema, and writes each file under
// migrate.Migration.Filename. Everything below that first line is that, verbatim
// — so what this produces is the shipped behaviour of #40, not a test's guess at
// it.
func writeUnguardedGeneration(t *testing.T, dir string) []migrate.Migration {
	t.Helper()
	prior, err := migrate.FoldContent(readMigrations(t, dir))
	require.NoError(t, err, "fold the graph-only history")

	// The history is consecutive from 1, so the count is the last version.
	planned := migrate.Plan(prior, mustModel(t, personSDL), "oops", len(migrationNames(t, dir))+1,
		migrate.Halves{})
	require.NotEmpty(t, planned, "the unguarded path must have something to write")
	for _, m := range planned {
		require.NoError(t,
			os.WriteFile(filepath.Join(dir, m.Filename()), []byte(m.Content()), 0o600),
			"write %s", m.Filename())
	}
	return planned
}

// TestAdoptingTheTablesHalfIsRefusedBeforeAnythingIsWritten is the integration
// proof for the defect #43 fixes: a directory whose history never created tables
// may not start managing them.
//
// The refusal is cheap to assert and proves little on its own — a check that
// refuses everything would pass it. So the test first *demonstrates the hazard*,
// against this same database, by writing the generation the unguarded code wrote
// and applying it. That half is the point: the failure is not a rejected
// migration, it is a committed graph teardown followed by a failed ALTER, which
// leaves the database with no property graph and the directory unapplyable both
// forwards and from zero. Nothing short of a real database shows that.
//
// This follows TestReplayFromZeroReproducesTheSchema's rule: assert the trap is
// present before relying on the guard that disarms it.
func TestAdoptingTheTablesHalfIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	bin := buildCLI(t)

	t.Run("without the guard the graph is dropped and the directory is stuck", func(t *testing.T) {
		pool, dsn := freshDB(t, "trap")
		foreignPersonTables(t, pool)
		root := t.TempDir()
		dir := graphOnlyHistory(t, bin, root, dsn)
		require.True(t, graphIsLive(t, pool), "the graph-only history must leave a working graph")

		planned := writeUnguardedGeneration(t, dir)
		require.Equal(t, []string{
			"0002_oops_graph_down.sql",
			"0003_oops_tables.sql",
			"0004_oops_graph.sql",
		}, plannedNames(planned), "the unguarded path tears the graph down before the ALTER")

		// The trap, stated rather than described: the tables file re-adds a column
		// the database already has. Without this the apply below might fail for some
		// other reason and prove nothing.
		tables := readMigrations(t, dir)[2]
		require.Contains(t, tables, "ADD COLUMN name",
			"0003 must re-add a column the foreign tables already have")

		err := applyDir(t, dsn, dir)
		require.Error(t, err, "the unguarded generation must fail against the real tables")
		assert.Contains(t, err.Error(), "already exists",
			"it fails because the fold of a graph-only history has no columns to diff against")

		// The damage, which is the whole reason this is refused at generate time.
		assert.False(t, graphIsLive(t, pool),
			"the teardown committed before the ALTER failed: the database is left with no property graph")
		assert.Error(t, applyDir(t, dsn, dir),
			"re-running does not recover — the DDL is what fails, and it fails identically")

		// And replaying the directory from zero is no better: same files, same
		// collision, same place.
		replay, replayDSN := freshDB(t, "replay")
		foreignPersonTables(t, replay)
		assert.Error(t, applyDir(t, replayDSN, dir),
			"a directory in this state is unapplyable from zero as well as forwards")
		assert.False(t, graphIsLive(t, replay), "and the replay leaves that database graphless too")
	})

	t.Run("the guard refuses at generate time with nothing written", func(t *testing.T) {
		pool, dsn := freshDB(t, "guarded")
		foreignPersonTables(t, pool)
		root := t.TempDir()
		dir := graphOnlyHistory(t, bin, root, dsn)
		before := migrationNames(t, dir)

		out, err := gopgql(t, bin, "generate", "--name", "oops",
			"--sdl", filepath.Join(root, "schema.graphql"), "--dir", dir)
		require.Error(t, err, "turning the tables half on over a graph-only history must be refused:\n%s", out)

		// The refusal has to name the half and the direction, because the operator's
		// next move differs per case — and it has to name the way forward.
		assert.Contains(t, out, "the tables half is on")
		assert.Contains(t, out, "no migration in this directory creates tables")
		assert.Contains(t, out, "fresh")
		assert.Contains(t, out, "--no-tables",
			"the message must name the flag that keeps this directory working")

		assert.Equal(t, before, migrationNames(t, dir), "nothing may be written")
		assert.True(t, graphIsLive(t, pool),
			"and the database must be untouched — the graph the refusal protected is still there")

		// The escape hatch the message names actually works.
		out, err = gopgql(t, bin, "generate", "--no-tables",
			"--sdl", filepath.Join(root, "schema.graphql"), "--dir", dir)
		require.NoError(t, err, "the graph half alone must keep working:\n%s", out)
		assert.Contains(t, out, "already up to date")
		assert.Equal(t, before, migrationNames(t, dir))
	})
}

// TestNoHalfEnvVarsAreParsedAsBooleans is the integration proof for the second
// defect: GOPGQL_NO_TABLES was read with `!= ""`, so `false` — exactly what a
// compose file or a Helm values block writes for "off" — turned the tables half
// *off*.
//
// It runs the real binary against a real database in both directions, because
// the consequence is not a wrong flag value but a migration directory that
// permanently manages the wrong half: the first generation fixes what a directory
// owns, so a misread env var on day one cannot be corrected on day two.
//
// The `true` case is the trap check. A `false` case alone would pass just as
// happily if the variable were ignored altogether, which is a different bug with
// the same symptom; asserting that `true` still turns the half off is what makes
// the `false` case mean the variable was read *and* parsed.
func TestNoHalfEnvVarsAreParsedAsBooleans(t *testing.T) {
	bin := buildCLI(t)

	t.Run("GOPGQL_NO_TABLES=false keeps the tables half on", func(t *testing.T) {
		pool, dsn := freshDB(t, "false")
		root := t.TempDir()
		dir := filepath.Join(root, "migrations")

		out, err := gopgqlEnv(t, bin, []string{"GOPGQL_NO_TABLES=false"}, "migrate",
			"--dsn", dsn, "--dir", dir, "--name", "init",
			"--sdl", writeSDL(t, root, "schema.graphql", personSDL))
		require.NoError(t, err, "false must not turn the tables half off:\n%s", out)

		assert.Equal(t, []string{"0001_init_tables.sql", "0002_init_graph.sql"},
			migrationNames(t, dir), "both halves must be generated")

		// End to end: the tables gopgql created carry data, and the graph over them
		// answers a MATCH.
		_, err = pool.Exec(context.Background(),
			`INSERT INTO persons (id, name) VALUES ($1, 'Ada')`, ada)
		require.NoError(t, err, "the tables half must have run")
		var name string
		require.NoError(t, pool.QueryRow(context.Background(), matchOnePersonName).Scan(&name),
			"the graph half must have run over them")
		assert.Equal(t, "Ada", name)
	})

	t.Run("GOPGQL_NO_TABLES=true still turns the tables half off", func(t *testing.T) {
		pool, dsn := freshDB(t, "true")
		foreignPersonTables(t, pool)
		root := t.TempDir()
		dir := filepath.Join(root, "migrations")

		out, err := gopgqlEnv(t, bin, []string{"GOPGQL_NO_TABLES=true"}, "migrate",
			"--dsn", dsn, "--dir", dir, "--name", "init",
			"--sdl", writeSDL(t, root, "schema.graphql", personSDL))
		require.NoError(t, err, "true must turn the tables half off:\n%s", out)

		assert.Equal(t, []string{"0001_init_graph.sql"}, migrationNames(t, dir),
			"the graph half alone — without this the false case above proves nothing")
		assert.True(t, graphIsLive(t, pool), "over the tables it did not create")
	})

	t.Run("GOPGQL_NO_GRAPH=false keeps the graph half on", func(t *testing.T) {
		pool, dsn := freshDB(t, "nograph")
		root := t.TempDir()
		dir := filepath.Join(root, "migrations")

		out, err := gopgqlEnv(t, bin, []string{"GOPGQL_NO_GRAPH=false"}, "migrate",
			"--dsn", dsn, "--dir", dir, "--name", "init",
			"--sdl", writeSDL(t, root, "schema.graphql", personSDL))
		require.NoError(t, err, "false must not turn the graph half off:\n%s", out)

		assert.Equal(t, []string{"0001_init_tables.sql", "0002_init_graph.sql"},
			migrationNames(t, dir))
		assert.True(t, graphIsLive(t, pool))
	})

	t.Run("a value that is not a boolean is an error, not a guessed default", func(t *testing.T) {
		root := t.TempDir()
		out, err := gopgqlEnv(t, bin, []string{"GOPGQL_NO_TABLES=maybe"}, "generate",
			"--dir", filepath.Join(root, "migrations"),
			"--sdl", writeSDL(t, root, "schema.graphql", personSDL))
		require.Error(t, err, "neither default is safe to guess at:\n%s", out)
		assert.Contains(t, out, "GOPGQL_NO_TABLES")
		assert.Contains(t, out, "maybe", "the message must quote what it could not parse")
		assert.Empty(t, migrationNames(t, filepath.Join(root, "migrations")),
			"nothing may be written when the environment is rejected")
	})
}

// gopgqlEnv runs the CLI with extra environment variables set, and returns its
// combined output. The variables are appended to the caller's environment, so a
// later assignment wins over anything the developer's shell happens to export.
func gopgqlEnv(t *testing.T, bin string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	t.Logf("$ %s gopgql %s\n%s", strings.Join(env, " "), strings.Join(args, " "), out)
	return string(out), err
}

// plannedNames reduces planned migrations to their filenames.
func plannedNames(planned []migrate.Migration) []string {
	out := make([]string, len(planned))
	for i, m := range planned {
		out[i] = m.Filename()
	}
	return out
}
