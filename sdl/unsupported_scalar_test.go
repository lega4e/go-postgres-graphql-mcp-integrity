package sdl_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/sdl"
)

// A scalar gopgql does not have is refused with the mapping to write instead.
//
// This is gopgql#53's smaller half, and the decision it records is "refuse
// clearly", not "support". Bytes in particular stays unimplemented on purpose:
// pgx scans bytea to []byte and json_build_object renders it as PostgreSQL's
// "\x6465" hex text, so a Bytes scalar would have to pick a canonical JSON form
// and make both shaping strategies agree on it — a SPEC.md §5.1 decision, not a
// bug fix. What was worth fixing is the message.
func TestAnUnsupportedScalarIsRefusedWithItsMapping(t *testing.T) {
	cases := map[string]struct{ field, wants string }{
		"Bytes":   {"data: Bytes!", `String! @column(type: "bytea")`},
		"Date":    {"day: Date!", `@column(type: "date")`},
		"UUID":    {"ref: UUID!", "Use ID"},
		"BigInt":  {"n: BigInt!", `@column(type: "bigint")`},
		"Decimal": {"amount: Decimal", `@column(type: "numeric(10,2)")`},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := sdl.Parse(
				"type Blob @node(label: \"blob\") {\n  id: ID!\n  " + tc.field + "\n}")
			require.Error(t, err, "a scalar gopgql does not have is never quietly accepted")

			assert.Contains(t, err.Error(), name, "the refusal names the scalar")
			assert.Contains(t, err.Error(), "schema.graphql:", "and where it was written")
			assert.Contains(t, err.Error(), tc.wants, "and what to write instead")
		})
	}
}

// The mapping is guidance appended to a real parse error, so a type that is
// genuinely just a typo still gets the plain message rather than advice about a
// scalar nobody mentioned.
func TestAnOrdinaryUndefinedTypeIsUnchanged(t *testing.T) {
	_, err := sdl.Parse(`type Blob @node(label: "blob") { id: ID! other: Widget! }`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Undefined type Widget")
	assert.NotContains(t, err.Error(), "@column(type:")
}

// The workaround the refusal recommends has to actually work, or the message is
// worse than none.
func TestTheRecommendedByteaMappingParses(t *testing.T) {
	doc, err := sdl.Parse(`type Blob @node(label: "blob") {
  id: ID!
  data: String! @column(type: "bytea")
}`)
	require.NoError(t, err)
	require.Len(t, doc.Nodes, 1)

	var found bool
	for _, f := range doc.Nodes[0].Fields {
		if f.Name == "data" {
			found = true
			assert.Equal(t, "bytea", f.ColumnType)
		}
	}
	assert.True(t, found, "the field is mapped")
}
