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

func mustModel(t *testing.T, src string) *schema.Schema {
	t.Helper()
	doc, err := sdl.Parse(src)
	if err != nil {
		t.Fatalf("sdl.Parse: %v", err)
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		t.Fatalf("generator.Build: %v", err)
	}
	return m
}

// freshDB gives each test its own database, so one test's tables cannot make
// another's assertion about "tables the SDL never mentioned" accidentally true.
func freshDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	name := "db_" + strings.ToLower(strings.NewReplacer("/", "", "-", "", " ", "").Replace(t.Name()))
	admin, err := pgxpool.New(ctx, connString)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name); err != nil {
		t.Fatalf("drop db: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create db: %v", err)
	}
	dsn := strings.Replace(connString, "/gopgql?", "/"+name+"?", 1)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
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
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	// gopgql supplies the graph half only.
	root := t.TempDir()
	graphDir := filepath.Join(root, migrate.GraphDir)
	if _, err := migrate.GenerateGraph(graphDir, mustModel(t, personSDL), "init"); err != nil {
		t.Fatalf("GenerateGraph: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(graphDir, migrate.InitFilename))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if strings.Contains(string(written), "CREATE TABLE") {
		t.Fatalf("the graph half must not create tables:\n%s", written)
	}
	if err := applyDir(t, dsn, graphDir); err != nil {
		t.Fatalf("apply graph half: %v", err)
	}

	// The graph works over tables gopgql never created.
	var name string
	err = pool.QueryRow(ctx, `
		SELECT name FROM GRAPH_TABLE (app_graph
			MATCH (p IS person)-[IS follows]->(q IS person)
			COLUMNS (q.name AS name))`).Scan(&name)
	if err != nil {
		t.Fatalf("query the graph: %v", err)
	}
	if name != "Grace" {
		t.Errorf("MATCH returned %q, want Grace", name)
	}
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
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	root := t.TempDir()
	graphDir := filepath.Join(root, migrate.GraphDir)
	model := mustModel(t, personSDL)
	if _, err := migrate.GenerateGraph(graphDir, model, "init"); err != nil {
		t.Fatalf("GenerateGraph: %v", err)
	}
	if err := applyDir(t, dsn, graphDir); err != nil {
		t.Fatalf("apply graph half: %v", err)
	}

	// Nothing the SDL did not mention was touched.
	var note string
	if err := pool.QueryRow(ctx, `SELECT private_note FROM persons`).Scan(&note); err != nil {
		t.Fatalf("the undeclared column was removed: %v", err)
	}
	if note != "keep me" {
		t.Errorf("private_note = %q, want %q", note, "keep me")
	}
	var amount int
	if err := pool.QueryRow(ctx, `SELECT amount FROM payroll`).Scan(&amount); err != nil {
		t.Fatalf("the undeclared table was removed: %v", err)
	}

	// A second run, with the database still holding what the SDL never
	// mentioned, must still emit nothing: absence from the SDL is not a
	// pending change.
	path, err := migrate.GenerateGraph(graphDir, model, "again")
	if err != nil {
		t.Fatalf("second GenerateGraph: %v", err)
	}
	if path != "" {
		data, _ := os.ReadFile(path)
		t.Errorf("an unchanged schema emitted a migration:\n%s", data)
	}

	// And the graph exposes only the declared properties.
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM GRAPH_TABLE (app_graph
			MATCH (p IS person) COLUMNS (p.name AS name))`).Scan(&n); err != nil {
		t.Fatalf("query the graph: %v", err)
	}
	if n != 1 {
		t.Errorf("graph returned %d rows, want 1", n)
	}
}

// TestGraphBeforeTablesFailsLoudly documents the ordering constraint by
// asserting the failure rather than describing it.
func TestGraphBeforeTablesFailsLoudly(t *testing.T) {
	_, dsn := freshDB(t)

	root := t.TempDir()
	dirs, err := migrate.WriteInitSplit(root, mustModel(t, personSDL))
	if err != nil {
		t.Fatalf("WriteInitSplit: %v", err)
	}
	if filepath.Base(dirs[0]) != migrate.TablesDir || filepath.Base(dirs[1]) != migrate.GraphDir {
		t.Fatalf("WriteInitSplit must return tables before graph, got %v", dirs)
	}

	// Applying the graph half first, against tables that do not exist.
	err = applyDir(t, dsn, dirs[1])
	if err == nil {
		t.Fatal("applying the graph half before the tables must fail")
	}
	if !strings.Contains(err.Error(), "persons") {
		t.Errorf("the error should name the missing table, got: %v", err)
	}

	// In the right order it works.
	if err := applyDir(t, dsn, dirs[0]); err != nil {
		t.Fatalf("apply tables: %v", err)
	}
	if err := applyDir(t, dsn, dirs[1]); err != nil {
		t.Fatalf("apply graph after tables: %v", err)
	}
}

// TestDropGraphKeepsTheData covers "drop the whole graph setup but keep the
// data" from the issue.
func TestDropGraphKeepsTheData(t *testing.T) {
	ctx := context.Background()
	pool, dsn := freshDB(t)

	root := t.TempDir()
	model := mustModel(t, personSDL)
	dirs, err := migrate.WriteInitSplit(root, model)
	if err != nil {
		t.Fatalf("WriteInitSplit: %v", err)
	}
	for _, d := range dirs {
		if err := applyDir(t, dsn, d); err != nil {
			t.Fatalf("apply %s: %v", d, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO persons (id, name) VALUES ('11111111-1111-1111-1111-111111111111', 'Ada')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The graph half, pointed at a schema that declares no graph.
	graphless := &schema.Schema{VertexTables: model.VertexTables, EdgeTables: model.EdgeTables}
	path, err := migrate.GenerateGraph(dirs[1], graphless, "drop graph")
	if err != nil {
		t.Fatalf("GenerateGraph (drop): %v", err)
	}
	if path == "" {
		t.Fatal("dropping the graph should emit a migration")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "DROP TABLE") || strings.Contains(string(data), "ALTER TABLE") {
		t.Errorf("dropping the graph must not touch a table:\n%s", data)
	}
	if err := applyDir(t, dsn, dirs[1]); err != nil {
		t.Fatalf("apply the drop: %v", err)
	}

	// The graph is gone; the row is not.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_propgraph_element`).Scan(&n); err == nil && n != 0 {
		t.Errorf("property graph elements still present: %d", n)
	}
	var name string
	if err := pool.QueryRow(ctx, `SELECT name FROM persons`).Scan(&name); err != nil {
		t.Fatalf("the data did not survive dropping the graph: %v", err)
	}
	if name != "Ada" {
		t.Errorf("name = %q, want Ada", name)
	}
}
