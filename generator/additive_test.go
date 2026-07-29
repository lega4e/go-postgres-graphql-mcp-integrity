package generator_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/sdl"
	"github.com/stretchr/testify/require"
)

// preM7SDL is a schema that uses every mapping directive gopgql had *before* M7
// and none of the four M7 constraint directives: relationships in both
// directions, an interface mapped to a shared label, @column(name:)/@column(type:),
// @unique, @index with and without an explicit name and access method, @ignore,
// arrays and the full scalar set.
//
// It exists to pin task 3.6's additive claim: adding @default, @check, @key and
// @renamedFrom must not move a single byte of the DDL for a schema that declares
// none of them.
const preM7SDL = `interface Actor @node(label: "actor") {
  id: ID!
  name: String!
}

type Person implements Actor @node(label: "person") {
  id: ID!
  name: String!
  email: String @column(name: "email_address") @unique
  age: Int @index
  score: Float @column(type: "numeric(10,2)")
  active: Boolean
  createdAt: DateTime @index(name: "persons_created_idx", using: "btree")
  meta: JSON
  nicknames: [String!]
  secret: String @ignore
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
  worksAt: [Company!]! @relationship(type: "works_at", direction: OUT)
}

type Company implements Actor @node(label: "company") {
  id: ID!
  name: String!
  employees: [Person!]! @relationship(type: "works_at", direction: IN)
                        @hasInverse(field: "worksAt")
}`

// goldenPath is the DDL this SDL produced before the M7 constraint surface
// existed. It is checked in, so the assertion is against a recorded fact rather
// than against whatever the generator happens to emit today.
const goldenPath = "testdata/pre_m7_schema.sql"

// TestConstraintDirectivesAreAdditive is task 3.6: a schema using none of the
// M7 directives generates byte-identical DDL to the schema that predates them.
//
// The generator gained three new emitters (DEFAULT on a column, named CHECK
// constraints, the natural key's UNIQUE and the graph's KEY clause). Each is
// guarded by "the model declares one"; this asserts that the guards hold, rather
// than trusting that they do.
func TestConstraintDirectivesAreAdditive(t *testing.T) {
	want, err := os.ReadFile(filepath.Clean(goldenPath))
	require.NoError(t, err, "read golden DDL")

	doc, err := sdl.Parse(preM7SDL)
	require.NoError(t, err)
	m, err := generator.Build(doc, "")
	require.NoError(t, err)

	require.Equal(t, string(want), generator.DDL(m),
		"a schema declaring no @default/@check/@key/@renamedFrom must generate the DDL it generated before those directives existed")
}
