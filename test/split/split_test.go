// Package split_test proves the guarantees that motivate splitting migrations
// (gopgql#38) against a real postgres:19beta2 container, rather than the file
// layout that happens to deliver them.
//
// The guarantees, in the order they matter:
//
//   - **Replaying the whole history from zero reproduces the current schema.**
//     This is the regression test for the defect that caused the design to be
//     amended: with a directory per half, replaying the graph half re-ran a
//     historical CREATE PROPERTY GRAPH against tables that had since moved on,
//     naming a column that no longer existed. In one chronological directory that
//     cannot happen, and this suite proves it does not. See sequence_test.go.
//   - **gopgql can manage the property graph over tables it did not create.**
//   - **An SDL may describe only part of a database**, and everything it does not
//     mention survives untouched. This one is worth testing hardest: it is the
//     guarantee people rely on when they point gopgql at a database someone else
//     owns, and it is invisible — nothing fails loudly if a future change starts
//     diffing tables it should not have looked at. So the test seeds tables and
//     columns the SDL never mentions and asserts they are still there afterwards.
//   - **Which halves a directory manages is fixed by its first generation**, and a
//     flag that contradicts its history is refused rather than obeyed.
package split_test

import (
	"context"
	"database/sql"
	"fmt"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lega4e/gopgql/conform"
	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/schema"
	"github.com/lega4e/gopgql/sdl"
)

var connString string

const personSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

func TestMain(m *testing.M) {
	ctx := context.Background()
	c, err := postgres.Run(ctx, "postgres:19beta2",
		postgres.WithDatabase("gopgql"),
		postgres.WithUsername("gopgql"),
		postgres.WithPassword("gopgql"),
		tc.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres: %v\n", err)
		os.Exit(1)
	}
	connString, err = c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = tc.TerminateContainer(c)
	os.Exit(code)
}

// mustModel builds the physical schema for an SDL source.
func mustModel(t *testing.T, src string) *schema.Schema {
	t.Helper()
	doc, err := sdl.Parse(src)
	require.NoError(t, err, "sdl.Parse")
	m, err := generator.Build(doc, "")
	require.NoError(t, err, "generator.Build")
	return m
}

// freshDB gives each database its own name, so one test's tables cannot make
// another's assertion about "tables the SDL never mentioned" accidentally true.
// Several tests need more than one — an incremental apply and a replay from zero
// have to be compared side by side — so the name carries a caller-supplied
// suffix.
func freshDB(t *testing.T, suffix string) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	name := dbName(t.Name() + "_" + suffix)
	admin, err := pgxpool.New(ctx, connString)
	require.NoError(t, err, "admin pool")
	defer admin.Close()
	_, err = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
	require.NoError(t, err, "drop db")
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err, "create db")
	dsn := strings.Replace(connString, "/gopgql?", "/"+name+"?", 1)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err, "pool")
	t.Cleanup(pool.Close)
	return pool, dsn
}

// dbName reduces a test name to a legal, unique PostgreSQL database name.
//
// Both halves matter and both were learned the hard way. Anything that is not a
// letter, a digit or an underscore is dropped rather than kept, because a subtest
// name is prose — a `=` in one lands unquoted in `CREATE DATABASE` as a syntax
// error. And the name is truncated *with a hash of the whole of it*, because
// PostgreSQL truncates identifiers at 63 bytes silently: two subtests agreeing in
// their first 60 characters would otherwise share one database, and the second
// would fail to drop the first's out from under a pool still using it.
func dbName(s string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return -1
		}
	}, s)
	sum := fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(s)))
	// "db_" + safe + "_" + sum, within PostgreSQL's 63-byte identifier limit.
	if limit := 63 - len("db_") - 1 - len(sum); len(safe) > limit {
		safe = safe[:limit]
	}
	return "db_" + safe + "_" + sum
}

// applyDir applies every pending migration in dir, in version order.
//
// It is the whole of applying, and that is the point: no version table to
// select, no half to interleave, no order for this helper to get right. It is
// also the assertion behind "ordering does not depend on gopgql" — this is
// goose's own API, with gopgql nowhere in the call.
func applyDir(t *testing.T, dsn, dir string) error {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.UpContext(context.Background(), db, dir)
}

// downOnce rolls the most recently applied migration in dir back.
func downOnce(t *testing.T, dsn, dir string) error {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.DownContext(context.Background(), db, dir)
}

// buildCLI compiles cmd/gopgql into a temporary directory.
//
// Several of these tests are about what the CLI does — the apply, the refusal to
// disown a half — so they drive the real binary. Re-implementing it in the test
// would prove only that the test agrees with itself.
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

