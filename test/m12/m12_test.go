// Package m12_test is the M12 integration suite. It boots a real
// postgres:19beta2 container, lets a hand-written init script create a schema
// gopgql does not own — standing in for the tool that really owns it — and then
// proves that gopgql surfaces it without touching it (SPEC.md §7 → M12):
//
//   - A generation over a @readonly type contains no CREATE TABLE, no ALTER, no
//     DROP and no CREATE INDEX for it, in either direction; the assertion is made
//     over the emitted files, because a model that merely *intends* to emit
//     nothing is not the same as a directory that contains nothing.
//   - The migration applies with goose and a compiled query returns the rows the
//     init script seeded — so the property graph really does span the two
//     schemas, and PG19beta2 really does accept one that does.
//   - `seq: Int! @column(name: "offset")` works end to end against a real column
//     called `offset`. `offset` is a reserved word, so an unquoted one is a
//     syntax error rather than a style problem; this is the regression scenario
//     that keeps it quoted (issue item 4).
//   - The managed half of the same SDL is generated exactly as before, in the
//     same generation, and a later delta still alters it.
package m12_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/jackc/pgx/v5/pgxpool"
	// Registers the "pgx" database/sql driver goose runs through.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/schema"
	"github.com/lega4e/gopgql/sdl"
)

var (
	pgc        *postgres.PostgresContainer
	connString string
)

// fixtureSQL is what the other tool created. gopgql never writes any of it, and
// the column called "offset" is here because that is the shape the requirement
// came from: it is a reserved word, and it is a real column.
const fixtureSQL = `
CREATE SCHEMA dbos;

CREATE TABLE dbos.streams (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    topic    text NOT NULL,
    "offset" integer NOT NULL,
    payload  jsonb NOT NULL DEFAULT '{}'::jsonb
);

INSERT INTO dbos.streams (topic, "offset", payload) VALUES
    ('orders', 1, '{"kind":"created"}'),
    ('orders', 2, '{"kind":"paid"}'),
    ('audit',  7, '{"kind":"login"}');
`

// TestFeatures is the godog entry point under `go test`. It always runs against
// a real postgres:19beta2 container — there is no skip path (SPEC.md §10).
func TestFeatures(t *testing.T) {
	ctx := context.Background()

	var err error
	pgc, err = postgres.Run(ctx, "postgres:19beta2",
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
		t.Fatalf("start postgres:19beta2 container: %v", err)
	}
	t.Cleanup(func() {
		if err := pgc.Terminate(context.Background()); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	if err := pgc.Snapshot(ctx); err != nil {
		t.Fatalf("snapshot baseline: %v", err)
	}

	connString, err = pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose set dialect: %v", err)
	}
	goose.SetLogger(goose.NopLogger())

	suite := godog.TestSuite{
		Name:                "m12",
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("m12 feature scenarios failed")
	}
}

type scenarioState struct {
	pool  *pgxpool.Pool
	doc   *sdl.Document
	model *schema.Schema
	dir   string

	// generated is the text of the files the most recent Generate wrote, and
	// nothing else: every "no migration mentions" assertion is made over what
	// was actually emitted.
	generated []string
	response  map[string]any
}

func InitializeScenario(sc *godog.ScenarioContext) {
	st := &scenarioState{}

	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		if err := pgc.Restore(ctx); err != nil {
			return ctx, fmt.Errorf("restore snapshot: %w", err)
		}
		pool, err := pgxpool.New(ctx, connString)
		if err != nil {
			return ctx, fmt.Errorf("open pool: %w", err)
		}
		st.pool = pool
		st.dir, err = os.MkdirTemp("", "gopgql-m12-migrations-")
		return ctx, err
	})

	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		if st.pool != nil {
			st.pool.Close()
		}
		if st.dir != "" {
			_ = os.RemoveAll(st.dir)
		}
		*st = scenarioState{}
		return ctx, nil
	})

	sc.Step(`^the schema "([^"]*)" already exists with its own tables and rows$`, st.installFixture)
	sc.Step(`^the SDL:$`, st.theSDL)
	sc.Step(`^I generate the migrations$`, st.generate)
	sc.Step(`^I generate the migrations again$`, st.generateAgain)
	sc.Step(`^I generate a delta for the SDL:$`, st.generateDelta)
	sc.Step(`^I apply the migrations with goose$`, st.apply)
	sc.Step(`^I compile and execute:$`, st.compileAndExecute)
	sc.Step(`^no migration contains "([^"]*)"$`, st.assertNoMention)
	sc.Step(`^the property graph names "([^"]*)"$`, st.assertGraphNames)
	sc.Step(`^the delta contains no "([^"]*)"$`, st.assertNoMention)
	sc.Step(`^the delta contains "([^"]*)"$`, st.assertContains)
	sc.Step(`^the delta rebuilds the property graph$`, st.assertGraphRebuilt)
	sc.Step(`^nothing was generated$`, st.assertNothingGenerated)
	sc.Step(`^the JSON response is:$`, st.assertJSON)
	sc.Step(`^the table "([^"]*)" still has a column named "([^"]*)"$`, st.assertColumn)
	sc.Step(`^the table "([^"]*)" exists$`, st.assertTableExists)
	sc.Step(`^the table "([^"]*)" has no column "([^"]*)"$`, st.assertNoColumn)
	sc.Step(`^the property graph exposes the elements "([^"]*)"$`, st.assertElements)
}

