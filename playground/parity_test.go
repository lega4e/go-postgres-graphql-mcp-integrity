package playground_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/playground"
)

// The Shaping tab runs one query under both strategies against one database and
// reports whether the responses agree. test/parity proves that against a real
// postgres:19beta2; these tests cover the leg that runs in the browser — turning
// the two result sets PostgreSQL returned into two responses, and comparing
// them — on results the browser did not produce.
//
// The two result sets here are written by hand *as PostgreSQL would return
// them*, which is the point: the Go-side one is flat and fanned out, the
// SQL-side one is a single text column of json_build_object's own rendering,
// spaces around the colons and all. If the comparison were string equality on
// what the database sent, these would not match. It is not: both are decoded
// into the same Go value and re-encoded by shape.Encode (design D3), and that
// is what the assertion below is really pinning.

// shapingVars binds ExampleShapingQuery's variable.
var shapingVars = map[string]any{"n": "Alice"}

// compileShaping compiles ExampleShapingQuery under one strategy and returns the
// projection, its root, and the two child selections — follows and followedBy,
// in selection order.
func compileShaping(t *testing.T, sqlSide bool) (*compiler.Selection, *compiler.Selection, *compiler.Selection) {
	t.Helper()
	out, err := playground.CompileWithShaping(
		playground.ExampleSDL, playground.ExampleShapingQuery, shapingVars, sqlSide)
	require.NoError(t, err, "CompileWithShaping")

	root := out.Projection.Root
	require.NotNil(t, root, "the compiled query carries a projection")
	require.Len(t, root.Children, 2, "follows and followedBy")
	return root, root.Children[0], root.Children[1]
}

// goSideResult is the flat result set the Go-side statement returns for
// ExampleShapingSeed: Alice follows Bob and Carol, Dave and Erin follow Alice,
// so the LEFT JOIN of the two branches is 2×2 = 4 rows for one person.
//
// The column names come out of the projection rather than being written down.
// They are the compiler's private naming scheme, and hardcoding them would test
// the scheme instead of the thing that matters — that the names shaping looks
// rows up by are the names the compiler projected them as.
func goSideResult(root, follows, followedBy *compiler.Selection) playground.Result {
	columns := []string{
		root.KeyColumns[0], root.Fields[0].Column,
		follows.KeyColumns[0], follows.Fields[0].Column,
		followedBy.KeyColumns[0], followedBy.Fields[0].Column,
	}
	rows := [][]any{}
	for _, f := range []struct{ key, name string }{{"k-bob", "Bob"}, {"k-carol", "Carol"}} {
		for _, b := range []struct{ key, name string }{{"k-dave", "Dave"}, {"k-erin", "Erin"}} {
			rows = append(rows, []any{"k-alice", "Alice", f.key, f.name, b.key, b.name})
		}
	}
	return playground.Result{Columns: columns, Rows: rows}
}

// sqlSideResult is the same data as the SQL-side statement returns it: one row
// of one `response` column, holding the whole nested response as text.
//
// The spacing is json_build_object's, not encoding/json's — `{"k" : v}`, keys in
// argument order. It is written that way deliberately, so a comparison that
// happened to be string equality on the database's bytes would fail here.
func sqlSideResult() playground.Result {
	return playground.Result{
		Columns: []string{"response"},
		Rows: [][]any{{
			`{"persons" : [{"name" : "Alice", ` +
				`"follows" : [{"name" : "Bob"}, {"name" : "Carol"}], ` +
				`"followedBy" : [{"name" : "Dave"}, {"name" : "Erin"}]}]}`,
		}},
	}
}

