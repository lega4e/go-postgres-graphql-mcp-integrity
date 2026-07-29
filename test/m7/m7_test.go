// Package m7_test is the M7 integration suite: the acceptance criteria of
// issue #9, each one proven against a real postgres:19beta2 container.
//
// Sections 1–6 of the milestone are already covered by unit tests that build a
// schema model, render DDL and compare strings. Every one of those tests can
// pass while the database rejects the statement, silently ignores it, or
// enforces something other than what was asked for. This suite exists to close
// exactly that gap, so it asserts on what the database *did* — rows returned,
// errors raised with their SQLSTATE, catalog contents — and not on SQL text.
// The single exception is task 7.4, which is explicitly about the *contents* of
// a generated migration: whether the delta renames a column or drops and re-adds
// it is not observable after the fact except as data loss, and asserting on the
// data alone would let a drop-and-add pass on an empty table.
//
// What each test proves:
//
//   - 7.1 @check reaches PostgreSQL as an enforced constraint: a violating row
//     is rejected with SQLSTATE 23514 under the deterministic constraint name,
//     and a conforming row is stored.
//   - 7.2 a natural-key vertex is matchable by MATCH — and, the milestone's open
//     question, that PostgreSQL accepts a graph whose vertex KEY is the natural
//     key while its edges still REFERENCE the surrogate id (design D1).
//   - 7.3 a duplicate natural key is refused by the database, not by gopgql.
//   - 7.4 @renamedFrom moves a column and its data instead of dropping it.
//   - 7.5 folding a rename back out of the emitted migrations reconstructs the
//     prior state correctly — the invisible half of design D3.
//   - 7.6 conformance is quiet on a database that matches its SDL.
//   - 7.7 conformance names out-of-band drift that was injected behind gopgql's
//     back, which is the entire reason the check exists.
//   - 7.8 @default is the database's default, not a value gopgql supplies.
//
// Every test gets its own freshly created database on the shared container, so
// no test can make another's assertion accidentally true by leaving a table
// behind.
package m7_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	// Registers the "pgx" database/sql driver goose runs migrations through.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/conform"
	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/schema"
	"github.com/lega4e/gopgql/sdl"
)

// --- SDL fixtures ---

// constraintsSDL exercises the whole M7 constraint surface on one type: a
// two-column natural key, a table-level check spanning two columns, a
// column-level check, two defaults, and a self-relationship so the edge table
// is there to disagree with the natural key if PostgreSQL wants it to (7.2).
const constraintsSDL = `type Person @node(label: "person")
    @key(fields: ["tenant", "email"])
    @check(expr: "nickname IS NULL OR nickname <> email") {
  id: ID!
  tenant: String!
  email: String!
  name: String!
  age: Int @default(value: "0") @check(expr: "age >= 0")
  nickname: String @default(value: "'unknown'")
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// renameBaseSDL is the schema a rename starts from: plain, so that the only
// difference between it and its successor is the thing under test.
const renameBaseSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
}`

