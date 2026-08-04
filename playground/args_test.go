package playground_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/playground"
)

// Compiled.Args is what makes the playground able to *execute* a compiled
// query rather than only display it: Params has always said "$1 = Alice", but
// the values behind that sentence were dropped on the floor. These tests pin
// the three things a caller binding to a driver depends on — the order, the
// emptiness, and the absence of a partial result after a refusal.

// TestCompileArgsFollowPlaceholderOrder proves Args is positional: Args[0] is
// the value $1 stands for. The constraints example is the one with two
// variables, so it is the only one where an order can be wrong.
func TestCompileArgsFollowPlaceholderOrder(t *testing.T) {
	out, err := playground.Compile(
		playground.ExampleConstraintsSDL,
		playground.ExampleConstraintsQuery,
		map[string]any{"t": "acme", "e": "ada@acme.example"},
	)
	require.NoError(t, err, "Compile")

	// The SQL fixes the order — tenant is compared to $1, email to $2 — and
	// Args has to agree with it, not with the order the variables were written
	// in the document.
	assert.Contains(t, out.SQL, "v0.tenant = $1 AND v0.email = $2")
	assert.Equal(t, []any{"acme", "ada@acme.example"}, out.Args)

	// Params is the rendering of exactly these values, unchanged by this
	// change: the two fields are one fact and its display form.
	assert.Equal(t, "$1 = acme, $2 = ada@acme.example", out.Params)
}

// TestCompileArgsEmptyWithoutVariables covers a query that binds nothing: the
// caller gets an empty slice to pass through, and the reader keeps the sentence
// that was there before.
func TestCompileArgsEmptyWithoutVariables(t *testing.T) {
	out, err := playground.Compile(
		playground.ExampleInterfaceSDL, playground.ExampleInterfaceQuery, nil)
	require.NoError(t, err, "Compile")

	assert.NotContains(t, out.SQL, "$1", "the interface example binds nothing")
	assert.Empty(t, out.Args)
	assert.Equal(t, "(no bind parameters)", out.Params)
}

// TestCompileArgsNilOnEveryErrorPath is regression protection rather than a
// description of today's code: `return Compiled{}, err` already gives nil Args
// everywhere. It exists so that a later change populating a partial result on
// the way to an error has to fail this test first — a caller that reads Args
// after ignoring err would otherwise bind values for SQL that was never
// emitted.
func TestCompileArgsNilOnEveryErrorPath(t *testing.T) {
	t.Run("depth exceeded", func(t *testing.T) {
		out, err := playground.Compile(
			playground.ExampleSDL, playground.ExampleDeepQuery,
			map[string]any{"n": "Alice"})
		require.Error(t, err)
		_, isDepth := playground.DepthExceeded(err)
		require.True(t, isDepth, "expected *compiler.DepthExceededError, got %v", err)
		assert.Nil(t, out.Args)
		assert.Empty(t, out.SQL)
	})

	t.Run("unparseable SDL", func(t *testing.T) {
		out, err := playground.Compile("type Person {", playground.ExampleQuery, nil)
		require.Error(t, err)
		assert.Nil(t, out.Args)
	})

	t.Run("unknown field", func(t *testing.T) {
		out, err := playground.Compile(
			playground.ExampleSDL, `{ persons { nope } }`, nil)
		require.Error(t, err)
		assert.Nil(t, out.Args)
	})

	t.Run("missing variable", func(t *testing.T) {
		out, err := playground.Compile(
			playground.ExampleSDL, playground.ExampleQuery, nil)
		require.Error(t, err)
		assert.Nil(t, out.Args)
	})
}

// kindsSDL carries one field of each scalar kind the crossing has to preserve.
const kindsSDL = `type Thing @node(label: "thing") {
  id: ID!
  name: String!
  count: Int!
  ratio: Float!
  active: Boolean!
  note: String
}`

// TestCompileArgsSurviveJSONEncoding is the Go half of the browser crossing:
// cmd/wasm hands the page json.Marshal(Args), so a value that does not survive
// that encoding is a value the page cannot bind. Each kind has to arrive as
// itself and not as its printed form — "7", not 7, would be bound as text and
// compared against an integer column.
//
// The other half — that JSON.parse on the page yields the same kinds — is
// asserted in the browser suite, because only a browser can prove it.
func TestCompileArgsSurviveJSONEncoding(t *testing.T) {
	out, err := playground.Compile(kindsSDL,
		`{ things(name: $s, count: $i, ratio: $f, active: $b) { name } }`,
		map[string]any{"s": "Chain", "i": float64(7), "f": 1.5, "b": true})
	require.NoError(t, err, "Compile")

	encoded, err := json.Marshal(out.Args)
	require.NoError(t, err, "the compiler's values must be JSON-encodable")
	assert.JSONEq(t, `["Chain", 7, 1.5, true]`, string(encoded))

	// A null variable is a bound NULL, not an absent parameter: it still
	// occupies its placeholder position.
	nullOut, err := playground.Compile(kindsSDL,
		`{ things(note: $z) { name } }`, map[string]any{"z": nil})
	require.NoError(t, err, "Compile")
	assert.Len(t, nullOut.Args, 1)
	nullEncoded, err := json.Marshal(nullOut.Args)
	require.NoError(t, err)
	assert.JSONEq(t, `[null]`, string(nullEncoded))
}
