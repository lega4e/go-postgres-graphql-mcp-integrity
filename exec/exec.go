// Package exec runs a compiled query against PostgreSQL and shapes the result.
//
// A query in gopgql is three steps: compile (compiler.CompileQuery) → execute
// the SQL with its ordered bind parameters → regroup the flat rows into the
// nested response (shape.Rows). The middle two are the same everywhere, and
// were written out by hand in every integration suite; SPEC.md §4.1 reserves
// this package for them.
//
// exec is also the seam where a different shaping strategy can be selected: the
// Go-side shaper is the only one today, and an SQL-side json_agg strategy is
// benchmarked against it in M8 (SPEC.md §3, decision 4).
//
// # Whose connection
//
// gopgql opens exactly one kind of pool and it is read-only ([OpenReadOnly]).
// Everything that could write runs through a [Handle] the *caller* owns and
// hands in — a pool, a connection, or a transaction it opened itself. That is
// deliberate and it is what keeps `@function` mutations (SPEC.md §7 → M11) from
// widening gopgql into something that holds a writable connection: a call is
// executable only through a handle somebody else is already responsible for.
//
// It is also the reason a caller can get exactly-once semantics out of a
// generated operation. A transaction the caller opened commits its own
// bookkeeping in the same transaction as the work; an operation that opened a
// connection of its own could not participate in that commit.
package exec

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"github.com/lega4e/goga/database"
	"github.com/lega4e/goga/database/pgxdb"
	"github.com/lega4e/goga/telemetry"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/shape"
)

// module is the goga module name this package's telemetry is attributed to; it
// is the value of the goga.module attribute on every span opened below.
const module = "exec"

// instr is the instrumentation handle. It is taken at package level, which is
// long before the process has configured telemetry: the handle resolves through
// OpenTelemetry's globals on every use, so it starts emitting the moment the
// composition root installs them and needs no wiring to get there.
var instr = telemetry.For(module)

// The attribute keys this package records. They are constants rather than
// literals at the call site so that one key cannot drift into two spellings.
const (
	// attrShaping is the strategy the query was compiled under — the single
	// fact that decides whether the nesting happened in Go or in PostgreSQL.
	attrShaping = attribute.Key("gopgql.shaping")
	// attrRows is the number of rows the statement returned.
	attrRows = attribute.Key("gopgql.rows")
)

// Query executes a compiled query and returns the nested GraphQL response.
//
// The compiled SQL carries placeholders for every argument; values travel as
// bind parameters and are never interpolated into the statement.
//
// It dispatches on the strategy the query was compiled under (SPEC.md §7 → M8):
// a Go-side query returns one flat column per projected field and is regrouped
// here, an SQL-side query returns a single `response` column PostgreSQL has
// already assembled and is decoded. The signature is the same either way, so
// every caller — the integration suites, mcp — keeps working and inherits
// whichever strategy its compiler was configured with (design D1).
//
// The result parameters are named because the deferred closer observes the
// error *variable*: with unnamed results a `return nil, err` would leave the
// local at nil and the span would record success on every failure.
func Query(ctx context.Context, db Querier, cq *compiler.Compiled) (_ map[string]any, err error) {
	// Before the span: a nil compiled query has no strategy to attribute an
	// operation to, and there is no operation — the call never reaches a
	// database.
	if cq == nil {
		return nil, fmt.Errorf("exec: nil compiled query")
	}

	ctx, end := instr.Start(ctx, "Query", attrShaping.String(cq.Shaping.String()))
	defer func() { end(err) }()

	// Every path below assigns err rather than returning an expression
	// directly, for the reason the comment above gives.
	if cq.Shaping == compiler.SQLSide {
		var shaped map[string]any
		shaped, err = queryShaped(ctx, db, cq)
		return shaped, err
	}
	var flat []map[string]any
	flat, err = rowsOf(ctx, db, cq.SQL, cq.Columns, cq.Args...)
	if err != nil {
		return nil, err
	}
	response, err := shape.Rows(cq.Projection, flat)
	return response, err
}

