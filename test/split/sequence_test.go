package split_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/migrate"
)

// salariedPersonSDL is personSDL plus one more scalar. The generator turns it
// into a column *and* a graph property, so removing it from the SDL changes both
// halves — which is what makes the apply order observable.
const salariedPersonSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  salary: Int
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// emailPersonSDL adds a different scalar to personSDL: a third generation that
// moves both halves again, so the history is long enough for a replay to get the
// order wrong somewhere in the middle.
const emailPersonSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// indexedPersonSDL is personSDL with an index on a field it already had. An index
// is a table change and nothing else: the CREATE PROPERTY GRAPH statement comes
// out identical, so this is the generation whose graph work is only "put back what
// the tables made me take down".
const indexedPersonSDL = `type Person @node(label: "person") {
  id: ID!
  name: String! @index
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// indexedSalariedPersonSDL is indexedPersonSDL with one more scalar: a generation
// that changes both halves, arriving after a generation that changed only the
// tables.
const indexedSalariedPersonSDL = `type Person @node(label: "person") {
  id: ID!
  name: String! @index
  salary: Int
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

const ada = "11111111-1111-1111-1111-111111111111"

// matchOnePersonName is the smallest query that proves the property graph is live
// over the tables.
const matchOnePersonName = `
	SELECT name FROM GRAPH_TABLE (app_graph
		MATCH (p IS person)
		COLUMNS (p.name AS name))`

// generate runs one generation of the CLI against dir and returns the files it
// wrote, newest last.
func generate(t *testing.T, bin, root, dir, name, source string) []string {
	t.Helper()
	before := migrationNames(t, dir)
	out, err := gopgql(t, bin, "generate",
		"--sdl", writeSDL(t, root, name+".graphql", source), "--dir", dir, "--name", name)
	require.NoError(t, err, "generate %s:\n%s", name, out)
	after := migrationNames(t, dir)
	require.Greater(t, len(after), len(before), "generation %s wrote nothing:\n%s", name, out)
	return after[len(before):]
}

// TestReplayFromZeroReproducesTheSchema is the regression test for the defect
// that caused this design to be amended, and the most valuable test in the suite.
//
// A CREATE PROPERTY GRAPH names the columns of *its own* generation. Under the
// two-directory design the graph half was a history of its own, so replaying it
// from zero ran generation one's graph statement against tables that generation
// two had already changed — `column "salary" does not exist`. The lockstep
// applier existed to work around that.
//
// Here there is no workaround because there is no problem to work around: every
// migration is in one directory in chronological order, so a graph statement is
// always immediately preceded by the table DDL of its own generation. The test
// builds exactly the history that broke — a first graph exposing a column a later
// generation drops — and replays all of it into an empty database.
//
// It also checks that the trap is really present. A replay test over a history
// that never removed anything would pass under both designs and prove nothing.
func TestReplayFromZeroReproducesTheSchema(t *testing.T) {
	ctx := context.Background()
	bin := buildCLI(t)
	root := t.TempDir()
	dir := filepath.Join(root, "migrations")

	// Three generations, each changing the tables and the graph, built the way an
	// operator builds them: generate and apply, one at a time.
	incremental, incrementalDSN := freshDB(t, "incremental")
	for _, gen := range []struct{ name, source string }{
		{"one", salariedPersonSDL}, // salary is a column and a graph property
		{"two", personSDL},         // it is neither
		{"three", emailPersonSDL},  // and email is both
	} {
		out, err := gopgql(t, bin, "migrate", "--dsn", incrementalDSN, "--dir", dir,
			"--name", gen.name, "--sdl", writeSDL(t, root, gen.name+".graphql", gen.source))
		require.NoError(t, err, "generate and apply generation %s:\n%s", gen.name, out)
	}

	require.Equal(t, []string{
		"0001_one_tables.sql",
		"0002_one_graph.sql",
		"0003_two_graph_down.sql",
		"0004_two_tables.sql",
		"0005_two_graph.sql",
		"0006_three_graph_down.sql",
		"0007_three_tables.sql",
		"0008_three_graph.sql",
	}, migrationNames(t, dir), "one chronological history, consecutively numbered")

	// The trap, stated rather than described: an early graph statement exposes a
	// column a later generation drops.
	history := readMigrations(t, dir)
	require.Contains(t, history[1], "PROPERTIES (id, name, salary)",
		"0002 must expose the column generation two drops")
	require.Contains(t, history[3], "DROP COLUMN salary",
		"0004 must drop it — without this the replay proves nothing")

	// Replay the whole thing into an empty database, through the CLI.
	replay, replayDSN := freshDB(t, "replay")
	out, err := gopgql(t, bin, "migrate", "--dsn", replayDSN, "--dir", dir)
	require.NoError(t, err,
		"replaying the history from zero must apply every migration in order; a "+
			"historical CREATE PROPERTY GRAPH must never reach a later schema:\n%s", out)

	assert.Equal(t, physicalFingerprint(t, incremental), physicalFingerprint(t, replay),
		"a replay from zero must reach the same physical schema as the incremental apply")
	assertSameGraph(t, incremental, replay)

	// And the graph the replay built is live and is the last generation's.
	_, err = replay.Exec(ctx, `INSERT INTO persons (id, name, email) VALUES ($1, 'Ada', 'ada@x')`, ada)
	require.NoError(t, err, "seed the replayed database")
	var email string
	require.NoError(t, replay.QueryRow(ctx, `
		SELECT email FROM GRAPH_TABLE (app_graph
			MATCH (p IS person)
			COLUMNS (p.email AS email))`).Scan(&email),
		"the replayed graph must expose the last generation's columns")
	assert.Equal(t, "ada@x", email)

	// Ordering does not depend on gopgql: goose alone, over the same directory,
	// reaches the same place.
	byGoose, gooseDSN := freshDB(t, "goose")
	require.NoError(t, applyDir(t, gooseDSN, dir),
		"the migrations must apply through goose with gopgql nowhere in the call")
	assert.Equal(t, physicalFingerprint(t, incremental), physicalFingerprint(t, byGoose))
	assertSameGraph(t, incremental, byGoose)
}

// TestATablesOnlyGenerationBetweenTwoGraphGenerations covers a change no graph
// statement mentions — an index. It is the case the two-directory design handled
// by leaving a gap in the graph half's numbering and having the applier rebuild
// the graph out of band.
//
// Here the generation is an ordinary run of three: the graph still has to come
// down for the index to be created, so it comes down and goes straight back up,
// unchanged. Nothing is skipped, nothing desynchronises, and a replay from zero
// still succeeds.
func TestATablesOnlyGenerationBetweenTwoGraphGenerations(t *testing.T) {
	ctx := context.Background()
	bin := buildCLI(t)
	root := t.TempDir()
	dir := filepath.Join(root, "migrations")

	incremental, incrementalDSN := freshDB(t, "incremental")
	require.Equal(t, []string{"0001_one_tables.sql", "0002_one_graph.sql"},
		generate(t, bin, root, dir, "one", personSDL))

	// The middle generation: an index, which no graph property mentions.
	middle := generate(t, bin, root, dir, "two", indexedPersonSDL)
	require.Equal(t, []string{
		"0003_two_graph_down.sql", "0004_two_tables.sql", "0005_two_graph.sql",
	}, middle, "the graph comes down for the index and goes straight back up")
	assert.Contains(t, readMigrations(t, dir)[3], "CREATE INDEX")

	// The rebuild is the same definition the generation before it built — the
	// graph is restored, not redefined.
	graphUp := func(name string) string {
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a generated path
		require.NoError(t, err)
		up, _, ok := strings.Cut(string(data), "-- +goose Down")
		require.True(t, ok, "%s must have a Down section", name)
		return strings.TrimPrefix(up, "-- +goose Up\n")
	}
	assert.Equal(t, graphUp("0002_one_graph.sql"), graphUp("0005_two_graph.sql"),
		"a tables-only generation restores the graph it took down, unchanged")

	// A third generation that does move the graph, on top of the middle one.
	require.Equal(t, []string{
		"0006_three_graph_down.sql", "0007_three_tables.sql", "0008_three_graph.sql",
	}, generate(t, bin, root, dir, "three", indexedSalariedPersonSDL))

	out, err := gopgql(t, bin, "migrate", "--dsn", incrementalDSN, "--dir", dir)
	require.NoError(t, err, "apply a history with a tables-only generation in it:\n%s", out)

	// Replay from zero has to reach the same place.
	replay, replayDSN := freshDB(t, "replay")
	out, err = gopgql(t, bin, "migrate", "--dsn", replayDSN, "--dir", dir)
	require.NoError(t, err, "replay a history with a tables-only generation in it:\n%s", out)
	assert.Equal(t, physicalFingerprint(t, incremental), physicalFingerprint(t, replay))
	assertSameGraph(t, incremental, replay)

	// The index is there, and the last generation's graph is live over it.
	var indexes int
	require.NoError(t, replay.QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes
		WHERE tablename = 'persons' AND indexdef LIKE '%(name)'`).Scan(&indexes))
	assert.Equal(t, 1, indexes, "the tables-only generation's index must survive the replay")

	_, err = replay.Exec(ctx, `INSERT INTO persons (id, name, salary) VALUES ($1, 'Ada', 120)`, ada)
	require.NoError(t, err, "seed")
	var salary int
	require.NoError(t, replay.QueryRow(ctx, `
		SELECT salary FROM GRAPH_TABLE (app_graph
			MATCH (p IS person)
			COLUMNS (p.salary AS salary))`).Scan(&salary),
		"the last generation's graph must expose the last generation's column")
	assert.Equal(t, 120, salary)
}

// TestRollingAGenerationBackReversesIt is what each file's Down section is for:
// undoing a generation's migrations newest first returns the database to the state
// it held before, property-graph definition included.
//
// The teardown migration is the interesting one. Its Up drops the graph; its Down
// has to re-create the definition the history held *before* that generation — not
// the new one, and not nothing.
func TestRollingAGenerationBackReversesIt(t *testing.T) {
	ctx := context.Background()
	bin := buildCLI(t)
	root := t.TempDir()
	dir := filepath.Join(root, "migrations")
	pool, dsn := freshDB(t, "main")

	require.Len(t, generate(t, bin, root, dir, "one", personSDL), 2)
	out, err := gopgql(t, bin, "migrate", "--dsn", dsn, "--dir", dir)
	require.NoError(t, err, "apply generation one:\n%s", out)
	before := physicalFingerprint(t, pool)

	_, err = pool.Exec(ctx, `INSERT INTO persons (id, name) VALUES ($1, 'Ada')`, ada)
	require.NoError(t, err, "seed")

	generation := generate(t, bin, root, dir, "two", salariedPersonSDL)
	require.Len(t, generation, 3, "a generation with table work under a live graph")
	out, err = gopgql(t, bin, "migrate", "--dsn", dsn, "--dir", dir)
	require.NoError(t, err, "apply generation two:\n%s", out)

	var salary *int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT salary FROM GRAPH_TABLE (app_graph
			MATCH (p IS person) COLUMNS (p.salary AS salary))`).Scan(&salary),
		"generation two exposes salary")

	// Roll the generation back, newest first.
	for i := range generation {
		require.NoError(t, downOnce(t, dsn, dir), "roll back %s", generation[len(generation)-1-i])
	}

	assert.Equal(t, before, physicalFingerprint(t, pool),
		"rolling a generation back must restore the tables it found")

	// The previous property-graph definition is live again — which means it does
	// *not* expose salary, and a query for it fails rather than returning null.
	var name string
	require.NoError(t, pool.QueryRow(ctx, matchOnePersonName).Scan(&name),
		"the previous property graph must be live again")
	assert.Equal(t, "Ada", name, "and the row is still there")

	err = pool.QueryRow(ctx, `
		SELECT salary FROM GRAPH_TABLE (app_graph
			MATCH (p IS person) COLUMNS (p.salary AS salary))`).Scan(&salary)
	require.Error(t, err,
		"the restored graph is the one from before the generation, so it cannot expose salary")
}