// migrationNames lists the migration filenames in dir, in version order.
func migrationNames(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	require.NoError(t, err, "glob %s", dir)
	sort.Strings(matches)
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = filepath.Base(m)
	}
	return out
}

// readMigrations returns the text of every migration in dir, in version order.
func readMigrations(t *testing.T, dir string) []string {
	t.Helper()
	names := migrationNames(t, dir)
	out := make([]string, len(names))
	for i, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n)) //nolint:gosec // a path this test generated
		require.NoError(t, err, "read %s", n)
		out[i] = string(data)
	}
	return out
}

// physicalFingerprint renders everything about a database's tables that applying
// migrations in the wrong order could get wrong — columns with their types,
// nullability and defaults, every named constraint with its definition, and every
// index — as a stable sorted string.
//
// The constraint definitions come from pg_get_constraintdef, the database's own
// rendering of what a constraint means, so the fingerprint cannot agree with a
// wrong constraint that happens to have the right name. goose's bookkeeping table
// is excluded: it records how the schema was reached, which is precisely the
// difference under test. Implicit NOT NULL constraints (contype 'n') are excluded
// because only their auto-generated names would ever differ, and the nullability
// itself is compared on every column line.
func physicalFingerprint(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var lines []string
	for _, q := range []string{`
		SELECT format('col %s.%s %s notnull=%s default=%s',
		              c.relname, a.attname, format_type(a.atttypid, a.atttypmod),
		              a.attnotnull, coalesce(pg_get_expr(d.adbin, d.adrelid), '-'))
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		  LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
		 WHERE n.nspname = 'public' AND c.relkind = 'r'
		   AND NOT starts_with(c.relname, 'goose_db_version')`, `
		SELECT format('con %s.%s %s', c.relname, con.conname, pg_get_constraintdef(con.oid))
		  FROM pg_constraint con
		  JOIN pg_class c ON c.oid = con.conrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND NOT starts_with(c.relname, 'goose_db_version')
		   AND con.contype <> 'n'`, `
		SELECT format('idx %s.%s %s', tablename, indexname, indexdef)
		  FROM pg_indexes
		 WHERE schemaname = 'public' AND NOT starts_with(tablename, 'goose_db_version')`,
	} {
		rows, err := pool.Query(context.Background(), q)
		require.NoError(t, err, "fingerprint query")
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			lines = append(lines, line)
		}
		require.NoError(t, rows.Err())
		rows.Close()
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// assertSameGraph compares two databases' property graphs with the project's own
// drift checker, which is exactly the question being asked of them.
func assertSameGraph(t *testing.T, want, got *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	wantGraph, err := conform.Reflect(ctx, want, "")
	require.NoError(t, err, "reflect the expected property graph")
	gotGraph, err := conform.Reflect(ctx, got, "")
	require.NoError(t, err, "reflect the actual property graph")
	report := conform.Check(wantGraph, gotGraph)
	assert.True(t, report.OK(), "the two property graphs differ: %+v", report.Findings)
}

// TestGraphOverForeignTables covers the case the issue opens with: the tables are
// somebody else's, and gopgql supplies only the property graph.
func TestGraphOverForeignTables(t *testing.T) {
	ctx := context.Background()
	pool, dsn := freshDB(t, "main")
	bin := buildCLI(t)

	// Tables created by hand — as Atlas, Flyway or a DBA would have.
	for _, stmt := range []string{
		`CREATE TABLE persons (id uuid PRIMARY KEY, name text NOT NULL)`,
		// The edge table needs a key: SQL/PGQ will not define an edge element
		// over a table without one. Tables owned elsewhere still have to be
		// shaped compatibly — gopgql supplies the mapping, not the schema.
		`CREATE TABLE follows (
			source_id uuid NOT NULL REFERENCES persons (id),
			target_id uuid NOT NULL REFERENCES persons (id),
			PRIMARY KEY (source_id, target_id)
		)`,
		`INSERT INTO persons (id, name) VALUES
			('11111111-1111-1111-1111-111111111111', 'Ada'),
			('22222222-2222-2222-2222-222222222222', 'Grace')`,
		`INSERT INTO follows (source_id, target_id) VALUES
			('11111111-1111-1111-1111-111111111111', '22222222-2222-2222-2222-222222222222')`,
	} {
		_, err := pool.Exec(ctx, stmt)
		require.NoError(t, err, "seed: %s", stmt)
	}

	root := t.TempDir()
	dir := filepath.Join(root, "migrations")
	sdlPath := writeSDL(t, root, "schema.graphql", personSDL)
	out, err := gopgql(t, bin, "generate", "--no-tables", "--sdl", sdlPath, "--dir", dir)
	require.NoError(t, err, "generate the graph half:\n%s", out)

	// One file: nothing to tear down, and nothing about a table.
	require.Equal(t, []string{"0001_schema_graph.sql"}, migrationNames(t, dir))
	for _, content := range readMigrations(t, dir) {
		for _, stmt := range []string{"CREATE TABLE", "ALTER TABLE", "DROP TABLE"} {
			assert.NotContains(t, content, stmt, "the graph half must not touch a table")
		}
	}
	require.NoError(t, applyDir(t, dsn, dir), "apply")

	// The graph works over tables gopgql never created.
	var name string
	err = pool.QueryRow(ctx, `
		SELECT name FROM GRAPH_TABLE (app_graph
			MATCH (p IS person)-[IS follows]->(q IS person)
			COLUMNS (q.name AS name))`).Scan(&name)
	require.NoError(t, err, "query the graph")
	assert.Equal(t, "Grace", name, "MATCH must traverse tables gopgql never created")

	// And the flag keeps agreeing with the history, forever.
	out, err = gopgql(t, bin, "generate", "--no-tables", "--sdl", sdlPath, "--dir", dir)
	require.NoError(t, err, "a directory that never owned the tables half keeps working:\n%s", out)
	assert.Contains(t, out, "already up to date", "an unchanged schema emits nothing")
	assert.Equal(t, []string{"0001_schema_graph.sql"}, migrationNames(t, dir))
}

// TestPartialProjectionLeavesTheRestAlone is the guarantee the SDL-as-projection
// case rests on: what the SDL does not mention must survive.
func TestPartialProjectionLeavesTheRestAlone(t *testing.T) {
	ctx := context.Background()
	pool, dsn := freshDB(t, "main")

	for _, stmt := range []string{
		// The tables the SDL describes — plus columns it does not.
		`CREATE TABLE persons (
			id uuid PRIMARY KEY,
			name text NOT NULL,
			salary integer,
			private_note text
		)`,
		`CREATE TABLE follows (
			source_id uuid NOT NULL REFERENCES persons (id),
			target_id uuid NOT NULL REFERENCES persons (id),
			PRIMARY KEY (source_id, target_id)
		)`,
		// A whole table the SDL has never heard of.
		`CREATE TABLE payroll (id serial PRIMARY KEY, amount integer NOT NULL)`,
		`INSERT INTO persons (id, name, salary, private_note)
			VALUES ('11111111-1111-1111-1111-111111111111', 'Ada', 120, 'keep me')`,
		`INSERT INTO payroll (amount) VALUES (42)`,
	} {
		_, err := pool.Exec(ctx, stmt)
		require.NoError(t, err, "seed: %s", stmt)
	}

	dir := filepath.Join(t.TempDir(), "migrations")
	model := mustModel(t, personSDL)
	paths, err := migrate.Generate(dir, model, "init", migrate.Halves{NoTables: true})
	require.NoError(t, err, "Generate")
	require.Len(t, paths, 1)

	// No migration refers to what the SDL does not declare. Asserted on the
	// text, not only on the outcome: the outcome would also be reached by a
	// migration that named them and happened not to break anything.
	for _, content := range readMigrations(t, dir) {
		for _, unmentioned := range []string{"payroll", "private_note", "salary"} {
			assert.NotContains(t, content, unmentioned,
				"absence from the SDL is not a reason to name something")
		}
	}
	require.NoError(t, applyDir(t, dsn, dir), "apply")

	// Nothing the SDL did not mention was touched.
	var note string
	err = pool.QueryRow(ctx, `SELECT private_note FROM persons`).Scan(&note)
	require.NoError(t, err, "a column the SDL never declared was removed")
	assert.Equal(t, "keep me", note)
	var amount int
	require.NoError(t, pool.QueryRow(ctx, `SELECT amount FROM payroll`).Scan(&amount),
		"a table the SDL never mentioned was removed")
	assert.Equal(t, 42, amount)

	// A second run, with the database still holding what the SDL never
	// mentioned, must still emit nothing: absence from the SDL is not a pending
	// change.
	again, err := migrate.Generate(dir, model, "again", migrate.Halves{NoTables: true})
	require.NoError(t, err, "second Generate")
	assert.Empty(t, again,
		"absence from the SDL is not a pending change: an unchanged schema emits nothing")

	// And the graph exposes only the declared properties.
	var n int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM GRAPH_TABLE (app_graph
			MATCH (p IS person) COLUMNS (p.name AS name))`).Scan(&n))
	assert.Equal(t, 1, n)
}

// TestDropGraphKeepsTheData covers "drop the whole graph setup but keep the data"
// from the issue — and the rule that a flag never does it: the graph half stays
// on and the *schema* stops declaring a graph, so the generation emits the
// teardown and no rebuild.
func TestDropGraphKeepsTheData(t *testing.T) {
	ctx := context.Background()
	pool, dsn := freshDB(t, "main")

	dir := filepath.Join(t.TempDir(), "migrations")
	model := mustModel(t, personSDL)
	_, err := migrate.Generate(dir, model, "init", migrate.Halves{})
	require.NoError(t, err, "Generate")
	require.NoError(t, applyDir(t, dsn, dir), "apply")
	_, err = pool.Exec(ctx,
		`INSERT INTO persons (id, name) VALUES ('11111111-1111-1111-1111-111111111111', 'Ada')`)
	require.NoError(t, err, "seed")

	// A desired schema that declares no property graph.
	graphless := &schema.Schema{
		VertexTables: model.VertexTables,
		EdgeTables:   model.EdgeTables,
		Indexes:      model.Indexes,
	}
	paths, err := migrate.Generate(dir, graphless, "drop graph", migrate.Halves{})
	require.NoError(t, err, "Generate (drop)")
	require.Equal(t, []string{"0003_drop_graph_graph_down.sql"}, baseNames(paths),
		"the teardown, and no rebuild")

	data, err := os.ReadFile(paths[0]) //nolint:gosec // a path this test just generated
	require.NoError(t, err)
	for _, stmt := range []string{"DROP TABLE", "ALTER TABLE", "CREATE TABLE"} {
		assert.NotContains(t, string(data), stmt, "dropping the graph must not touch a table")
	}
	require.NoError(t, applyDir(t, dsn, dir), "apply the drop")

	// The graph is gone; the row is not.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_propgraph_element`).Scan(&n); err == nil {
		assert.Zero(t, n, "the property graph should be gone")
	}
	var name string
	require.NoError(t, pool.QueryRow(ctx, `SELECT name FROM persons`).Scan(&name),
		"the data must survive dropping the graph")
	assert.Equal(t, "Ada", name)
}