// TestShapeParityAgrees is M8's claim as the playground runs it: two result
// sets of different shapes, one response.
func TestShapeParityAgrees(t *testing.T) {
	root, follows, followedBy := compileShaping(t, false)

	out, err := playground.ShapeParity(
		playground.ExampleSDL, playground.ExampleShapingQuery, shapingVars,
		goSideResult(root, follows, followedBy), sqlSideResult())
	require.NoError(t, err, "ShapeParity")

	assert.True(t, out.Identical,
		"the two strategies shape into the same response\nGo-side:\n%s\nSQL-side:\n%s",
		out.GoJSON, out.SQLJSON)
	assert.Equal(t, out.GoJSON, out.SQLJSON,
		"and the rendered forms agree too, since both render one value")
	assert.Positive(t, out.Bytes, "the canonical encoding has a length")
	assert.JSONEq(t,
		`{"persons":[{"name":"Alice",`+
			`"follows":[{"name":"Bob"},{"name":"Carol"}],`+
			`"followedBy":[{"name":"Dave"},{"name":"Erin"}]}]}`,
		out.GoJSON,
		"four flat rows collapse back to one person with two children on each branch")
}

// TestShapeParityReportsDisagreement pins that Identical is a fact being
// checked and not a constant. A panel that said "identical" whatever it was
// given would prove nothing, which is the failure mode a claim like this has.
func TestShapeParityReportsDisagreement(t *testing.T) {
	root, follows, followedBy := compileShaping(t, false)

	wrong := sqlSideResult()
	wrong.Rows[0][0] = `{"persons" : [{"name" : "Alice", ` +
		`"follows" : [{"name" : "Bob"}], ` +
		`"followedBy" : [{"name" : "Dave"}, {"name" : "Erin"}]}]}`

	out, err := playground.ShapeParity(
		playground.ExampleSDL, playground.ExampleShapingQuery, shapingVars,
		goSideResult(root, follows, followedBy), wrong)
	require.NoError(t, err, "ShapeParity: a disagreement is a result, not an error")

	assert.False(t, out.Identical, "a missing child is a different response")
	assert.NotEqual(t, out.GoJSON, out.SQLJSON)
}

// TestShapeSQLSideRejectsAWrongResultShape covers the guard that keeps the
// SQL-side leg from quietly reinterpreting a result that did not come from an
// SQL-side statement. Guessing which value in it was the response is exactly
// the silent divergence the milestone rules out.
func TestShapeSQLSideRejectsAWrongResultShape(t *testing.T) {
	out, err := playground.CompileWithShaping(
		playground.ExampleSDL, playground.ExampleShapingQuery, shapingVars, true)
	require.NoError(t, err, "CompileWithShaping")

	tests := map[string]struct {
		res  playground.Result
		want string
	}{
		"two columns": {
			res:  playground.Result{Columns: []string{"a", "b"}, Rows: [][]any{{"{}", "{}"}}},
			want: "one column, got 2",
		},
		"no rows": {
			res:  playground.Result{Columns: []string{"response"}, Rows: [][]any{}},
			want: "one row, got 0",
		},
		"two rows": {
			res:  playground.Result{Columns: []string{"response"}, Rows: [][]any{{"{}"}, {"{}"}}},
			want: "one row, got 2",
		},
		"not text": {
			res:  playground.Result{Columns: []string{"response"}, Rows: [][]any{{42.0}}},
			want: "not the text the compiler casts it to",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := playground.ShapeSQLSide(out.Projection, tc.res)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestCompileWithShapingCarriesArgs pins the reason the shaping surface stopped
// returning SQL alone: the page executes both statements, and a statement whose
// bind values were dropped would be refused by PostgreSQL rather than run.
func TestCompileWithShapingCarriesArgs(t *testing.T) {
	for _, sqlSide := range []bool{false, true} {
		out, err := playground.CompileWithShaping(
			playground.ExampleSDL, playground.ExampleShapingQuery, shapingVars, sqlSide)
		require.NoError(t, err, "CompileWithShaping(sqlSide=%v)", sqlSide)

		assert.Equal(t, []any{"Alice"}, out.Args,
			"the bind values are the same under both strategies")
		assert.NotNil(t, out.Projection.Root,
			"and so is the projection, which is what makes the two comparable")
	}
}
