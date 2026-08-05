// Package m11_test is the M11 integration suite. It boots a real
// postgres:19beta2 container and proves the @function mutation surface end to
// end (SPEC.md §7 → M11), against hand-written PL/pgSQL in the fixture:
//
//   - A call runs inside a transaction the *caller* opened, and its effect is
//     invisible outside it until that transaction commits. This is the property
//     a consumer needs for exactly-once work: the call and whatever bookkeeping
//     the caller commits alongside it either both happen or neither does, and a
//     package that opened its own connection could not provide it. A pool cannot
//     fake it, which is why it is asserted on visibility rather than on a
//     signature.
//   - Arguments map to parameters by name: the same operation written in another
//     order writes the same row.
//   - An argument the operation document omits reaches the function's own SQL
//     DEFAULT, and passing it as an unset nullable variable instead sends NULL.
//     Both are asserted on the value the function *wrote*, because that is the
//     only place the difference is real.
//   - A declared VOID function is executed and reports success, with its side
//     effect visible.
//   - RAISE EXCEPTION … USING ERRCODE surfaces as an *exec.FunctionError
//     carrying that SQLSTATE, reachable through errors.As.
//   - A call attempted on the read-only pool gopgql opens is refused by the
//     database with 25006 — the second belt, with an error that explains itself.
package m11_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/sdl"
)

var (
	pgc        *postgres.PostgresContainer
	connString string
)

// fixtureSQL is the schema and the functions a `dbos migrate` equivalent would
// have created: gopgql neither generates nor owns any of it.
//
// enqueue_workflow's `priority integer DEFAULT 5` is the whole of the
// DEFAULT-versus-NULL assertion: a call that omits the argument stores 5, and
// one that passes NULL stores NULL, and nothing but the stored value can tell
// the two apart.
const fixtureSQL = `
CREATE SCHEMA app;

CREATE TABLE app.workflows (
    id        text PRIMARY KEY,
    digest    text NOT NULL,
    user_id   text NOT NULL,
    queue     text NOT NULL,
    priority  integer
);

CREATE TABLE app.streams (
    id         text PRIMARY KEY,
    touched_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION app.enqueue_workflow(
    digest text,
    user_id text,
    queue text DEFAULT 'default',
    priority integer DEFAULT 5
) RETURNS text
LANGUAGE plpgsql AS $$
DECLARE
    wf text;
BEGIN
    wf := 'wf-' || (SELECT count(*) + 1 FROM app.workflows);
    INSERT INTO app.workflows (id, digest, user_id, queue, priority)
    VALUES (wf, digest, user_id, queue, priority);
    RETURN wf;
END;
$$;

CREATE FUNCTION app.touch_stream(stream_id text) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO app.streams (id) VALUES (stream_id)
    ON CONFLICT (id) DO UPDATE SET touched_at = now();
END;
$$;

CREATE FUNCTION app.explode(code text) RETURNS text
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'deliberate failure' USING ERRCODE = code;
END;
$$;
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

	suite := godog.TestSuite{
		Name:                "m11",
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("m11 feature scenarios failed")
	}
}

type scenarioState struct {
	pool *pgxpool.Pool
	tx   pgx.Tx
	c    *compiler.Compiler

	result   any
	lastErr  error
	finished bool // the scenario's transaction has been committed or rolled back
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
		if st.tx != nil && !st.finished {
			_ = st.tx.Rollback(ctx)
		}
		if st.pool != nil {
			st.pool.Close()
		}
		*st = scenarioState{}
		return ctx, nil
	})

	sc.Step(`^the schema "([^"]*)" with its functions$`, st.installFixture)
	sc.Step(`^the SDL:$`, st.theSDL)
	sc.Step(`^I call in a transaction:$`, st.callInTx)
	sc.Step(`^I call in a transaction and it fails:$`, st.callInTxExpectingFailure)
	sc.Step(`^I call on a read-only pool and it fails:$`, st.callReadOnlyExpectingFailure)
	sc.Step(`^I commit the transaction$`, st.commit)
	sc.Step(`^I roll the transaction back$`, st.rollback)
	sc.Step(`^the call returned "([^"]*)"$`, st.assertResultString)
	sc.Step(`^the call returned true$`, st.assertResultTrue)
	sc.Step(`^the transaction can see (\d+) rows? in "([^"]*)"$`, st.assertRowsInTx)
	sc.Step(`^a pool outside the transaction sees (\d+) rows? in "([^"]*)"$`, st.assertRowsOutside)
	sc.Step(`^after committing, "([^"]*)" holds (\d+) rows?$`, st.assertRowsAfterCommit)
	sc.Step(`^"([^"]*)" holds (\d+) rows?$`, st.assertRows)
	sc.Step(`^the workflow "([^"]*)" has "([^"]*)" = "([^"]*)"$`, st.assertWorkflowColumn)
	sc.Step(`^the workflow "([^"]*)" has a null "([^"]*)"$`, st.assertWorkflowNull)
	sc.Step(`^the error carries SQLSTATE "([^"]*)"$`, st.assertSQLSTATE)
	sc.Step(`^the error message is "([^"]*)"$`, st.assertErrorMessage)
}

