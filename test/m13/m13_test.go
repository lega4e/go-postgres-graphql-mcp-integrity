// Package m13_test is the M13 integration suite: a vertex identified by a
// declared @key(fields:) rather than by a surrogate `id`, over tables gopgql
// does not own (SPEC.md §7 → M13).
//
// The fixture is deliberately not synthetic. It is the shape the requirement
// came from — `dbos.operation_outputs` keyed (workflow_uuid, function_id) and
// `dbos.streams` keyed (workflow_uuid, key, offset) — created by a hand-written
// init script standing in for the tool that owns them. Neither has an `id`
// column, and one has a column named after a reserved word, so items 3 and 4 of
// the issue and M13 are proven together against the real thing.
//
// What the container proves that a golden file cannot:
//
//   - A composite key really is projected, grouped and ordered by, so a query
//     over a table with no `id` returns rows at all.
//   - A parent deduplicates on its **whole** key across a one-to-many fan-out.
//     A wrong identity does not error here; it merges rows that are not the same
//     row, which is why this is asserted on returned data.
//   - A NULL in one key column does not silently drop the row — the failure the
//     NULL-safe isomorphism guard exists to prevent.
//   - Keys containing a space and a bracket do not collide in the shaper's dedup
//     encoding. A dedup collision is silent, so nothing but an assertion catches
//     it.
//   - One table serves as both a vertex element and an edge element, which
//     SPEC.md §5.3 invariant 4 refused before M13 narrowed it.
package m13_test

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

// fixtureSQL is what the other tool created. Every table here is keyed by its
// own columns and none has an `id`; `dbos.streams."offset"` is a reserved word,
// and `operation_outputs` is both a step and the join connecting a workflow to
// one — the shape §5.3 invariant 4 used to refuse.
const fixtureSQL = `
CREATE SCHEMA dbos;

CREATE TABLE dbos.workflows (
    workflow_uuid uuid PRIMARY KEY,
    status        text NOT NULL
);

CREATE TABLE dbos.operation_outputs (
    workflow_uuid       uuid NOT NULL REFERENCES dbos.workflows (workflow_uuid),
    function_id         integer NOT NULL,
    output              text,
    child_workflow_uuid uuid REFERENCES dbos.workflows (workflow_uuid),
    PRIMARY KEY (workflow_uuid, function_id)
);

CREATE TABLE dbos.streams (
    workflow_uuid uuid NOT NULL REFERENCES dbos.workflows (workflow_uuid),
    key           text,
    "offset"      integer NOT NULL,
    value         text
);
CREATE UNIQUE INDEX streams_key ON dbos.streams (workflow_uuid, key, "offset");

INSERT INTO dbos.workflows (workflow_uuid, status) VALUES
    ('00000000-0000-0000-0000-00000000000a', 'done'),
    ('00000000-0000-0000-0000-00000000000b', 'running');

-- Step (a, 1) starts workflow b: the row is a step of one workflow and the edge
-- to another at the same time, which is the two-labels-on-one-table shape.
INSERT INTO dbos.operation_outputs (workflow_uuid, function_id, output, child_workflow_uuid) VALUES
    ('00000000-0000-0000-0000-00000000000a', 0, 's0', NULL),
    ('00000000-0000-0000-0000-00000000000a', 1, 's1', '00000000-0000-0000-0000-00000000000b'),
    ('00000000-0000-0000-0000-00000000000b', 0, 't0', NULL);

INSERT INTO dbos.streams (workflow_uuid, key, "offset", value) VALUES
    ('00000000-0000-0000-0000-00000000000a', 'a', 1, 'a1'),
    ('00000000-0000-0000-0000-00000000000a', 'a', 2, 'a2'),
    ('00000000-0000-0000-0000-00000000000a', 'b', 1, 'b1');
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
		Name:                "m13",
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("m13 feature scenarios failed")
	}
}

type scenarioState struct {
	pool  *pgxpool.Pool
	doc   *sdl.Document
	model *schema.Schema
	dir   string

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
		st.dir, err = os.MkdirTemp("", "gopgql-m13-migrations-")
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
	sc.Step(`^the schemas "([^"]*)" and "([^"]*)" each already own a "([^"]*)" table$`, st.installTwinSchemas)
	sc.Step(`^the property graph declares the edge labels "([^"]*)" on "([^"]*)"$`, st.assertEdgeLabels)
	sc.Step(`^the graph has (\d+) edge elements? on "([^"]*)"$`, st.assertEdgeElementCount)
	sc.Step(`^generating again writes no migration$`, st.assertGeneratingAgainIsANoOp)
	sc.Step(`^the SDL:$`, st.theSDL)
	sc.Step(`^I generate and apply the migrations$`, st.generateAndApply)
	sc.Step(`^I compile and execute:$`, st.compileAndExecute)
	sc.Step(`^the JSON response is:$`, st.assertJSON)
	sc.Step(`^the property graph exposes the elements "([^"]*)"$`, st.assertElements)
	sc.Step(`^the graph has an edge element on "([^"]*)"$`, st.assertEdgeElement)
	sc.Step(`^a stream row whose "([^"]*)" is NULL$`, st.insertNullKeyRow)
	sc.Step(`^the response holds (\d+) streams$`, st.assertStreamCount)
	sc.Step(`^the stream rows:$`, st.insertStreamRows)
	sc.Step(`^the streams "([^"]*)"/(\d+) and "([^"]*)"/(\d+) are distinct rows$`, st.assertDistinct)
	sc.Step(`^no migration contains "([^"]*)"$`, st.assertNoMention)
	sc.Step(`^the property graph names "([^"]*)"$`, st.assertGraphNames)
}

