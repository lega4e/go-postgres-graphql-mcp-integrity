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

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/sdl"
)

// The global JSON type, proved against the database rather than against the DDL
// text (gopgql#54).
//
// scalars_test.go establishes the premise the setting rests on: jsonb does not
// round-trip a document byte-identically and json does. What is left to prove is
// that the *global* setting delivers that per-column property to a column with
// no annotation on it, and — the part that cannot be argued from the DDL — that
// moving an already-deployed schema onto it is a migration PostgreSQL accepts.
//
// The second half matters most. The delta has to say USING, because PostgreSQL
// has no assignment cast from jsonb to json and rejects the bare ALTER with
// "column cannot be cast automatically to type json". That is a claim about
// PostgreSQL, so it is run against PostgreSQL.

const globalJSONSDL = `
type Note @node(label: "note", table: "note") {
  id: ID!
  payload: JSON
  indexed: JSON @column(type: "jsonb")
}
`

// The document exercises all three of jsonb's normalisations at once — key
// order, insignificant whitespace and a duplicated key — so a column that
// returns it unchanged is genuinely json and not a jsonb that happens to have
// preserved order.
const globalJSONDoc = `{"b":1, "a":2, "b":3}`

// TestGlobalJSONTypeRoundTripsAnUnannotatedColumn: the setting exists so that a
// schema on a round-trip path declares the safe type once rather than repeating
// @column(type: "json") on every column and having the one that was forgotten
// stay invisible until a stored value has more than one key.
func TestGlobalJSONTypeRoundTripsAnUnannotatedColumn(t *testing.T) {
	ctx := context.Background()
	pool, _ := applyWith(ctx, t, "global_json", globalJSONSDL,
		generator.Options{JSONType: generator.JSONTypeJSON})

	assert.Equal(t, "json", noteColumnType(ctx, t, pool, "payload"),
		"the global default reaches a column with no annotation on it")
	assert.Equal(t, "jsonb", noteColumnType(ctx, t, pool, "indexed"),
		"@column(type:) still wins per column: the global setting moves the default, "+
			"and an exception stays an exception")

	_, err := pool.Exec(ctx,
		"INSERT INTO note (payload, indexed) VALUES ($1::json, $2::json)", globalJSONDoc, globalJSONDoc)
	require.NoError(t, err)

	var payload, indexed string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT payload::text, indexed::text FROM note").Scan(&payload, &indexed))

	assert.Equal(t, globalJSONDoc, payload,
		"the point of the setting: byte-identical round trip, with nothing written on the column")
	assert.NotEqual(t, globalJSONDoc, indexed,
		"and the annotated column keeps jsonb's normalisation, which is what it was annotated for")
}

// TestMovingADeployedSchemaOntoJSON is the behaviour-change half of the
// acceptance, run end to end: a directory generated under the jsonb default,
// applied, then regenerated with --json-type json and applied again.
//
// A generator that reported "already up to date" here would be the exact failure
// this issue is about — a tool accepting a setting it then does not act on. So
// the delta is required to exist, required to apply, and required to leave a
// column that actually round-trips.
func TestMovingADeployedSchemaOntoJSON(t *testing.T) {
	ctx := context.Background()
	pool, dir := applyWith(ctx, t, "global_json_move", globalJSONSDL, generator.Options{})

	require.Equal(t, "jsonb", noteColumnType(ctx, t, pool, "payload"),
		"the starting point is the deployed default")
	_, err := pool.Exec(ctx, "INSERT INTO note (payload) VALUES ($1::json)", globalJSONDoc)
	require.NoError(t, err)

	// Regenerate the same directory under the new default.
	written := generateInto(t, dir, globalJSONSDL, "tojson",
		generator.Options{JSONType: generator.JSONTypeJSON})
	require.NotEmpty(t, written,
		"changing the JSON type must write a migration; reporting the directory up to date "+
			"would leave the database disagreeing with the schema, silently")

	require.NoError(t, gooseUp(ctx, connTo("global_json_move"), dir),
		"the delta has to be SQL PostgreSQL accepts: there is no assignment cast from jsonb "+
			"to json, so the ALTER only works because it carries an explicit USING")

	assert.Equal(t, "json", noteColumnType(ctx, t, pool, "payload"))

	// The existing row survived — a type change is an ALTER, not a drop and
	// re-add — even though what jsonb had already stored is jsonb's normalised
	// form, which no migration can un-normalise.
	var rows int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM note").Scan(&rows))
	assert.Equal(t, 1, rows, "the column's data is migrated, not discarded")

	// What the move buys is every document written from here on.
	_, err = pool.Exec(ctx, "INSERT INTO note (payload) VALUES ($1::json)", globalJSONDoc)
	require.NoError(t, err)
	var got string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT payload::text FROM note ORDER BY payload::text DESC LIMIT 1").Scan(&got))
	assert.Equal(t, globalJSONDoc, got, "after the migration the column round-trips")
}

// applyWith generates src into a fresh directory under opts, applies it to a
// fresh database, and returns a pool on it alongside the migration directory so
// a caller can generate a second time into the same history.
func applyWith(ctx context.Context, t *testing.T, database, src string, opts generator.Options) (*pgxpool.Pool, string) {
	t.Helper()
	require.NoError(t, createDatabases(ctx, database))
	conn := connTo(database)

	dir, err := os.MkdirTemp("", "gopgql-global-json-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	require.NotEmpty(t, generateInto(t, dir, src, "init", opts))
	require.NoError(t, gooseUp(ctx, conn, dir))

	pool, err := pgxpool.New(ctx, conn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool, dir
}

// generateInto writes the generation src calls for into dir and returns the
// paths written.
func generateInto(t *testing.T, dir, src, name string, opts generator.Options) []string {
	t.Helper()
	doc, err := sdl.Parse(src)
	require.NoError(t, err)
	model, err := generator.BuildWith(doc, opts)
	require.NoError(t, err)
	paths, err := migrate.Generate(dir, model, name, migrate.Halves{})
	require.NoError(t, err)
	return paths
}

func gooseUp(ctx context.Context, conn, dir string) error {
	db, err := sql.Open("pgx", conn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return goose.UpContext(ctx, db, dir)
}

// noteColumnType reads the physical type PostgreSQL actually built for a column
// of the note table, from the catalogue rather than from the DDL gopgql printed.
func noteColumnType(ctx context.Context, t *testing.T, pool *pgxpool.Pool, column string) string {
	t.Helper()
	var got string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT data_type FROM information_schema.columns
		WHERE table_name = 'note' AND column_name = $1`, column).Scan(&got))
	return got
}