// queryShaped runs an SQL-side-shaped query, whose result is one row of one text
// column holding the whole response.
//
// The column is read as a string and never through the driver's JSON codec: a
// generic JSON decode turns `19.90` into float64 19.9, which would lose on this
// path a digit the Go-side path keeps.
func queryShaped(ctx context.Context, db Querier, cq *compiler.Compiled) (map[string]any, error) {
	rows, err := db.Query(ctx, cq.SQL, cq.Args...)
	if err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	defer closeCursor(rows)

	// The column count is checked only where the cursor can report it. A
	// portable cursor cannot, and the statement's shape is the compiler's rather
	// than the caller's: an SQL-side render emits one column by construction.
	//
	// No columns at all is not a shape complaint but a statement that never
	// produced a result set — see [noResultSet]. The Next/Err below reports why.
	if named, ok := rows.(NamedCursor); ok {
		if cols := named.Columns(); len(cols) > 1 {
			return nil, fmt.Errorf("exec: an SQL-side shaped query returns one column, got %d", len(cols))
		}
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("exec: %w", err)
		}
		return nil, fmt.Errorf("exec: an SQL-side shaped query returns one row, got none")
	}

	var response string
	if err := rows.Scan(&response); err != nil {
		return nil, fmt.Errorf("exec: read the shaped response: %w", err)
	}
	if rows.Next() {
		return nil, fmt.Errorf("exec: an SQL-side shaped query returns one row, got more than one")
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	return shape.Decode(cq.Projection, response)
}

// Rows executes a statement and returns its rows as column-name maps, the flat
// form the shaper consumes. It is exported for callers that want the rows
// themselves — a hand-written statement in a test, or a future SQL-side shaper
// whose result is already nested and must not go through a projection.
//
// The statement is a raw one, so nothing has recorded its output columns: the
// handle has to be able to name them, which the pgx adapter can and a portable
// cursor cannot. Use [Query] with a compiled query to read through a portable
// handle — a compiled query carries its own column list.
//
// The statement text is deliberately not an attribute: it is unbounded, and its
// literals are the caller's data. The row count is, because "how much came
// back" is the question a slow read raises.
func Rows(ctx context.Context, db Querier, sql string, args ...any) (rows []map[string]any, err error) {
	ctx, end := instr.Start(ctx, "Rows")
	defer func() { end(err) }()

	rows, err = rowsOf(ctx, db, sql, nil, args...)
	if err != nil {
		return nil, err
	}
	trace.SpanFromContext(ctx).SetAttributes(attrRows.Int(len(rows)))
	return rows, nil
}

// rowsOf runs a statement and keys each row by its output column names, taking
// them from the cursor when it can say and from the compiled statement's
// recorded SELECT list otherwise.
//
// Where both can say, they are compared. The cursor's names are the database's
// own and win a disagreement, but a disagreement is a bug — the recorded list is
// the SELECT list that produced the result set — so it is reported rather than
// resolved. That is the check that keeps Compiled.Columns honest on the path
// where it is not needed, so it can be trusted on the path where it is.
func rowsOf(ctx context.Context, db Querier, sql string, recorded []string, args ...any) ([]map[string]any, error) {
	if db == nil {
		return nil, fmt.Errorf("exec: nil database handle")
	}
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	defer closeCursor(rows)

	cols, err := columnsOf(rows, recorded)
	if err != nil {
		return nil, err
	}
	return scan(rows, cols)
}

// columnsOf settles which names a flat row is keyed by.
func columnsOf(rows Cursor, recorded []string) ([]string, error) {
	named, ok := rows.(NamedCursor)
	if !ok {
		if len(recorded) == 0 {
			return nil, fmt.Errorf("exec: this handle's cursor cannot name its output columns and the " +
				"statement does not record them; run a compiled query through Query (its columns travel " +
				"with it), or adapt a pgx handle with exec.Pgx")
		}
		return recorded, nil
	}
	cols := named.Columns()
	if noResultSet(cols) {
		return recorded, nil
	}
	if len(recorded) > 0 && !sameNames(cols, recorded) {
		return nil, fmt.Errorf("exec: the result set has columns (%s) but the statement was emitted with "+
			"(%s); the compiled query and the statement it ran have drifted apart",
			strings.Join(cols, ", "), strings.Join(recorded, ", "))
	}
	return cols, nil
}

