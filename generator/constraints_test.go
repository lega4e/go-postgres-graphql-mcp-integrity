package generator_test

import (
	"testing"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// constraintSDL exercises all four M7 constraint surfaces at once: a column
// default, a column-level check, two table-level checks and a two-column natural
// key.
const constraintSDL = `type Person @node(label: "person")
    @key(fields: ["tenant", "email"])
    @check(expr: "age < 200")
    @check(expr: "char_length(tenant) > 0") {
  id: ID!
  tenant: String!
  email: String!
  age: Int @default(value: "0") @check(expr: "age >= 0")
  nickname: String @default(value: "'unknown'")
}`

// TestConstraintModel is task 3.1: the physical model has somewhere to put a
// check body and a natural key, and the generator fills it in.
func TestConstraintModel(t *testing.T) {
	m := buildModel(t, constraintSDL)
	require.Len(t, m.VertexTables, 1)
	vt := m.VertexTables[0]

	require.NotNil(t, vt.NaturalKey, "@key must reach the physical model")
	assert.Equal(t, "persons_key", vt.NaturalKey.Name)
	assert.Equal(t, []string{"tenant", "email"}, vt.NaturalKey.Columns,
		"the key's columns keep the order the SDL declared them in")

	assert.Equal(t, []string{"age < 200", "char_length(tenant) > 0"}, vt.Checks,
		"type-level @check directives become table-level checks in declaration order")

	byName := map[string]schema.Column{}
	for _, c := range vt.Columns {
		byName[c.Name] = c
	}
	assert.Equal(t, "age >= 0", byName["age"].Check)
	assert.Equal(t, "0", byName["age"].Default)
	assert.Equal(t, "'unknown'", byName["nickname"].Default)
	assert.Empty(t, byName["tenant"].Check, "a field with no @check gets no check")
}

// TestDefaultEmittedVerbatim is task 3.2. A default is raw SQL the generator
// never parses (design D6), so what the SDL wrote is what the DDL says.
func TestDefaultEmittedVerbatim(t *testing.T) {
	ddl := buildDDL(t, constraintSDL)
	assert.Contains(t, ddl, "age integer DEFAULT 0")
	assert.Contains(t, ddl, "nickname text DEFAULT 'unknown'")
}

// TestChecksAreNamed is task 3.3: both check forms are emitted, and both carry a
// deterministic name, so a later delta can drop them without asking the database
// what name it invented.
func TestChecksAreNamed(t *testing.T) {
	ddl := buildDDL(t, constraintSDL)
	assert.Contains(t, ddl, "CONSTRAINT persons_age_check CHECK (age >= 0)",
		"a column-level check is named <table>_<column>_check")
	assert.Contains(t, ddl, "CONSTRAINT persons_check_1 CHECK (age < 200)",
		"a table-level check is named <table>_check_<n>, numbered by declaration order")
	assert.Contains(t, ddl, "CONSTRAINT persons_check_2 CHECK (char_length(tenant) > 0)")
	assert.NotContains(t, ddl, "age integer DEFAULT 0 CHECK",
		"a check is never emitted inline on the column: an anonymous constraint cannot be dropped by name")
}

// TestNaturalKeyIsANamedUnique is task 3.4.
func TestNaturalKeyIsANamedUnique(t *testing.T) {
	ddl := buildDDL(t, constraintSDL)
	assert.Contains(t, ddl, "CONSTRAINT persons_key UNIQUE (tenant, email)")
}

// TestSingleColumnNaturalKey covers the natural-keys spec's "single-column keys
// are allowed" scenario: one property is emitted the same way as several, and in
// particular is *not* mistaken for the @unique constraint on that column, which
// has a different name.
func TestSingleColumnNaturalKey(t *testing.T) {
	ddl := buildDDL(t, `type Person @node(label: "person") @key(fields: ["email"]) {
  id: ID!
  email: String!
}`)
	assert.Contains(t, ddl, "CONSTRAINT persons_key UNIQUE (email)")
	assert.NotContains(t, ddl, "persons_email_key")
}

// TestNaturalKeyInGraph is task 3.5 and design D1: the key's columns are the
// element's key in the property graph, and they stay in PROPERTIES so a MATCH can
// filter on them. The surrogate id stays a property too — the natural key sits
// alongside it and does not replace it.
func TestNaturalKeyInGraph(t *testing.T) {
	ddl := buildDDL(t, constraintSDL)
	assert.Contains(t, ddl, "persons KEY (tenant, email) LABEL person PROPERTIES (id, tenant, email, age, nickname)")
}

// TestNoNaturalKeyEmitsNoKeyClause pins the other half of the additive claim: a
// vertex table without a natural key carries no KEY clause at all, so PostgreSQL
// keeps falling back to the primary key exactly as it did before M7.
func TestNoNaturalKeyEmitsNoKeyClause(t *testing.T) {
	ddl := buildDDL(t, `type Person @node(label: "person") {
  id: ID!
  email: String!
}`)
	assert.NotContains(t, ddl, "KEY (")
	assert.Contains(t, ddl, "persons LABEL person PROPERTIES (id, email)")
}

// TestNaturalKeyColumnUsesColumnOverride checks the GraphQL → physical mapping:
// @key names *fields*, and the constraint has to name the columns those fields
// map to, which @column(name:) can move.
func TestNaturalKeyColumnUsesColumnOverride(t *testing.T) {
	m := buildModel(t, `type Person @node(label: "person") @key(fields: ["email"]) {
  id: ID!
  email: String! @column(name: "email_address")
}`)
	require.NotNil(t, m.VertexTables[0].NaturalKey)
	assert.Equal(t, []string{"email_address"}, m.VertexTables[0].NaturalKey.Columns)
}

// TestTableConstraintsOrder pins the emission order the migrate package relies on
// to name table-level checks: the ordinal in <table>_check_<n> is the check's
// position in the SDL, so it must not depend on map iteration.
func TestTableConstraintsOrder(t *testing.T) {
	m := buildModel(t, constraintSDL)
	got := generator.TableConstraints(&m.VertexTables[0])
	names := make([]string, len(got))
	for i, c := range got {
		names[i] = c.Name
	}
	assert.Equal(t, []string{"persons_key", "persons_age_check", "persons_check_1", "persons_check_2"}, names)
}

// TestExpressionWhitespaceNormalized guards the round trip. internal/ddl collapses
// whitespace when it reads a check back, so the generator collapses it on the way
// in; otherwise a schema written with wide spacing would fold back different from
// how it was emitted, and every run would emit a migration that changes nothing.
func TestExpressionWhitespaceNormalized(t *testing.T) {
	m := buildModel(t, `type Person @node(label: "person") {
  id: ID!
  age: Int @check(expr: "age   >=    0") @default(value: "0   ")
}`)
	byName := map[string]schema.Column{}
	for _, c := range m.VertexTables[0].Columns {
		byName[c.Name] = c
	}
	assert.Equal(t, "age >= 0", byName["age"].Check)
	assert.Equal(t, "0", byName["age"].Default)
}
