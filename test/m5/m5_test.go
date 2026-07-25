// Package m5_test is the M5 integration suite. It boots a real postgres:19beta2
// container, applies a generated migration for a follow graph, and proves the
// multi-pattern workaround (SPEC.md §7 → M5):
//
//   - A level selecting two relationships compiles to separate GRAPH_TABLE calls
//     joined on projected ids — never the comma-separated pattern PG19 parses
//     but does not execute — and returns correct rows.
//   - The workaround is equivalent to a hand-written join: the same data shaped
//     through a hand-typed query gives an identical response.
//   - The split preserves what a single pattern gave for free: the root filter
//     reaches every branch, a parent missing one branch keeps the other, and the
//     isomorphism guards still exclude a branch that walks back to an ancestor.
//   - GRAPH_TABLE output joins ordinary relational tables.
package m5_test

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
// empty database, restored before every scenario (mirrors the M1–M4 suites).
var (
	pgc        *postgres.PostgresContainer
	connString string
)

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
		Name:                "m5",
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("m5 feature scenarios failed")
	}
}

// scenarioState carries per-scenario state between steps.
type scenarioState struct {
	pool     *pgxpool.Pool
	doc      *sdl.Document
	model    *schema.Schema
	response map[string]any
	// lastSQL / lastArgs / lastProj record the most recent compilation, so a
	// following step can assert on the emitted SQL or re-shape the rows of an
	// equivalent hand-written query through the same projection.
	lastSQL  string
	lastArgs []any
	lastProj compiler.Projection
	// rows holds the result of a hand-written statement executed by a step.
	rows []map[string]any
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
		st.lastSQL = ""
		st.lastArgs = nil
		st.lastProj = compiler.Projection{}
		st.rows = nil
		return ctx, nil
	})

	sc.Step(`^the SDL:$`, st.theSDL)
	sc.Step(`^I generate and apply the initial migration via goose$`, st.generateAndApply)
	sc.Step(`^the following persons exist:$`, st.personsExist)
	sc.Step(`^"([^"]*)" follows "([^"]*)"$`, st.follows)
	sc.Step(`^the ordinary table "([^"]*)":$`, st.ordinaryTable)
	sc.Step(`^I compile and execute "([^"]*)" with variable "([^"]*)" bound to "([^"]*)"$`, st.compileWithVar)
	sc.Step(`^I compile and execute "([^"]*)"$`, st.compile)
	sc.Step(`^I execute the SQL:$`, st.executeSQL)
	sc.Step(`^the compiled query splits into (\d+) GRAPH_TABLE calls$`, st.assertGraphTableCount)
	sc.Step(`^the compiled query emits no comma-separated pattern$`, st.assertNoCommaPattern)
	sc.Step(`^the compiled query joins the branches on projected ids$`, st.assertJoinOnIDs)
	sc.Step(`^the compiled query guards against self-matches$`, st.assertGuarded)
	sc.Step(`^the shaped result matches the hand-written query:$`, st.assertMatchesHandWritten)
	sc.Step(`^the rows are:$`, st.assertRows)
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
	dir, err := os.MkdirTemp("", "gopgql-m5-migrations-")
	if err != nil {
		return err
	}
	st.dirs = append(st.dirs, dir)
	path, err := migrate.WriteInit(dir, st.model)
	if err != nil {
		return err
	}
	if got := filepath.Base(path); got != migrate.InitFilename {
		return fmt.Errorf("migration filename = %s, want %s", got, migrate.InitFilename)
	}

	db, err := sql.Open("pgx", connString)
	if err != nil {
		return fmt.Errorf("open sql.DB for goose: %w", err)
	}
	defer db.Close()
	if err := goose.UpContext(ctx, db, dir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

func (st *scenarioState) personsExist(ctx context.Context, table *godog.Table) error {
	cols := header(table)
	name, ok := cols["name"]
	if !ok {
		return fmt.Errorf("persons table needs a name column")
	}
	for _, row := range table.Rows[1:] {
		if _, err := st.pool.Exec(ctx, `INSERT INTO persons (name) VALUES ($1)`, row.Cells[name].Value); err != nil {
			return fmt.Errorf("insert person: %w", err)
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
		return fmt.Errorf("follows %q->%q matched %d rows, want 1", from, to, ct.RowsAffected())
	}
	return nil
}

// ordinaryTable creates a plain relational table — one outside the property
// graph entirely — and seeds it, so a scenario can prove GRAPH_TABLE output
// joins ordinary tables (SPEC.md §7 → M5).
func (st *scenarioState) ordinaryTable(ctx context.Context, name string, table *godog.Table) error {
	if len(table.Rows) == 0 {
		return fmt.Errorf("table %q needs a header row", name)
	}
	var cols []string
	for _, cell := range table.Rows[0].Cells {
		cols = append(cols, cell.Value+" text")
	}
	if _, err := st.pool.Exec(ctx, fmt.Sprintf("CREATE TABLE %s (%s)", name, strings.Join(cols, ", "))); err != nil {
		return fmt.Errorf("create table %s: %w", name, err)
	}

	names := make([]string, len(table.Rows[0].Cells))
	placeholders := make([]string, len(table.Rows[0].Cells))
	for i, cell := range table.Rows[0].Cells {
		names[i] = cell.Value
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", name,
		strings.Join(names, ", "), strings.Join(placeholders, ", "))
	for _, row := range table.Rows[1:] {
		args := make([]any, len(row.Cells))
		for i, cell := range row.Cells {
			args[i] = cell.Value
		}
		if _, err := st.pool.Exec(ctx, stmt, args...); err != nil {
			return fmt.Errorf("insert into %s: %w", name, err)
		}
	}
	return nil
}

func (st *scenarioState) compile(ctx context.Context, query string) error {
	return st.compileAndExecute(ctx, query, nil)
}

func (st *scenarioState) compileWithVar(ctx context.Context, query, name, value string) error {
	return st.compileAndExecute(ctx, query, map[string]any{name: value})
}

// compileAndExecute compiles the GraphQL query, runs the emitted SQL with its
// ordered bind parameters, and shapes the flat rows into the nested response.
func (st *scenarioState) compileAndExecute(ctx context.Context, query string, vars map[string]any) error {
	cq, err := compiler.New(st.doc).CompileQuery(query, vars)
	if err != nil {
		return fmt.Errorf("compile %q: %w", query, err)
	}
	st.lastSQL = cq.SQL
	st.lastArgs = cq.Args
	st.lastProj = cq.Projection

	flat, err := st.query(ctx, cq.SQL, cq.Args...)
	if err != nil {
		return err
	}
	st.response = shape.Rows(cq.Projection, flat)
	return nil
}

// executeSQL runs a hand-written statement and keeps its rows for assertion.
func (st *scenarioState) executeSQL(ctx context.Context, stmt *godog.DocString) error {
	rows, err := st.query(ctx, stmt.Content)
	if err != nil {
		return err
	}
	st.rows = rows
	return nil
}

// query runs a statement and returns its rows as column-name maps.
func (st *scenarioState) query(ctx context.Context, stmt string, args ...any) ([]map[string]any, error) {
	rows, err := st.pool.Query(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("execute SQL: %w\nSQL:\n%s", err, stmt)
	}
	defer rows.Close()

	fds := rows.FieldDescriptions()
	var out []map[string]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		m := make(map[string]any, len(fds))
		for i, fd := range fds {
			m[fd.Name] = vals[i]
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// assertGraphTableCount proves the split happened — and how far it went.
func (st *scenarioState) assertGraphTableCount(want int) error {
	if got := strings.Count(st.lastSQL, "GRAPH_TABLE"); got != want {
		return fmt.Errorf("compiled SQL contains %d GRAPH_TABLE calls, want %d:\n%s", got, want, st.lastSQL)
	}
	return nil
}

// assertNoCommaPattern proves gopgql did not emit the construct PG19 parses but
// refuses to execute: two path patterns separated by a comma inside one MATCH.
func (st *scenarioState) assertNoCommaPattern() error {
	for _, line := range strings.Split(st.lastSQL, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "MATCH ") {
			continue
		}
		if strings.Contains(trimmed, ",") {
			return fmt.Errorf("MATCH carries a comma-separated pattern:\n%s", line)
		}
	}
	return nil
}

// assertJoinOnIDs proves the branches are stitched back together on projected
// ids, and that a branch missing its match cannot delete the parent row.
func (st *scenarioState) assertJoinOnIDs() error {
	if !strings.Contains(st.lastSQL, "LEFT JOIN") {
		return fmt.Errorf("compiled SQL has no join:\n%s", st.lastSQL)
	}
	if !strings.Contains(st.lastSQL, "_j = ") {
		return fmt.Errorf("compiled SQL does not join on a projected id column:\n%s", st.lastSQL)
	}
	return nil
}

func (st *scenarioState) assertGuarded() error {
	if !strings.Contains(st.lastSQL, ".id <> ") && !strings.Contains(st.lastSQL, "_k <> ") {
		return fmt.Errorf("compiled SQL carries no isomorphism guard:\n%s", st.lastSQL)
	}
	return nil
}

// assertMatchesHandWritten runs an equivalent hand-typed query over the same
// data, shapes it through the same projection, and requires an identical
// response — the milestone's exit criterion (SPEC.md §7 → M5).
func (st *scenarioState) assertMatchesHandWritten(ctx context.Context, stmt *godog.DocString) error {
	if st.response == nil {
		return fmt.Errorf("no compiled response to compare against; compile and execute first")
	}
	flat, err := st.query(ctx, stmt.Content, st.lastArgs...)
	if err != nil {
		return err
	}
	want := shape.Rows(st.lastProj, flat)

	gotBytes, err := json.Marshal(st.response)
	if err != nil {
		return err
	}
	wantBytes, err := json.Marshal(want)
	if err != nil {
		return err
	}
	var gotAny, wantAny any
	if err := json.Unmarshal(gotBytes, &gotAny); err != nil {
		return err
	}
	if err := json.Unmarshal(wantBytes, &wantAny); err != nil {
		return err
	}
	if !reflect.DeepEqual(canon(gotAny), canon(wantAny)) {
		return fmt.Errorf("the workaround disagrees with the hand-written query:\n--- compiled ---\n%s\n--- hand-written ---\n%s\n--- compiled SQL ---\n%s",
			gotBytes, wantBytes, st.lastSQL)
	}
	return nil
}

// assertRows compares the rows of a hand-written statement with a table, by
// column name, in order.
func (st *scenarioState) assertRows(table *godog.Table) error {
	if len(table.Rows) == 0 {
		return fmt.Errorf("expected-rows table needs a header")
	}
	cols := table.Rows[0].Cells
	if len(st.rows) != len(table.Rows)-1 {
		return fmt.Errorf("got %d rows, want %d:\n%v", len(st.rows), len(table.Rows)-1, st.rows)
	}
	for i, row := range table.Rows[1:] {
		for j, cell := range row.Cells {
			name := cols[j].Value
			got := fmt.Sprintf("%v", st.rows[i][name])
			if got != cell.Value {
				return fmt.Errorf("row %d column %q = %q, want %q", i+1, name, got, cell.Value)
			}
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
		return fmt.Errorf("JSON mismatch:\n--- got ---\n%s\n--- want ---\n%s\n--- SQL ---\n%s",
			gotBytes, want.Content, st.lastSQL)
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
