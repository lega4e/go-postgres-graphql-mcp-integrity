// Package m6_test is the M6 integration suite. It boots a real postgres:19beta2
// container and proves the widened SDL surface end to end (SPEC.md §7 → M6):
//
//   - `@column(type: "numeric(10,2)")` reaches the database as that type and the
//     values round-trip exactly, checked against information_schema and by
//     reading the stored text back.
//   - `@unique` is enforced by the database: a duplicate insert fails with
//     SQLSTATE 23505, not with a client-side check.
//   - `@index` produces an index that exists in pg_indexes with the requested
//     access method and that the planner actually chooses, asserted via EXPLAIN
//     over enough rows for a sequential scan to be the wrong plan.
//   - `@column(name:)` renames the physical column, and that is the name the
//     graph exposes and the compiler projects and filters on.
//   - The differ handles both new shapes: a delta adds a UNIQUE constraint and an
//     index, and its Down section removes exactly those again.
package m6_test

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
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/jackc/pgx/v5/pgconn"
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
		Name:                "m6",
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("m6 feature scenarios failed")
	}
}

type scenarioState struct {
	pool     *pgxpool.Pool
	doc      *sdl.Document
	model    *schema.Schema
	dir      string
	response map[string]any
	lastSQL  string
	// lastErr holds an error a step expected the database to raise.
	lastErr error
	dirs    []string
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
		st.dirs, st.dir = nil, ""
		st.doc, st.model, st.response = nil, nil, nil
		st.lastSQL, st.lastErr = "", nil
		return ctx, nil
	})

	sc.Step(`^the SDL:$`, st.theSDL)
	sc.Step(`^I generate and apply the initial migration via goose$`, st.generateAndApply)
	sc.Step(`^I generate and apply a delta migration for the SDL:$`, st.generateAndApplyDelta)
	sc.Step(`^I roll the delta migration back$`, st.rollBack)
	sc.Step(`^the following products exist:$`, st.productsExist)
	sc.Step(`^(\d+) products in category "([^"]*)" and (\d+) in category "([^"]*)"$`, st.bulkProducts)
	sc.Step(`^the column "([^"]*)" of table "([^"]*)" has type "([^"]*)"$`, st.assertColumnType)
	sc.Step(`^the value of "([^"]*)" for sku "([^"]*)" is exactly "([^"]*)"$`, st.assertExactValue)
	sc.Step(`^the constraint "([^"]*)" exists on table "([^"]*)"$`, st.assertConstraintExists)
	sc.Step(`^the constraint "([^"]*)" does not exist on table "([^"]*)"$`, st.assertConstraintAbsent)
	sc.Step(`^inserting sku "([^"]*)" again is rejected by the database with a unique violation$`, st.assertDuplicateSku)
	sc.Step(`^inserting name "([^"]*)" again is rejected by the database with a unique violation$`, st.assertDuplicateName)
	sc.Step(`^the index "([^"]*)" exists in pg_indexes$`, st.assertIndexExists)
	sc.Step(`^the index "([^"]*)" does not exist in pg_indexes$`, st.assertIndexAbsent)
	sc.Step(`^it uses the "([^"]*)" access method$`, st.assertIndexMethod)
	sc.Step(`^a query filtering on "([^"]*)" uses the index "([^"]*)"$`, st.assertIndexUsed)
	sc.Step(`^the table "([^"]*)" has a column "([^"]*)" and no column "([^"]*)"$`, st.assertColumnRenamed)
	sc.Step(`^I compile and execute "([^"]*)" with variable "([^"]*)" bound to "([^"]*)"$`, st.compileWithVar)
	sc.Step(`^the compiled query projects the column "([^"]*)"$`, st.assertProjects)
	sc.Step(`^the JSON response is:$`, st.assertJSON)
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

func (st *scenarioState) generateAndApply(ctx context.Context) error {
	if st.model == nil {
		return fmt.Errorf("no schema model; the SDL step must run first")
	}
	dir, err := os.MkdirTemp("", "gopgql-m6-migrations-")
	if err != nil {
		return err
	}
	st.dirs = append(st.dirs, dir)
	st.dir = dir
	path, err := migrate.WriteInit(dir, st.model)
	if err != nil {
		return err
	}
	if got := filepath.Base(path); got != migrate.InitFilename {
		return fmt.Errorf("migration filename = %s, want %s", got, migrate.InitFilename)
	}
	return st.goose(ctx, "up")
}