func (st *scenarioState) installFixture(ctx context.Context, name string) error {
	if name != "dbos" {
		return fmt.Errorf("the fixture creates the schema %q, not %q", "dbos", name)
	}
	_, err := st.pool.Exec(ctx, fixtureSQL)
	return err
}

func (st *scenarioState) theSDL(src *godog.DocString) error {
	doc, err := sdl.Parse(src.Content)
	if err != nil {
		return err
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		return err
	}
	st.doc, st.model = doc, m
	return nil
}

func (st *scenarioState) generate() error {
	paths, err := migrate.Generate(st.dir, st.model, "schema", migrate.Halves{})
	if err != nil {
		return err
	}
	return st.record(paths)
}

// generateAgain regenerates from the same SDL. It is separate from generate
// only so the feature file can say "again" and then assert that nothing came of
// it — the round trip that an unmanaged table would otherwise break.
func (st *scenarioState) generateAgain() error {
	paths, err := migrate.Generate(st.dir, st.model, "again", migrate.Halves{})
	if err != nil {
		return err
	}
	return st.record(paths)
}

func (st *scenarioState) generateDelta(src *godog.DocString) error {
	if err := st.theSDL(src); err != nil {
		return err
	}
	paths, err := migrate.Generate(st.dir, st.model, "delta", migrate.Halves{})
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("expected a delta, but the schemas were identical")
	}
	return st.record(paths)
}

func (st *scenarioState) record(paths []string) error {
	st.generated = nil
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		st.generated = append(st.generated, filepath.Base(p)+"\n"+string(data))
	}
	return nil
}

func (st *scenarioState) apply(ctx context.Context) error {
	db, err := sql.Open("pgx", connString)
	if err != nil {
		return err
	}
	defer db.Close()
	return goose.UpContext(ctx, db, st.dir)
}

func (st *scenarioState) compileAndExecute(ctx context.Context, op *godog.DocString) error {
	if st.doc == nil {
		return fmt.Errorf("no schema; the SDL step must run first")
	}
	cq, err := compiler.New(st.doc).CompileQuery(op.Content, nil)
	if err != nil {
		return err
	}
	res, err := exec.Query(ctx, st.pool, cq)
	if err != nil {
		return err
	}
	st.response = res
	return nil
}

func (st *scenarioState) assertNoMention(needle string) error {
	for _, file := range st.generated {
		if strings.Contains(file, needle) {
			return fmt.Errorf("a generated migration contains %q, which gopgql must never emit for a table "+
				"it does not own:\n%s", needle, file)
		}
	}
	return nil
}

// assertGraphNames is the other half of the rule, and it is asserted in the
// same scenario on purpose: an unmanaged table is absent from every statement
// that would *change* it and present in the one that surfaces it. A check that
// only looked for absence would pass just as well if the table had been dropped
// from the graph altogether.
func (st *scenarioState) assertGraphNames(qualified string) error {
	for _, file := range st.generated {
		if strings.Contains(file, "CREATE PROPERTY GRAPH") && strings.Contains(file, qualified) {
			return nil
		}
	}
	return fmt.Errorf("no CREATE PROPERTY GRAPH names %q:\n%s", qualified, strings.Join(st.generated, "\n"))
}