// renameColumnSDL renames Person.email to Person.contact and says so.
const renameColumnSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  contact: String @renamedFrom(name: "email")
}`

// renameTableSDL renames the type, and with it the table its rows live in.
const renameTableSDL = `type Human @node(label: "human") @renamedFrom(name: "Person") {
  id: ID!
  name: String!
  email: String
}`

// foldBaseSDL and foldRenamedSDL are 7.5's pair. They carry a natural key, a
// default and a check on purpose: a rename has to drag the constraints that
// mention the renamed column along with it, and the fold has to read every one
// of those statements back out of the migration it just wrote.
const foldBaseSDL = `type Person @node(label: "person") @key(fields: ["tenant", "email"]) {
  id: ID!
  tenant: String!
  email: String!
  age: Int @default(value: "0") @check(expr: "age >= 0")
}`

const foldRenamedSDL = `type Person @node(label: "person") @key(fields: ["tenant", "contact"]) {
  id: ID!
  tenant: String!
  contact: String! @renamedFrom(name: "email")
  age: Int @default(value: "0") @check(expr: "age >= 0")
}`

// --- container lifecycle ---

// baseDSN points at the container's bootstrap database. Tests never use it for
// schema work; it is the handle newEnv issues CREATE DATABASE through.
var baseDSN string

// TestMain boots one postgres:19beta2 container for the whole package. There is
// no skip path when Docker is absent (SPEC.md §10): a suite that quietly passes
// without a database would assert nothing that matters here.
func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// runSuite exists so the container's cleanup runs before os.Exit, which defers
// in TestMain would not.
func runSuite(m *testing.M) int {
	ctx := context.Background()

	pgc, err := postgres.Run(ctx, "postgres:19beta2",
		postgres.WithDatabase("gopgql"),
		postgres.WithUsername("gopgql"),
		postgres.WithPassword("gopgql"),
		tc.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(3*time.Minute),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres:19beta2 container: %v\n", err)
		return 1
	}
	defer func() {
		if err := pgc.Terminate(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "terminate container: %v\n", err)
		}
	}()

	baseDSN, err = pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		return 1
	}

	if err := goose.SetDialect("postgres"); err != nil {
		fmt.Fprintf(os.Stderr, "goose set dialect: %v\n", err)
		return 1
	}
	goose.SetLogger(goose.NopLogger())

	return m.Run()
}

// --- per-test environment ---

// env is one test's private database plus the migration directory and the SDL
// document that were used to build it.
type env struct {
	t   *testing.T
	ctx context.Context
	dsn string
	// pool is the handle every assertion reads through.
	pool *pgxpool.Pool
	// dir is the migration directory — one directory, one goose history, with
	// the graph teardown and rebuild numbered around the table DDL of their own
	// generation (gopgql#38).
	dir   string
	doc   *sdl.Document
	model *schema.Schema
}

// dbUnsafe matches everything CREATE DATABASE will not take unquoted.
var dbUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// dbSeq numbers the databases handed out, so two envs inside one test (7.5
// needs exactly that) never collide and no env can be handed a database another
// one still has open.
var dbSeq atomic.Int64

// dbName derives a unique, legal database name that still says which test owns
// it, truncated to PostgreSQL's 63-byte identifier limit.
func dbName(testName string) string {
	slug := strings.Trim(dbUnsafe.ReplaceAllString(strings.ToLower(testName), "_"), "_")
	const maxSlug = 48
	if len(slug) > maxSlug {
		slug = slug[:maxSlug]
	}
	return fmt.Sprintf("m7_%02d_%s", dbSeq.Add(1), slug)
}

// newEnv creates a database of this test's own and returns a handle on it.
//
// Isolation is a fresh database rather than a shared one that is truncated
// between tests, because half of what this suite asserts on lives in the
// catalogs — constraints, indexes, property graphs — where a leftover from an
// earlier test would not be truncated away and could satisfy a later
// assertion by accident.
func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := t.Context()

	name := dbName(t.Name())

	admin, err := pgxpool.New(ctx, baseDSN)
	require.NoError(t, err, "connect to the bootstrap database")
	defer admin.Close()
	_, err = admin.Exec(ctx, `CREATE DATABASE `+pgQuote(name))
	require.NoError(t, err, "create this test's own database")

	u, err := url.Parse(baseDSN)
	require.NoError(t, err)
	u.Path = "/" + name

	e := &env{t: t, ctx: ctx, dsn: u.String(), dir: t.TempDir()}
	e.pool, err = pgxpool.New(ctx, e.dsn)
	require.NoError(t, err, "connect to %s", name)
	t.Cleanup(e.pool.Close)
	return e
}

// pgQuote double-quotes an identifier for a statement that cannot take a bind
// parameter. Only ever applied to names this file constructs.
func pgQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// applyInit parses src, writes the first generation and applies it. It is the
// whole gopgql pipeline — SDL, model, DDL, goose — so a schema that PostgreSQL
// refuses fails here rather than in an assertion further down.
func (e *env) applyInit(src string) {
	e.t.Helper()
	e.build(src)
	paths, err := migrate.Generate(e.dir, e.model, "init", migrate.Halves{})
	require.NoError(e.t, err)
	require.Len(e.t, paths, 2, "a first generation is the tables, then the graph")
	e.gooseUp()
}

// applyDelta folds the migrations written so far, diffs them against src,
// applies the generation and returns the *tables* migration of it so a caller can
// assert on what it says (7.4).
//
// The tables migration is the one under test throughout this suite: every
// statement M7 is about — a rename, a constraint, a default — is table work, and
// the two graph migrations around it only take the property graph down and put it
// back (gopgql#38).
func (e *env) applyDelta(src string) string {
	e.t.Helper()
	e.build(src)
	paths, err := migrate.Generate(e.dir, e.model, "delta", migrate.Halves{})
	require.NoError(e.t, err, "generate delta")
	require.NotEmpty(e.t, paths, "expected a delta, but the schemas were identical")

	tables := ""
	for _, p := range paths {
		if strings.HasSuffix(p, "_"+migrate.SuffixTables+".sql") {
			tables = p
		}
	}
	require.NotEmpty(e.t, tables, "expected table work in %v", paths)
	content, err := os.ReadFile(tables) //nolint:gosec // path is one this test just wrote
	require.NoError(e.t, err)
	e.gooseUp()
	return string(content)
}

// build parses SDL into the model the rest of the pipeline runs on.
func (e *env) build(src string) {
	e.t.Helper()
	doc, err := sdl.Parse(src)
	require.NoError(e.t, err, "parse SDL")
	m, err := generator.Build(doc, "")
	require.NoError(e.t, err, "build schema model")
	e.doc, e.model = doc, m
}

// gooseUp applies every unapplied migration in this env's directory, through a
// database handle of its own.
//
// A plain forward apply in version order, against goose's own default version
// table. That is enough because a generation's files are numbered in the order
// they have to run in (gopgql#38, design D3).
func (e *env) gooseUp() {
	e.t.Helper()
	db, err := sql.Open("pgx", e.dsn)
	require.NoError(e.t, err)
	defer db.Close()
	require.NoError(e.t, goose.UpContext(e.ctx, db, e.dir), "apply %s", e.dir)
}

// mustExec runs a statement that is expected to succeed.
//
// The statements these helpers run are written out in full, with their values
// inline and no bind parameters, and that is deliberate: they stand in for the
// writes gopgql never sees — what a psql session, an ETL job or an ORM sharing
// the database would issue — which is precisely what makes them evidence that
// the database is enforcing something rather than gopgql. The suite's claim
// about bind parameters (7.2) is a claim about the *compiler's* output, and is
// asserted there, on cq.SQL and cq.Args.
func (e *env) mustExec(query string) {
	e.t.Helper()
	_, err := e.pool.Exec(e.ctx, query)
	require.NoError(e.t, err, "statement failed: %s", query)
}

// execErr runs a statement that is expected to be rejected, and returns the
// rejection.
func (e *env) execErr(query string) error {
	e.t.Helper()
	_, err := e.pool.Exec(e.ctx, query)
	return err
}

// run compiles a GraphQL query against this env's document and executes it,
// returning both the shaped response and the compiled form, so a test can
// assert on the answer and on how the values travelled.
func (e *env) run(query string, vars map[string]any) (map[string]any, *compiler.Compiled) {
	e.t.Helper()
	cq, err := compiler.New(e.doc).CompileQuery(query, vars)
	require.NoError(e.t, err, "compile %q", query)
	resp, err := exec.Query(e.ctx, e.pool, cq)
	require.NoError(e.t, err, "execute compiled SQL:\n%s", cq.SQL)
	return resp, cq
}

// scalar reads a single value out of the database.
func scalar[T any](e *env, query string) T {
	e.t.Helper()
	var out T
	require.NoError(e.t, e.pool.QueryRow(e.ctx, query).Scan(&out), "query: %s", query)
	return out
}

// reflectGraph reads the live property graph back into a schema model.
func (e *env) reflectGraph() *schema.Schema {
	e.t.Helper()
	actual, err := conform.Reflect(e.ctx, e.pool, "")
	require.NoError(e.t, err, "reflect the property graph")
	return actual
}

// requirePgError insists a statement was refused by PostgreSQL with a specific
// SQLSTATE, and returns the error for further assertions. Checking the code
// rather than the message is what distinguishes "the database enforced the
// constraint" from "something else went wrong and the write happened not to
// land".
func requirePgError(t *testing.T, err error, sqlstate string) *pgconn.PgError {
	t.Helper()
	require.Error(t, err, "the statement succeeded; the database did not enforce the constraint")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "want a PostgreSQL error, got %T", err)
	require.Equal(t, sqlstate, pgErr.Code, "SQLSTATE (%s)", pgErr.Message)
	return pgErr
}

// upSection returns the -- +goose Up half of a migration file. Task 7.4 asks
// what the *forward* migration does; the Down half legitimately contains the
// inverse statements and would otherwise poison a whole-file assertion.
func upSection(t *testing.T, content string) string {
	t.Helper()
	up, _, found := strings.Cut(content, "-- +goose Down")
	require.True(t, found, "migration has no Down section:\n%s", content)
	return strings.TrimPrefix(up, "-- +goose Up")
}

// downSection returns the -- +goose Down half of a migration file.
func downSection(t *testing.T, content string) string {
	t.Helper()
	_, down, found := strings.Cut(content, "-- +goose Down")
	require.True(t, found, "migration has no Down section:\n%s", content)
	return down
}

// --- 7.1 @check ---

// TestCheckConstraintsAreEnforcedByTheDatabase is task 7.1.
//
// @check is only worth having if PostgreSQL is the one saying no. gopgql never
// parses the expression (design D6, Non-Goals), so the only evidence that a
// check works is a write the database refuses — identified by SQLSTATE 23514 and
// by the constraint name the generator chose, which also proves the naming rule
// the differ later drops constraints by is the name that actually landed.
func TestCheckConstraintsAreEnforcedByTheDatabase(t *testing.T) {
	e := newEnv(t)
	e.applyInit(constraintsSDL)

	t.Run("valid data is stored", func(t *testing.T) {
		e.mustExec(`INSERT INTO persons (tenant, email, name, age, nickname)
		            VALUES ('acme', 'alice@example.com', 'Alice', 30, 'ally')`)
		assert.Equal(t, int64(1), scalar[int64](e, `SELECT count(*) FROM persons`))
	})

	t.Run("a column-level check refuses a violating row", func(t *testing.T) {
		err := e.execErr(`INSERT INTO persons (tenant, email, name, age)
		                  VALUES ('acme', 'bob@example.com', 'Bob', -1)`)
		pgErr := requirePgError(t, err, "23514")
		assert.Equal(t, "persons_age_check", pgErr.ConstraintName,
			"the check landed under the deterministic name a later delta drops it by")
		assert.Equal(t, "persons", pgErr.TableName)
	})

	t.Run("a table-level check spanning two columns refuses a violating row", func(t *testing.T) {
		err := e.execErr(`INSERT INTO persons (tenant, email, name, nickname)
		                  VALUES ('acme', 'carol@example.com', 'Carol', 'carol@example.com')`)
		pgErr := requirePgError(t, err, "23514")
		assert.Equal(t, "persons_check_1", pgErr.ConstraintName,
			"a type-level @check is emitted at table level, numbered by declaration order")
	})

	t.Run("only the violating rows were refused", func(t *testing.T) {
		assert.Equal(t, int64(1), scalar[int64](e, `SELECT count(*) FROM persons`),
			"the rejected inserts left nothing behind")
	})
}

// --- 7.2 natural key is matchable by MATCH ---

// TestNaturalKeyVertexIsMatchable is task 7.2, and it is the experiment that
// settles the milestone's open question.
//
// Design D1 puts the natural key in the property graph's KEY (...) clause while
// leaving edge tables referencing the surrogate id, and section 3's author could
// not verify that PostgreSQL accepts the combination without a database: it was
// plausible that SQL/PGQ requires an edge's referenced columns to *be* the
// referenced element's key, which would have forced either multi-column edge
// references or dropping the KEY clause. The applyInit below is the answer —
// CREATE PROPERTY GRAPH is in the migration, so a server that rejected the
// combination would fail the test here, before a single assertion runs.
//
// Beyond that, the test proves the key is usable: a MATCH filtering on both key
// columns picks exactly the right vertex out of rows that collide on each column
// separately, and a traversal over the edge table still works from that vertex,
// which is what "alongside the surrogate id, not instead of it" has to mean.
func TestNaturalKeyVertexIsMatchable(t *testing.T) {
	e := newEnv(t)
	// Applying this at all is the load-bearing step: it runs CREATE PROPERTY
	// GRAPH with `persons KEY (tenant, email)` and an edge table declaring
	// `REFERENCES persons (id)` in the same statement.
	e.applyInit(constraintsSDL)

	// Two rows share a tenant, two share an email: neither column alone selects
	// a unique row, so a query that returns one row is using the composite key.
	e.mustExec(`INSERT INTO persons (tenant, email, name) VALUES
	            ('acme',   'alice@example.com', 'Alice'),
	            ('acme',   'bob@example.com',   'Bob'),
	            ('globex', 'alice@example.com', 'Alicia')`)
	e.mustExec(`INSERT INTO follows (source_id, target_id)
	            SELECT a.id, b.id FROM persons a, persons b
	            WHERE a.name = 'Alice' AND b.name = 'Bob'`)

	t.Run("the graph exposes the key columns as properties", func(t *testing.T) {
		actual := e.reflectGraph()
		require.Len(t, actual.VertexTables, 1)
		assert.Subset(t, actual.VertexTables[0].Properties, []string{"tenant", "email"},
			"a key column that is not a property could not be filtered on")
		assert.Contains(t, actual.VertexTables[0].Properties, "id",
			"the surrogate id stays a property; the natural key is alongside it")
	})

	t.Run("a MATCH filtering on the natural key returns the one row", func(t *testing.T) {
		resp, cq := e.run(
			`{ persons(tenant: $tenant, email: $email) { name } }`,
			map[string]any{"tenant": "acme", "email": "alice@example.com"})

		assert.Equal(t, map[string]any{
			"persons": []any{map[string]any{"name": "Alice"}},
		}, resp, "the composite key selected exactly one of three rows")

		assert.Equal(t, []any{"acme", "alice@example.com"}, cq.Args,
			"the key's values travel as bind parameters")
		assert.NotContains(t, cq.SQL, "alice@example.com",
			"a filter value is bound, never interpolated (SPEC.md §6.2)")
	})

	t.Run("edges still traverse from a natural-key vertex", func(t *testing.T) {
		resp, _ := e.run(
			`{ persons(tenant: $tenant, email: $email) { name follows { name } } }`,
			map[string]any{"tenant": "acme", "email": "alice@example.com"})

		assert.Equal(t, map[string]any{
			"persons": []any{map[string]any{
				"name":    "Alice",
				"follows": []any{map[string]any{"name": "Bob"}},
			}},
		}, resp, "the edge table references the surrogate id and still resolves")
	})
}

// --- 7.3 duplicate natural key ---

// TestDuplicateNaturalKeyIsRefused is task 7.3. The natural key is a promise
// about the data, and a promise the application keeps is not a constraint — so
// the assertion is a SQLSTATE 23505 from PostgreSQL under the constraint name
// the generator emitted, on an insert gopgql never sees.
func TestDuplicateNaturalKeyIsRefused(t *testing.T) {
	e := newEnv(t)
	e.applyInit(constraintsSDL)

	e.mustExec(`INSERT INTO persons (tenant, email, name)
	            VALUES ('acme', 'alice@example.com', 'Alice')`)

	t.Run("a duplicate key is rejected", func(t *testing.T) {
		err := e.execErr(`INSERT INTO persons (tenant, email, name)
		                  VALUES ('acme', 'alice@example.com', 'Impostor')`)
		pgErr := requirePgError(t, err, "23505")
		assert.Equal(t, "persons_key", pgErr.ConstraintName)
	})

	t.Run("a row differing in one key column is accepted", func(t *testing.T) {
		// The constraint has to be over the pair, not over either column: if it
		// were on tenant alone, or on email alone, one of these would fail.
		e.mustExec(`INSERT INTO persons (tenant, email, name)
		            VALUES ('globex', 'alice@example.com', 'Alicia')`)
		e.mustExec(`INSERT INTO persons (tenant, email, name)
		            VALUES ('acme', 'bob@example.com', 'Bob')`)
		assert.Equal(t, int64(3), scalar[int64](e, `SELECT count(*) FROM persons`))
	})
}

// --- 7.4 @renamedFrom ---

// TestRenameMovesDataInsteadOfDroppingIt is task 7.4.
//
// The difference between a rename and a drop-and-add is invisible in the final
// schema and visible only in the rows, so both halves are asserted: the delta
// says RENAME and says nothing about dropping the old object, and the rows that
// existed before it ran are still there afterwards with their values intact.
func TestRenameMovesDataInsteadOfDroppingIt(t *testing.T) {
	t.Run("column", func(t *testing.T) {
		e := newEnv(t)
		e.applyInit(renameBaseSDL)
		e.mustExec(`INSERT INTO persons (name, email) VALUES
		            ('Alice', 'alice@example.com'),
		            ('Bob',   'bob@example.com')`)

		content := e.applyDelta(renameColumnSDL)
		up := upSection(t, content)

		assert.Contains(t, up, "ALTER TABLE persons RENAME COLUMN email TO contact;")
		assert.NotContains(t, up, "DROP COLUMN email",
			"a drop would take the column's rows with it")
		assert.NotContains(t, up, "ADD COLUMN contact",
			"an add would create the column empty next to the one holding the data")
		assert.Contains(t, downSection(t, content),
			"ALTER TABLE persons RENAME COLUMN contact TO email;",
			"the Down section is the exact inverse")

		assert.Equal(t, int64(1), scalar[int64](e,
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_schema = 'public' AND table_name = 'persons' AND column_name = 'contact'`))
		assert.Equal(t, int64(0), scalar[int64](e,
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_schema = 'public' AND table_name = 'persons' AND column_name = 'email'`))

		assert.Equal(t, "alice@example.com",
			scalar[string](e, `SELECT contact FROM persons WHERE name = 'Alice'`),
			"the seeded value travelled with the column")
		assert.Equal(t, int64(2), scalar[int64](e, `SELECT count(*) FROM persons`))
	})

	t.Run("table", func(t *testing.T) {
		e := newEnv(t)
		e.applyInit(renameBaseSDL)
		e.mustExec(`INSERT INTO persons (name, email) VALUES ('Alice', 'alice@example.com')`)

		content := e.applyDelta(renameTableSDL)
		up := upSection(t, content)

		assert.Contains(t, up, "ALTER TABLE persons RENAME TO humans;")
		assert.NotContains(t, up, "DROP TABLE", "a drop would take the rows with it")
		assert.NotContains(t, up, "CREATE TABLE humans",
			"a create would leave the rows in the old table")

		assert.Equal(t, "alice@example.com",
			scalar[string](e, `SELECT email FROM humans WHERE name = 'Alice'`),
			"the rows are in the renamed table")
	})

	// Re-generating from the same SDL after the rename has landed must be a
	// no-op: the hint now names something the prior state no longer has, and
	// design D2 says that is nothing to do rather than an error. Without this,
	// a schema could only ever be migrated once after a rename.
	t.Run("regenerating after the rename lands emits nothing", func(t *testing.T) {
		e := newEnv(t)
		e.applyInit(renameBaseSDL)
		e.applyDelta(renameColumnSDL)

		e.build(renameColumnSDL)
		paths, err := migrate.Generate(e.dir, e.model, "delta", migrate.Halves{})
		require.NoError(t, err)
		assert.Empty(t, paths, "the same SDL, hint and all, still generates cleanly")
	})
}

// --- 7.5 fold correctness across a rename ---

// TestFoldAcrossARenameMatchesADirectApply is task 7.5 and the proof of design
// D3, the half of a rename that is invisible until it goes wrong.
//
// migrate.Fold reconstructs prior state by re-parsing gopgql's own migrations.
// If the reader does not understand a RENAME — or an ADD/DROP CONSTRAINT that
// travelled with it — the reconstructed state is wrong, and the *next* delta is
// computed against a schema in which the rename never happened. Nothing about
// that is visible in the migration that did the renaming; it surfaces one
// migration later, as a drop of a column that was already renamed.
//
// Comparing the folded path against a direct apply of the same final SDL catches
// it: the two databases must be indistinguishable in their tables, columns,
// defaults, constraints and indexes, and in the property graph laid over them.
// The schemas here deliberately carry a natural key over the renamed column, so
// the fold has constraint statements to read back and not just the rename.
func TestFoldAcrossARenameMatchesADirectApply(t *testing.T) {
	folded := newEnv(t)
	folded.applyInit(foldBaseSDL)
	// Rows exist across the rename so this path is a realistic migration and
	// not a rename of an empty table.
	folded.mustExec(`INSERT INTO persons (tenant, email, age) VALUES ('acme', 'alice@example.com', 30)`)
	content := folded.applyDelta(foldRenamedSDL)
	require.Contains(t, upSection(t, content), "RENAME COLUMN email TO contact;",
		"this scenario is only meaningful if the delta actually renamed something")

	direct := newEnv(t)
	direct.applyInit(foldRenamedSDL)

	assert.Equal(t, physicalFingerprint(direct), physicalFingerprint(folded),
		"a folded apply must reach the same physical schema as a direct apply")

	// The property graph is metadata a delta always drops and recreates, so it
	// is compared separately — and with the milestone's own drift checker, which
	// is exactly the question being asked of two databases.
	report := conform.Check(direct.reflectGraph(), folded.reflectGraph())
	assert.True(t, report.OK(), "the two property graphs differ: %+v", report.Findings)

	assert.Equal(t, "alice@example.com",
		scalar[string](folded, `SELECT contact FROM persons WHERE tenant = 'acme'`),
		"and the row survived the path that got there by migrating")
}

// physicalFingerprint renders everything about a database's tables that a
// migration can get wrong — columns with their types, nullability and defaults,
// every named constraint with its definition, and every index — as a stable
// sorted string.
//
// It reads the catalogs rather than information_schema for the constraint
// definitions, because pg_get_constraintdef is the database's own rendering of
// what a constraint means and so cannot agree with a wrong constraint that
// happens to have the right name. goose's bookkeeping table is excluded: it
// records how a schema was reached, which is precisely the difference under
// test.
//
// PostgreSQL's implicit NOT NULL constraints (contype 'n') are excluded, and
// only their names would have differed. Since PostgreSQL 18 a NOT NULL is a
// named pg_constraint row, auto-named <table>_<column>_not_null — and
// ALTER TABLE … RENAME COLUMN does not rename it, so a column renamed from
// email to contact keeps a constraint called persons_email_not_null where a
// direct apply calls it persons_contact_not_null (verified directly against
// postgres:19beta2). gopgql never writes that name: it emits `contact text NOT
// NULL` inside the column definition and lets the server name the constraint.
// Matching the names would mean the differ hard-coding PostgreSQL's private
// naming scheme and emitting a RENAME CONSTRAINT for a name it never chose.
// The nullability itself is what carries meaning, and it is still compared —
// notnull= on every column line above.
func physicalFingerprint(e *env) string {
	e.t.Helper()
	var lines []string

	lines = append(lines, fingerprintRows(e, `
		SELECT format('col %s.%s %s notnull=%s default=%s',
		              c.relname, a.attname, format_type(a.atttypid, a.atttypmod),
		              a.attnotnull, coalesce(pg_get_expr(d.adbin, d.adrelid), '-'))
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		  LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
		 WHERE n.nspname = 'public' AND c.relkind = 'r'
		   AND NOT starts_with(c.relname, 'goose_db_version')`)...)

	lines = append(lines, fingerprintRows(e, `
		SELECT format('con %s.%s %s', c.relname, con.conname, pg_get_constraintdef(con.oid))
		  FROM pg_constraint con
		  JOIN pg_class c ON c.oid = con.conrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND NOT starts_with(c.relname, 'goose_db_version')
		   AND con.contype <> 'n'`)...)

	lines = append(lines, fingerprintRows(e, `
		SELECT format('idx %s.%s %s', tablename, indexname, indexdef)
		  FROM pg_indexes
		 WHERE schemaname = 'public' AND NOT starts_with(tablename, 'goose_db_version')`)...)

	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func fingerprintRows(e *env, query string) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, query)
	require.NoError(e.t, err, "fingerprint query: %s", query)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		require.NoError(e.t, rows.Scan(&line))
		out = append(out, line)
	}
	require.NoError(e.t, rows.Err())
	return out
}