// generateAndApplyDelta folds the migrations written so far, diffs them against
// the revised SDL and applies the delta — the path a widened schema takes in
// practice (SPEC.md §7 → M2, extended for constraints and indexes in M6).
func (st *scenarioState) generateAndApplyDelta(ctx context.Context, src *godog.DocString) error {
	if st.dir == "" {
		return fmt.Errorf("no initial migration; apply one first")
	}
	doc, err := sdl.Parse(src.Content)
	if err != nil {
		return err
	}
	desired, err := generator.Build(doc, "")
	if err != nil {
		return err
	}
	path, err := migrate.Generate(st.dir, desired, "delta")
	if err != nil {
		return fmt.Errorf("generate delta: %w", err)
	}
	if path == "" {
		return fmt.Errorf("expected a delta migration, but the schemas were identical")
	}
	st.doc, st.model = doc, desired
	return st.goose(ctx, "up")
}

func (st *scenarioState) rollBack(ctx context.Context) error {
	return st.goose(ctx, "down")
}

// goose runs the named goose command against the scenario's migration dir.
func (st *scenarioState) goose(ctx context.Context, cmd string) error {
	db, err := sql.Open("pgx", connString)
	if err != nil {
		return fmt.Errorf("open sql.DB for goose: %w", err)
	}
	defer db.Close()
	switch cmd {
	case "up":
		return goose.UpContext(ctx, db, st.dir)
	case "down":
		return goose.DownContext(ctx, db, st.dir)
	default:
		return fmt.Errorf("unknown goose command %q", cmd)
	}
}

func (st *scenarioState) productsExist(ctx context.Context, table *godog.Table) error {
	cols := header(table)
	for _, row := range table.Rows[1:] {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO products (sku, name, price, category) VALUES ($1, $2, $3, $4)`,
			row.Cells[cols["sku"]].Value, row.Cells[cols["name"]].Value,
			row.Cells[cols["price"]].Value, row.Cells[cols["category"]].Value,
		); err != nil {
			return fmt.Errorf("insert product: %w", err)
		}
	}
	return nil
}

// bulkProducts seeds enough rows that a sequential scan is the wrong plan, so
// EXPLAIN choosing the index is evidence rather than coincidence. ANALYZE runs
// afterwards so the planner has statistics to decide with.
func (st *scenarioState) bulkProducts(ctx context.Context, nMain int, mainCat string, nOther int, otherCat string) error {
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO products (sku, name, price, category)
		 SELECT 'BULK-' || g, 'Bulk ' || g, 9.99, $2 FROM generate_series(1, $1) g`,
		nMain, mainCat); err != nil {
		return fmt.Errorf("seed %s: %w", mainCat, err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO products (sku, name, price, category)
		 SELECT 'OTHER-' || g, 'Other ' || g, 9.99, $2 FROM generate_series(1, $1) g`,
		nOther, otherCat); err != nil {
		return fmt.Errorf("seed %s: %w", otherCat, err)
	}
	_, err := st.pool.Exec(ctx, `ANALYZE products`)
	return err
}

func (st *scenarioState) assertColumnType(ctx context.Context, column, table, want string) error {
	var got string
	err := st.pool.QueryRow(ctx,
		`SELECT format_type(a.atttypid, a.atttypmod)
		 FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
		 WHERE c.relname = $1 AND a.attname = $2 AND a.attnum > 0`,
		table, column).Scan(&got)
	if err != nil {
		return fmt.Errorf("read column type: %w", err)
	}
	if normalizeType(got) != normalizeType(want) {
		return fmt.Errorf("%s.%s is %q, want %q", table, column, got, want)
	}
	return nil
}

// normalizeType ignores the spacing difference between how the SDL writes a
// modified type and how PostgreSQL formats it back.
func normalizeType(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), " ", "")
}

// assertExactValue reads the stored value back as text, so the comparison is
// against what the database holds — not a float the driver reconstructed.
func (st *scenarioState) assertExactValue(ctx context.Context, column, sku, want string) error {
	var got string
	if err := st.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT %s::text FROM products WHERE sku = $1`, column), sku).Scan(&got); err != nil {
		return fmt.Errorf("read %s: %w", column, err)
	}
	if got != want {
		return fmt.Errorf("%s for %s = %q, want %q (the value did not round-trip exactly)", column, sku, got, want)
	}
	return nil
}

