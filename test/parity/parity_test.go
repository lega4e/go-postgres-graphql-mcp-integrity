// Package parity_test is M8's acceptance criterion: every query the M1–M7
// suites execute, re-run against a real postgres:19beta2 container under both
// shaping strategies, with the encoded responses required to be byte-equal.
//
// One suite, two strategies — not a copied suite. Each catalogue entry is
// compiled twice from the same SDL and run twice against the same database
// state, and the two responses go through shape.Encode. Byte-identity is
// defined as those bytes being equal (SPEC.md §7 → M8, design D3), and it holds
// by construction: the SQL-side path decodes the JSON PostgreSQL returned into
// the same Go value the Go-side path builds, and one encoder writes both.
//
// Two things keep the suite honest. Order is asserted **exactly** here — no
// canon, no array-order-ignoring — because list order is the divergence most
// likely to occur and the one a lenient comparison would hide (task 6.4). And
// each response is checked against what the owning milestone suite already
// asserts, so parity cannot be satisfied by both strategies being wrong in the
// same way (task 6.3).
package parity_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	// Registers the "pgx" database/sql driver goose applies migrations through.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/sdl"
	"github.com/lega4e/gopgql/shape"
)

// baseDSN points at the container's bootstrap database. One container serves
// the whole package and each world gets a database of its own inside it: the CI
// test job is capped at 25 minutes and this change adds two container-backed
// packages, so booting one per world is not affordable (design, Risks).
var baseDSN string

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
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

// built is a world that has been created, migrated and seeded, with the SDL
// document its queries compile against.
type built struct {
	pool *pgxpool.Pool
	doc  *sdl.Document
	// dsn is kept so a test can open a second connection with different
	// session settings — which is the only way to show that the response does
	// not depend on them.
	dsn string
}

// build creates the world's database, applies its schema through the real
// generate-and-goose pipeline, and seeds it. A world is built once and shared by
// every catalogue entry that names it.
func (w *world) build(t *testing.T) *built {
	t.Helper()
	ctx := t.Context()

	admin, err := pgxpool.New(ctx, baseDSN)
	require.NoError(t, err, "connect to the bootstrap database")
	defer admin.Close()
	_, err = admin.Exec(ctx, `CREATE DATABASE "`+w.name+`"`)
	require.NoError(t, err, "create database for world %q", w.name)

	u, err := url.Parse(baseDSN)
	require.NoError(t, err)
	u.Path = "/" + w.name

	doc, err := sdl.Parse(w.sdl)
	require.NoError(t, err, "parse the SDL of world %q", w.name)
	model, err := generator.Build(doc, "")
	require.NoError(t, err, "build the schema model of world %q", w.name)

	dir := t.TempDir()
	_, err = migrate.Generate(dir, model, "init", migrate.Halves{})
	require.NoError(t, err, "generate the initial migration")

	db, err := sql.Open("pgx", u.String())
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, goose.UpContext(ctx, db, dir), "apply the migration for world %q", w.name)

	pool, err := pgxpool.New(ctx, u.String())
	require.NoError(t, err, "connect to world %q", w.name)
	t.Cleanup(pool.Close)

	for _, stmt := range w.seed {
		_, err := pool.Exec(ctx, stmt)
		require.NoError(t, err, "seed world %q with:\n%s", w.name, stmt)
	}
	return &built{pool: pool, doc: doc, dsn: u.String()}
}

// TestParity is the milestone's exit criterion. Every catalogue entry is
// compiled and executed under both strategies and the encoded responses must be
// byte-equal.
func TestParity(t *testing.T) {
	worlds := map[*world]*built{}
	for _, sc := range catalogue {
		if _, done := worlds[sc.world]; !done {
			worlds[sc.world] = sc.world.build(t)
		}
	}

	for _, sc := range catalogue {
		t.Run(sc.name, func(t *testing.T) {
			w := worlds[sc.world]
			ctx := t.Context()

			goSide := runStrategy(ctx, t, w, sc, compiler.GoSide)
			sqlSide := runStrategy(ctx, t, w, sc, compiler.SQLSide)

			// The claim, stated as bytes. Order is part of it: the two
			// encodings are compared as they are, with nothing sorted first.
			require.Equal(t, string(goSide.encoded), string(sqlSide.encoded),
				"the two strategies disagree.\n--- go-side SQL ---\n%s\n--- sql-side SQL ---\n%s",
				goSide.sql, sqlSide.sql)

			// Parity is not enough on its own: both strategies could be wrong
			// together. Each response is also what the owning milestone claims.
			if sc.want != "" {
				assertMatchesMilestone(t, sc, goSide.response)
			}

			// The two statements are genuinely different queries — the whole
			// premise of the milestone is that they are.
			assert.NotEqual(t, goSide.sql, sqlSide.sql,
				"the two strategies emitted the same SQL, so only one of them ran")

			// And the SQL-side query really did the assembly: one row, one
			// column, built in the database.
			assert.Contains(t, sqlSide.sql, "json_agg", "the SQL-side query aggregates in-database")
			assert.NotContains(t, sqlSide.sql, "jsonb_build_object",
				"jsonb sorts keys by length-then-bytes and drops duplicates (design D2)")
		})
	}
}