// --- 7.6 conformance on a clean database ---

// TestConformancePassesOnACleanDatabase is task 7.6. A drift detector that
// reports drift on a database gopgql itself just built is worse than none: every
// real finding would be lost in the noise and operators would learn to ignore
// it. So the assertion is that the report is empty, on the most directive-dense
// schema in the suite.
func TestConformancePassesOnACleanDatabase(t *testing.T) {
	e := newEnv(t)
	e.applyInit(constraintsSDL)

	report := conform.Check(e.model, e.reflectGraph())
	assert.True(t, report.OK(), "unexpected findings: %+v", report.Findings)
	assert.Empty(t, report.Findings)
}

// TestReflectReportsAMissingGraph covers the conformance spec's "No graph"
// scenario, which only a real connection can prove. An empty model would compare
// as total drift and send an operator looking for a catastrophe when the truth is
// usually that the migrations were never applied or the DSN points somewhere
// else — so reflection has to say "no graph", not "no elements".
func TestReflectReportsAMissingGraph(t *testing.T) {
	e := newEnv(t)

	_, err := conform.Reflect(e.ctx, e.pool, "")
	require.ErrorIs(t, err, conform.ErrGraphNotFound)
	assert.Contains(t, err.Error(), generator.DefaultGraphName,
		"the error names the graph it looked for")
}

