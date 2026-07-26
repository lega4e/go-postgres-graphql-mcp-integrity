// Package m4_test is the M4 integration suite. It boots a real postgres:19beta2
// container, generates and applies the initial migration from an SDL document
// carrying interfaces, seeds a follow graph containing a self-loop and a cycle,
// and proves the three things M4 adds (SPEC.md §7 → M4):
//
//   - Multi-hop MATCH chains. A three-hop query executes as one GRAPH_TABLE and
//     returns correct rows.
//   - The depth ceiling. A four-hop selection fails compilation with a typed
//     *compiler.DepthExceededError, and the pool's query tracer proves no
//     statement was sent to the database.
//   - Interfaces and isomorphism guards. An interface spanning two tables
//     traverses correctly — matched either by the shared label its implementors
//     expose or by alternation over their own labels — with self-matches
//     excluded by the `<>` guards PostgreSQL will not apply on its own.
package m4_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/jackc/pgx/v5"
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
// empty database, restored before every scenario (mirrors the M1/M2/M3 suites).
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
		Name:                "m4",
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("m4 feature scenarios failed")
	}
}

// queryCounter is a pgx.QueryTracer that counts every statement the driver
// sends. It is how the depth-rejection scenarios prove a rejected compilation
// never reaches the database: the count is read at the driver boundary, not
// from the step that would have executed the query.
type queryCounter struct{ n atomic.Int64 }

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *queryCounter) count() int64 { return c.n.Load() }

// scenarioState carries per-scenario state between steps.
type scenarioState struct {
	pool     *pgxpool.Pool
	queries  *queryCounter
	doc      *sdl.Document
	model    *schema.Schema
	response map[string]any
	// lastSQL / lastArgs record the most recent compilation; lastErr records a
	// compilation that was expected to fail.
	lastSQL  string
	lastArgs []any
	lastErr  error
	// queriesBefore is the driver's statement count as the compile step began,
	// so a rejection scenario can assert nothing was sent afterwards.
	queriesBefore int64
	dirs          []string
}

// InitializeScenario resets the database to the empty snapshot, opens a fresh
// traced pool, and registers steps.
func InitializeScenario(sc *godog.ScenarioContext) {
	st := &scenarioState{}

	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		if err := pgc.Restore(ctx); err != nil {
			return ctx, fmt.Errorf("restore snapshot: %w", err)
		}
		cfg, err := pgxpool.ParseConfig(connString)
		if err != nil {
			return ctx, fmt.Errorf("parse pool config: %w", err)
		}
		st.queries = &queryCounter{}
		cfg.ConnConfig.Tracer = st.queries
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
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
		st.lastErr = nil
		st.queriesBefore = 0
		return ctx, nil
	})

	sc.Step(`^the SDL:$`, st.theSDL)
	sc.Step(`^I generate and apply the initial migration via goose$`, st.generateAndApply)
	sc.Step(`^the following persons exist:$`, st.personsExist)
	sc.Step(`^the following bots exist:$`, st.botsExist)
	sc.Step(`^"([^"]*)" follows "([^"]*)"$`, st.follows)
	sc.Step(`^I compile and execute "([^"]*)" with variable "([^"]*)" bound to "([^"]*)"$`, st.compileWithVar)
	sc.Step(`^I compile and execute "([^"]*)"$`, st.compile)
	sc.Step(`^I compile "([^"]*)" with a max depth of (\d+)$`, st.compileOnlyMaxDepth)
	sc.Step(`^I compile "([^"]*)"$`, st.compileOnly)
	sc.Step(`^compilation failed with a depth error at max depth (\d+)$`, st.assertDepthError)
	sc.Step(`^no query reached the database$`, st.assertNoQuery)
	sc.Step(`^the compiled query is a single GRAPH_TABLE$`, st.assertSingleGraphTable)
	sc.Step(`^the compiled query bound the filter as a parameter$`, st.assertParameterised)
	sc.Step(`^the compiled query guards against self-matches$`, st.assertGuarded)
	sc.Step(`^the compiled query matches the shared label "([^"]*)"$`, st.assertSharedLabel)
	sc.Step(`^the compiled query alternates the labels "([^"]*)"$`, st.assertAlternation)
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
	dir, err := os.MkdirTemp("", "gopgql-m4-migrations-")
	if err != nil {
		return err
	}
	st.dirs = append(st.dirs, dir)
	// Both halves, in apply order: the graph references the tables, so the
	// tables directory has to go first.
	dirs, err := migrate.WriteInitSplit(dir, st.model)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, migrate.InitFilename)); err != nil {
			return fmt.Errorf("missing %s in %s: %w", migrate.InitFilename, d, err)
		}
	}

	db, err := sql.Open("pgx", connString)
	if err != nil {
		return fmt.Errorf("open sql.DB for goose: %w", err)
	}
	defer db.Close()
	if err := upAll(ctx, db, dirs); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

