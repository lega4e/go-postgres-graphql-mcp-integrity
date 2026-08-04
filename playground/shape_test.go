package playground_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/playground"
)

// The playground's Run button is a round trip: compile a GraphQL query to
// GRAPH_TABLE here, execute it somewhere that has a PostgreSQL, and shape the
// flat rows that come back into the nested GraphQL response. These tests cover
// the last leg — the part that runs in the browser, on rows the browser did not
// produce.
//
// The column names are read out of the projection rather than written down.
// They are the compiler's private naming scheme (v0_k, v0_c0, …), and a test
// that hardcoded them would be asserting on an implementation detail while
// missing the thing that matters: that the names Shape looks rows up by are the
// same names the compiler projected them as.

// nestedQuery selects one level of relationship, which is the smallest query
// that can fan out — and fanning out is the whole reason shaping exists.
const nestedQuery = `{ persons(name: $n) { name follows { name } } }`

// compileNested compiles nestedQuery against the worked example and returns the
// projection with the root and child selections it produced.
func compileNested(t *testing.T) (compiler.Projection, *compiler.Selection, *compiler.Selection) {
	t.Helper()
	compiled, err := playground.Compile(playground.ExampleSDL, nestedQuery, map[string]any{"n": "Alice"})
	require.NoError(t, err, "Compile")

	root := compiled.Projection.Root
	require.NotNil(t, root, "the compiled query carries a projection")
	require.Len(t, root.Children, 1, "one relationship level")
	return compiled.Projection, root, root.Children[0]
}

// columnsOf is the flat result's column list for a one-level projection, in the
// order the rows below supply values.
func columnsOf(root, child *compiler.Selection) []string {
	return []string{
		root.KeyColumn, root.Fields[0].Column,
		child.KeyColumn, child.Fields[0].Column,
	}
}

// TestCompileCarriesTheProjection pins the reason Compile stopped throwing the
// projection away. Without it the WASM entry point has nothing to shape with,
// and the playground can only ever show rows.
func TestCompileCarriesTheProjection(t *testing.T) {
	compiled, err := playground.Compile(playground.ExampleSDL, nestedQuery, map[string]any{"n": "Alice"})
	require.NoError(t, err, "Compile")

	require.NotNil(t, compiled.Projection.Root)
	assert.Equal(t, "persons", compiled.Projection.Root.ResponseKey,
		"the root selection is keyed by the GraphQL response key, not the table")
}

// TestCompileErrorCarriesNoProjection: every error path returns the zero
// Compiled, so a caller that ignored the error would get a nil root from Shape
// rather than a response shaped by a projection that was never built.
func TestCompileErrorCarriesNoProjection(t *testing.T) {
	compiled, err := playground.Compile(playground.ExampleSDL, `{ nosuchfield { name } }`, nil)
	require.Error(t, err)
	assert.Nil(t, compiled.Projection.Root)
}

