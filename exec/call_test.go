package exec

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/sdl"
)

// fakeHandle extends the package's fake database with the Exec half of Handle,
// so the two paths Call chooses between can both be driven without a container.
// The real thing is proved by the integration suites (SPEC.md §10); what is
// asserted here is which path a declaration selects and what an error becomes.
type fakeHandle struct {
	fakeDB

	tag     pgconn.CommandTag
	execErr error

	execSQL   string
	execArgs  []any
	execCalls int
}

func (h *fakeHandle) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	h.execCalls++
	h.execSQL = sql
	h.execArgs = args
	return h.tag, h.execErr
}

func scalarCall() *compiler.CompiledCall {
	return &compiler.CompiledCall{
		SQL:      "SELECT dbos.enqueue_workflow(agent_digest => $1)",
		Args:     []any{"sha256:abc"},
		Returns:  sdl.ReturnScalar,
		Schema:   "dbos",
		Function: "enqueue_workflow",
	}
}

func voidCall() *compiler.CompiledCall {
	return &compiler.CompiledCall{
		SQL:      "SELECT dbos.send_message(destination => $1)",
		Args:     []any{"wf-1"},
		Returns:  sdl.ReturnVoid,
		Schema:   "dbos",
		Function: "send_message",
	}
}

func TestCallScalarReadsOneRowOneColumn(t *testing.T) {
	h := &fakeHandle{fakeDB: fakeDB{rows: &fakeRows{
		cols:   []string{"enqueue_workflow"},
		values: [][]any{{"wf-42"}},
	}}}

	got, err := Call(context.Background(), h, scalarCall())
	require.NoError(t, err)
	assert.Equal(t, "wf-42", got)
	assert.Equal(t, 1, h.calls, "a scalar function is read, so it goes through Query")
	assert.Zero(t, h.execCalls)
	assert.Equal(t, []any{"sha256:abc"}, h.gotArgs)
}

func TestCallVoidExecutesAndReportsSuccess(t *testing.T) {
	h := &fakeHandle{}

	got, err := Call(context.Background(), h, voidCall())
	require.NoError(t, err)
	assert.Equal(t, true, got,
		"a declared VOID return has no value to map, so success itself is the result")
	assert.Equal(t, 1, h.execCalls, "a void function has no result set, so it goes through Exec")
	assert.Zero(t, h.calls)
	assert.Equal(t, []any{"wf-1"}, h.execArgs)
}

// TestCallSurfacesSQLSTATE is the assertion behind "errors raised by the
// function surface as GraphQL errors with the SQLSTATE": gopgql builds no
// GraphQL envelope, so what it owes the consumer is the code, as data, reachable
// structurally. A regression that dropped the code — or that let the wrapping
// swallow *pgconn.PgError — fails here.
func TestCallSurfacesSQLSTATE(t *testing.T) {
	raised := &pgconn.PgError{
		Code:           "P0001",
		Message:        "stream is closed",
		Detail:         "stream_id=s-1",
		Hint:           "open it first",
		ConstraintName: "streams_open_check",
	}

	for _, tc := range []struct {
		name string
		call *compiler.CompiledCall
		h    *fakeHandle
	}{
		{"scalar", scalarCall(), &fakeHandle{fakeDB: fakeDB{err: raised}}},
		{"void", voidCall(), &fakeHandle{execErr: raised}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Call(context.Background(), tc.h, tc.call)
			require.Error(t, err)

			var fnErr *FunctionError
			require.ErrorAs(t, err, &fnErr,
				"a raised exception must arrive as a *FunctionError, not as prose")
			assert.Equal(t, "P0001", fnErr.SQLSTATE)
			assert.Equal(t, "stream is closed", fnErr.Message)
			assert.Equal(t, "stream_id=s-1", fnErr.Detail)
			assert.Equal(t, "open it first", fnErr.Hint)
			assert.Equal(t, "streams_open_check", fnErr.Constraint)
			assert.Equal(t, tc.call.Schema, fnErr.Schema)
			assert.Equal(t, tc.call.Function, fnErr.Function)
			assert.Contains(t, fnErr.Error(), "P0001")

			var pgErr *pgconn.PgError
			assert.ErrorAs(t, err, &pgErr, "the driver error stays reachable underneath")
		})
	}
}

// A failure that did not come from the server carries no SQLSTATE, and
// inventing one would make the field untrustworthy exactly where a caller
// branches on it.
func TestCallNonServerErrorIsNotAFunctionError(t *testing.T) {
	h := &fakeHandle{fakeDB: fakeDB{err: errors.New("connection reset")}}

	_, err := Call(context.Background(), h, scalarCall())
	require.Error(t, err)
	var fnErr *FunctionError
	assert.NotErrorAs(t, err, &fnErr)
	assert.Contains(t, err.Error(), "dbos.enqueue_workflow")
}

func TestCallScalarRejectsWrongShape(t *testing.T) {
	t.Run("no rows", func(t *testing.T) {
		h := &fakeHandle{fakeDB: fakeDB{rows: &fakeRows{cols: []string{"f"}}}}
		_, err := Call(context.Background(), h, scalarCall())
		require.ErrorContains(t, err, "returned 0 rows")
	})
	t.Run("several rows", func(t *testing.T) {
		h := &fakeHandle{fakeDB: fakeDB{rows: &fakeRows{
			cols: []string{"f"}, values: [][]any{{"a"}, {"b"}},
		}}}
		_, err := Call(context.Background(), h, scalarCall())
		require.ErrorContains(t, err, "returned 2 rows")
	})
	t.Run("several columns", func(t *testing.T) {
		h := &fakeHandle{fakeDB: fakeDB{rows: &fakeRows{
			cols: []string{"a", "b"}, values: [][]any{{"x", "y"}},
		}}}
		_, err := Call(context.Background(), h, scalarCall())
		require.ErrorContains(t, err, "returned 2 columns")
	})
}

func TestCallRefusesNilHandle(t *testing.T) {
	_, err := Call(context.Background(), nil, scalarCall())
	require.ErrorContains(t, err, "no handle supplied")
}

// The three pgx types a caller can realistically hold satisfy Handle. The
// package asserts this at compile time; the test states the guarantee so that a
// reader looking for it finds it in the tests too.
func TestPgxTypesSatisfyHandle(*testing.T) {
	var _ Handle = (pgx.Tx)(nil)
	var h Handle = &fakeHandle{}
	var _ Querier = h
}