func (st *scenarioState) installFixture(ctx context.Context, name string) error {
	if name != "dbos" {
		return fmt.Errorf("the fixture creates the schema %q, not %q", "dbos", name)
	}
	_, err := st.pool.Exec(ctx, fixtureSQL)
	return err
}

// twinSchemaSQL is the second fixture: two schemas that independently own a
// table of the same name. Element aliases are unqualified, so `alpha.links` and
// `beta.links` are two tables that both want to be called `links` — the case
// where keying an edge on its bare table name loses one of them.
const twinSchemaSQL = `
CREATE SCHEMA alpha;
CREATE SCHEMA beta;

CREATE TABLE alpha.alpha_nodes (alpha_id uuid PRIMARY KEY);
CREATE TABLE alpha.links (
    alpha_src uuid NOT NULL REFERENCES alpha.alpha_nodes (alpha_id),
    alpha_dst uuid NOT NULL REFERENCES alpha.alpha_nodes (alpha_id),
    PRIMARY KEY (alpha_src, alpha_dst)
);

CREATE TABLE beta.beta_nodes (beta_id uuid PRIMARY KEY);
CREATE TABLE beta.links (
    beta_src uuid NOT NULL REFERENCES beta.beta_nodes (beta_id),
    beta_dst uuid NOT NULL REFERENCES beta.beta_nodes (beta_id),
    PRIMARY KEY (beta_src, beta_dst)
);
`

func (st *scenarioState) installTwinSchemas(ctx context.Context, first, second, table string) error {
	if first != "alpha" || second != "beta" || table != "links" {
		return fmt.Errorf("the fixture creates %q.%q and %q.%q, not %q.%q and %q.%q",
			"alpha", "links", "beta", "links", first, table, second, table)
	}
	_, err := st.pool.Exec(ctx, twinSchemaSQL)
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

func (st *scenarioState) generateAndApply(ctx context.Context) error {
	paths, err := migrate.Generate(st.dir, st.model, "schema", migrate.Halves{})
	if err != nil {
		return err
	}
	st.generated = nil
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		st.generated = append(st.generated, string(data))
	}

	db, err := sql.Open("pgx", connString)
	if err != nil {
		return err
	}
	defer db.Close()
	return goose.UpContext(ctx, db, st.dir)
}

func (st *scenarioState) compileAndExecute(ctx context.Context, op *godog.DocString) error {
	cq, err := compiler.New(st.doc).CompileQuery(op.Content, nil)
	if err != nil {
		return err
	}
	res, err := exec.Query(ctx, exec.PgxQuerier(st.pool), cq)
	if err != nil {
		return fmt.Errorf("%w\n\nSQL:\n%s", err, cq.SQL)
	}
	st.response = res
	return nil
}