// --- 7.7 conformance detects injected drift ---

// TestConformanceDetectsOutOfBandDrift is task 7.7, and the reason the package
// exists at all. SPEC.md §3.1 assumes nobody alters the database behind gopgql's
// back; the generator, differ and compiler all reason from the SDL and would go
// on agreeing with each other while the database quietly diverged. Here the
// assumption is deliberately broken with a statement gopgql never emits, and the
// check has to notice — naming the property and its element, as a kind a caller
// can branch on rather than a sentence it would have to parse.
func TestConformanceDetectsOutOfBandDrift(t *testing.T) {
	e := newEnv(t)
	e.applyInit(constraintsSDL)
	require.True(t, conform.Check(e.model, e.reflectGraph()).OK(),
		"the database must be clean before drift is injected, or the finding proves nothing")

	// Out of band on purpose: no migration, no SDL change, nothing gopgql could
	// have folded. This is what a hand-edited database looks like.
	e.mustExec(`ALTER PROPERTY GRAPH "app_graph"
	            ALTER VERTEX TABLE "persons"
	            ALTER LABEL "person" DROP PROPERTIES ("nickname")`)

	report := conform.Check(e.model, e.reflectGraph())
	assert.False(t, report.OK(), "the check must not pass on a drifted database")
	require.Len(t, report.Findings, 1, "one property was dropped: %+v", report.Findings)

	assert.Equal(t, conform.Finding{
		Kind:     conform.MissingProperty,
		Element:  "persons",
		Property: "nickname",
		Want:     "nickname",
	}, report.Findings[0], "the finding names the element, the property and the kind of drift")
}

