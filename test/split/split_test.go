// Package split_test proves the two claims that motivate splitting migrations
// (gopgql#38), against a real postgres:19beta2 container:
//
//   - gopgql can manage the property graph over tables it did not create and
//     does not own;
//   - an SDL may describe only part of a database, and everything it does not
//     mention survives untouched.
//
// The second is the one worth testing hardest. It is a guarantee people rely on
// when they point gopgql at a database someone else owns, and it is invisible —
// nothing fails loudly if a future change starts diffing tables it should not
// have looked at. So the test seeds tables and columns the SDL never mentions
// and asserts they are still there afterwards.
package split_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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

// mustModel builds the physical schema for personSDL — the one schema this
// suite is about.
func mustModel(t *testing.T) *schema.Schema {
	t.Helper()
	doc, err := sdl.Parse(personSDL)
	require.NoError(t, err, "sdl.Parse")
	m, err := generator.Build(doc, "")
	require.NoError(t, err, "generator.Build")
	return m
}

// freshDB gives each test its own database, so one test's tables cannot make
// another's assertion about "tables the SDL never mentioned" accidentally true.
func freshDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	name := "db_" + strings.ToLower(strings.NewReplacer("/", "", "-", "", " ", "").Replace(t.Name()))
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

// applyDir runs goose up over one half's directory, with that half's own
// version table.
func applyDir(t *testing.T, dsn, dir string) error {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	goose.SetTableName(migrate.VersionTable(filepath.Base(dir)))
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.UpContext(context.Background(), db, dir)
}

