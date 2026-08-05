// Package m1_test is the M1 integration suite. It boots a real postgres:19beta2
// container, generates the initial migration from an SDL document, applies it
// via goose, seeds rows, runs the compiler-produced GRAPH_TABLE SQL, and
// asserts on the returned data — the nested JSON response (SPEC.md §7 → M1).
//
// It also carries the SPEC.md §6.3 spike: a scenario proving that a bind
// parameter ($1) works inside a GRAPH_TABLE MATCH/WHERE against real PG19.
package m1_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/jackc/pgx/v5/pgxpool"
	// Registers the "pgx" database/sql driver used by goose and by
	// postgres.WithSQLDriver for Snapshot/Restore.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/schema"
	"github.com/lega4e/gopgql/sdl"
	"github.com/lega4e/gopgql/shape"
)

// A single container is shared across the suite. The baseline snapshot is the
// empty database (no gopgql schema), so every scenario restores to empty and
// re-applies the generated migration via goose — exercising generation and
// application, not just a static fixture.
var (
	pgc        *postgres.PostgresContainer
	connString string
)

// TestFeatures is the godog entry point under `go test`. It always runs against
// a real postgres:19beta2 container — there is no skip path (SPEC.md §10: every
// milestone proves itself against real infrastructure, never a mock).
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

	// Snapshot the empty database as the per-scenario reset baseline.
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
		Name:                "m1",
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("m1 feature scenarios failed")
	}
}

// scenarioState carries per-scenario state between steps.
type scenarioState struct {
	pool  *pgxpool.Pool
	doc   *sdl.Document
	model *schema.Schema
	// response is the shaped JSON response from the last compiled query.
	response map[string]any
	// names is the result of the bind-parameter spike query.
	names []string
	// dirs holds temp migration directories to clean up.
	dirs []string
}

// InitializeScenario resets the database to the empty snapshot, opens a fresh
// pool, and registers steps.
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
		return ctx, nil
	})

	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		if st.pool != nil {
			st.pool.Close()
			st.pool = nil
		}
		for _, d := range st.dirs {
			_ = os.RemoveAll(d)
		}
		st.dirs = nil
		st.doc = nil
		st.model = nil
		st.response = nil
		st.names = nil
		return ctx, nil
	})

	sc.Step(`^the SDL:$`, st.theSDL)
	sc.Step(`^I generate and apply the initial migration via goose$`, st.generateAndApply)
	sc.Step(`^the following persons exist:$`, st.personsExist)
	sc.Step(`^"([^"]*)" follows "([^"]*)"$`, st.follows)
	sc.Step(`^I compile and execute "([^"]*)"$`, st.compileAndExecute)
	sc.Step(`^the JSON response is:$`, st.assertJSON)
	sc.Step(`^I filter persons by name "([^"]*)" using a bind parameter inside GRAPH_TABLE$`, st.filterByBindParam)
	sc.Step(`^the returned names are:$`, st.assertNames)
}

// theSDL parses and validates the SDL and builds the physical schema model.
func (st *scenarioState) theSDL(src *godog.DocString) error {
	doc, err := sdl.Parse(src.Content)
	if err != nil {
		return err
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		return err
	}
	st.model = m
	st.doc = doc
	return nil
}