// --- 7.8 @default ---

// TestDefaultIsAppliedByTheDatabase is task 7.8. @default has to be the
// *column's* default, so that a write gopgql never sees still gets it; a default
// gopgql applied when building an INSERT would be no default at all for the
// psql session, the ETL job or the ORM sharing the database.
func TestDefaultIsAppliedByTheDatabase(t *testing.T) {
	e := newEnv(t)
	e.applyInit(constraintsSDL)

	// The insert names neither defaulted column, and does not go through gopgql.
	e.mustExec(`INSERT INTO persons (tenant, email, name)
	            VALUES ('acme', 'alice@example.com', 'Alice')`)

	assert.Equal(t, int32(0), scalar[int32](e, `SELECT age FROM persons WHERE name = 'Alice'`),
		"@default(value: \"0\") reached the column")
	assert.Equal(t, "unknown", scalar[string](e, `SELECT nickname FROM persons WHERE name = 'Alice'`),
		"a quoted default is emitted verbatim, so the stored value is the string")

	// The default is recorded on the column, not merely applied by one insert.
	assert.Equal(t, "0", scalar[string](e, `
		SELECT column_default FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'persons' AND column_name = 'age'`))

	// An explicit value still wins, which is what makes it a default rather than
	// a constant.
	e.mustExec(`INSERT INTO persons (tenant, email, name, age, nickname)
	            VALUES ('acme', 'bob@example.com', 'Bob', 41, 'bobby')`)
	assert.Equal(t, int32(41), scalar[int32](e, `SELECT age FROM persons WHERE name = 'Bob'`))
	assert.Equal(t, "bobby", scalar[string](e, `SELECT nickname FROM persons WHERE name = 'Bob'`))
}