// TestGraphOverForeignTables covers the case the issue opens with: the tables
// are somebody else's, and gopgql supplies only the property graph.
func TestGraphOverForeignTables(t *testing.T) {
	ctx := context.Background()
	pool, dsn := freshDB(t)

	// Tables created by hand — as Atlas, Flyway or a DBA would have.
	for _, stmt := range []string{
		`CREATE TABLE persons (id uuid PRIMARY KEY, name text NOT NULL)`,
		// The edge table needs a key: SQL/PGQ requires one to define an edge
		// element over it. Tables owned elsewhere still have to be shaped
		// compatibly — gopgql supplies the mapping, not the schema.
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

	// gopgql supplies the graph half only.
	root := t.TempDir()
	graphDir := filepath.Join(root, migrate.GraphDir)
	version, err := migrate.NextVersion(root)
	require.NoError(t, err, "NextVersion")
	_, err = migrate.GenerateGraph(graphDir, mustModel(t), "init", version)
	require.NoError(t, err, "GenerateGraph")
	written, err := os.ReadFile(filepath.Join(graphDir, migrate.InitFilename))
	require.NoError(t, err, "read migration")
	require.NotContains(t, string(written), "CREATE TABLE",
		"the graph half must not create tables")
	require.NoError(t, applyDir(t, dsn, graphDir), "apply graph half")

	// The graph works over tables gopgql never created.
	var name string
	err = pool.QueryRow(ctx, `
		SELECT name FROM GRAPH_TABLE (app_graph
			MATCH (p IS person)-[IS follows]->(q IS person)
			COLUMNS (q.name AS name))`).Scan(&name)
	require.NoError(t, err, "query the graph")
	assert.Equal(t, "Grace", name, "MATCH must traverse tables gopgql never created")
}

// TestPartialProjectionLeavesTheRestAlone is the guarantee the SDL-as-projection
// case rests on: what the SDL does not mention must survive.
func TestPartialProjectionLeavesTheRestAlone(t *testing.T) {
	ctx := context.Background()
	pool, dsn := freshDB(t)

	for _, stmt := range []string{
		// The tables the SDL describes — plus a column it does not.
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

	root := t.TempDir()
	graphDir := filepath.Join(root, migrate.GraphDir)
	model := mustModel(t)
	version, err := migrate.NextVersion(root)
	require.NoError(t, err, "NextVersion")
	_, err = migrate.GenerateGraph(graphDir, model, "init", version)
	require.NoError(t, err, "GenerateGraph")
	require.NoError(t, applyDir(t, dsn, graphDir), "apply graph half")

	// Nothing the SDL did not mention was touched.
	var note string
	err = pool.QueryRow(ctx, `SELECT private_note FROM persons`).Scan(&note)
	require.NoError(t, err, "a column the SDL never declared was removed")
	assert.Equal(t, "keep me", note)
	var amount int
	err = pool.QueryRow(ctx, `SELECT amount FROM payroll`).Scan(&amount)
	require.NoError(t, err, "a table the SDL never mentioned was removed")

	// A second run, with the database still holding what the SDL never
	// mentioned, must still emit nothing: absence from the SDL is not a
	// pending change.
	next, err := migrate.NextVersion(root)
	require.NoError(t, err, "NextVersion")
	path, err := migrate.GenerateGraph(graphDir, model, "again", next)
	require.NoError(t, err, "second GenerateGraph")
	assert.Empty(t, path,
		"absence from the SDL is not a pending change: an unchanged schema must emit nothing")

	// And the graph exposes only the declared properties.
	var n int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM GRAPH_TABLE (app_graph
			MATCH (p IS person) COLUMNS (p.name AS name))`).Scan(&n)
	require.NoError(t, err, "query the graph")
	assert.Equal(t, 1, n)
}

// TestGraphBeforeTablesFailsLoudly documents the ordering constraint by
// asserting the failure rather than describing it.
func TestGraphBeforeTablesFailsLoudly(t *testing.T) {
	_, dsn := freshDB(t)

	root := t.TempDir()
	dirs, err := migrate.WriteInitSplit(root, mustModel(t))
	require.NoError(t, err, "WriteInitSplit")
	require.Equal(t, []string{migrate.TablesDir, migrate.GraphDir},
		[]string{filepath.Base(dirs[0]), filepath.Base(dirs[1])},
		"WriteInitSplit must return the halves in apply order")

	// Applying the graph half first, against tables that do not exist.
	err = applyDir(t, dsn, dirs[1])
	require.Error(t, err, "applying the graph half before the tables must fail")
	assert.Contains(t, err.Error(), "persons", "the error should name the missing table")

	// In the right order it works.
	require.NoError(t, applyDir(t, dsn, dirs[0]), "apply tables")
	require.NoError(t, applyDir(t, dsn, dirs[1]), "apply graph after tables")
}

// TestDropGraphKeepsTheData covers "drop the whole graph setup but keep the
// data" from the issue.
func TestDropGraphKeepsTheData(t *testing.T) {
	ctx := context.Background()
	pool, dsn := freshDB(t)

	root := t.TempDir()
	model := mustModel(t)
	dirs, err := migrate.WriteInitSplit(root, model)
	require.NoError(t, err, "WriteInitSplit")
	for _, d := range dirs {
		require.NoError(t, applyDir(t, dsn, d), "apply %s", d)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO persons (id, name) VALUES ('11111111-1111-1111-1111-111111111111', 'Ada')`)
	require.NoError(t, err, "seed")

	// The graph half, pointed at a schema that declares no graph.
	graphless := &schema.Schema{VertexTables: model.VertexTables, EdgeTables: model.EdgeTables}
	next, err := migrate.NextVersion(root)
	require.NoError(t, err, "NextVersion")
	path, err := migrate.GenerateGraph(dirs[1], graphless, "drop graph", next)
	require.NoError(t, err, "GenerateGraph (drop)")
	require.NotEmpty(t, path, "dropping the graph should emit a migration")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "DROP TABLE", "dropping the graph must not touch a table")
	assert.NotContains(t, string(data), "ALTER TABLE", "dropping the graph must not touch a table")
	require.NoError(t, applyDir(t, dsn, dirs[1]), "apply the drop")

	// The graph is gone; the row is not.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_propgraph_element`).Scan(&n); err == nil {
		assert.Zero(t, n, "the property graph should be gone")
	}
	var name string
	err = pool.QueryRow(ctx, `SELECT name FROM persons`).Scan(&name)
	require.NoError(t, err, "the data must survive dropping the graph")
	assert.Equal(t, "Ada", name)
}
