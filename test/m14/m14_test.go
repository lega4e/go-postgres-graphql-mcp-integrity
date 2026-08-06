// Package m14_test is the M14 integration suite: the generated Go client, run
// against a real postgres:19beta2 container (SPEC.md §7 → M14).
//
// The client in gen/ is **committed**, and that is the point twice over. It
// makes the generator's output reviewable in a diff rather than only assertable
// in a test, and it lets this suite do inside gopgql what a consumer's CI does
// downstream: regenerate, and fail if a single byte moved. A determinism
// regression is then caught here, on the commit that causes it, instead of in
// somebody else's build.
//
// What is then proven against the database is what a golden file cannot say —
// that the generated methods actually work:
//
//   - Every method takes the handle as its second parameter, and the suite hands
//     it a transaction it opened itself. Writes made through generated mutations
//     and reads made through a generated query see each other inside that
//     transaction, and nothing outside sees them until it commits.
//   - A generated query assembles its nested result into the generated structs,
//     including a nullable column that is genuinely NULL and a one-to-many
//     fan-out that must deduplicate its parent.
//   - Every scalar whose canonical response form is not the Go type pgx scanned
//     it as — Int, Float, a Float stored as numeric, DateTime, an Int list —
//     survives the trip through shape and back out into a typed field. A schema
//     with no numeric field at all is what let issue #51 ship in v0.2.0.
//   - A generated mutation whose function raises still carries the SQLSTATE
//     through the generated layer, as an *exec.FunctionError.
package m14_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
	"github.com/lega4e/gopgql/generator/client"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/sdl"
	"github.com/lega4e/gopgql/test/m14/gen"
)

// fixtureSQL is the command surface gopgql does not own: four PL/pgSQL
// functions over the tables gopgql itself generates. add_person, follow and
// measure are what the generated mutations call; explode exists to raise.
const fixtureSQL = `
CREATE SCHEMA app;

CREATE FUNCTION app.add_person(person_name text) RETURNS text
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO persons (name) VALUES (person_name);
    RETURN person_name;
END;
$$;

CREATE FUNCTION app.follow(from_name text, to_name text) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO follows (source_id, target_id)
    SELECT s.id, t.id FROM persons s, persons t
    WHERE s.name = from_name AND t.name = to_name;
END;
$$;

-- Fills in every scalar column the generated decoders read, and returns one of
-- them so the unshaped exec.Call path carries a number too.
--
-- marks, tally, ticket, active and notes are derived rather than passed: gopgql
-- binds no list parameter, and the point of ticket in particular is a literal
-- the caller could not have sent through a float64 without losing it —
-- 9007199254740993 is 2^53+1, the smallest integer a float64 rounds away.
CREATE FUNCTION app.measure(
    person_name text,
    person_age integer,
    person_score double precision,
    person_rating numeric,
    person_seen timestamptz
) RETURNS integer
LANGUAGE plpgsql AS $$
DECLARE
    stored integer;
BEGIN
    UPDATE persons
    SET age = person_age,
        score = person_score,
        rating = person_rating,
        seen = person_seen,
        marks = ARRAY[person_age - 1, person_age, person_age + 1],
        tally = person_age * 2,
        ticket = 9007199254740993,
        active = true,
        notes = '{"note":"measured","exact":19.90}'::jsonb
    WHERE name = person_name
    RETURNING age INTO stored;
    RETURN stored;
END;
$$;

CREATE FUNCTION app.explode(code text) RETURNS text
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'the function refused' USING ERRCODE = code;
END;
$$;
`

var connString string

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
	connString, err = pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		panic("connection string: " + err.Error())
	}
	if err := goose.SetDialect("postgres"); err != nil {
		panic("goose set dialect: " + err.Error())
	}
	goose.SetLogger(goose.NopLogger())

	if err := applyFixture(ctx); err != nil {
		panic("apply fixture: " + err.Error())
	}

	code := m.Run()
	_ = pgc.Terminate(context.Background())
	os.Exit(code)
}