// noResultSet reports that a cursor which *can* name its columns named none, so
// there is no result set to describe and nothing to disagree with.
//
// It is the difference between "the statement returned the wrong columns" and
// "the statement failed", and the two must not be confused, because pgx reaches
// the second through the first: a statement whose failure happens at execution
// rather than at prepare — a PL/pgSQL RAISE and its SQLSTATE, a permission
// denial, a serialisation failure — returns a cursor without error and defers
// the failure to Next/Err. That cursor has no field descriptions.
//
// Reporting a column disagreement there would replace the database's own message
// with gopgql's, and the database's is the one a caller acts on: SPEC.md §7 → M11
// requires a raised exception to reach the caller carrying its SQLSTATE. So the
// columns fall back to the recorded list and the scan proceeds, where Err is read
// and the real error surfaces. There are no rows to mis-key: a statement that
// produced no result set produces no rows either.
func noResultSet(cols []string) bool { return len(cols) == 0 }

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// closeCursor releases a cursor in a defer. A Cursor's Close reports an error
// because database/sql's does; nothing here can act on it, and every read below
// has already reported through Err.
func closeCursor(rows Cursor) { _ = rows.Close() }

// PoolOption adjusts the pool [OpenReadOnly] builds — a connection limit in a
// long-lived process.
//
// It is an alias for goga/database/pgxdb's own option type rather than a
// func(*pgxpool.Config) of gopgql's, because the pool is now pgxdb's to build
// and pgxdb accepts no arbitrary hook into the configuration: the otelpgx
// tracer it installs is the whole of its guarantee that no uninstrumented pool
// leaves it, so nothing a caller passes may reach the configuration after it.
// A test that needs pgx's single Tracer field for itself — the MCP suite, which
// asserts on the SQL the tools emit — builds that one pool by hand from
// [ReadOnlyDSN], which is what pgxdb documents such a caller must do.
type PoolOption = pgxdb.Option

// readOnlyParam is the PostgreSQL connection parameter that makes every session
// on a pool start read-only. Sessions inherit it as a GUC, so a statement that
// tries to write is refused before it reaches a table.
const readOnlyParam = "default_transaction_read_only"

// ReadOnlyDSN returns dsn with [readOnlyParam] set to on.
//
// The parameter travels in the connection string rather than being written onto
// a parsed *pgxpool.Config, because the pool is built by
// [github.com/lega4e/goga/database/pgxdb.Open], which takes a DSN and
// exposes no hook into the configuration it parses out of it. pgx puts any
// setting it does not recognise into ConnConfig.RuntimeParams, so the two
// spellings reach the server identically.
//
// It is exported for the one caller that cannot use [OpenReadOnly] — a test
// needing pgx's Tracer field, which pgxdb owns — so that the belt below has a
// single definition rather than a second copy that could drift from it.
//
// A dsn already carrying the parameter has it overwritten, in both the URL and
// the keyword/value form: on is not negotiable. A malformed URL is returned
// unchanged, so that pgx reports it rather than this function turning a parse
// error into a silently different connection string.
func ReadOnlyDSN(dsn string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn
		}
		q := u.Query()
		q.Set(readOnlyParam, "on")
		u.RawQuery = q.Encode()
		return u.String()
	}
	// Keyword/value form. pgx folds the pairs into a map left to right, so an
	// appended pair overrides an earlier one; TrimSpace covers the empty dsn
	// pgx accepts as "take everything from the environment".
	return strings.TrimSpace(dsn + " " + readOnlyParam + "=on")
}

