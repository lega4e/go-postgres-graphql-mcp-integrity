// Package dbostx_test is the integration suite for gopgql#53: the two defects
// that blocked a DBOS-backed consumer, each proved against a real
// postgres:19beta2 and, for the second, a real DBOS transaction.
//
// Both defects are of the kind a unit test can miss, so both are proved here as
// well as in their packages:
//
//   - **Owned tables must be created (item A).** The migrate package asserts on
//     the emitted DDL, which is the right place to catch it. But the emitted DDL
//     was not merely incomplete — it carried a foreign key into a table nothing
//     created, so it did not apply at all. A text assertion would still pass if
//     someone later broke *application*; goose running the generation against a
//     live server cannot.
//
//   - **A generated client must accept a DBOS transaction (item B).** No unit
//     test can prove this, because what was wrong was a *type* relationship
//     between two packages: dbos.Tx (sysdb.Tx) could not satisfy a pgx-typed
//     exec.Handle, and Go matches interfaces on exact signatures. A hand-written
//     double shaped like sysdb.Tx would prove the adapter compiles against
//     something; only the real thing proves it compiles against DBOS, and only a
//     real transaction proves the statement runs inside it.
//
// The suite runs both against one container, in two databases, so neither
// fixture can see the other — DBOS creates its own tables in a `dbos` schema,
// and item A's fixture needs a `dbos` schema of its own standing in for the one
// DBOS owns.
package dbostx_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5/pgxpool"

	// Registers the "pgx" database/sql driver goose runs through.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/sdl"
	"github.com/lega4e/gopgql/test/m14/gen"
)

// twoSchemaSDL is the shape gopgql#53 item A was found in: an application schema
// gopgql owns joined to one it does not, every join a foreign-key-mapped edge
// because an edge touching a @readonly type has to be.
//
// agentiq.session is the table the defect turned on — an owned vertex table that
// is also the HAS_SESSION edge's table. dbos.operation_outputs is the boundary
// case beside it: also an edge's table, but its vertex is @readonly, so gopgql
// must create nothing for it.
const twoSchemaSDL = `
type Workflow @node(label: "workflow", table: "workflow_status", schema: "dbos") @readonly
  @key(fields: ["workflowUuid"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  sessions: [Session!]! @relationship(
    type: "HAS_SESSION" direction: OUT table: "session" schema: "agentiq"
    sourceKey: ["workflow_uuid"] destKey: ["id"]
  )
  steps: [Step!]! @relationship(
    type: "HAS_STEP" direction: OUT table: "operation_outputs" schema: "dbos"
    sourceKey: ["workflow_uuid"] destKey: ["workflow_uuid", "function_id"]
  )
}
type Step @node(label: "step", table: "operation_outputs", schema: "dbos") @readonly
  @key(fields: ["workflowUuid", "functionId"]) {
  workflowUuid: ID! @column(name: "workflow_uuid")
  functionId: Int! @column(name: "function_id")
}
type Session @node(label: "session", table: "session", schema: "agentiq") {
  id: ID!
  workflowUuid: ID! @column(name: "workflow_uuid")
  events: [Event!]! @relationship(type: "HAS_EVENT", direction: OUT)
}
type Event @node(label: "event", table: "event", schema: "agentiq") {
  id: ID!
  kind: String!
}
`

// foreignFixture is what the other tool already created: the two @readonly
// tables, and the schema gopgql's own tables go in. gopgql emits no CREATE
// SCHEMA — a schema it does not own is not its to create — so agentiq is made
// here too.
const foreignFixture = `
CREATE SCHEMA dbos;
CREATE SCHEMA agentiq;

CREATE TABLE dbos.workflow_status (
    workflow_uuid uuid PRIMARY KEY
);

CREATE TABLE dbos.operation_outputs (
    workflow_uuid uuid NOT NULL REFERENCES dbos.workflow_status (workflow_uuid),
    function_id   integer NOT NULL,
    PRIMARY KEY (workflow_uuid, function_id)
);
`

var baseConn string

