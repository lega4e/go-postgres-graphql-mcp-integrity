package sdl_test

import (
	"testing"

	"github.com/lega4e/gopgql/sdl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPriorNamesFromRenameHint pins the bridge between the two namespaces
// @renamedFrom lives across.
//
// The hint states the previous *GraphQL* name — a type name on OBJECT, a field
// name on FIELD_DEFINITION — but the differ renames tables and columns. Only sdl
// knows the defaulting rules that connect the two, so it is sdl that derives the
// candidates; the differ then resolves them against the folded prior state, which
// is the only place the answer actually exists.
func TestPriorNamesFromRenameHint(t *testing.T) {
	doc, err := sdl.Parse(`type Human @node(label: "human") @renamedFrom(name: "Person") {
  id: ID!
  contact: String! @renamedFrom(name: "email")
  tenant: String! @column(name: "tenant_id") @renamedFrom(name: "org")
  plain: String!
}`)
	require.NoError(t, err)

	n := doc.NodeByType("Human")
	require.NotNil(t, n)

	// A type's table is pluralize(@node(label:)), and the label is conventionally
	// the lowercased type name — so that is the first candidate. The hint read
	// literally is the second, for an SDL whose @node(table:) was explicit.
	assert.Equal(t, []string{"persons", "Person"}, n.PriorTableNames())

	byName := map[string]*sdl.Field{}
	for _, f := range n.Fields {
		byName[f.Name] = f
	}
	// A field with no @column(name:) maps to a column of its own name, so the
	// hint *is* the prior column name.
	assert.Equal(t, []string{"email"}, byName["contact"].PriorColumnNames())
	// The override applies to the current name only; the prior column is still
	// whatever the prior field name produced.
	assert.Equal(t, []string{"org"}, byName["tenant"].PriorColumnNames())
	assert.Nil(t, byName["plain"].PriorColumnNames(), "no hint, no candidates")
}

// TestPriorTableNamesOfferTheLiteralHint covers the author who wrote a physical
// table name rather than a GraphQL type name. Offering it as a second candidate
// is safe because a candidate is only ever accepted when the prior state actually
// holds it, so a wrong guess costs a no-op rather than a wrong rename.
func TestPriorTableNamesOfferTheLiteralHint(t *testing.T) {
	n := &sdl.Node{TypeName: "Human", RenamedFrom: "people_table"}
	assert.Equal(t, []string{"people_tables", "people_table"}, n.PriorTableNames())
}

// TestNoHintNoCandidates is the case every other rule depends on: a type with no
// @renamedFrom offers nothing, so the differ never so much as looks for a rename.
func TestNoHintNoCandidates(t *testing.T) {
	n := &sdl.Node{TypeName: "Human"}
	assert.Nil(t, n.PriorTableNames())
}