// outcome is one strategy's run of one entry.
type outcome struct {
	sql      string
	response map[string]any
	encoded  []byte
}

// runStrategy compiles the entry under one strategy and executes it.
func runStrategy(ctx context.Context, t *testing.T, w *built, sc scenario, s compiler.Shaping) outcome {
	t.Helper()

	cq, err := compiler.New(w.doc, compiler.WithShaping(s)).CompileQuery(sc.query, sc.vars)
	require.NoError(t, err, "compile %q under %s", sc.query, s)
	require.Equal(t, s, cq.Shaping, "the compiled query records the strategy it was compiled under")

	resp, err := exec.Query(ctx, exec.PgxQuerier(w.pool), cq)
	require.NoError(t, err, "execute under %s:\n%s", s, cq.SQL)

	encoded, err := shape.Encode(resp)
	require.NoError(t, err, "encode the %s response", s)

	return outcome{sql: cq.SQL, response: resp, encoded: encoded}
}

// assertMatchesMilestone checks a response against what the owning milestone
// suite asserts.
//
// The comparison ignores array order, because that is the strength the milestone
// asserted — it had no deterministic order to assert (design D4), and holding its
// expectations to one now would be reading a claim into them they never made.
// The exact-order claim is the strategy-versus-strategy comparison above, which
// is where it belongs.
func assertMatchesMilestone(t *testing.T, sc scenario, got map[string]any) {
	t.Helper()

	gotBytes, err := json.Marshal(got)
	require.NoError(t, err)

	var gotAny, wantAny any
	require.NoError(t, json.Unmarshal(gotBytes, &gotAny))
	require.NoError(t, json.Unmarshal([]byte(sc.want), &wantAny),
		"the expected JSON recorded for %s is not valid JSON", sc.name)

	assert.True(t, reflect.DeepEqual(canon(gotAny), canon(wantAny)),
		"%s no longer returns what the %s suite asserts:\n--- got ---\n%s\n--- want ---\n%s",
		sc.name, sc.milestone, gotBytes, sc.want)
}

// canon recursively sorts arrays so a comparison ignores their order. It is the
// same helper the milestone suites use (test/m3/m3_test.go), and it is used here
// for exactly one purpose: comparing against those suites' expectations.
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

// TestSQLSideRefusesAnUnshapeableScalar is the other half of the scalar contract
// (design D5): a type gopgql has no canonical form for is refused at compile
// time under SQL-side shaping, and still accepted under Go-side, which makes no
// cross-strategy promise about it.
//
// It needs no container — a compile-time refusal is the point.
func TestSQLSideRefusesAnUnshapeableScalar(t *testing.T) {
	const src = `type Event @node(label: "event") {
  id: ID!
  name: String!
  window: String! @column(type: "interval")
}`
	doc, err := sdl.Parse(src)
	require.NoError(t, err)

	_, _, err = compiler.New(doc).Compile(`{ events { name window } }`, nil)
	assert.NoError(t, err, "Go-side shaping keeps accepting it")

	_, _, err = compiler.New(doc, compiler.WithShaping(compiler.SQLSide)).
		Compile(`{ events { name window } }`, nil)

	var unshapeable *compiler.UnshapeableScalarError
	require.ErrorAs(t, err, &unshapeable,
		"a scalar with no canonical form must be a typed error, so a caller can fall back without matching English")
	assert.Equal(t, "window", unshapeable.Field)
	assert.Equal(t, "interval", unshapeable.ColumnType)

	// A field the contract does cover still compiles under both.
	_, _, err = compiler.New(doc, compiler.WithShaping(compiler.SQLSide)).
		Compile(`{ events { name } }`, nil)
	assert.NoError(t, err)
}