func (st *scenarioState) personsExist(ctx context.Context, table *godog.Table) error {
	return st.insertRows(ctx, table, "persons", []string{"name"})
}

func (st *scenarioState) botsExist(ctx context.Context, table *godog.Table) error {
	return st.insertRows(ctx, table, "bots", []string{"name", "vendor"})
}

// insertRows inserts one row per data row, taking the named columns from the
// table header. A column the table omits is left to its database default.
func (st *scenarioState) insertRows(ctx context.Context, table *godog.Table, into string, columns []string) error {
	cols := header(table)
	var present []string
	for _, c := range columns {
		if _, ok := cols[c]; ok {
			present = append(present, c)
		}
	}
	if len(present) == 0 {
		return fmt.Errorf("%s table needs at least one of the columns %v", into, columns)
	}
	placeholders := make([]string, len(present))
	for i := range present {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		into, strings.Join(present, ", "), strings.Join(placeholders, ", "))

	for _, row := range table.Rows[1:] {
		args := make([]any, len(present))
		for i, c := range present {
			args[i] = row.Cells[cols[c]].Value
		}
		if _, err := st.pool.Exec(ctx, stmt, args...); err != nil {
			return fmt.Errorf("insert into %s: %w", into, err)
		}
	}
	return nil
}

// follows inserts one edge. The source may be a person or a bot: the two map to
// separate edge tables that share the `follows` label, which is what lets one
// pattern traverse both (SPEC.md §5.3 invariant 5).
func (st *scenarioState) follows(ctx context.Context, from, to string) error {
	for _, tbl := range []struct{ name, source string }{
		{"follows", "persons"},
		{"bot_follows", "bots"},
	} {
		ct, err := st.pool.Exec(ctx, fmt.Sprintf(
			`INSERT INTO %s (source_id, target_id)
			 SELECT s.id, t.id FROM %s s, persons t
			 WHERE s.name = $1 AND t.name = $2`, tbl.name, tbl.source), from, to)
		if err != nil {
			return fmt.Errorf("insert %s %q->%q: %w", tbl.name, from, to, err)
		}
		if ct.RowsAffected() == 1 {
			return nil
		}
	}
	return fmt.Errorf("follows %q->%q matched no person or bot source", from, to)
}

// compile compiles and executes a query with no variables.
func (st *scenarioState) compile(ctx context.Context, query string) error {
	return st.compileAndExecute(ctx, query, nil)
}

// compileWithVar compiles and executes a query, binding the named variable to
// the given (string) value.
func (st *scenarioState) compileWithVar(ctx context.Context, query, name, value string) error {
	return st.compileAndExecute(ctx, query, map[string]any{name: value})
}

// compileOnly attempts a compilation that is expected to fail and records the
// error rather than returning it, so a following step can assert on its type.
// The driver's statement count is snapshotted first: nothing may be sent after
// this point.
func (st *scenarioState) compileOnly(query string) error {
	return st.attemptCompile(query, compiler.New(st.doc))
}