// applyFixture generates and applies gopgql's own migrations for the fixture
// SDL, then installs the PL/pgSQL the mutations call.
func applyFixture(ctx context.Context) error {
	source, err := os.ReadFile("schema.graphql")
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
	dir, err := os.MkdirTemp("", "gopgql-m14-migrations-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if _, err := migrate.Generate(dir, model, "init", migrate.Halves{}); err != nil {
		return err
	}

	db, err := sql.Open("pgx", connString)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := goose.UpContext(ctx, db, dir); err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return err
	}
	defer pool.Close()
	_, err = pool.Exec(ctx, fixtureSQL)
	return err
}

// withTx runs fn inside a transaction the *test* opened and always rolls it
// back, so every case starts from the same database and nothing it wrote
// survives. The rollback is also what proves the point: everything below is
// visible to fn and to nothing else.
func withTx(t *testing.T, fn func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, connString)
	require.NoError(t, err)
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	fn(ctx, tx)
}

// TestGeneratedClientIsUpToDate is the `go generate && git diff --exit-code`
// check, run inside gopgql rather than left to a consumer's CI. Regenerating the
// committed client from the committed inputs must produce exactly the committed
// bytes.
func TestGeneratedClientIsUpToDate(t *testing.T) {
	source, err := os.ReadFile("schema.graphql")
	require.NoError(t, err)
	doc, err := sdl.Parse(string(source))
	require.NoError(t, err)

	sources, err := client.Load("operations")
	require.NoError(t, err)
	files, err := client.Generate(doc, sources, client.Options{Package: "gen"})
	require.NoError(t, err)
	require.Len(t, files, 1)

	committed, err := os.ReadFile(filepath.Join("gen", files[0].Name))
	require.NoError(t, err, "the generated client is committed on purpose: it is the diff a reviewer reads")
	assert.Equal(t, string(committed), string(files[0].Content),
		"gen/%s is stale — regenerate it with:\n"+
			"  go run ./cmd/gopgql generate client --sdl test/m14/schema.graphql "+
			"--operations test/m14/operations --out test/m14/gen --package gen",
		files[0].Name)
}

// The committed client must also build and vet as part of the module, which
// `go test ./...` already enforces by compiling this package. Stated as a test
// so the guarantee is findable.
func TestGeneratedClientCompilesIntoTheModule(t *testing.T) {
	require.NotNil(t, gen.New(), "if this package did not compile, the suite would not run")
}

// TestGeneratedMutationsRunInTheCallersTransaction is task 5.17: the generated
// methods take the handle, the handle is the test's own transaction, and the
// writes are visible to a generated *query* through that same transaction.
func TestGeneratedMutationsRunInTheCallersTransaction(t *testing.T) {
	withTx(t, func(ctx context.Context, tx pgx.Tx) {
		c := gen.New()

		name, err := c.AddPerson(ctx, exec.Pgx(tx), gen.AddPersonInput{PersonName: "Ada"})
		require.NoError(t, err)
		assert.Equal(t, "Ada", name, "a scalar-returning function's value comes back typed")

		_, err = c.AddPerson(ctx, exec.Pgx(tx), gen.AddPersonInput{PersonName: "Grace"})
		require.NoError(t, err)

		// A void-returning mutation's method returns only whether it worked.
		require.NoError(t, c.Follow(ctx, exec.Pgx(tx), gen.FollowInput{FromName: "Ada", ToName: "Grace"}))

		people, err := c.ListPeople(ctx, exec.Pgx(tx), gen.ListPeopleInput{})
		require.NoError(t, err)
		require.Len(t, people, 1,
			"only Ada has a follow, and the query's MATCH requires one")
		assert.Equal(t, "Ada", people[0].Name)
		assert.Nil(t, people[0].Nickname, "a NULL column stays nil rather than becoming \"\"")
		require.Len(t, people[0].Follows, 1)
		assert.Equal(t, "Grace", people[0].Follows[0].Name)
	})
}