// generateAndApply writes 0001_init.sql and applies it with goose.
func (st *scenarioState) generateAndApply(ctx context.Context) error {
	if st.model == nil {
		return fmt.Errorf("no schema model; the SDL step must run first")
	}
	dir, err := os.MkdirTemp("", "gopgql-m1-migrations-")
	if err != nil {
		return err
	}
	st.dirs = append(st.dirs, dir)
	// One directory, one history: the tables, then the property graph over
	// them. Nothing here decides an order — the file numbering is the order.
	paths, err := migrate.Generate(dir, st.model, "init", migrate.Halves{})
	if err != nil {
		return err
	}
	if len(paths) != 2 {
		return fmt.Errorf("a first generation is two migrations, got %v", paths)
	}

	db, err := sql.Open("pgx", connString)
	if err != nil {
		return fmt.Errorf("open sql.DB for goose: %w", err)
	}
	defer db.Close()
	if err := upAll(ctx, db, dir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

func (st *scenarioState) personsExist(ctx context.Context, table *godog.Table) error {
	cols := header(table)
	nameIdx, ok := cols["name"]
	if !ok {
		return fmt.Errorf("persons table needs a 'name' column")
	}
	emailIdx, hasEmail := cols["email"]
	for _, row := range table.Rows[1:] {
		name := row.Cells[nameIdx].Value
		var email any
		if hasEmail {
			if v := row.Cells[emailIdx].Value; v != "" {
				email = v
			}
		}
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO persons (name, email) VALUES ($1, $2)`, name, email); err != nil {
			return fmt.Errorf("insert person %q: %w", name, err)
		}
	}
	return nil
}

func (st *scenarioState) follows(ctx context.Context, from, to string) error {
	ct, err := st.pool.Exec(ctx,
		`INSERT INTO follows (source_id, target_id)
		 SELECT s.id, t.id FROM persons s, persons t
		 WHERE s.name = $1 AND t.name = $2`, from, to)
	if err != nil {
		return fmt.Errorf("insert follows %q->%q: %w", from, to, err)
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("follows %q->%q affected %d rows, want 1", from, to, ct.RowsAffected())
	}
	return nil
}

// compileAndExecute compiles the GraphQL query, runs the emitted GRAPH_TABLE
// SQL, and shapes the flat rows into the nested JSON response.
func (st *scenarioState) compileAndExecute(ctx context.Context, query string) error {
	c := compiler.New(st.doc)
	cq, err := c.CompileQuery(query, nil)
	if err != nil {
		return fmt.Errorf("compile %q: %w", query, err)
	}
	rows, err := st.pool.Query(ctx, cq.SQL)
	if err != nil {
		return fmt.Errorf("execute compiled SQL: %w\nSQL:\n%s", err, cq.SQL)
	}
	defer rows.Close()

	fds := rows.FieldDescriptions()
	var flat []map[string]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return err
		}
		m := make(map[string]any, len(fds))
		for i, fd := range fds {
			m[fd.Name] = vals[i]
		}
		flat = append(flat, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	st.response, err = shape.Rows(cq.Projection, flat)
	return err
}

func (st *scenarioState) assertJSON(want *godog.DocString) error {
	gotBytes, err := json.Marshal(st.response)
	if err != nil {
		return err
	}
	var gotAny, wantAny any
	if err := json.Unmarshal(gotBytes, &gotAny); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(want.Content), &wantAny); err != nil {
		return fmt.Errorf("expected JSON is invalid: %w", err)
	}
	if !reflect.DeepEqual(canon(gotAny), canon(wantAny)) {
		return fmt.Errorf("JSON mismatch:\n--- got ---\n%s\n--- want ---\n%s", gotBytes, want.Content)
	}
	return nil
}

// filterByBindParam is the SPEC.md §6.3 spike: it runs a GRAPH_TABLE query with
// a bind parameter ($1) inside the MATCH/WHERE and collects the matched names.
func (st *scenarioState) filterByBindParam(ctx context.Context, name string) error {
	const q = `SELECT name FROM GRAPH_TABLE (app_graph
  MATCH (v IS person)
  WHERE v.name = $1
  COLUMNS (v.name AS name))`
	rows, err := st.pool.Query(ctx, q, name)
	if err != nil {
		return fmt.Errorf("bind-parameter GRAPH_TABLE query failed (spike): %w", err)
	}
	defer rows.Close()
	st.names = nil
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		st.names = append(st.names, n)
	}
	return rows.Err()
}

func (st *scenarioState) assertNames(table *godog.Table) error {
	want := make([]string, 0, len(table.Rows)-1)
	for _, row := range table.Rows[1:] {
		want = append(want, row.Cells[0].Value)
	}
	got := append([]string(nil), st.names...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("names = %v, want %v", got, want)
	}
	return nil
}

// canon recursively canonicalizes decoded JSON so comparisons ignore array
// order (GraphQL list order for an unordered root field is unspecified).
func canon(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = canon(val)
		}
		return m
	case []any:
		items := make([]any, len(t))
		for i, val := range t {
			items[i] = canon(val)
		}
		sort.Slice(items, func(i, j int) bool {
			bi, _ := json.Marshal(items[i])
			bj, _ := json.Marshal(items[j])
			return string(bi) < string(bj)
		})
		return items
	default:
		return v
	}
}

// header maps column names to their index in the table's first row.
func header(table *godog.Table) map[string]int {
	cols := map[string]int{}
	if len(table.Rows) == 0 {
		return cols
	}
	for i, cell := range table.Rows[0].Cells {
		cols[cell.Value] = i
	}
	return cols
}

// upAll applies every pending migration in dir, in ascending version order.
//
// A plain forward apply against goose's own default version table is all it
// takes. The migrations are one chronological history, so the order they need
// is the order goose already applies them in — no half to interleave, no
// version table to select (gopgql#38, design D3).
func upAll(ctx context.Context, db *sql.DB, dir string) error {
	return goose.UpContext(ctx, db, dir)
}
