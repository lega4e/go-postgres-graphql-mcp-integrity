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
package exec

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/shape"
)

// Querier is the subset of a pgx connection pool exec needs. *pgxpool.Pool,
// *pgx.Conn and pgx.Tx all satisfy it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

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
func Query(ctx context.Context, db Querier, cq *compiler.Compiled) (map[string]any, error) {
	if cq == nil {
		return nil, fmt.Errorf("exec: nil compiled query")
	}
	if cq.Shaping == compiler.SQLSide {
		return queryShaped(ctx, db, cq)
	}
	flat, err := Rows(ctx, db, cq.SQL, cq.Args...)
	if err != nil {
		return nil, err
	}
	return shape.Rows(cq.Projection, flat)
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
	defer rows.Close()

	if fds := rows.FieldDescriptions(); len(fds) != 1 {
		return nil, fmt.Errorf("exec: an SQL-side shaped query returns one column, got %d", len(fds))
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
func Rows(ctx context.Context, db Querier, sql string, args ...any) ([]map[string]any, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	defer rows.Close()
	return scan(rows)
}

// PoolOption adjusts the pool configuration OpenReadOnly builds — a tracer in a
// test, a connection limit in a long-lived process.
type PoolOption func(*pgxpool.Config)

// OpenReadOnly opens a pool whose every session starts with
// default_transaction_read_only=on, and pings it.
//
// The compiler emits nothing but a SELECT over GRAPH_TABLE, so there is no
// write path to begin with; this is the second belt (SPEC.md §3, design D4): a
// statement that somehow tried to write would be refused by the database
// itself. It is also why a read-only database role is recommended rather than
// required.
//
// The ping means an unreachable database is reported when the process starts,
// not on every call it would otherwise fail.
func OpenReadOnly(ctx context.Context, dsn string, opts ...PoolOption) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("exec: parse connection string: %w", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	for _, opt := range opts {
		opt(cfg)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("exec: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("exec: connect to the database: %w", err)
	}
	return pool, nil
}

// scan drains a result set into one map per row, keyed by output column name.
func scan(rows pgx.Rows) ([]map[string]any, error) {
	fds := rows.FieldDescriptions()
	var out []map[string]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("exec: read row: %w", err)
		}
		row := make(map[string]any, len(fds))
		for i, fd := range fds {
			if i < len(vals) {
				row[fd.Name] = jsonValue(vals[i])
			}
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
