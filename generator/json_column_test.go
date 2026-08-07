package generator_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/sdl"
)

// The JSON default and its escape, decided in gopgql#53.
//
// JSON stays jsonb: it is what can be indexed and queried, and changing the
// default would emit an ALTER TABLE … TYPE json for every existing JSON column in
// every schema already in production. What jsonb costs is byte-identical round
// trip — it sorts keys, drops insignificant whitespace and keeps only the last of
// a duplicated key — so the requirement is met per column instead, which is the
// grain at which it actually differs.
//
// Both halves are asserted together on purpose: a default is only defensible
// while the escape from it works.
func TestJSONDefaultsToJSONBAndIsOverridable(t *testing.T) {
	doc, err := sdl.Parse(`type Doc @node(label: "doc") {
  id: ID!
  indexed: JSON
  verbatim: JSON @column(type: "json")
}`)
	require.NoError(t, err)
	m, err := generator.Build(doc, "")
	require.NoError(t, err)

	ddl := generator.TablesDDL(m)
	assert.Contains(t, ddl, "indexed jsonb",
		"JSON defaults to jsonb: indexable, queryable, and what most schemas want")
	assert.Contains(t, ddl, "verbatim json",
		"@column(type: \"json\") is the documented escape for a column that must round-trip "+
			"byte-identically; jsonb normalises key order and whitespace")
}

// jsonSDL declares the two cases a global default has to keep apart: a column
// that takes whatever the default is, and one that names its own type.
const jsonSDL = `type Doc @node(label: "doc") {
  id: ID!
  plain: JSON
  pinned: JSON @column(type: "jsonb")
}`

// TestJSONTypeIsConfigurableGlobally is the ergonomic half of gopgql#53's
// decision (gopgql#54): a schema on a byte-identical round-trip path says "json"
// once, rather than repeating @column(type: "json") on every JSON column and
// having the one that was forgotten stay invisible until a stored value has more
// than one key.
func TestJSONTypeIsConfigurableGlobally(t *testing.T) {
	doc, err := sdl.Parse(jsonSDL)
	require.NoError(t, err)

	for _, tc := range []struct {
		name     string
		jsonType string
		plain    string
	}{
		{name: "unset is jsonb", jsonType: "", plain: "plain jsonb"},
		{name: "jsonb, said out loud", jsonType: "jsonb", plain: "plain jsonb"},
		{name: "json, for round-trip fidelity", jsonType: "json", plain: "plain json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := generator.BuildWith(doc, generator.Options{JSONType: tc.jsonType})
			require.NoError(t, err)
			ddl := generator.TablesDDL(m)

			assert.Contains(t, ddl, tc.plain, "the default moves with --json-type")
			assert.Contains(t, ddl, "pinned jsonb",
				"@column(type:) still wins on the column that carries it: the global setting "+
					"moves the default, and an exception stays an exception")
		})
	}
}

// TestUnknownJSONTypeIsRefused: a default nobody re-reads must not be able to
// make every JSON column in a schema a type PostgreSQL will reject — the failure
// would arrive at migration time, naming a column rather than the setting.
func TestUnknownJSONTypeIsRefused(t *testing.T) {
	doc, err := sdl.Parse(jsonSDL)
	require.NoError(t, err)

	_, err = generator.BuildWith(doc, generator.Options{JSONType: "jsonbb"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown JSON type "jsonbb"`)
	assert.Contains(t, err.Error(), "@column(type: ...)",
		"the error has to name the way to get an arbitrary type")
}

// TestBuildIsBuildWithDefaults keeps the two entry points from drifting: Build
// is the old signature, and it must mean exactly the zero Options.
func TestBuildIsBuildWithDefaults(t *testing.T) {
	doc, err := sdl.Parse(jsonSDL)
	require.NoError(t, err)

	old, err := generator.Build(doc, "custom_graph")
	require.NoError(t, err)
	fresh, err := generator.BuildWith(doc, generator.Options{GraphName: "custom_graph"})
	require.NoError(t, err)

	assert.Equal(t, generator.TablesDDL(old), generator.TablesDDL(fresh))
	assert.Equal(t, generator.GraphDDL(old), generator.GraphDDL(fresh))
}

// TestJSONTypeDoesNotLeakBetweenBuilds guards the one way a shared default map
// could go wrong: a build that pointed JSON at json must not have moved the
// package-level mapping every later build reads.
func TestJSONTypeDoesNotLeakBetweenBuilds(t *testing.T) {
	doc, err := sdl.Parse(jsonSDL)
	require.NoError(t, err)

	_, err = generator.BuildWith(doc, generator.Options{JSONType: "json"})
	require.NoError(t, err)

	after, err := generator.Build(doc, "")
	require.NoError(t, err)
	assert.Contains(t, generator.TablesDDL(after), "plain jsonb",
		"the default is still jsonb for the next build")
}
