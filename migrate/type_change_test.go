package migrate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/schema"
	"github.com/lega4e/gopgql/sdl"
)

// The gap gopgql#54 had to close before a global JSON type could mean anything.
//
// Until now a column whose *type* moved was not diffed at all: the differ
// compared column lists by name, so changing @column(type:) — and, once it
// existed, --json-type — produced no delta. The generator reported the directory
// "already up to date", exited 0, and the database kept the old type. Nothing
// said so; the schema simply was not what the SDL said it was.

// modelWith builds the desired schema for an SDL source under a JSON type.
func modelWith(t *testing.T, src, jsonType string) *schema.Schema {
	t.Helper()
	doc, err := sdl.Parse(src)
	require.NoError(t, err)
	m, err := generator.BuildWith(doc, generator.Options{JSONType: jsonType})
	require.NoError(t, err)
	return m
}

const jsonDocSDL = `type Doc @node(label: "doc") {
  id: ID!
  payload: JSON
}`

// TestGlobalJSONTypeChangeMigratesADeployedSchema is the behaviour-change half
// of the acceptance: a directory generated under the jsonb default and then
// pointed at json must emit the ALTER that moves it, not silently agree.
func TestGlobalJSONTypeChangeMigratesADeployedSchema(t *testing.T) {
	prior := foldOf(t, migrate.Init(modelWith(t, jsonDocSDL, "jsonb")))

	up, down, changed := migrate.Delta(prior, modelWith(t, jsonDocSDL, "json"))
	require.True(t, changed, "the column's type moved; a generation that wrote nothing would be a silent no-op")

	assert.Contains(t, up, `ALTER TABLE docs ALTER COLUMN payload TYPE json USING payload::json;`)
	assert.Contains(t, down, `ALTER TABLE docs ALTER COLUMN payload TYPE jsonb USING payload::jsonb;`,
		"Down puts the deployed schema back exactly as it was")

	// The column keeps its data: a type change is an ALTER, never a drop and
	// re-add, which would silently discard every row's value.
	assert.NotContains(t, up, `DROP COLUMN payload`)
	assert.NotContains(t, up, `ADD COLUMN payload`)
}

// TestPerColumnTypeChangeIsMigratedToo: --json-type is one way to move a type
// and @column(type:) is the other, so both go through the same diff.
func TestPerColumnTypeChangeIsMigratedToo(t *testing.T) {
	const before = `type Person @node(label: "person") {
  id: ID!
  ticket: Int!
}`
	const after = `type Person @node(label: "person") {
  id: ID!
  ticket: Int! @column(type: "bigint")
}`
	prior := applied(t, before)

	up, down, changed := delta(t, prior, after)
	require.True(t, changed)
	assert.Contains(t, up, `ALTER TABLE persons ALTER COLUMN ticket TYPE bigint USING ticket::bigint;`)
	assert.Contains(t, down, `ALTER TABLE persons ALTER COLUMN ticket TYPE integer USING ticket::integer;`)
}

// TestArrayColumnTypeChangeCarriesTheArrayMarker: the type in the ALTER is the
// column's full DDL type, so a list field moves as an array rather than losing
// its dimension.
func TestArrayColumnTypeChangeCarriesTheArrayMarker(t *testing.T) {
	const before = `type Person @node(label: "person") {
  id: ID!
  marks: [Int!]
}`
	const after = `type Person @node(label: "person") {
  id: ID!
  marks: [Int!] @column(type: "bigint")
}`
	prior := applied(t, before)

	up, _, changed := delta(t, prior, after)
	require.True(t, changed)
	assert.Contains(t, up, `TYPE bigint[] USING marks::bigint[];`)
}

// TestUnchangedTypesProposeNothing is the failure that would be worse than the
// one being fixed: a diff that saw a change where there is none would emit an
// ALTER on every generation, so `generate` would never again report a directory
// up to date.
func TestUnchangedTypesProposeNothing(t *testing.T) {
	for _, jsonType := range []string{"jsonb", "json"} {
		t.Run(jsonType, func(t *testing.T) {
			desired := modelWith(t, jsonDocSDL, jsonType)
			prior := foldOf(t, migrate.Init(desired))

			_, _, changed := migrate.Delta(prior, desired)
			assert.False(t, changed, "generating twice from one schema must propose nothing")
		})
	}
}

// TestTypeComparisonIgnoresSpellingNotSubstance: a modifier's whitespace and a
// type's case are not changes. The round trip through the DDL parser preserves
// the text an author wrote, and a differ that treated `numeric(10, 2)` and
// `numeric(10,2)` as different types would propose a migration for a reformat.
func TestTypeComparisonIgnoresSpellingNotSubstance(t *testing.T) {
	const spaced = `type Person @node(label: "person") {
  id: ID!
  rating: Float @column(type: "numeric(10, 2)")
}`
	const tight = `type Person @node(label: "person") {
  id: ID!
  rating: Float @column(type: "NUMERIC(10,2)")
}`
	prior := applied(t, spaced)

	_, _, changed := delta(t, prior, tight)
	assert.False(t, changed, "same type, different spelling")

	const widened = `type Person @node(label: "person") {
  id: ID!
  rating: Float @column(type: "numeric(12, 2)")
}`
	up, _, changed := delta(t, prior, widened)
	require.True(t, changed, "a different precision is a different type")
	assert.Contains(t, up, `TYPE numeric(12, 2)`)
}