// TestTheSequenceEqualsACombinedMigration is the claim the split rests on: the
// run of single-purpose migrations reaches exactly the database one combined
// migration would have, and a query returns the same rows across it.
func TestTheSequenceEqualsACombinedMigration(t *testing.T) {
	ctx := context.Background()
	model := mustModel(t, salariedPersonSDL)

	sequenced, sequencedDSN := freshDB(t, "sequenced")
	seqDir := filepath.Join(t.TempDir(), "sequenced")
	paths, err := migrate.Generate(seqDir, model, "init", migrate.Halves{})
	require.NoError(t, err, "Generate")
	require.Len(t, paths, 2)
	// No migration carries both concerns — the property the combined migration
	// could not have.
	for _, content := range readMigrations(t, seqDir) {
		hasTable := strings.Contains(content, "CREATE TABLE") ||
			strings.Contains(content, "ALTER TABLE") ||
			strings.Contains(content, "CREATE INDEX")
		hasGraph := strings.Contains(content, "PROPERTY GRAPH")
		assert.False(t, hasTable && hasGraph, "no migration may hold both halves:\n%s", content)
	}
	require.NoError(t, applyDir(t, sequencedDSN, seqDir), "apply the sequence")

	combined, combinedDSN := freshDB(t, "combined")
	combinedDir := filepath.Join(t.TempDir(), "combined")
	require.NoError(t, os.MkdirAll(combinedDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(combinedDir, "0001_init.sql"),
		[]byte(migrate.Init(model)), 0o600))
	require.NoError(t, applyDir(t, combinedDSN, combinedDir), "apply the combined migration")

	assert.Equal(t, physicalFingerprint(t, combined), physicalFingerprint(t, sequenced),
		"the sequence must reach the database the combined migration would have")
	assertSameGraph(t, combined, sequenced)

	// And a query returns the same rows.
	rows := func(what string, pool *pgxpool.Pool) string {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO persons (id, name, salary) VALUES ($1, 'Ada', 120)`, ada)
		require.NoError(t, err, "seed %s", what)
		var got string
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT format('%s/%s', name, salary) FROM GRAPH_TABLE (app_graph
				MATCH (p IS person) COLUMNS (p.name AS name, p.salary AS salary))`).Scan(&got),
			"query %s", what)
		return got
	}
	assert.Equal(t, rows("combined", combined), rows("sequenced", sequenced),
		"a query must compile and return the same rows across the split")
}