// installFixture applies the schema and the functions gopgql does not own. The
// step names the schema so the feature file states what is somebody else's.
func (st *scenarioState) installFixture(ctx context.Context, name string) error {
	if name != "app" {
		return fmt.Errorf("the fixture creates the schema %q, not %q", "app", name)
	}
	_, err := st.pool.Exec(ctx, fixtureSQL)
	return err
}

func (st *scenarioState) theSDL(src *godog.DocString) error {
	doc, err := sdl.Parse(src.Content)
	if err != nil {
		return err
	}
	st.c = compiler.New(doc)
	return nil
}

// callInTx is the step the milestone exists for: the caller opens the
// transaction, and gopgql runs the call inside it through the handle it is
// given. Nothing here asks gopgql for a connection.
func (st *scenarioState) callInTx(ctx context.Context, op *godog.DocString) error {
	if err := st.begin(ctx); err != nil {
		return err
	}
	cc, err := st.compile(op)
	if err != nil {
		return err
	}
	st.result, st.lastErr = exec.Call(ctx, st.tx, cc)
	return st.lastErr
}

func (st *scenarioState) callInTxExpectingFailure(ctx context.Context, op *godog.DocString) error {
	if err := st.callInTx(ctx, op); err == nil {
		return fmt.Errorf("the call was expected to fail, but it succeeded")
	}
	// The transaction is aborted; roll it back so the After hook has nothing to
	// clean up and the next assertion reads a settled database.
	_ = st.tx.Rollback(ctx)
	st.finished = true
	return nil
}

// callReadOnlyExpectingFailure runs the same call on the only pool gopgql ever
// opens. Nothing here is a gopgql-side check: the refusal comes from the
// database, which is what makes it a belt rather than a policy.
func (st *scenarioState) callReadOnlyExpectingFailure(ctx context.Context, op *godog.DocString) error {
	pool, err := exec.OpenReadOnly(ctx, connString)
	if err != nil {
		return err
	}
	defer pool.Close()

	cc, err := st.compile(op)
	if err != nil {
		return err
	}
	if _, err := exec.Call(ctx, pool, cc); err != nil {
		st.lastErr = err
		return nil
	}
	return fmt.Errorf("a write through the read-only pool was expected to be refused")
}

func (st *scenarioState) compile(op *godog.DocString) (*compiler.CompiledCall, error) {
	if st.c == nil {
		return nil, fmt.Errorf("no compiler; the SDL step must run first")
	}
	// The operation travels as a doc string rather than inline, so it appears in
	// the feature file exactly as it would be written anywhere else — a Gherkin
	// step argument would need every quote in it escaped.
	return st.c.CompileMutation(op.Content, nil)
}

func (st *scenarioState) begin(ctx context.Context) error {
	if st.tx != nil {
		return nil
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return err
	}
	st.tx = tx
	return nil
}

func (st *scenarioState) commit(ctx context.Context) error {
	if st.tx == nil {
		return fmt.Errorf("no transaction to commit")
	}
	if err := st.tx.Commit(ctx); err != nil {
		return err
	}
	st.finished = true
	return nil
}

func (st *scenarioState) rollback(ctx context.Context) error {
	if st.tx == nil {
		return fmt.Errorf("no transaction to roll back")
	}
	if err := st.tx.Rollback(ctx); err != nil {
		return err
	}
	st.finished = true
	return nil
}

func (st *scenarioState) assertResultString(want string) error {
	got, ok := st.result.(string)
	if !ok {
		return fmt.Errorf("the call returned %T (%v), want a string", st.result, st.result)
	}
	if got != want {
		return fmt.Errorf("the call returned %q, want %q", got, want)
	}
	return nil
}

func (st *scenarioState) assertResultTrue() error {
	got, ok := st.result.(bool)
	if !ok {
		return fmt.Errorf("the call returned %T (%v), want a bool", st.result, st.result)
	}
	if !got {
		return fmt.Errorf("the call returned false; a successful void call reports true")
	}
	return nil
}