// TestTurningAHalfOffAgainstItsHistoryIsRefused is design D4a through the CLI, in
// both directions: which halves a directory manages is fixed by its first
// generation, and thereafter a contradicting flag is refused with nothing written
// and a message naming what to do instead.
func TestTurningAHalfOffAgainstItsHistoryIsRefused(t *testing.T) {
	bin := buildCLI(t)

	for _, tc := range []struct {
		name string
		flag string
		says []string
	}{{
		name: "the graph half, against a history that creates a property graph",
		flag: "--no-graph",
		says: []string{
			"creates a property graph",
			// The one legitimate reason to want this is to get rid of the graph,
			// so the message has to name the deliberate way to do that.
			"declares no", "teardown",
		},
	}, {
		name: "the tables half, against a history that creates tables",
		flag: "--no-tables",
		says: []string{"creates tables", "fresh --dir"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "migrations")
			sdlPath := writeSDL(t, root, "schema.graphql", personSDL)
			out, err := gopgql(t, bin, "generate", "--sdl", sdlPath, "--dir", dir)
			require.NoError(t, err, "the first generation sets what the directory manages:\n%s", out)
			before := migrationNames(t, dir)
			require.Len(t, before, 2)

			out, err = gopgql(t, bin, "generate", tc.flag,
				"--sdl", writeSDL(t, root, "v2.graphql", salariedPersonSDL), "--dir", dir)
			require.Error(t, err, "the flag contradicts the history:\n%s", out)
			for _, want := range tc.says {
				assert.Contains(t, out, want)
			}
			assert.Equal(t, before, migrationNames(t, dir), "nothing may be written")
		})
	}

	t.Run("both halves off asks for nothing", func(t *testing.T) {
		root := t.TempDir()
		out, err := gopgql(t, bin, "generate", "--no-tables", "--no-graph",
			"--sdl", writeSDL(t, root, "schema.graphql", personSDL),
			"--dir", filepath.Join(root, "migrations"))
		require.Error(t, err, "both halves off must fail rather than generate nothing silently")
		assert.Contains(t, out, "nothing to do")
	})

	t.Run("a directory that never owned the graph half keeps working", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "migrations")
		out, err := gopgql(t, bin, "generate", "--no-graph", "--name", "v1",
			"--sdl", writeSDL(t, root, "v1.graphql", personSDL), "--dir", dir)
		require.NoError(t, err, "%s", out)
		out, err = gopgql(t, bin, "generate", "--no-graph", "--name", "v2",
			"--sdl", writeSDL(t, root, "v2.graphql", salariedPersonSDL), "--dir", dir)
		require.NoError(t, err, "the flag agrees with the history:\n%s", out)
		assert.Equal(t, []string{"0001_v1_tables.sql", "0002_v2_tables.sql"},
			migrationNames(t, dir), "the tables half keeps going, alone, forever")
	})
}

// baseNames reduces written paths to their filenames.
func baseNames(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}