// TestReRunningMigrateIsANoOp is what makes the init container in every example
// work: with an ephemeral --dir it regenerates the whole history every time, and
// an already-migrated database must come out unchanged with its graph still up.
func TestReRunningMigrateIsANoOp(t *testing.T) {
	ctx := context.Background()
	bin := buildCLI(t)
	root := t.TempDir()
	dir := filepath.Join(root, "migrations")
	pool, dsn := freshDB(t, "main")
	sdlPath := writeSDL(t, root, "schema.graphql", salariedPersonSDL)

	out, err := gopgql(t, bin, "migrate", "--dsn", dsn, "--dir", dir, "--sdl", sdlPath)
	require.NoError(t, err, "first migrate:\n%s", out)
	names := migrationNames(t, dir)
	fingerprint := physicalFingerprint(t, pool)

	_, err = pool.Exec(ctx, `INSERT INTO persons (id, name, salary) VALUES ($1, 'Ada', 120)`, ada)
	require.NoError(t, err, "seed")

	out, err = gopgql(t, bin, "migrate", "--dsn", dsn, "--dir", dir, "--sdl", sdlPath)
	require.NoError(t, err, "second migrate:\n%s", out)
	assert.Contains(t, out, "already up to date",
		"regenerating against an unchanged schema must emit nothing")
	assert.Equal(t, names, migrationNames(t, dir), "no new migration may appear")
	assert.Equal(t, fingerprint, physicalFingerprint(t, pool), "nothing may change")

	var name string
	require.NoError(t, pool.QueryRow(ctx, matchOnePersonName).Scan(&name),
		"re-running migrate must leave the property graph up")
	assert.Equal(t, "Ada", name)
}

// TestNoSubdirectoriesAreCreated is the layout claim, kept as a single cheap
// assertion rather than spread through the suite: everything goes into --dir.
func TestNoSubdirectoriesAreCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "migrations")
	_, err := migrate.Generate(dir, mustModel(t, personSDL), "init", migrate.Halves{})
	require.NoError(t, err, "Generate")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, e.IsDir(), "no per-concern subdirectory may be created, found %s", e.Name())
		assert.True(t, strings.HasSuffix(e.Name(), ".sql"), "unexpected entry %s", e.Name())
	}
	assert.Len(t, entries, 2, fmt.Sprintf("in %s", dir))
}
