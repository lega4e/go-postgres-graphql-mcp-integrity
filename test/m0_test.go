package m0_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/jackc/pgx/v5/pgxpool"
	// Registers the "pgx" database/sql driver used by postgres.WithSQLDriver
	// for Snapshot/Restore.
	_ "github.com/jackc/pgx/v5/stdlib"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// A single container is shared across the whole suite; each scenario resets to
// the schema-only snapshot via Restore (SPEC.md §M0).
var (
	pgc        *postgres.PostgresContainer
	connString string
)

// TestFeatures is the godog entry point under `go test`.
func TestFeatures(t *testing.T) {
	if os.Getenv("GOPGQL_SKIP_INTEGRATION") == "1" {
		t.Skip("integration tests skipped (GOPGQL_SKIP_INTEGRATION=1)")
	}

	ctx := context.Background()

	var err error
	pgc, err = postgres.Run(ctx, "postgres:19beta2",
		postgres.WithInitScripts(filepath.Join("fixtures", "m0_schema.sql")),
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

	// The init script has run, so the database now holds the schema and the
	// property graph but no rows. Capture that as the reset baseline.
	if err := pgc.Snapshot(ctx); err != nil {
		t.Fatalf("snapshot baseline: %v", err)
	}

	connString, err = pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	suite := godog.TestSuite{
		Name:                "m0",
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("m0 feature scenarios failed")
	}
}

// scenarioState carries the per-scenario connection pool and the last query
// result between steps.
type scenarioState struct {
	pool  *pgxpool.Pool
	names []string
	pairs [][2]string
}

// InitializeScenario is invoked once per scenario. It resets the database to
// the snapshot, opens a fresh pool, and registers steps.
func InitializeScenario(sc *godog.ScenarioContext) {
	st := &scenarioState{}

	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		// Reset to the schema-only baseline. Any pool from a prior scenario is
		// already closed by its After hook, so no connection blocks the drop.
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
		return ctx, nil
	})

	sc.Step(`^the property graph "([^"]*)" is available$`, st.graphAvailable)
	sc.Step(`^the following persons exist:$`, st.personsExist)
	sc.Step(`^"([^"]*)" follows "([^"]*)"$`, st.follows)
	sc.Step(`^I run the vertex query$`, st.runVertexQuery)
	sc.Step(`^I run the follows query$`, st.runFollowsQuery)
	sc.Step(`^the returned names are:$`, st.assertNames)
	sc.Step(`^no rows are returned$`, st.assertNoRows)
	sc.Step(`^the returned follow pairs are:$`, st.assertPairs)
}

// graphAvailable confirms the property graph exists and SQL/PGQ is usable by
// executing a trivial GRAPH_TABLE against it.
func (st *scenarioState) graphAvailable(ctx context.Context, name string) error {
	// The graph name is a schema identifier, not user data, and cannot be a
	// bind parameter inside GRAPH_TABLE; it is a fixed literal from the fixture.
	if name != "app_graph" {
		return fmt.Errorf("unexpected graph name %q", name)
	}
	q := `SELECT count(*) FROM GRAPH_TABLE (app_graph
  MATCH (v IS person)
  COLUMNS (v.id AS id))`
	var n int
	if err := st.pool.QueryRow(ctx, q).Scan(&n); err != nil {
		return fmt.Errorf("graph %q not usable: %w", name, err)
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

func (st *scenarioState) runVertexQuery(ctx context.Context) error {
	q := `SELECT name FROM GRAPH_TABLE (app_graph
  MATCH (v IS person)
  COLUMNS (v.name AS name)) ORDER BY name`
	rows, err := st.pool.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("vertex query: %w", err)
	}
	defer rows.Close()
	st.names = nil
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		st.names = append(st.names, name)
	}
	return rows.Err()
}

func (st *scenarioState) runFollowsQuery(ctx context.Context) error {
	q := `SELECT follower, followed FROM GRAPH_TABLE (app_graph
  MATCH (s IS person) -[e IS follows]-> (t IS person)
  COLUMNS (s.name AS follower, t.name AS followed)) ORDER BY follower, followed`
	rows, err := st.pool.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("follows query: %w", err)
	}
	defer rows.Close()
	st.pairs = nil
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return err
		}
		st.pairs = append(st.pairs, [2]string{a, b})
	}
	return rows.Err()
}

func (st *scenarioState) assertNames(table *godog.Table) error {
	want := make([]string, 0, len(table.Rows)-1)
	for _, row := range table.Rows[1:] {
		want = append(want, row.Cells[0].Value)
	}
	if len(st.names) != len(want) {
		return fmt.Errorf("got %d names %v, want %d %v", len(st.names), st.names, len(want), want)
	}
	for i := range want {
		if st.names[i] != want[i] {
			return fmt.Errorf("name[%d] = %q, want %q (got %v)", i, st.names[i], want[i], st.names)
		}
	}
	return nil
}

func (st *scenarioState) assertNoRows() error {
	if len(st.names) != 0 {
		return fmt.Errorf("expected no rows, got %v", st.names)
	}
	return nil
}

func (st *scenarioState) assertPairs(table *godog.Table) error {
	cols := header(table)
	fi, ok1 := cols["follower"]
	ti, ok2 := cols["followed"]
	if !ok1 || !ok2 {
		return fmt.Errorf("follow pairs table needs 'follower' and 'followed' columns")
	}
	want := make([][2]string, 0, len(table.Rows)-1)
	for _, row := range table.Rows[1:] {
		want = append(want, [2]string{row.Cells[fi].Value, row.Cells[ti].Value})
	}
	if len(st.pairs) != len(want) {
		return fmt.Errorf("got %d pairs %v, want %d %v", len(st.pairs), st.pairs, len(want), want)
	}
	for i := range want {
		if st.pairs[i] != want[i] {
			return fmt.Errorf("pair[%d] = %v, want %v (got %v)", i, st.pairs[i], want[i], st.pairs)
		}
	}
	return nil
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