func (st *scenarioState) assertContains(needle string) error {
	for _, file := range st.generated {
		if strings.Contains(file, needle) {
			return nil
		}
	}
	return fmt.Errorf("no generated migration contains %q:\n%s", needle, strings.Join(st.generated, "\n"))
}

func (st *scenarioState) assertGraphRebuilt() error {
	if err := st.assertContains("CREATE PROPERTY GRAPH"); err != nil {
		return err
	}
	return st.assertContains("DROP PROPERTY GRAPH IF EXISTS")
}

func (st *scenarioState) assertNothingGenerated() error {
	if len(st.generated) != 0 {
		return fmt.Errorf("an unchanged schema planned %d migrations:\n%s",
			len(st.generated), strings.Join(st.generated, "\n"))
	}
	return nil
}

func (st *scenarioState) assertJSON(want *godog.DocString) error {
	var expected map[string]any
	if err := json.Unmarshal([]byte(want.Content), &expected); err != nil {
		return fmt.Errorf("parse expected JSON: %w", err)
	}
	// Round-tripping the actual response through JSON normalises the numeric
	// types pgx returns, so the comparison is about the data rather than about
	// whether an integer arrived as int32 or int64.
	raw, err := json.Marshal(st.response)
	if err != nil {
		return err
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, got) {
		return fmt.Errorf("response is %s, want %s", raw, want.Content)
	}
	return nil
}

// columnExists asks the catalog directly, so what is asserted is the database's
// own view rather than gopgql's.
func (st *scenarioState) columnExists(ctx context.Context, qualified, column string) (bool, error) {
	schemaName, table, found := strings.Cut(qualified, ".")
	if !found {
		return false, fmt.Errorf("%q is not a schema-qualified table name", qualified)
	}
	rows, err := exec.Rows(ctx, st.pool, `
		SELECT count(*) AS n
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2 AND column_name = $3`,
		schemaName, table, column)
	if err != nil {
		return false, err
	}
	return count(rows) > 0, nil
}

func (st *scenarioState) assertColumn(ctx context.Context, qualified, column string) error {
	ok, err := st.columnExists(ctx, qualified, column)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s has no column %q — gopgql must not have changed it", qualified, column)
	}
	return nil
}

func (st *scenarioState) assertNoColumn(ctx context.Context, qualified, column string) error {
	ok, err := st.columnExists(ctx, qualified, column)
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("%s still has column %q", qualified, column)
	}
	return nil
}

func (st *scenarioState) assertTableExists(ctx context.Context, qualified string) error {
	schemaName, table, found := strings.Cut(qualified, ".")
	if !found {
		return fmt.Errorf("%q is not a schema-qualified table name", qualified)
	}
	rows, err := exec.Rows(ctx, st.pool, `
		SELECT count(*) AS n
		FROM information_schema.tables
		WHERE table_schema = $1 AND table_name = $2`, schemaName, table)
	if err != nil {
		return err
	}
	if count(rows) == 0 {
		return fmt.Errorf("%s does not exist", qualified)
	}
	return nil
}

// assertElements reads the graph's elements back out of the catalogs, which is
// the only evidence that a property graph spanning two schemas was accepted.
func (st *scenarioState) assertElements(ctx context.Context, want string) error {
	rows, err := exec.Rows(ctx, st.pool, `
		SELECT c.relname::text AS name
		FROM pg_catalog.pg_propgraph_element e
		JOIN pg_catalog.pg_class c ON c.oid = e.pgerelid
		ORDER BY c.relname`)
	if err != nil {
		return err
	}
	var got []string
	for _, row := range rows {
		name, _ := row["name"].(string)
		got = append(got, name)
	}
	sort.Strings(got)

	expected := strings.Split(want, ", ")
	sort.Strings(expected)
	if !reflect.DeepEqual(expected, got) {
		return fmt.Errorf("the graph exposes %v, want %v", got, expected)
	}
	return nil
}

func count(rows []map[string]any) int {
	if len(rows) != 1 {
		return 0
	}
	switch n := rows[0]["n"].(type) {
	case int64:
		return int(n)
	case int32:
		return int(n)
	default:
		return 0
	}
}
