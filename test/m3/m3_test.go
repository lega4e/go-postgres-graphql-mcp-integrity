// Package m3_test is the M3 integration suite. It boots a real postgres:19beta2
// container, generates and applies the initial migration from an SDL document,
// seeds a small follow graph, compiles nested one-hop GraphQL queries — with a
// bound variable turned into a $n placeholder — executes the emitted
// GRAPH_TABLE SQL, and asserts on the shaped nested JSON: correct child nesting
// under each parent and no duplicated parents across the one-to-many fan-out
// (SPEC.md §7 → M3).
package m3_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
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

// A single container is shared across the suite; the baseline snapshot is the
// empty database, restored before every scenario (mirrors the M1/M2 suites).
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
		Name:                "m3",
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("m3 feature scenarios failed")
	}
}

// scenarioState carries per-scenario state between steps.
type scenarioState struct {
	pool     *pgxpool.Pool
	doc      *sdl.Document
	model    *schema.Schema
	response map[string]any
	// lastSQL / lastArgs record the most recent compilation for parameterisation
	// assertions.
	lastSQL  string
	lastArgs []any
	dirs     []string
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
		st.lastSQL = ""
		st.lastArgs = nil
		return ctx, nil
	})

	sc.Step(`^the SDL:$`, st.theSDL)
	sc.Step(`^I generate and apply the initial migration via goose$`, st.generateAndApply)
	sc.Step(`^the following persons exist:$`, st.personsExist)
	sc.Step(`^"([^"]*)" follows "([^"]*)"$`, st.follows)
	sc.Step(`^I compile and execute "([^"]*)" with variable "([^"]*)" bound to "([^"]*)"$`, st.compileWithVar)
	sc.Step(`^I compile and execute "([^"]*)"$`, st.compile)
	sc.Step(`^the compiled query bound the filter as a parameter$`, st.assertParameterised)
	sc.Step(`^the JSON response is:$`, st.assertJSON)
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
	dir, err := os.MkdirTemp("", "gopgql-m3-migrations-")
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
	for _, row := range table.Rows[1:] {
		name := row.Cells[nameIdx].Value
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO persons (name) VALUES ($1)`, name); err != nil {
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

// compile compiles and executes a query with no variables.
func (st *scenarioState) compile(ctx context.Context, query string) error {
	return st.compileAndExecute(ctx, query, nil)
}

// compileWithVar compiles and executes a query, binding the named variable to
// the given (string) value — proving GraphQL variables flow through as ordered
// bind parameters.
func (st *scenarioState) compileWithVar(ctx context.Context, query, name, value string) error {
	return st.compileAndExecute(ctx, query, map[string]any{name: value})
}

// compileAndExecute compiles the GraphQL query, runs the emitted GRAPH_TABLE SQL
// with its ordered bind parameters, and shapes the flat rows into the nested
// JSON response.
func (st *scenarioState) compileAndExecute(ctx context.Context, query string, vars map[string]any) error {
	c := compiler.New(st.doc)
	cq, err := c.CompileQuery(query, vars)
	if err != nil {
		return fmt.Errorf("compile %q: %w", query, err)
	}
	st.lastSQL = cq.SQL
	st.lastArgs = cq.Args

	rows, err := st.pool.Query(ctx, cq.SQL, cq.Args...)
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
	st.response = shape.Rows(cq.Projection, flat)
	return nil
}

// assertParameterised confirms the filter value was compiled to a bind
// parameter, never interpolated into the SQL text (SPEC.md §6.2).
func (st *scenarioState) assertParameterised() error {
	if !strings.Contains(st.lastSQL, "$1") {
		return fmt.Errorf("compiled SQL carries no $1 placeholder:\n%s", st.lastSQL)
	}
	if len(st.lastArgs) == 0 {
		return fmt.Errorf("expected at least one bind parameter, got none")
	}
	for _, a := range st.lastArgs {
		if s, ok := a.(string); ok && strings.Contains(st.lastSQL, "'"+s+"'") {
			return fmt.Errorf("bind value %q appears interpolated in the SQL:\n%s", s, st.lastSQL)
		}
	}
	return nil
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

// canon recursively canonicalizes decoded JSON so comparisons ignore array
// order (GraphQL list order for an unordered field is unspecified).
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