func TestMain(m *testing.M) {
	ctx := context.Background()

	pgc, err := postgres.Run(ctx, "postgres:19beta2",
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
		panic("start postgres:19beta2 container: " + err.Error())
	}
	if baseConn, err = pgc.ConnectionString(ctx, "sslmode=disable"); err != nil {
		panic("connection string: " + err.Error())
	}
	if err := goose.SetDialect("postgres"); err != nil {
		panic("goose set dialect: " + err.Error())
	}
	goose.SetLogger(goose.NopLogger())

	// One database per case. DBOS's system tables live in a `dbos` schema, and
	// item A's fixture needs a `dbos` schema standing in for exactly that, so
	// sharing one database would have each fixture fighting the other.
	if err := createDatabases(ctx, "twoschema", "dbostx"); err != nil {
		panic("create databases: " + err.Error())
	}
	// The M14 fixture is applied once, here, rather than by whichever DBOS case
	// runs first: both of them need it, and either must be runnable alone with
	// -run.
	if err := applyM14Fixture(ctx, connTo("dbostx")); err != nil {
		panic("apply the M14 fixture: " + err.Error())
	}

	code := m.Run()
	_ = pgc.Terminate(context.Background())
	os.Exit(code)
}

func createDatabases(ctx context.Context, names ...string) error {
	pool, err := pgxpool.New(ctx, baseConn)
	if err != nil {
		return err
	}
	defer pool.Close()
	for _, name := range names {
		if _, err := pool.Exec(ctx, "CREATE DATABASE "+name); err != nil {
			return err
		}
	}
	return nil
}

// connTo points the container's connection string at another database in it.
func connTo(database string) string {
	return strings.Replace(baseConn, "/gopgql?", "/"+database+"?", 1)
}

// A DBOS transaction reaches exec.Handle and exec.Querier through the adapter,
// and the type parameters are inferred — the caller writes exec.Portable(tx) and
// nothing else.
//
// This is a compile-time assertion, which is the right kind: what gopgql#53 item
// B reported was not a runtime failure but a build one —
//
//	sysdb.Tx does not implement exec.Handle (wrong type for method Exec)
//	    have Exec(context.Context, string, ...any) (sysdb.Result, error)
//	    want Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
//
// so a package that compiles is the guarantee. It is stated as a var rather than
// left implicit in the tests below so that a reader looking for "can a DBOS
// transaction be a gopgql handle" finds the answer, and so that it keeps holding
// if the runtime tests are ever skipped.
var (
	_ = func(tx dbos.Tx) exec.Handle { return exec.Portable(tx) }
	_ = func(tx dbos.Tx) exec.Querier { return exec.PortableQuerier(tx) }
	_ = func(p dbos.Pool) exec.Handle { return exec.Portable(p) }
)

