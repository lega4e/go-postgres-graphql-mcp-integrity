package exec

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/compiler"
)

// A statement that fails after it was prepared must reach the caller with the
// database's own message, not with gopgql's account of the columns.
//
// This is the shape pgx delivers a PL/pgSQL RAISE in: Query returns a cursor and
// no error, the cursor has no field descriptions because no result set was ever
// produced, and the failure — the SQLSTATE the caller acts on (SPEC.md §7 → M11)
// — arrives at Next/Err. Every check gopgql makes *before* iterating therefore
// sees an empty column list, and a check that treats empty as a disagreement
// swallows the exception and reports a gopgql-internal drift instead.
//
// It is asserted here as well as in test/m11, test/m14 and test/mcp because
// those need a container and this does not: the regression was introduced by the
// portable-handle rewrite (gopgql#53) and caught only by the integration suites,
// which is three container boots too late.
func TestADeferredStatementErrorIsNotMaskedByTheColumnCheck(t *testing.T) {
	raised := errors.New(`ERROR: person does not exist (SQLSTATE P0001)`)

	// Both read paths, because the compiler picks between them and the caller
	// does not: Go-side shaping reads a flat result set keyed by column name,
	// SQL-side shaping reads one text column holding the whole response.
	t.Run("go-side", func(t *testing.T) {
		db := &fakeDB{rows: &fakeRows{err: raised}}
		_, err := Query(context.Background(), PgxQuerier(db), &compiler.Compiled{
			SQL:     "SELECT v0_k, v0_c0 FROM ...",
			Columns: []string{"v0_k", "v0_c0"},
		})
		require.ErrorIs(t, err, raised,
			"a statement that failed after prepare surfaces the database's error")
		assert.NotContains(t, err.Error(), "drifted apart",
			"and never a column disagreement: there were no columns because there was no result set")
	})

	t.Run("sql-side", func(t *testing.T) {
		db := &fakeDB{rows: &fakeRows{err: raised}}
		_, err := Query(context.Background(), PgxQuerier(db), &compiler.Compiled{
			SQL:     "SELECT json_build_object(...) AS response FROM ...",
			Columns: []string{"response"},
			Shaping: compiler.SQLSide,
		})
		require.ErrorIs(t, err, raised)
		assert.NotContains(t, err.Error(), "returns one column",
			"an empty column list is a failed statement, not the wrong shape")
	})
}

// The @function path has the same shape check and the same trap, and it is the
// one gopgql#53's regression was caught by: test/m14 asserts that a mutation's
// raised exception reaches the generated client as a *FunctionError carrying its
// SQLSTATE, and a shape complaint about "0 columns" is prose that satisfies
// neither.
//
// The existing coverage in call_test.go raises at *prepare* time, where Query
// returns the error directly. This is the other half — a function that raises
// while executing, which is what PL/pgSQL RAISE actually does.
func TestADeferredFunctionErrorStillArrivesTyped(t *testing.T) {
	raised := &pgconn.PgError{Severity: "ERROR", Code: "P0001", Message: "stream is closed"}
	h := &fakeHandle{fakeDB: fakeDB{rows: &fakeRows{err: raised}}}

	_, err := Call(context.Background(), Pgx(h), scalarCall())
	require.Error(t, err)

	var fnErr *FunctionError
	require.ErrorAs(t, err, &fnErr,
		"a raised exception must arrive typed however late the driver reports it")
	assert.Equal(t, "P0001", fnErr.SQLSTATE)
	assert.NotContains(t, err.Error(), "returned 0 columns",
		"an empty column list is the raise, not the wrong return shape")
}

// The check the fix relaxes still has to fire where it means something: a result
// set that really does disagree with the SELECT list the statement was emitted
// with is a compiler bug, and reading rows keyed by the wrong names would be
// silently empty objects rather than a failure.
func TestARealColumnDisagreementIsStillReported(t *testing.T) {
	db := &fakeDB{rows: &fakeRows{
		cols:   []string{"a", "b"},
		values: [][]any{{int64(1), "Ada"}},
	}}
	_, err := Query(context.Background(), PgxQuerier(db), &compiler.Compiled{
		SQL:     "SELECT a, b FROM ...",
		Columns: []string{"v0_k", "v0_c0"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drifted apart")
}

// An SQL-side statement whose cursor reports more than one column is still the
// shape complaint it was: only *no* columns changed meaning.
func TestSQLSideStillRefusesMoreThanOneColumn(t *testing.T) {
	db := &fakeDB{rows: &fakeRows{
		cols:   []string{"response", "extra"},
		values: [][]any{{"{}", "x"}},
	}}
	_, err := Query(context.Background(), PgxQuerier(db), &compiler.Compiled{
		SQL:     "SELECT ... FROM ...",
		Columns: []string{"response"},
		Shaping: compiler.SQLSide,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "returns one column, got 2")
}
