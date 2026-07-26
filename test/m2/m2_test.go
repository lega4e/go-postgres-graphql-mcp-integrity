// Package m2_test is the M2 integration suite. It boots a real postgres:19beta2
// container and proves delta migration generation end to end (SPEC.md §7 → M2):
// it applies an initial migration, folds it back into a schema, diffs it against
// a widened SDL, applies the generated delta, and asserts on returned data. A
// third scenario proves fold correctness by comparing a folded apply to a direct
// apply of the same final schema.
package m2_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
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
// empty database, restored before each scenario.
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
		Name:                "m2",
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("m2 feature scenarios failed")
	}
}

// scenarioState carries per-scenario state between steps.
type scenarioState struct {
	pool *pgxpool.Pool
	// dir is the active migration directory; doc is the desired document the
	// compiler queries against.
	dir string
	// halves are the migration directories in apply order: tables, then graph.
	halves []string
	doc    *sdl.Document
	// response is the shaped JSON response from the last compiled query.
	response map[string]any
	// base/widened models for the fold-correctness scenario.
	baseModel    *schema.Schema
	widenedModel *schema.Schema
	// foldedFP/directFP are schema fingerprints compared for fold correctness.
	foldedFP string
	directFP string
	// dirs holds temp migration directories to clean up.
	dirs []string
}

// InitializeScenario resets the database, opens a fresh pool, and registers
// steps.
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
		st.dir = ""
		st.doc = nil
		st.response = nil
		st.baseModel = nil
		st.widenedModel = nil
		st.foldedFP = ""
		st.directFP = ""
		return ctx, nil
	})

	sc.Step(`^the SDL is applied as the initial migration:$`, st.applyInitial)
	sc.Step(`^I generate and apply a delta migration from the SDL:$`, st.generateAndApplyDelta)
	sc.Step(`^the following persons exist:$`, st.personsExist)
	sc.Step(`^I compile and execute "([^"]*)"$`, st.compileAndExecute)
	sc.Step(`^the JSON response is:$`, st.assertJSON)
	sc.Step(`^the "([^"]*)" table has no column "([^"]*)"$`, st.tableHasNoColumn)

	sc.Step(`^the base SDL:$`, st.baseSDL)
	sc.Step(`^the widened SDL:$`, st.widenedSDL)
	sc.Step(`^I apply the base SDL then a delta to the widened SDL$`, st.applyFoldedPath)
	sc.Step(`^I apply the widened SDL directly as a single migration$`, st.applyDirectPath)
	sc.Step(`^the two resulting schemas are identical$`, st.assertSchemasIdentical)
}

// applyInitial parses the SDL, writes 0001_init.sql to a fresh directory, and
// applies it via goose.
func (st *scenarioState) applyInitial(ctx context.Context, src *godog.DocString) error {
	doc, err := sdl.Parse(src.Content)
	if err != nil {
		return err
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "gopgql-m2-migrations-")
	if err != nil {
		return err
	}
	st.dirs = append(st.dirs, dir)
	st.dir = dir
	st.doc = doc

	dirs, err := migrate.WriteInitSplit(dir, m)
	if err != nil {
		return err
	}
	st.halves = dirs
	return applyAll(ctx, dirs)
}

// generateAndApplyDelta folds the migrations in the active directory, diffs
// against the widened SDL, writes the delta, and applies it.
func (st *scenarioState) generateAndApplyDelta(ctx context.Context, src *godog.DocString) error {
	if st.dir == "" {
		return fmt.Errorf("no initial migration; the initial-migration step must run first")
	}
	doc, err := sdl.Parse(src.Content)
	if err != nil {
		return err
	}
	desired, err := generator.Build(doc, "")
	if err != nil {
		return err
	}
	paths, err := migrate.GenerateSplit(st.dir, desired, "delta")
	if err != nil {
		return fmt.Errorf("generate delta: %w", err)
	}
	var wrote []string
	for _, p := range paths {
		if p != "" {
			wrote = append(wrote, p)
		}
	}
	if len(wrote) == 0 {
		return fmt.Errorf("expected a delta migration, but the schemas were identical")
	}
	for _, p := range wrote {
		if base := filepath.Base(p); base[:4] != "0002" {
			return fmt.Errorf("delta filename = %s, want a 0002_* file", base)
		}
	}
	st.doc = doc
	return applyAll(ctx, st.halves)
}