// OpenReadOnly opens a pool whose every session starts with
// default_transaction_read_only=on, and pings it.
//
// This is the second belt (SPEC.md §3, design D4): a statement that tried to
// write through this pool is refused by the database itself. It is also why a
// read-only database role is recommended rather than required.
//
// The first belt used to be stated as "the compiler emits nothing but a SELECT
// over GRAPH_TABLE, so there is no write path to begin with". That reason is no
// longer true: a `@function` mutation compiles to a plain function call, which
// can write. What is still true — and is what the belt now rests on — is that
// gopgql never opens a pool that could run it. This is the only pool gopgql
// opens, and it is this one; a call goes through a [Handle] the caller supplies.
// A `@function` attempted on a pool from here fails with SQLSTATE 25006, which
// is the belt doing its job rather than a bug.
//
// The pool itself is pgx's own *pgxpool.Pool, built by
// [github.com/lega4e/goga/database/pgxdb.Open]. Nothing about what flows
// through gopgql changes — pgxdb deliberately has no portable handle and
// nothing to unwrap — but the pool arrives with otelpgx's tracer and pgx's pool
// statistics already on it, so every statement run through it is a span and the
// pool's own gauges are metrics.
//
// The ping means an unreachable database is reported when the process starts,
// not on every call it would otherwise fail. It is also the only thing that
// does: pgxdb.Open validates the configuration and connects lazily.
//
// The open is instrumented because it is the one place gopgql talks to a
// database before any query does: a span here separates "the database was
// unreachable at startup" from "a query failed", which is otherwise the same
// connection error seen twice. It is also what makes the ping's own statement
// visible, because otelpgx traces a query only inside a recording span.
func OpenReadOnly(ctx context.Context, dsn string, opts ...PoolOption) (_ *pgxpool.Pool, err error) {
	ctx, end := instr.Start(ctx, "OpenReadOnly")
	defer func() { end(err) }()

	pool, err := pgxdb.Open(ctx, database.DSN(ReadOnlyDSN(dsn)), opts...)
	if err != nil {
		return nil, fmt.Errorf("exec: open pool: %w", err)
	}
	// Assigned to the named result rather than shadowed with `if err := …`: a
	// shadow here would leave the deferred closer looking at a nil err and
	// recording an unreachable database as a successful open.
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("exec: connect to the database: %w", err)
	}
	return pool, nil
}

// scan drains a result set into one map per row, keyed by output column name.
//
// Each value is scanned into an `any`, which is the one destination every driver
// agrees on: pgx decodes the column to the Go type its OID maps to — the same
// value pgx.Rows.Values would have produced — and database/sql yields its own
// driver value. Scanning positionally is also what makes the read portable,
// because a portable cursor offers Scan and nothing else.
func scan(rows Cursor, cols []string) ([]map[string]any, error) {
	vals := make([]any, len(cols))
	dest := make([]any, len(cols))
	for i := range vals {
		dest[i] = &vals[i]
	}

	var out []map[string]any
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("exec: read row: %w", err)
		}
		row := make(map[string]any, len(cols))
		for i, name := range cols {
			row[name] = jsonValue(vals[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	return out, nil
}

// jsonValue renders a scanned value as the shape a caller expects to see.
//
// Every gopgql node has a surrogate `id uuid` key (SPEC.md §5.1), and pgx
// decodes uuid to a [16]byte array. Left alone that marshals to a JSON array of
// sixteen numbers — so `id` comes back as [80,0,0,…] instead of the identifier
// the caller filtered on, and cannot be fed back into a query. Rendering it in
// the canonical 8-4-4-4-12 text form makes the value round-trip.
//
// Only the fixed-size array is converted: a bytea column decodes to a []byte
// slice, which is a different Go type and is left as it is.
func jsonValue(v any) any {
	if u, ok := v.([16]byte); ok {
		return uuidString(u)
	}
	return v
}

// uuidString formats a raw uuid in the canonical hyphenated text form.
func uuidString(u [16]byte) string {
	var b [36]byte
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], u[10:16])
	return string(b[:])
}
