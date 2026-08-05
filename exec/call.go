package exec

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/sdl"
)

// FunctionError is an error raised by a called PL/pgSQL function, carrying the
// SQLSTATE that identifies it.
//
// The SQLSTATE is the whole point. `RAISE EXCEPTION … USING ERRCODE = 'P0001'`
// is how a function says *which* thing went wrong, and a consumer that had to
// tell one failure from another by matching on English text would break the
// first time somebody rewords a message.
//
// What this is not: a GraphQL error. gopgql builds no response envelope at all
// (SPEC.md §1.1) — it returns SQL and values, and the consumer that owns the
// GraphQL surface owns the envelope. So "errors surface as GraphQL errors
// carrying the SQLSTATE" is met here by carrying the SQLSTATE as *data*, in a
// typed error reachable through errors.As, that a consumer copies into its own
// `extensions`. Anything more would be gopgql deciding the shape of somebody
// else's API.
//
// It lives in exec rather than in compiler because it wraps a *pgconn.PgError,
// and pgconn is a database dependency: SPEC.md §4.1 keeps compiler on the WASM
// side, where there is no pgconn to wrap.
type FunctionError struct {
	// Schema and Function name the function that raised it.
	Schema   string
	Function string
	// SQLSTATE is the five-character error code (e.g. "P0001" for a bare RAISE
	// EXCEPTION, "25006" for a write attempted in a read-only transaction).
	SQLSTATE string
	// Message, Detail, Hint and Constraint are the server's own fields, carried
	// through unchanged.
	Message    string
	Detail     string
	Hint       string
	Constraint string

	// PgError is the underlying driver error, so errors.As reaches it too and a
	// caller that wants a field this struct does not name can still have it.
	PgError *pgconn.PgError
}

func (e *FunctionError) Error() string {
	return fmt.Sprintf("exec: %s.%s raised SQLSTATE %s: %s", e.Schema, e.Function, e.SQLSTATE, e.Message)
}

// Unwrap exposes the driver error, so errors.Is/As reach *pgconn.PgError
// through a *FunctionError.
func (e *FunctionError) Unwrap() error { return e.PgError }

// asFunctionError wraps a driver error raised by a call, or returns err
// unchanged when it did not come from the server (a cancelled context, a
// connection that went away). Only a server error carries a SQLSTATE, and
// inventing one for anything else would make the code untrustworthy exactly
// where a caller depends on it.
func asFunctionError(cc *compiler.CompiledCall, err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("exec: call %s.%s: %w", cc.Schema, cc.Function, err)
	}
	return &FunctionError{
		Schema:     cc.Schema,
		Function:   cc.Function,
		SQLSTATE:   pgErr.Code,
		Message:    pgErr.Message,
		Detail:     pgErr.Detail,
		Hint:       pgErr.Hint,
		Constraint: pgErr.ConstraintName,
		PgError:    pgErr,
	}
}

// Call runs a compiled mutation through a handle the caller owns and returns
// the function's result.
//
// The handle is the caller's on purpose: gopgql opens one pool and it is
// read-only, so a call is executable only through a connection somebody else is
// already responsible for — a transaction, most usefully, so that the call and
// whatever the caller commits alongside it either both happen or neither does.
//
// How the result is read follows the *declaration*, never the connection:
//
//   - SCALAR reads one row of one column. More than one row, or more than one
//     column, is an error naming what came back — not a panic and not a silent
//     first-value.
//   - VOID executes the statement and yields true on success. Nothing is read
//     back, because there is nothing to read; the command tag is the evidence.
//
// Nothing here inspects the function's type OIDs or asks the catalog what it
// returns. That is what keeps compilation pure, and it is what stops a
// successful void call from reporting false — which is exactly what sniffing a
// `Boolean!` result would do.
func Call(ctx context.Context, h Handle, cc *compiler.CompiledCall) (any, error) {
	if cc == nil {
		return nil, fmt.Errorf("exec: nil compiled call")
	}
	if h == nil {
		return nil, fmt.Errorf("exec: call %s.%s: no handle supplied; a mutation runs through a connection the "+
			"caller owns (gopgql opens only read-only pools)", cc.Schema, cc.Function)
	}

	if cc.Returns == sdl.ReturnVoid {
		if _, err := h.Exec(ctx, cc.SQL, cc.Args...); err != nil {
			return nil, asFunctionError(cc, err)
		}
		return true, nil
	}

	rows, err := h.Query(ctx, cc.SQL, cc.Args...)
	if err != nil {
		return nil, asFunctionError(cc, err)
	}
	defer rows.Close()

	flat, err := scan(rows)
	if err != nil {
		// scan's error is the driver's, so a function that raised while
		// streaming its result still arrives as a *FunctionError.
		return nil, asFunctionError(cc, err)
	}
	return scalarResult(cc, flat)
}

// scalarResult reduces a scalar-returning call's result set to its one value.
func scalarResult(cc *compiler.CompiledCall, flat []map[string]any) (any, error) {
	if len(flat) != 1 {
		return nil, fmt.Errorf("exec: %s.%s returned %d rows; a scalar function returns exactly one "+
			"(declare a set-returning function differently — gopgql cannot map one)",
			cc.Schema, cc.Function, len(flat))
	}
	row := flat[0]
	if len(row) != 1 {
		return nil, fmt.Errorf("exec: %s.%s returned %d columns; a scalar function returns exactly one",
			cc.Schema, cc.Function, len(row))
	}
	for _, v := range row {
		return v, nil
	}
	return nil, nil // unreachable: len(row) == 1
}
