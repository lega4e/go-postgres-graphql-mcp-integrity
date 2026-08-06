package dbostx_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/sdl"
)

// The two smaller decisions gopgql#53 asked for, proved against the database
// rather than against the DDL text.
//
// Both are *decisions to leave something as it is* — JSON keeps mapping to
// jsonb, and Bytes stays unimplemented — and a decision like that is only worth
// anything if the premises under it are true. A DDL assertion cannot tell: it
// shows what gopgql emitted, not what PostgreSQL then does with it, and what
// PostgreSQL does with it is the entire argument. So each premise is run here.

const jsonSDL = `
type Doc @node(label: "doc", table: "doc") {
  id: ID!
  indexed: JSON
  verbatim: JSON @column(type: "json")
}
`

const byteaSDL = `
type Blob @node(label: "blob", table: "blob") {
  id: ID!
  data: String! @column(type: "bytea")
}
`

// TestJSONDefaultsToJSONBAndJSONIsTheEscape is the live half of the JSON
// decision (SPEC.md §5.1).
//
// The claim the decision rests on is that `jsonb` does not round-trip a document
// byte-identically and `json` does. That is documented PostgreSQL behaviour, and
// documented behaviour is exactly the kind that a version bump changes quietly:
// the escape gopgql now points every round-trip column at would stop working and
// nothing would say so. So both halves are asserted on the same row, written and
// read through the same path.
//
// The input is chosen to exercise all three normalisations jsonb performs at
// once — key order, insignificant whitespace, and a duplicated key — because a
// test that only reordered keys would pass against a jsonb that had merely
// become order-preserving.
func TestJSONDefaultsToJSONBAndJSONIsTheEscape(t *testing.T) {
	ctx := context.Background()
	pool := applySDL(ctx, t, "scalars_json", jsonSDL)

	// The types really are what the SDL asked for. Read from the catalogue, not
	// from the DDL gopgql printed: what matters is what PostgreSQL built.
	for _, tc := range []struct{ column, want string }{
		{"indexed", "jsonb"},
		{"verbatim", "json"},
	} {
		var got string
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT data_type FROM information_schema.columns
			WHERE table_name = 'doc' AND column_name = $1`, tc.column).Scan(&got))
		assert.Equal(t, tc.want, got, "column %q", tc.column)
	}

	const document = `{"b":1, "a":2, "b":3}`
	_, err := pool.Exec(ctx,
		"INSERT INTO doc (indexed, verbatim) VALUES ($1::json, $2::json)", document, document)
	require.NoError(t, err)

	var indexed, verbatim string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT indexed::text, verbatim::text FROM doc").Scan(&indexed, &verbatim))

	assert.Equal(t, document, verbatim,
		`@column(type: "json") is the documented escape for a column that must round-trip `+
			`byte-identically, so it has to actually do it`)
	assert.NotEqual(t, document, indexed,
		"and the default does not, which is the whole reason the escape is documented")
	assert.Equal(t, `{"a": 2, "b": 3}`, indexed,
		"jsonb reorders keys, drops insignificant whitespace and keeps the last of a duplicate — "+
			"all three, which is what makes it the wrong default for a signed or hashed payload "+
			"and the right one for everything that is queried")
}

// TestTheByteaMappingIsWhatBytesWouldHaveHadToDecide is the live half of the
// Bytes decision (SPEC.md §5.1).
//
// Two things are proved, and the decision needs both.
//
// First, that the mapping the refusal recommends works: `String @column(type:
// "bytea")` really produces a bytea column, and a value written to it comes back
// through a compiled gopgql query. A refusal that recommends something broken is
// worse than no refusal.
//
// Second, what a `Bytes` scalar would have had to settle, and that gopgql
// already refuses to settle it by accident. A byte string has no canonical
// response form the two shaping strategies can both produce — the Go-side path
// scans the value with pgx, the SQL-side path lets PostgreSQL render it into
// JSON — and gopgql's compiler already knows: compiling a bytea-mapped field
// under SQL-side shaping is refused, by name, with the alternative. Adding
// `Bytes` means choosing that canonical form and teaching both paths to produce
// it, which is a SPEC.md §5.1 decision rather than a bug fix. The refusal is the
// existing, deliberate position; the scalar's absence is consistent with it.
//
// So the test pins both sides of the guard: readable where the mapping is
// supported, refused with guidance where it is not. Either one alone would let a
// regression through — silently returning something on the SQL-side path is
// exactly the byte-identity violation SPEC.md §4 forbids.
func TestTheByteaMappingIsWhatBytesWouldHaveHadToDecide(t *testing.T) {
	ctx := context.Background()
	pool := applySDL(ctx, t, "scalars_bytea", byteaSDL)

	var udt string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT udt_name FROM information_schema.columns
		WHERE table_name = 'blob' AND column_name = 'data'`).Scan(&udt))
	require.Equal(t, "bytea", udt, `String @column(type: "bytea") really is a bytea column`)

	_, err := pool.Exec(ctx, `INSERT INTO blob (data) VALUES ('\x6465'::bytea)`)
	require.NoError(t, err)

	doc, err := sdl.Parse(byteaSDL)
	require.NoError(t, err)

	// Go-side: the mapping the refusal recommends works end to end.
	cq, err := compiler.New(doc, compiler.WithShaping(compiler.GoSide)).
		CompileQuery(`{ blob { data } }`, nil)
	require.NoError(t, err)

	out, err := exec.Query(ctx, exec.PgxQuerier(pool), cq)
	require.NoError(t, err, "the recommended mapping has to be readable, or the advice is bad")

	blobs, ok := out["blob"].([]any)
	require.True(t, ok, "the query returns a list of blobs, got %#v", out)
	require.Len(t, blobs, 1)
	row, ok := blobs[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []byte("de"), row["data"],
		"the bytes written come back as bytes; pgx scans bytea to []byte, which is the Go-side "+
			"form and the reason there is no single form to standardise on")

	// SQL-side: refused before a statement is ever built, naming the field and
	// the way out. This is the decision, enforced.
	_, err = compiler.New(doc, compiler.WithShaping(compiler.SQLSide)).
		CompileQuery(`{ blob { data } }`, nil)
	require.Error(t, err,
		"a byte string has no canonical response form, so SQL-side shaping must refuse rather "+
			"than promise a value it cannot make equal to the Go-side one")
	assert.Contains(t, err.Error(), "no canonical response form")
	assert.Contains(t, err.Error(), `@column(type: "bytea")`, "the refusal names the mapping")
	assert.Contains(t, err.Error(), "compiler.GoSide", "and the way to read the column anyway")
}

// applySDL builds the SDL's migrations, applies them to a database of its own in
// the shared container, and returns a pool on it. A database per case so that no
// case can see another's tables — the same reason TestMain gives the two
// existing ones their own.
func applySDL(ctx context.Context, t *testing.T, database, src string) *pgxpool.Pool {
	t.Helper()

	require.NoError(t, createDatabases(ctx, database))
	conn := connTo(database)

	doc, err := sdl.Parse(src)
	require.NoError(t, err)
	model, err := generator.Build(doc, "")
	require.NoError(t, err)

	dir, err := os.MkdirTemp("", "gopgql-scalars-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	_, err = migrate.Generate(dir, model, "init", migrate.Halves{})
	require.NoError(t, err)

	db, err := sql.Open("pgx", conn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	require.NoError(t, goose.UpContext(ctx, db, dir))

	pool, err := pgxpool.New(ctx, conn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}