func (st *scenarioState) personsExist(ctx context.Context, table *godog.Table) error {
	cols := header(table)
	nameIdx, ok := cols["name"]
	if !ok {
		return fmt.Errorf("persons table needs a 'name' column")
	}
	emailIdx, hasEmail := cols["email"]
	ageIdx, hasAge := cols["age"]
	for _, row := range table.Rows[1:] {
		fields := []string{"name"}
		args := []any{row.Cells[nameIdx].Value}
		if hasEmail {
			fields = append(fields, "email")
			var email any
			if v := row.Cells[emailIdx].Value; v != "" {
				email = v
			}
			args = append(args, email)
		}
		if hasAge {
			fields = append(fields, "age")
			var age any
			if v := row.Cells[ageIdx].Value; v != "" {
				n, err := strconv.Atoi(v)
				if err != nil {
					return fmt.Errorf("age %q is not an integer: %w", v, err)
				}
				age = n
			}
			args = append(args, age)
		}
		placeholders := make([]string, len(fields))
		for i := range fields {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
		query := fmt.Sprintf("INSERT INTO persons (%s) VALUES (%s)",
			joinComma(fields), joinComma(placeholders))
		if _, err := st.pool.Exec(ctx, query, args...); err != nil {
			return fmt.Errorf("insert person %q: %w", args[0], err)
		}
	}
	return nil
}

// compileAndExecute compiles the GraphQL query against the current desired
// document, runs the GRAPH_TABLE SQL, and shapes the rows.
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
	st.response = shape.Rows(cq.Projection, flat)
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

// tableHasNoColumn asserts, against information_schema, that a column was
// actually dropped by the delta migration.
func (st *scenarioState) tableHasNoColumn(ctx context.Context, table, column string) error {
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`,
		table, column).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("column %q still exists on table %q after the delta", column, table)
	}
	return nil
}

// baseSDL and widenedSDL stash the two revisions for the fold-correctness
// scenario.
func (st *scenarioState) baseSDL(src *godog.DocString) error {
	m, err := buildModel(src.Content)
	if err != nil {
		return err
	}
	st.baseModel = m
	return nil
}

func (st *scenarioState) widenedSDL(src *godog.DocString) error {
	m, err := buildModel(src.Content)
	if err != nil {
		return err
	}
	st.widenedModel = m
	return nil
}

// applyFoldedPath applies the base migration then a generated delta to reach the
// widened schema, and records the resulting schema fingerprint.
func (st *scenarioState) applyFoldedPath(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "gopgql-m2-folded-")
	if err != nil {
		return err
	}
	st.dirs = append(st.dirs, dir)
	dirs, err := migrate.WriteInitSplit(dir, st.baseModel)
	if err != nil {
		return err
	}
	if err := applyAll(ctx, dirs); err != nil {
		return fmt.Errorf("apply base: %w", err)
	}
	paths, err := migrate.GenerateSplit(dir, st.widenedModel, "delta")
	if err != nil {
		return err
	}
	wrote := false
	for _, p := range paths {
		if p != "" {
			wrote = true
		}
	}
	if !wrote {
		return fmt.Errorf("expected a delta between base and widened schemas")
	}
	if err := applyAll(ctx, dirs); err != nil {
		return fmt.Errorf("apply delta: %w", err)
	}
	fp, err := schemaFingerprint(ctx, st.pool)
	if err != nil {
		return err
	}
	st.foldedFP = fp
	return nil
}

// applyDirectPath resets the database and applies the widened schema as a single
// initial migration, recording its fingerprint for comparison.
func (st *scenarioState) applyDirectPath(ctx context.Context) error {
	if err := st.resetDB(ctx); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "gopgql-m2-direct-")
	if err != nil {
		return err
	}
	st.dirs = append(st.dirs, dir)
	dirs, err := migrate.WriteInitSplit(dir, st.widenedModel)
	if err != nil {
		return err
	}
	if err := applyAll(ctx, dirs); err != nil {
		return fmt.Errorf("apply direct: %w", err)
	}
	fp, err := schemaFingerprint(ctx, st.pool)
	if err != nil {
		return err
	}
	st.directFP = fp
	return nil
}

func (st *scenarioState) assertSchemasIdentical() error {
	if st.foldedFP == "" || st.directFP == "" {
		return fmt.Errorf("both apply steps must run before comparing schemas")
	}
	if st.foldedFP != st.directFP {
		return fmt.Errorf("folded schema differs from direct apply:\n--- folded ---\n%s\n--- direct ---\n%s",
			st.foldedFP, st.directFP)
	}
	return nil
}

// resetDB drops all state by restoring the empty snapshot; the pool must be
// closed first because open connections to the target database break restore.
func (st *scenarioState) resetDB(ctx context.Context) error {
	if st.pool != nil {
		st.pool.Close()
		st.pool = nil
	}
	if err := pgc.Restore(ctx); err != nil {
		return fmt.Errorf("restore for direct apply: %w", err)
	}
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return fmt.Errorf("reopen pool: %w", err)
	}
	st.pool = pool
	return nil
}

// applyAll runs goose up over each directory in order — tables before the graph
// that references them.
// applyAll applies the halves in the only order that works: the property graph
// is taken down first, because PostgreSQL refuses to alter a column the graph
// exposes, then the tables go up, then the graph is rebuilt on top. On a fresh
// database the first step is a no-op.
func applyAll(ctx context.Context, dirs []string) error {
	if err := releaseGraph(ctx, dirs); err != nil {
		return err
	}
	for _, d := range dirs {
		// Each half has its own version table — both start at 0001, so a
		// shared one makes goose skip the second half entirely.
		goose.SetTableName(migrate.VersionTable(filepath.Base(d)))
		if err := applyMigrations(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

// releaseGraph drops the live property graph so the tables half can alter the
// columns it exposes. Not a goose rollback: replaying the graph directory from
// zero would re-run historical CREATE PROPERTY GRAPH statements against tables
// that have since changed.
func releaseGraph(ctx context.Context, dirs []string) error {
	for _, d := range dirs {
		if filepath.Base(d) != migrate.GraphDir {
			continue
		}
		folded, err := migrate.Fold(d)
		if err != nil || folded == nil || folded.GraphName == "" {
			return nil
		}
		db, err := sql.Open("pgx", connString)
		if err != nil {
			return err
		}
		defer db.Close()
		_, err = db.ExecContext(ctx, migrate.DropGraphSQL(folded.GraphName))
		return err
	}
	return nil
}

// applyMigrations runs goose up over dir with its own database handle.
func applyMigrations(ctx context.Context, dir string) error {
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

// schemaFingerprint captures the physical schema of the gopgql tables — every
// column with its type, and every index — as a stable string, ordered by name
// so it ignores physical column position (an ALTER appends; a direct build keeps
// declaration order). Comparing two fingerprints proves a folded apply reaches
// the same schema as a direct apply (SPEC.md §7 → M2 acceptance).
func schemaFingerprint(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var lines []string

	colRows, err := pool.Query(ctx,
		`SELECT table_name, column_name, data_type, is_nullable
		   FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name IN ('persons', 'follows')
		  ORDER BY table_name, column_name`)
	if err != nil {
		return "", err
	}
	defer colRows.Close()
	for colRows.Next() {
		var tbl, col, typ, nullable string
		if err := colRows.Scan(&tbl, &col, &typ, &nullable); err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("col %s.%s %s nullable=%s", tbl, col, typ, nullable))
	}
	if err := colRows.Err(); err != nil {
		return "", err
	}

	idxRows, err := pool.Query(ctx,
		`SELECT tablename, indexname
		   FROM pg_indexes
		  WHERE schemaname = 'public' AND tablename IN ('persons', 'follows')
		  ORDER BY tablename, indexname`)
	if err != nil {
		return "", err
	}
	defer idxRows.Close()
	for idxRows.Next() {
		var tbl, idx string
		if err := idxRows.Scan(&tbl, &idx); err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("idx %s.%s", tbl, idx))
	}
	if err := idxRows.Err(); err != nil {
		return "", err
	}

	sort.Strings(lines)
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out, nil
}

func buildModel(src string) (*schema.Schema, error) {
	doc, err := sdl.Parse(src)
	if err != nil {
		return nil, err
	}
	return generator.Build(doc, "")
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

// joinComma joins tokens with ", ".
func joinComma(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}