func (st *scenarioState) assertJSON(want *godog.DocString) error {
	var expected map[string]any
	if err := json.Unmarshal([]byte(want.Content), &expected); err != nil {
		return fmt.Errorf("parse expected JSON: %w", err)
	}
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

// insertNullKeyRow adds a row whose `key` is NULL. A @key column is UNIQUE but
// never NOT NULL, so this is legal — and before the NULL-safe guard it is the
// row that vanished.
func (st *scenarioState) insertNullKeyRow(ctx context.Context, column string) error {
	if column != "key" {
		return fmt.Errorf("the fixture nulls %q, not %q", "key", column)
	}
	_, err := st.pool.Exec(ctx,
		`INSERT INTO dbos.streams (workflow_uuid, key, "offset", value)
		 VALUES ('00000000-0000-0000-0000-00000000000a', NULL, 3, 'nullkey')`)
	return err
}

func (st *scenarioState) assertStreamCount(want int) error {
	list, _ := st.response["streams"].([]any)
	if len(list) != want {
		raw, _ := json.Marshal(st.response)
		return fmt.Errorf("the response holds %d streams, want %d: %s", len(list), want, raw)
	}
	return nil
}

// insertStreamRows seeds keys containing a space and a bracket. Encoded with a
// printable separator, ("a b", 1) and ("a", …) can collide; NUL cannot occur in
// a PostgreSQL text value, which is why the shaper joins with it.
func (st *scenarioState) insertStreamRows(ctx context.Context, table *godog.Table) error {
	if _, err := st.pool.Exec(ctx, `DELETE FROM dbos.streams`); err != nil {
		return err
	}
	for _, row := range table.Rows[1:] {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO dbos.streams (workflow_uuid, key, "offset", value)
			 VALUES ('00000000-0000-0000-0000-00000000000a', $1, $2, $3)`,
			row.Cells[0].Value, row.Cells[1].Value, row.Cells[2].Value); err != nil {
			return err
		}
	}
	// A bracket in a key value, for the same reason.
	_, err := st.pool.Exec(ctx,
		`INSERT INTO dbos.streams (workflow_uuid, key, "offset", value)
		 VALUES ('00000000-0000-0000-0000-00000000000a', '[x]', 1, 'bracket')`)
	return err
}

func (st *scenarioState) assertDistinct(k1 string, s1 int, k2 string, s2 int) error {
	list, _ := st.response["streams"].([]any)
	seen := map[string]string{}
	for _, item := range list {
		row, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("unexpected row %T", item)
		}
		key := fmt.Sprintf("%v/%v", row["key"], row["seq"])
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("two rows shaped to the same key %q (values %q and %v)", key, prev, row["value"])
		}
		seen[key] = fmt.Sprintf("%v", row["value"])
	}
	for _, want := range []string{fmt.Sprintf("%s/%d", k1, s1), fmt.Sprintf("%s/%d", k2, s2)} {
		if _, ok := seen[want]; !ok {
			raw, _ := json.Marshal(st.response)
			return fmt.Errorf("no row %q in the response: %s", want, raw)
		}
	}
	return nil
}

func (st *scenarioState) assertElements(ctx context.Context, want string) error {
	rows, err := exec.Rows(ctx, exec.PgxQuerier(st.pool), `
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
	got = distinct(got)
	sort.Strings(got)
	expected := strings.Split(want, ", ")
	sort.Strings(expected)
	if !reflect.DeepEqual(expected, got) {
		return fmt.Errorf("the graph exposes %v, want %v", got, expected)
	}
	return nil
}

// assertEdgeElement is the invariant-4 narrowing, proven in the catalog: the
// same table carries a vertex element and an edge element at once.
func (st *scenarioState) assertEdgeElement(ctx context.Context, qualified string) error {
	schemaName, table, found := strings.Cut(qualified, ".")
	if !found {
		return fmt.Errorf("%q is not a schema-qualified table name", qualified)
	}
	rows, err := exec.Rows(ctx, exec.PgxQuerier(st.pool), `
		SELECT e.pgekind::text AS kind
		FROM pg_catalog.pg_propgraph_element e
		JOIN pg_catalog.pg_class c ON c.oid = e.pgerelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2
		ORDER BY e.pgekind`, schemaName, table)
	if err != nil {
		return err
	}
	kinds := map[string]bool{}
	for _, row := range rows {
		k, _ := row["kind"].(string)
		kinds[k] = true
	}
	if !kinds["v"] || !kinds["e"] {
		return fmt.Errorf("%s carries kinds %v; it must be both a vertex ('v') and an edge ('e')",
			qualified, kinds)
	}
	return nil
}

// assertEdgeLabels reads the *emitted DDL* rather than the catalog, because the
// failure this guards against was silent: the second label vanished from
// CREATE PROPERTY GRAPH and generation still exited 0. A check that only ran
// after a successful apply would have passed on the broken generator too — the
// graph it wrote was valid, it was just missing an edge.
func (st *scenarioState) assertEdgeLabels(want, qualified string) error {
	graph := ""
	for _, file := range st.generated {
		if strings.Contains(file, "CREATE PROPERTY GRAPH") {
			graph = file
			break
		}
	}
	if graph == "" {
		return fmt.Errorf("no generated migration carries a CREATE PROPERTY GRAPH:\n%s",
			strings.Join(st.generated, "\n"))
	}
	for _, label := range strings.Split(want, ", ") {
		// The element is "<table> AS "<label>" SOURCE KEY …": both halves are
		// asserted together so an edge on the *wrong* table cannot satisfy this.
		element := fmt.Sprintf("%s AS %q SOURCE KEY", qualified, label)
		if !strings.Contains(graph, element) {
			return fmt.Errorf("the property graph declares no edge %q on %s; it reads:\n%s",
				label, qualified, graph)
		}
		if !strings.Contains(graph, fmt.Sprintf("LABEL %q PROPERTIES", label)) {
			return fmt.Errorf("the edge element %q on %s carries no LABEL clause:\n%s",
				label, qualified, graph)
		}
	}
	return nil
}

// assertEdgeElementCount is the catalog counterpart: PostgreSQL accepted the
// graph and holds that many *edge* elements over the table. Together with
// assertEdgeLabels it says the DDL named them and the server kept them.
func (st *scenarioState) assertEdgeElementCount(ctx context.Context, want int, qualified string) error {
	schemaName, table, found := strings.Cut(qualified, ".")
	if !found {
		return fmt.Errorf("%q is not a schema-qualified table name", qualified)
	}
	rows, err := exec.Rows(ctx, exec.PgxQuerier(st.pool), `
		SELECT e.pgealias::text AS alias
		FROM pg_catalog.pg_propgraph_element e
		JOIN pg_catalog.pg_class c ON c.oid = e.pgerelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND e.pgekind = 'e'
		ORDER BY e.pgealias`, schemaName, table)
	if err != nil {
		return err
	}
	if len(rows) != want {
		var aliases []string
		for _, row := range rows {
			a, _ := row["alias"].(string)
			aliases = append(aliases, a)
		}
		return fmt.Errorf("%s carries %d edge elements %v, want %d", qualified, len(rows), aliases, want)
	}
	return nil
}

// assertGeneratingAgainIsANoOp is the idempotency gate. `go generate ./... &&
// git diff --exit-code` is a CI step for every consumer of a generated
// migration directory, and it cannot pass unless a second generation of an
// unchanged schema writes nothing at all — which means the folder has to read
// back every clause the emitter wrote, including the edge alias.
func (st *scenarioState) assertGeneratingAgainIsANoOp() error {
	before, err := os.ReadDir(st.dir)
	if err != nil {
		return err
	}
	paths, err := migrate.Generate(st.dir, st.model, "schema", migrate.Halves{})
	if err != nil {
		return fmt.Errorf("a second generation of an unchanged schema failed: %w", err)
	}
	if len(paths) > 0 {
		return fmt.Errorf("a second generation of an unchanged schema wrote %v", paths)
	}
	after, err := os.ReadDir(st.dir)
	if err != nil {
		return err
	}
	if len(before) != len(after) {
		return fmt.Errorf("the migration directory went from %d files to %d", len(before), len(after))
	}
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

func (st *scenarioState) assertGraphNames(qualified string) error {
	for _, file := range st.generated {
		if strings.Contains(file, "CREATE PROPERTY GRAPH") && strings.Contains(file, qualified) {
			return nil
		}
	}
	return fmt.Errorf("no CREATE PROPERTY GRAPH names %q:\n%s", qualified, strings.Join(st.generated, "\n"))
}

func distinct(xs []string) []string {
	seen := map[string]bool{}
	out := xs[:0:0]
	for _, x := range xs {
		if seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}