// TestTheTwoSchemaGenerationApplies is the acceptance for item A that a text
// assertion cannot make.
//
// Before the fix this generation emitted no CREATE TABLE for agentiq.session
// while still emitting "HAS_EVENT" with REFERENCES agentiq.session (id). goose
// stops on the first failing statement, so the failure here is the real one a
// consumer hit: the migration does not apply, and the database is left without
// the schema the SDL describes.
func TestTheTwoSchemaGenerationApplies(t *testing.T) {
	ctx := context.Background()
	conn := connTo("twoschema")

	pool, err := pgxpool.New(ctx, conn)
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Exec(ctx, foreignFixture)
	require.NoError(t, err, "the tables gopgql does not own are created by whoever owns them")

	doc, err := sdl.Parse(twoSchemaSDL)
	require.NoError(t, err)
	model, err := generator.Build(doc, "")
	require.NoError(t, err)

	dir := t.TempDir()
	paths, err := migrate.Generate(dir, model, "init", migrate.Halves{})
	require.NoError(t, err)
	require.NotEmpty(t, paths, "a schema with owned tables generates something")

	db, err := sql.Open("pgx", conn)
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, goose.UpContext(ctx, db, dir),
		"the generation must apply; gopgql#53 emitted a foreign key into a table it never created")

	// The tables gopgql owns are there…
	for _, owned := range []string{"agentiq.session", "agentiq.event", "public.\"HAS_EVENT\""} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			"SELECT to_regclass($1) IS NOT NULL", owned).Scan(&exists))
		assert.True(t, exists, "%s is a table gopgql owns and must have created", owned)
	}

	// …and the property graph really was built over them, which is what makes
	// the tables' existence more than a coincidence.
	var labels int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_catalog.pg_propgraph_label l
		JOIN pg_catalog.pg_propgraph_element_label el ON el.pgellabelid = l.oid`).Scan(&labels))
	assert.Positive(t, labels, "the graph half was applied too")

	// A second generation against the applied directory writes nothing, and now
	// says so truthfully rather than by accident.
	again, err := migrate.Generate(dir, model, "again", migrate.Halves{})
	require.NoError(t, err, "an unchanged schema whose tables all exist is genuinely up to date")
	assert.Empty(t, again)
}

// TestAGeneratedMethodRunsInsideADBOSTransaction is the acceptance for item B.
//
// It is the whole point of the portable handle, and it is written the way the
// consumer writes it: a workflow, a DataSource over the application's own pool,
// and generated client calls inside dbos.RunAsTransaction with the transaction
// DBOS hands in. exec.Portable(tx) is the only line that is gopgql's doing, and
// its type parameters are inferred — gopgql names no DBOS type anywhere, so a
// generated client whose schema has nothing to do with workflows gains no
// workflow dependency.
//
// Both paths are exercised on purpose, because the defect broke both: AddPerson
// and Follow are `@function` mutations (exec.Call → Exec and a scalar read), and
// ListPeople is a compiled GRAPH_TABLE query (exec.Query → the flat read that
// needs column names a portable cursor cannot report).
func TestAGeneratedMethodRunsInsideADBOSTransaction(t *testing.T) {
	ctx := context.Background()
	conn := connTo("dbostx")

	pool, err := pgxpool.New(ctx, conn)
	require.NoError(t, err)
	defer pool.Close()

	dctx, err := dbos.NewContext(ctx, dbos.Config{AppName: "gopgql-dbostx", DatabaseURL: conn})
	require.NoError(t, err)
	t.Cleanup(func() { _ = dbos.Shutdown(dctx, 30*time.Second) })

	ds, err := dbos.NewDataSource(dctx, pool, dbos.WithDataSourceName("app"))
	require.NoError(t, err)

	c := gen.New()

	// The workflow writes two people, relates them, and reads them back — all
	// through generated methods, all on the transaction DBOS opened.
	wf := func(wctx dbos.Context, _ string) ([]string, error) {
		return dbos.RunAsTransaction(wctx, ds, func(ctx context.Context, tx dbos.Tx) ([]string, error) {
			h := exec.Portable(tx)

			if _, err := c.AddPerson(ctx, h, gen.AddPersonInput{PersonName: "Ada"}); err != nil {
				return nil, err
			}
			if _, err := c.AddPerson(ctx, h, gen.AddPersonInput{PersonName: "Grace"}); err != nil {
				return nil, err
			}
			if err := c.Follow(ctx, h, gen.FollowInput{FromName: "Ada", ToName: "Grace"}); err != nil {
				return nil, err
			}

			// A compiled query on the same handle sees the uncommitted writes,
			// which is the property that makes a caller-supplied transaction
			// worth having at all.
			people, err := c.ListPeople(ctx, h, gen.ListPeopleInput{})
			if err != nil {
				return nil, err
			}
			var out []string
			for _, p := range people {
				for _, f := range p.Follows {
					out = append(out, p.Name+"->"+f.Name)
				}
			}
			return out, nil
		})
	}
	dbos.RegisterWorkflow(dctx, wf)
	require.NoError(t, dbos.Launch(dctx))

	handle, err := dbos.RunWorkflow(dctx, wf, "")
	require.NoError(t, err)
	edges, err := handle.GetResult()
	require.NoError(t, err, "a generated method must run inside a caller-supplied DBOS transaction")

	assert.Equal(t, []string{"Ada->Grace"}, edges,
		"the generated query read, inside the transaction, what the generated mutations had just written")

	// The transaction committed, so the writes are visible outside it too.
	var people int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM persons").Scan(&people))
	assert.Equal(t, 2, people)
}

// A transaction that fails rolls its writes back, and the generated methods that
// made them are inside that rollback. This is the half that proves the
// statements really ran in DBOS's transaction rather than on some connection of
// their own — a generated method that quietly opened its own connection would
// leave the row behind.
func TestAFailedDBOSTransactionRollsBackGeneratedWrites(t *testing.T) {
	ctx := context.Background()
	conn := connTo("dbostx")

	pool, err := pgxpool.New(ctx, conn)
	require.NoError(t, err)
	defer pool.Close()

	var before int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM persons").Scan(&before))

	dctx, err := dbos.NewContext(ctx, dbos.Config{AppName: "gopgql-dbostx-rollback", DatabaseURL: conn})
	require.NoError(t, err)
	t.Cleanup(func() { _ = dbos.Shutdown(dctx, 30*time.Second) })

	ds, err := dbos.NewDataSource(dctx, pool, dbos.WithDataSourceName("app"))
	require.NoError(t, err)

	c := gen.New()
	wf := func(wctx dbos.Context, _ string) (string, error) {
		return dbos.RunAsTransaction(wctx, ds, func(ctx context.Context, tx dbos.Tx) (string, error) {
			h := exec.Portable(tx)
			if _, err := c.AddPerson(ctx, h, gen.AddPersonInput{PersonName: "Rolled Back"}); err != nil {
				return "", err
			}

			// The write is read back on the transaction's own handle before the
			// failure, so the assertion after the rollback means what it says.
			// Without this the test would also pass against a generated method
			// that wrote nothing at all — before and after would be equal for
			// the wrong reason, which is the failure mode a rollback test is
			// most prone to.
			rows, err := tx.Query(ctx, "SELECT count(*) FROM persons WHERE name = $1", "Rolled Back")
			if err != nil {
				return "", err
			}
			defer func() { _ = rows.Close() }()
			if !rows.Next() {
				return "", fmt.Errorf("counting inside the transaction returned no row: %w", rows.Err())
			}
			var inside int
			if err := rows.Scan(&inside); err != nil {
				return "", err
			}
			if inside != 1 {
				return "", fmt.Errorf("the write is not visible inside its own transaction: got %d", inside)
			}

			return "", fmt.Errorf("the application refused after writing")
		}, dbos.WithStepMaxRetries(1))
	}
	dbos.RegisterWorkflow(dctx, wf)
	require.NoError(t, dbos.Launch(dctx))

	handle, err := dbos.RunWorkflow(dctx, wf, "")
	require.NoError(t, err)
	_, err = handle.GetResult()
	require.Error(t, err, "the workflow fails, because the transaction did")
	require.Contains(t, err.Error(), "the application refused after writing",
		"the transaction failed where this test meant it to — not on the read-back that "+
			"proves the write landed inside it")

	var after int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM persons").Scan(&after))
	assert.Equal(t, before, after,
		"a write made through a generated method inside a rolled-back transaction must not survive it")
}

// applyM14Fixture builds the M14 schema and its PL/pgSQL into the given
// database. The SDL, the operations and the committed client are M14's; reusing
// them is deliberate, because what is under test here is the handle, and a
// second generated client would only add a second thing that could be wrong.
func applyM14Fixture(ctx context.Context, conn string) error {
	source, err := os.ReadFile("../m14/schema.graphql")
	if err != nil {
		return err
	}
	doc, err := sdl.Parse(string(source))
	if err != nil {
		return err
	}
	model, err := generator.Build(doc, "")
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "gopgql-dbostx-migrations-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if _, err := migrate.Generate(dir, model, "init", migrate.Halves{}); err != nil {
		return err
	}

	db, err := sql.Open("pgx", conn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := goose.UpContext(ctx, db, dir); err != nil {
		return err
	}

	functions, err := os.ReadFile("fixture.sql")
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, conn)
	if err != nil {
		return err
	}
	defer pool.Close()
	_, err = pool.Exec(ctx, string(functions))
	return err
}
