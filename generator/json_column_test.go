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