// TestShapeDedupesParentsAcrossFanOut is the property the whole package exists
// for. A parent with two children arrives as two flat rows that repeat the
// parent; the response has to carry it once, with both children under it.
func TestShapeDedupesParentsAcrossFanOut(t *testing.T) {
	proj, root, child := compileNested(t)

	shaped, err := playground.Shape(proj, playground.Result{
		Columns: columnsOf(root, child),
		Rows: [][]any{
			{"alice-id", "Alice", "bob-id", "Bob"},
			{"alice-id", "Alice", "carol-id", "Carol"},
		},
	})
	require.NoError(t, err, "Shape")

	persons, ok := shaped["persons"].([]any)
	require.True(t, ok, "the response is keyed by the root field")
	require.Len(t, persons, 1, "two rows repeat one parent; the response carries it once")

	person, ok := persons[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Alice", person["name"])

	follows, ok := person["follows"].([]any)
	require.True(t, ok)
	require.Len(t, follows, 2, "both children nest under the one parent")
	assert.Equal(t, "Bob", follows[0].(map[string]any)["name"])
	assert.Equal(t, "Carol", follows[1].(map[string]any)["name"])
}

// TestShapeEmptyResultIsAnEmptyList: a query that matched nothing still has a
// response. The playground renders it, because "no rows" and "the request
// failed" are different outcomes and have to look different.
func TestShapeEmptyResultIsAnEmptyList(t *testing.T) {
	proj, root, child := compileNested(t)

	shaped, err := playground.Shape(proj, playground.Result{
		Columns: columnsOf(root, child),
		Rows:    nil,
	})
	require.NoError(t, err, "Shape")

	persons, ok := shaped["persons"].([]any)
	require.True(t, ok)
	assert.Empty(t, persons)
}

// TestShapeRejectsMisalignedRows: a row whose width disagrees with the column
// list means the two came from different executions. Padding it would produce a
// response that is wrong in a way nobody could see, so it is refused instead.
func TestShapeRejectsMisalignedRows(t *testing.T) {
	proj, root, child := compileNested(t)

	_, err := playground.Shape(proj, playground.Result{
		Columns: columnsOf(root, child),
		Rows:    [][]any{{"alice-id", "Alice"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2 values")
	assert.Contains(t, err.Error(), "4 columns")
}

// TestShapeWithoutAProjection names the missing piece rather than panicking on
// a nil root — which is what a caller that ignored a compile error would hand
// over.
func TestShapeWithoutAProjection(t *testing.T) {
	_, err := playground.Shape(compiler.Projection{}, playground.Result{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no projection")
}

// TestShapeJSONIsTheNestedResponse walks the whole entry point the WASM export
// calls: compile from source, shape the rows, render JSON. The exact document
// is asserted, because it is what a reader sees in the panel.
func TestShapeJSONIsTheNestedResponse(t *testing.T) {
	_, root, child := compileNested(t)

	out, err := playground.ShapeJSON(
		playground.ExampleSDL, nestedQuery, map[string]any{"n": "Alice"},
		playground.MaxDepth(),
		playground.Result{
			Columns: columnsOf(root, child),
			Rows: [][]any{
				{"alice-id", "Alice", "bob-id", "Bob"},
				{"alice-id", "Alice", "carol-id", "Carol"},
			},
		})
	require.NoError(t, err, "ShapeJSON")

	assert.JSONEq(t, `{"persons":[{"name":"Alice","follows":[{"name":"Bob"},{"name":"Carol"}]}]}`, out)

	// Indented, because it is read by a person in a panel and not by a parser.
	assert.Contains(t, out, "\n  ")

	// No GraphQL envelope. gopgql shapes a payload; it does not answer requests,
	// and a `data` key it never produces would be the page's one invented value.
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &decoded))
	assert.NotContains(t, decoded, "data")
	assert.Len(t, decoded, 1)
}

// TestShapeJSONReportsACompileError: the page sends back the same inputs it
// compiled with, so this only fires when they were edited between the two — but
// it has to say so rather than render an empty response.
func TestShapeJSONReportsACompileError(t *testing.T) {
	_, err := playground.ShapeJSON(playground.ExampleSDL, `{ nosuchfield { name } }`, nil,
		playground.MaxDepth(), playground.Result{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nosuchfield")
}

// TestShapeJSONHonoursTheDepthCeiling. The Depth tab runs a query that only
// compiles once the reader raises the ceiling; shaping its result recompiles,
// so it has to recompile at the same ceiling or the second pass would be
// refused where the first one was not.
func TestShapeJSONHonoursTheDepthCeiling(t *testing.T) {
	const deep = playground.ExampleDeepQuery
	vars := map[string]any{"n": "Alice"}

	_, err := playground.ShapeJSON(playground.ExampleSDL, deep, vars,
		playground.MaxDepth(), playground.Result{})
	require.Error(t, err, "the default ceiling refuses this query")
	_, exceeded := playground.DepthExceeded(err)
	assert.True(t, exceeded, "and refuses it as a depth rejection")

	compiled, err := playground.CompileWithMaxDepth(playground.ExampleSDL, deep, vars, 4)
	require.NoError(t, err, "the raised ceiling compiles it")

	out, err := playground.ShapeJSON(playground.ExampleSDL, deep, vars, 4, playground.Result{
		Columns: []string{compiled.Projection.Root.KeyColumn},
		Rows:    [][]any{{"alice-id"}},
	})
	require.NoError(t, err, "and shaping at the same ceiling succeeds")
	assert.Contains(t, out, "persons")
}
