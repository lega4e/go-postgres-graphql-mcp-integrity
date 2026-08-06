// Package m10_test is the M10 integration suite. It boots a real
// postgres:19beta2 container and proves that execution runs against a handle the
// caller owns (SPEC.md §7 → M10).
//
// The assertions are about *visibility*, not about signatures. A compiled query
// executed through a caller's `pgx.Tx` returns rows that transaction inserted
// and has not committed; a separate pool, querying at the same moment, does not
// see them. That is the property a package which opened its own connection could
// not provide, and it is what makes a caller's commit and gopgql's work atomic
// together — the basis of an exactly-once append.
//
// The read path through `exec.OpenReadOnly` is asserted to be unchanged, and a
// write through that pool is asserted to be refused by the database itself with
// SQLSTATE 25006 — the second belt (SPEC.md §3, design D4), which is what lets
// gopgql keep opening exactly one kind of pool while mutations exist.
package m10_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	"github.com/lega4e/gopgql/sdl"
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
		Name:                "m10",
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("m10 feature scenarios failed")
	}
}

type scenarioState struct {
	pool *pgxpool.Pool
	tx   pgx.Tx
	c    *compiler.Compiler
	dir  string

	names   []string
	lastErr error
	done    bool
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
		st.dir, err = os.MkdirTemp("", "gopgql-m10-migrations-")
		return ctx, err
	})

	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		if st.tx != nil && !st.done {
			_ = st.tx.Rollback(ctx)
		}
		if st.pool != nil {
			st.pool.Close()
		}
		if st.dir != "" {
			_ = os.RemoveAll(st.dir)
		}
		*st = scenarioState{}
		return ctx, nil
	})

	sc.Step(`^the SDL:$`, st.theSDL)
	sc.Step(`^I generate and apply the migrations$`, st.generateAndApply)
	sc.Step(`^the following people already exist:$`, st.peopleExist)
	sc.Step(`^I begin a transaction and insert "([^"]*)" without committing$`, st.beginAndInsert)
	sc.Step(`^I execute the query through that transaction:$`, st.queryThroughTx)
	sc.Step(`^I execute the query through a separate pool:$`, st.queryThroughSeparatePool)
	sc.Step(`^I execute the query through a read-only pool:$`, st.queryThroughReadOnly)
	sc.Step(`^I attempt a write on a read-only pool$`, st.writeOnReadOnly)
	sc.Step(`^the response names "([^"]*)"$`, st.assertNames)
	sc.Step(`^it fails with SQLSTATE "([^"]*)"$`, st.assertSQLSTATE)
}

func (st *scenarioState) theSDL(src *godog.DocString) error {
	doc, err := sdl.Parse(src.Content)
	if err != nil {
		return err
	}
	model, err := generator.Build(doc, "")
	if err != nil {
		return err
	}
	st.c = compiler.New(doc)
	if _, err := migrate.Generate(st.dir, model, "init", migrate.Halves{}); err != nil {
		return err
	}
	return nil
}

func (st *scenarioState) generateAndApply(ctx context.Context) error {
	db, err := sql.Open("pgx", connString)
	if err != nil {
		return err
	}
	defer db.Close()
	return goose.UpContext(ctx, db, st.dir)
}

func (st *scenarioState) peopleExist(ctx context.Context, table *godog.Table) error {
	for _, row := range table.Rows[1:] {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO persons (name) VALUES ($1)`, row.Cells[0].Value); err != nil {
			return err
		}
	}
	return nil
}

// beginAndInsert is the caller opening its own transaction. Nothing below asks
// gopgql for a connection: the transaction is the caller's and stays the
// caller's.
func (st *scenarioState) beginAndInsert(ctx context.Context, name string) error {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return err
	}
	st.tx = tx
	_, err = tx.Exec(ctx, `INSERT INTO persons (name) VALUES ($1)`, name)
	return err
}

func (st *scenarioState) queryThroughTx(ctx context.Context, op *godog.DocString) error {
	if st.tx == nil {
		return fmt.Errorf("no transaction; the caller must open one first")
	}
	return st.query(ctx, exec.PgxQuerier(st.tx), op)
}

func (st *scenarioState) queryThroughSeparatePool(ctx context.Context, op *godog.DocString) error {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return err
	}
	defer pool.Close()
	return st.query(ctx, exec.PgxQuerier(pool), op)
}

func (st *scenarioState) queryThroughReadOnly(ctx context.Context, op *godog.DocString) error {
	pool, err := exec.OpenReadOnly(ctx, connString)
	if err != nil {
		return err
	}
	defer pool.Close()
	return st.query(ctx, exec.PgxQuerier(pool), op)
}

// query compiles once and executes through whichever handle the scenario chose,
// so the only thing that differs between the scenarios is the connection.
func (st *scenarioState) query(ctx context.Context, q exec.Querier, op *godog.DocString) error {
	cq, err := st.c.CompileQuery(op.Content, nil)
	if err != nil {
		return err
	}
	res, err := exec.Query(ctx, q, cq)
	if err != nil {
		return err
	}
	st.names = nil
	list, _ := res["persons"].([]any)
	for _, item := range list {
		row, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("unexpected row %T", item)
		}
		name, _ := row["name"].(string)
		st.names = append(st.names, name)
	}
	return nil
}

// writeOnReadOnly proves the belt is the database's and not gopgql's: the
// statement is an ordinary INSERT, refused because of how the pool was opened.
func (st *scenarioState) writeOnReadOnly(ctx context.Context) error {
	pool, err := exec.OpenReadOnly(ctx, connString)
	if err != nil {
		return err
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `INSERT INTO persons (name) VALUES ('Nope')`); err != nil {
		st.lastErr = err
		return nil
	}
	return fmt.Errorf("a write through the read-only pool was expected to be refused")
}

func (st *scenarioState) assertNames(want string) error {
	expected := strings.Split(want, ", ")
	sort.Strings(expected)
	got := append([]string(nil), st.names...)
	sort.Strings(got)
	if strings.Join(got, ", ") != strings.Join(expected, ", ") {
		return fmt.Errorf("the response names %v, want %v", got, expected)
	}
	return nil
}

func (st *scenarioState) assertSQLSTATE(want string) error {
	if st.lastErr == nil {
		return fmt.Errorf("no error was recorded")
	}
	var pgErr *pgconn.PgError
	if !errors.As(st.lastErr, &pgErr) {
		return fmt.Errorf("the error is %v, and carries no SQLSTATE", st.lastErr)
	}
	if pgErr.Code != want {
		return fmt.Errorf("SQLSTATE is %q, want %q", pgErr.Code, want)
	}
	return nil
}