func (st *scenarioState) constraintCount(ctx context.Context, name, table string) (int, error) {
	var n int
	err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid
		 WHERE t.relname = $1 AND c.conname = $2 AND c.contype = 'u'`,
		table, name).Scan(&n)
	return n, err
}

func (st *scenarioState) assertConstraintExists(ctx context.Context, name, table string) error {
	n, err := st.constraintCount(ctx, name, table)
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("unique constraint %q on %s: found %d, want 1", name, table, n)
	}
	return nil
}

func (st *scenarioState) assertConstraintAbsent(ctx context.Context, name, table string) error {
	n, err := st.constraintCount(ctx, name, table)
	if err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("unique constraint %q on %s still exists after the rollback", name, table)
	}
	return nil
}

func (st *scenarioState) assertDuplicateSku(ctx context.Context, sku string) error {
	_, err := st.pool.Exec(ctx,
		`INSERT INTO products (sku, name, price, category) VALUES ($1, 'Duplicate', 1.00, 'parts')`, sku)
	return requireUniqueViolation(err)
}

func (st *scenarioState) assertDuplicateName(ctx context.Context, name string) error {
	_, err := st.pool.Exec(ctx,
		`INSERT INTO products (sku, name, price, category) VALUES ('SKU-DUP', $1, 1.00, 'parts')`, name)
	return requireUniqueViolation(err)
}

// requireUniqueViolation insists the rejection came from PostgreSQL itself,
// identified by SQLSTATE 23505 — not from any check gopgql could have made.
func requireUniqueViolation(err error) error {
	if err == nil {
		return fmt.Errorf("the duplicate insert succeeded; the database did not enforce the constraint")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("insert failed with %T (%v), want a *pgconn.PgError", err, err)
	}
	if pgErr.Code != "23505" {
		return fmt.Errorf("SQLSTATE = %s (%s), want 23505 unique_violation", pgErr.Code, pgErr.Message)
	}
	return nil
}

func (st *scenarioState) indexRow(ctx context.Context, name string) (string, error) {
	var def string
	err := st.pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = $1`, name).Scan(&def)
	return def, err
}

func (st *scenarioState) assertIndexExists(ctx context.Context, name string) error {
	def, err := st.indexRow(ctx, name)
	if err != nil {
		return fmt.Errorf("index %q not found in pg_indexes: %w", name, err)
	}
	if def == "" {
		return fmt.Errorf("index %q has no definition", name)
	}
	return nil
}

func (st *scenarioState) assertIndexAbsent(ctx context.Context, name string) error {
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = $1`, name).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("index %q still exists after the rollback", name)
	}
	return nil
}

func (st *scenarioState) assertIndexMethod(ctx context.Context, method string) error {
	def, err := st.indexRow(ctx, "products_category_idx")
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(def), "using "+strings.ToLower(method)) {
		return fmt.Errorf("index definition %q does not use the %s method", def, method)
	}
	return nil
}

// assertIndexUsed proves the index is not merely present: the planner picks it
// for an equality filter over the seeded rows.
func (st *scenarioState) assertIndexUsed(ctx context.Context, column, index string) error {
	rows, err := st.pool.Query(ctx,
		fmt.Sprintf(`EXPLAIN SELECT id FROM products WHERE %s = $1`, column), "safety")
	if err != nil {
		return fmt.Errorf("EXPLAIN: %w", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !strings.Contains(plan.String(), index) {
		return fmt.Errorf("the planner did not use %q:\n%s", index, plan.String())
	}
	return nil
}

func (st *scenarioState) assertColumnRenamed(ctx context.Context, table, present, absent string) error {
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`,
		table, present).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%s has no column %q", table, present)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`,
		table, absent).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("%s still has a column named %q; @column(name:) did not rename it", table, absent)
	}
	return nil
}

func (st *scenarioState) compileWithVar(ctx context.Context, query, name, value string) error {
	cq, err := compiler.New(st.doc).CompileQuery(query, map[string]any{name: value})
	if err != nil {
		return fmt.Errorf("compile %q: %w", query, err)
	}
	st.lastSQL = cq.SQL

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

// assertProjects checks the compiler read the *column*, which @column(name:)
// renamed away from the GraphQL field name.
func (st *scenarioState) assertProjects(column string) error {
	if !strings.Contains(st.lastSQL, "."+column+" AS ") {
		return fmt.Errorf("compiled SQL does not project the column %q:\n%s", column, st.lastSQL)
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