// compileOnlyMaxDepth is compileOnly with an explicitly configured ceiling,
// proving MaxDepth is configuration and not a constant (SPEC.md §6.2).
func (st *scenarioState) compileOnlyMaxDepth(query string, maxDepth int) error {
	return st.attemptCompile(query, compiler.New(st.doc, compiler.WithMaxDepth(maxDepth)))
}

func (st *scenarioState) attemptCompile(query string, c *compiler.Compiler) error {
	st.queriesBefore = st.queries.count()
	cq, err := c.CompileQuery(query, map[string]any{"n": "Alice"})
	st.lastErr = err
	if err == nil {
		st.lastSQL = cq.SQL
		return fmt.Errorf("expected %q to fail compilation, but it produced:\n%s", query, cq.SQL)
	}
	return nil
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

// assertDepthError requires the last compilation to have failed with the typed
// depth error carrying the expected ceiling — not merely with some error.
func (st *scenarioState) assertDepthError(maxDepth int) error {
	if st.lastErr == nil {
		return fmt.Errorf("expected a compilation failure, got none")
	}
	var depthErr *compiler.DepthExceededError
	if !errors.As(st.lastErr, &depthErr) {
		return fmt.Errorf("error is %T (%v), want *compiler.DepthExceededError", st.lastErr, st.lastErr)
	}
	if depthErr.MaxDepth != maxDepth {
		return fmt.Errorf("MaxDepth = %d, want %d", depthErr.MaxDepth, maxDepth)
	}
	if depthErr.Depth <= depthErr.MaxDepth {
		return fmt.Errorf("Depth = %d, which does not exceed MaxDepth %d", depthErr.Depth, depthErr.MaxDepth)
	}
	return nil
}

// assertNoQuery proves the rejection never became a database round-trip. The
// count comes from the pgx tracer, so it covers anything the driver would have
// sent, not just what this suite chose to execute.
func (st *scenarioState) assertNoQuery() error {
	if got := st.queries.count(); got != st.queriesBefore {
		return fmt.Errorf("%d statement(s) reached the database after the rejected compilation, want 0",
			got-st.queriesBefore)
	}
	if st.lastSQL != "" {
		return fmt.Errorf("a rejected compilation produced SQL:\n%s", st.lastSQL)
	}
	return nil
}

// assertSingleGraphTable confirms a multi-hop selection stayed one statement:
// nesting extends the MATCH pattern, it does not spawn a second query
// (SPEC.md §6.2).
func (st *scenarioState) assertSingleGraphTable() error {
	if n := strings.Count(st.lastSQL, "GRAPH_TABLE"); n != 1 {
		return fmt.Errorf("compiled SQL contains %d GRAPH_TABLE calls, want exactly 1:\n%s", n, st.lastSQL)
	}
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

// assertGuarded confirms the isomorphism guard is present. The shaped JSON in
// the same scenario proves it does its job; this checks it was gopgql that put
// it there rather than the data happening not to self-match.
func (st *scenarioState) assertGuarded() error {
	if !strings.Contains(st.lastSQL, ".id <> ") {
		return fmt.Errorf("compiled SQL carries no isomorphism guard:\n%s", st.lastSQL)
	}
	return nil
}

func (st *scenarioState) assertSharedLabel(label string) error {
	if !strings.Contains(st.lastSQL, "IS "+label+")") {
		return fmt.Errorf("compiled SQL does not match the shared label %q:\n%s", label, st.lastSQL)
	}
	return nil
}

func (st *scenarioState) assertAlternation(expr string) error {
	if !strings.Contains(st.lastSQL, "IS "+expr+")") {
		return fmt.Errorf("compiled SQL does not alternate labels as %q:\n%s", expr, st.lastSQL)
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

// upAll applies the migration directories in the order given — tables before
// the graph that references them.
func upAll(ctx context.Context, db *sql.DB, dirs []string) error {
	for _, d := range dirs {
		// Each half has its own version table — both start at 0001, so a
		// shared one makes goose skip the second half entirely.
		goose.SetTableName(migrate.VersionTable(filepath.Base(d)))
		if err := goose.UpContext(ctx, db, d); err != nil {
			return err
		}
	}
	return nil
}