// Nothing the generated methods wrote is visible outside the transaction that
// wrote it. This is the exactly-once property, and a client that opened its own
// connection could not provide it.
func TestGeneratedWritesAreInvisibleOutsideTheTransaction(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, connString)
	require.NoError(t, err)
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	c := gen.New()
	_, err = c.AddPerson(ctx, exec.Pgx(tx), gen.AddPersonInput{PersonName: "Hopper"})
	require.NoError(t, err)

	inside, err := c.FindPerson(ctx, exec.Pgx(tx), gen.FindPersonInput{Name: "Hopper"})
	require.NoError(t, err)
	assert.Len(t, inside, 1, "the caller's own transaction sees its own write")

	// A second pool, at the same moment, sees nothing.
	other, err := pgxpool.New(ctx, connString)
	require.NoError(t, err)
	defer other.Close()

	outside, err := c.FindPerson(ctx, exec.Pgx(other), gen.FindPersonInput{Name: "Hopper"})
	require.NoError(t, err)
	assert.Empty(t, outside, "an uncommitted write must not be visible to anything else")
}

// Issue #51, end to end and against the database: every scalar the generated
// decoders handle, read back through the generated structs.
//
// This is the case the suite could not previously have: until the schema
// declared a numeric field, all seven of the client's decode sites were
// gopgqlAsString, so gopgqlAsInt64, gopgqlAsFloat64 and gopgqlAsTime were
// emitted and never invoked. Both halves — shape's canonicalisation and the
// generated decoders — were individually tested, and nothing executed the join.
//
// It projects the scalars that were *not* broken as well. The bug was never
// specific to Int; it was a decoder written against the wrong producer, and the
// only fixture that can notice the next one is the one that reads them all.
//
// The deterministic version of the same assertion, under both shaping
// strategies, is in gen/join_test.go; this one proves it against real rows a
// real driver scanned.
func TestGeneratedClientDecodesTheCanonicalScalarForms(t *testing.T) {
	withTx(t, func(ctx context.Context, tx pgx.Tx) {
		c := gen.New()
		_, err := c.AddPerson(ctx, exec.Pgx(tx), gen.AddPersonInput{PersonName: "Ada"})
		require.NoError(t, err)

		// Before anything is measured: the SDL's defaults decode, and the
		// columns that are genuinely NULL stay nil rather than becoming zero.
		unmeasured, err := c.FindMeasurements(ctx, exec.Pgx(tx), gen.FindMeasurementsInput{Name: "Ada"})
		require.NoError(t, err)
		require.Len(t, unmeasured, 1)
		assert.Equal(t, int64(0), unmeasured[0].Age, "@default(value: \"0\") reaches the caller as 0")
		assert.Nil(t, unmeasured[0].Rating, "a NULL numeric stays nil")
		assert.Nil(t, unmeasured[0].Seen, "a NULL timestamptz stays nil")
		assert.Nil(t, unmeasured[0].Marks)
		assert.Nil(t, unmeasured[0].Tally, "a NULL Int stays nil rather than becoming 0")
		assert.Nil(t, unmeasured[0].Notes, "a NULL jsonb stays nil")
		assert.False(t, unmeasured[0].Active, "@default(value: \"false\") reaches the caller as false")
		assert.Equal(t, int64(0), unmeasured[0].Ticket)

		// The offset is deliberately not UTC: shape converts a DateTime to UTC
		// so the response cannot depend on a session's TimeZone, and what comes
		// back is the same instant rather than the same wall clock.
		seen := time.Date(2020, 1, 2, 3, 4, 5, 0, time.FixedZone("+02:00", 2*60*60))
		age, err := c.Measure(ctx, exec.Pgx(tx), gen.MeasureInput{
			PersonName:   "Ada",
			PersonAge:    41,
			PersonScore:  9.5,
			PersonRating: 12.5,
			PersonSeen:   seen,
		})
		require.NoError(t, err)
		assert.Equal(t, int64(41), age,
			"a scalar-returning function's Int is unshaped: exec.Call hands back what pgx scanned")

		people, err := c.FindMeasurements(ctx, exec.Pgx(tx), gen.FindMeasurementsInput{Name: "Ada"})
		require.NoError(t, err)
		require.Len(t, people, 1)

		assert.Equal(t, int64(41), people[0].Age, "an Int reaches the decoder as json.Number")
		assert.Equal(t, 9.5, people[0].Score, "a Float reaches the decoder as json.Number")
		require.NotNil(t, people[0].Rating)
		assert.Equal(t, 12.5, *people[0].Rating, "so does a Float stored as numeric(10,2)")
		require.NotNil(t, people[0].Seen)
		assert.True(t, seen.Equal(*people[0].Seen),
			"a DateTime reaches the decoder as RFC3339Nano text in UTC")
		assert.Equal(t, []int64{40, 41, 42}, people[0].Marks, "an Int list is canonicalised elementwise")

		require.NotNil(t, people[0].Tally)
		assert.Equal(t, int64(82), *people[0].Tally, "a nullable Int that has a value decodes like any other")

		// The assertion that a decode routed through float64 cannot pass:
		// 9007199254740993 is 2^53+1, which a float64 rounds to ...992.
		assert.Equal(t, int64(9007199254740993), people[0].Ticket,
			"an Int beyond float64 precision survives, because json.Number.Int64 reads the digits")

		// String, ID, Boolean and JSON: canonicalised to the Go types pgx already
		// scans, so these are the cases that must keep passing rather than start.
		assert.Equal(t, "Ada", people[0].Name)
		assert.NotEmpty(t, people[0].Id, "an ID is canonicalised from pgx's [16]byte to uuid text")
		assert.True(t, people[0].Active)
		require.NotNil(t, people[0].Notes)
		assert.Equal(t, map[string]any{
			"note": "measured",
			// shape decodes an embedded document with UseNumber, so the
			// database's own digits survive inside it too — 19.90, not 19.9.
			"exact": json.Number("19.90"),
		}, *people[0].Notes, "a JSON document arrives decoded, and gopgqlAsAny passes it through")
	})
}