// TestEncodeIsTheOnlyThingThatWritesBytes records what byte-identity does not
// claim. PostgreSQL's own rendering is not gopgql's, and the difference stops at
// the decode boundary rather than being papered over (design D3).
func TestEncodeIsTheOnlyThingThatWritesBytes(t *testing.T) {
	resp := map[string]any{"zebra": 1, "a": 2}
	encoded, err := shape.Encode(resp)
	require.NoError(t, err)
	assert.Equal(t, `{"a":2,"zebra":1}`, string(encoded),
		"Go sorts map keys, which is what decides response key order on both paths")
	assert.NotContains(t, string(encoded), " : ",
		"json_build_object emits `{\"k\" : v}`; those bytes never reach a caller")
}

// requireStrategiesDifferOnlyOutsideMatch is task 3.4: the MATCH pattern, its
// predicates and its bind parameters are identical between strategies, so which
// rows match is not part of what parity has to prove.
func TestStrategiesShareTheMatchPattern(t *testing.T) {
	doc, err := sdl.Parse(personSDL)
	require.NoError(t, err)

	for _, query := range []string{
		`{ persons(name: $n) { name follows { name } } }`,
		`{ persons(name: $n) { name follows { name } followedBy { name } } }`,
		`{ persons(name: $n) { name follows { name follows { name } followedBy { name } } } }`,
	} {
		t.Run(query, func(t *testing.T) {
			vars := map[string]any{"n": "Alice"}
			flat, err := compiler.New(doc).CompileQuery(query, vars)
			require.NoError(t, err)
			shaped, err := compiler.New(doc, compiler.WithShaping(compiler.SQLSide)).CompileQuery(query, vars)
			require.NoError(t, err)

			assert.Equal(t, flat.Args, shaped.Args, "the bind parameters and their $n order are the same")
			assert.Equal(t, matchBlocks(flat.SQL), matchBlocks(shaped.SQL),
				"the two strategies must differ only outside MATCH")
		})
	}
}

// matchBlocks extracts every MATCH line of a statement, with leading whitespace
// stripped so a difference in indentation is not read as a difference in
// pattern.
func matchBlocks(sqlText string) []string {
	var out []string
	for _, line := range strings.Split(sqlText, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "MATCH ") || strings.HasPrefix(trimmed, "WHERE ") ||
			strings.HasPrefix(trimmed, "COLUMNS (") {
			out = append(out, trimmed)
		}
	}
	return out
}

// TestDateTimeDoesNotDependOnTheSessionTimeZone is the case TestParity cannot
// reach on its own.
//
// PostgreSQL renders a timestamptz in the *session's* TimeZone, so the same row
// is "…+00:00" on one connection and "…+02:00" on another. The container's
// session is UTC, and Go's RFC3339 layout writes a zero offset as "Z" — so on a
// UTC session the normalisation to UTC is a no-op and removing it breaks
// nothing. That was verified by removing it: every parity case still passed.
//
// A response that is correct only because the runner happened to be in UTC is
// not the guarantee design D5 describes, so this runs the same queries over a
// deliberately non-UTC session. Both strategies must still agree, and both must
// still say Z.
func TestDateTimeDoesNotDependOnTheSessionTimeZone(t *testing.T) {
	w := tzWorld.build(t)
	ctx := t.Context()

	cfg, err := pgxpool.ParseConfig(w.dsn)
	require.NoError(t, err)
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["TimeZone"] = "Europe/Berlin"

	tilted, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err, "open a session in a non-UTC time zone")
	t.Cleanup(tilted.Close)

	var rendered string
	require.NoError(t,
		tilted.QueryRow(ctx, `SELECT to_json(at)::text FROM readings WHERE label = 'root'`).Scan(&rendered))
	require.Contains(t, rendered, "+02:00",
		"this session must render timestamptz with a non-UTC offset, or the test proves nothing")

	const query = `{ readings(label: $l) { label at } }`
	vars := map[string]any{"l": "root"}
	sc := scenario{name: "tz", query: query, vars: vars}
	shifted := &built{pool: tilted, doc: w.doc, dsn: w.dsn}

	goSide := runStrategy(ctx, t, shifted, sc, compiler.GoSide)
	sqlSide := runStrategy(ctx, t, shifted, sc, compiler.SQLSide)

	assert.Equal(t, string(goSide.encoded), string(sqlSide.encoded),
		"the two strategies must agree whatever the session TimeZone is")
	assert.Contains(t, string(sqlSide.encoded), `"at":"2026-07-30T12:00:00Z"`,
		"a timestamp normalises to UTC, so the response does not carry the session's offset")
}
