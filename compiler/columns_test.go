package compiler_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/sdl"
)

// selectList reads the output column names back out of an emitted statement, in
// SELECT order.
//
// It reads the *statement*, which is the point: Compiled.Columns is only worth
// anything if it is what the SQL actually returns, and the one place that is
// recorded independently of the field being tested is the text of the SELECT.
// Each item is either `col` or `expr::type AS col`, so the name is the last
// whitespace-separated token.
func selectList(t *testing.T, sql string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(sql, "SELECT ")
	require.True(t, ok, "the statement has a SELECT list")
	list, _, ok := strings.Cut(rest, "\nFROM ")
	require.True(t, ok, "the SELECT list ends at FROM")

	var out []string
	for _, item := range strings.Split(list, ", ") {
		fields := strings.Fields(item)
		require.NotEmpty(t, fields)
		name := fields[len(fields)-1]
		// A multi-fragment statement qualifies each column with its fragment.
		if _, after, qualified := strings.Cut(name, "."); qualified {
			name = after
		}
		out = append(out, name)
	}
	return out
}

// TestCompiledColumnsAreTheSelectList is the invariant the portable read path
// rests on.
//
// A driver-agnostic cursor — DBOS's sysdb.Rows, database/sql's — reports
// Next/Scan/Err/Close and nothing that names a column, so reading a result set
// through one means knowing the names in advance. Compiled.Columns is where they
// come from, and it is trustworthy only for as long as it is the SELECT list. If
// the two ever drift, every row read through a portable handle is keyed by the
// wrong names, silently: the shaper would find none of the columns it projected
// and return empty objects rather than fail.
//
// The branching case is the one that matters. A projection walk would produce a
// different order there — a branch is compiled into its own fragment and every
// fragment's columns are emitted together, so the walk interleaves where the
// SELECT list does not — which is why the list is recorded by the renderer
// rather than re-derived from the projection.
func TestCompiledColumnsAreTheSelectList(t *testing.T) {
	cases := map[string]struct {
		compiler *compiler.Compiler
		op       string
	}{
		"one level":               {newCompiler(t), `{ persons { id name email } }`},
		"one nested relationship": {newCompiler(t), `{ persons { name follows { name } } }`},
		"two relationships, so the pattern splits across fragments": {
			newCompiler(t), `{ persons { name follows { name } followedBy { name } } }`},
		"an interface level": {newInterfaceCompiler(t), `{ actors { id name } }`},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cq, err := tc.compiler.CompileQuery(tc.op, nil)
			require.NoError(t, err)

			require.NotEmpty(t, cq.Columns, "every compiled query records its output columns")
			assert.Equal(t, selectList(t, cq.SQL), cq.Columns,
				"Compiled.Columns must be the statement's SELECT list, in order; "+
					"a portable handle keys every row by it and cannot check it")
		})
	}
}

// Every column the projection refers to has to be in the recorded list, or a row
// read through a portable handle would be missing exactly the values the shaper
// looks up. The reverse does not hold: a branch's join keys are projected for the
// join and deliberately not exposed to the projection.
func TestEveryProjectedColumnIsRecorded(t *testing.T) {
	cq, err := newCompiler(t).CompileQuery(
		`{ persons { name follows { name } followedBy { name } } }`, nil)
	require.NoError(t, err)

	recorded := map[string]bool{}
	for _, c := range cq.Columns {
		recorded[c] = true
	}

	var walk func(*compiler.Selection)
	walk = func(s *compiler.Selection) {
		for _, k := range s.KeyColumns {
			assert.True(t, recorded[k], "key column %q is projected but not recorded", k)
		}
		for _, f := range s.Fields {
			assert.True(t, recorded[f.Column], "field column %q is projected but not recorded", f.Column)
		}
		for _, child := range s.Children {
			walk(child)
		}
	}
	walk(cq.Projection.Root)
}

// The SQL-side strategy returns one column holding the whole response, and it is
// recorded too — so the two strategies differ in what the list says and not in
// whether there is one.
func TestSQLSideRecordsItsSingleColumn(t *testing.T) {
	doc, err := sdl.Parse(exampleSDL)
	require.NoError(t, err)
	c := compiler.New(doc, compiler.WithShaping(compiler.SQLSide))

	cq, err := c.CompileQuery(`{ persons { id name } }`, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"response"}, cq.Columns)
}