// Task 5.18: a failing generated mutation surfaces *exec.FunctionError with the
// SQLSTATE intact, through the generated layer.
func TestGeneratedMutationCarriesTheSQLSTATE(t *testing.T) {
	withTx(t, func(ctx context.Context, tx pgx.Tx) {
		_, err := gen.New().Explode(ctx, exec.Pgx(tx), gen.ExplodeInput{Code: "P0001"})
		require.Error(t, err)

		var fnErr *exec.FunctionError
		require.ErrorAs(t, err, &fnErr,
			"the generated layer must not flatten the typed error into prose")
		assert.Equal(t, "P0001", fnErr.SQLSTATE)
		assert.Equal(t, "the function refused", fnErr.Message)
		assert.Equal(t, "app", fnErr.Schema)
		assert.Equal(t, "explode", fnErr.Function)
	})
}

// A generated method runs on a pool as readily as on a transaction — the handle
// is whatever the caller owns, and `*pgxpool.Pool` satisfies it too.
func TestGeneratedQueryRunsOnAPool(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, connString)
	require.NoError(t, err)
	defer pool.Close()

	_, err = gen.New().FindPerson(ctx, exec.Pgx(pool), gen.FindPersonInput{Name: "nobody"})
	require.NoError(t, err)
}

// The generated package is regenerated by this command; the test above fails
// when it has not been. Keeping it as a //go:generate line means the fix is
// copy-pasteable from the file that needs it.
//
//go:generate go run ../../cmd/gopgql generate client --sdl schema.graphql --operations operations --out gen --package gen