// countIn counts rows through the given handle, which is what makes the
// visibility assertions mean anything: the same query answers differently inside
// and outside an uncommitted transaction.
func countIn(ctx context.Context, q exec.Querier, table string) (int, error) {
	rows, err := exec.Rows(ctx, q, "SELECT count(*) AS n FROM "+table)
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 {
		return 0, fmt.Errorf("count returned %d rows", len(rows))
	}
	switch n := rows[0]["n"].(type) {
	case int64:
		return int(n), nil
	case int32:
		return int(n), nil
	default:
		return 0, fmt.Errorf("count returned %T", rows[0]["n"])
	}
}

func (st *scenarioState) assertRowsInTx(ctx context.Context, want int, table string) error {
	if st.tx == nil {
		return fmt.Errorf("no transaction")
	}
	return assertCount(ctx, st.tx, table, want, "inside the transaction")
}

func (st *scenarioState) assertRowsOutside(ctx context.Context, want int, table string) error {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return err
	}
	defer pool.Close()
	return assertCount(ctx, pool, table, want,
		"outside the transaction — an uncommitted call must not be visible")
}

func (st *scenarioState) assertRowsAfterCommit(ctx context.Context, table string, want int) error {
	if err := st.commit(ctx); err != nil {
		return err
	}
	return st.assertRows(ctx, table, want)
}

func (st *scenarioState) assertRows(ctx context.Context, table string, want int) error {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return err
	}
	defer pool.Close()
	return assertCount(ctx, pool, table, want, "after the transaction settled")
}

func assertCount(ctx context.Context, q exec.Querier, table string, want int, where string) error {
	got, err := countIn(ctx, q, table)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s has %d rows %s, want %d", table, got, where, want)
	}
	return nil
}

// readWorkflow reads one column of one workflow through a fresh pool, so what
// is asserted is what the function committed rather than what gopgql sent.
func (st *scenarioState) readWorkflow(ctx context.Context, id, column string) (any, error) {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	// The column name comes from the feature file, which is gopgql's own test
	// fixture and not user input; it is still checked against the fixture's
	// columns rather than interpolated blind.
	switch column {
	case "digest", "user_id", "queue", "priority":
	default:
		return nil, fmt.Errorf("app.workflows has no column %q", column)
	}
	rows, err := exec.Rows(ctx, pool,
		"SELECT "+column+" AS v FROM app.workflows WHERE id = $1", id)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("workflow %q: %d rows", id, len(rows))
	}
	return rows[0]["v"], nil
}

func (st *scenarioState) assertWorkflowColumn(ctx context.Context, id, column, want string) error {
	got, err := st.readWorkflow(ctx, id, column)
	if err != nil {
		return err
	}
	if got == nil {
		return fmt.Errorf("workflow %q: %s is null, want %q", id, column, want)
	}
	if text := render(got); text != want {
		return fmt.Errorf("workflow %q: %s is %q, want %q", id, column, text, want)
	}
	return nil
}

func (st *scenarioState) assertWorkflowNull(ctx context.Context, id, column string) error {
	got, err := st.readWorkflow(ctx, id, column)
	if err != nil {
		return err
	}
	if got != nil {
		return fmt.Errorf("workflow %q: %s is %v, want null — NULL and DEFAULT are different things",
			id, column, got)
	}
	return nil
}

func render(v any) string {
	switch n := v.(type) {
	case string:
		return n
	case int32:
		return strconv.Itoa(int(n))
	case int64:
		return strconv.FormatInt(n, 10)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func (st *scenarioState) assertSQLSTATE(want string) error {
	if st.lastErr == nil {
		return fmt.Errorf("no error was recorded")
	}
	var fnErr *exec.FunctionError
	if !errors.As(st.lastErr, &fnErr) {
		return fmt.Errorf("the error is %v, and not an *exec.FunctionError; "+
			"a consumer branches on the code, not on the prose", st.lastErr)
	}
	if fnErr.SQLSTATE != want {
		return fmt.Errorf("SQLSTATE is %q, want %q", fnErr.SQLSTATE, want)
	}
	return nil
}

func (st *scenarioState) assertErrorMessage(want string) error {
	var fnErr *exec.FunctionError
	if !errors.As(st.lastErr, &fnErr) {
		return fmt.Errorf("the error is not an *exec.FunctionError: %v", st.lastErr)
	}
	if fnErr.Message != want {
		return fmt.Errorf("message is %q, want %q", fnErr.Message, want)
	}
	return nil
}
