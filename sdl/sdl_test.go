package sdl_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/sdl"
)

const exampleSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

func TestParseWorkedExample(t *testing.T) {
	doc, err := sdl.Parse(exampleSDL)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(doc.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(doc.Nodes))
	}
	n := doc.Nodes[0]
	if n.TypeName != "Person" || n.Label != "person" || n.Table != "persons" {
		t.Errorf("node = %+v, want Person/person/persons", n)
	}
	follows := field(n, "follows")
	if follows == nil || follows.Rel == nil {
		t.Fatal("follows relationship missing")
	}
	if follows.Rel.Direction != sdl.Out || follows.Rel.Type != "follows" {
		t.Errorf("follows rel = %+v", follows.Rel)
	}
	followedBy := field(n, "followedBy")
	if followedBy == nil || followedBy.Rel == nil {
		t.Fatal("followedBy relationship missing")
	}
	if followedBy.Rel.Direction != sdl.In || followedBy.Rel.HasInverse != "follows" {
		t.Errorf("followedBy rel = %+v", followedBy.Rel)
	}
}

func TestDefaultTableFromLabel(t *testing.T) {
	// table is optional and derived from the label by pluralization.
	doc, err := sdl.Parse(`type Company @node(label: "company") { id: ID! name: String! }`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	n := doc.Nodes[0]
	if n.Label != "company" || n.Table != "companies" {
		t.Errorf("defaults = %s/%s, want company/companies", n.Label, n.Table)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"no node":           `type Person { id: ID! }`,
		"missing label":     `type Person @node { id: ID! name: String! }`,
		"missing key":       `type Person @node(label: "person") { name: String! }`,
		"non-list rel":      `type Person @node(label: "person") { id: ID! friend: Person @relationship(type: "f", direction: OUT) }`,
		"rel to non-node":   `type Person @node(label: "person") { id: ID! tags: [Tag!]! @relationship(type: "t", direction: OUT) } type Tag { id: ID! }`,
		"bad hasInverse":    `type Person @node(label: "person") { id: ID! a: [Person!]! @relationship(type: "x", direction: OUT) @hasInverse(field: "missing") }`,
		"duplicate table":   `type A @node(label: "a", table: "t") { id: ID! } type B @node(label: "b", table: "t") { id: ID! }`,
		"unknown directive": `type Person @node(label: "person") { id: ID! name: String! @bogus }`,
		"invalid graphql":   `type Person @node(label: "person") { id: ID! `,
	}
	for name, src := range cases {
		if _, err := sdl.Parse(src); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestInverseDirectionMismatch(t *testing.T) {
	// Both fields declared OUT but paired by @hasInverse must be rejected.
	src := `type Person @node(label: "person") {
  id: ID!
  a: [Person!]! @relationship(type: "x", direction: OUT)
  b: [Person!]! @relationship(type: "x", direction: OUT) @hasInverse(field: "a")
}`
	_, err := sdl.Parse(src)
	if err == nil || !strings.Contains(err.Error(), "opposite") {
		t.Errorf("expected opposite-direction error, got %v", err)
	}
}

func field(n *sdl.Node, name string) *sdl.Field {
	for _, f := range n.Fields {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// interfaceSDL exercises both interface mappings at once (SPEC.md §7 → M4):
// Actor carries @node, so it becomes a shared label; Profile does not, so it
// resolves to label alternation.
const interfaceSDL = `interface Actor @node(label: "actor") {
  id: ID!
  name: String!
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}

interface Profile {
  id: ID!
  name: String!
}

type Person implements Actor & Profile @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}

type Bot implements Actor & Profile @node(label: "bot") {
  id: ID!
  name: String!
  vendor: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT, table: "bot_follows")
}`

func TestParseInterfaces(t *testing.T) {
	doc, err := sdl.Parse(interfaceSDL)
	if err != nil {
		t.Fatalf("sdl.Parse: %v", err)
	}
	if len(doc.Interfaces) != 2 {
		t.Fatalf("interfaces = %d, want 2", len(doc.Interfaces))
	}

	actor := doc.InterfaceByType("Actor")
	if actor == nil {
		t.Fatal("Actor interface missing")
	}
	if actor.Label != "actor" || actor.RootField != "actors" {
		t.Errorf("Actor label/root = %q/%q, want actor/actors", actor.Label, actor.RootField)
	}
	if len(actor.Implementors) != 2 {
		t.Errorf("Actor implementors = %d, want 2", len(actor.Implementors))
	}

	profile := doc.InterfaceByType("Profile")
	if profile == nil {
		t.Fatal("Profile interface missing")
	}
	if profile.Label != "" || profile.RootField != "profiles" {
		t.Errorf("Profile label/root = %q/%q, want \"\"/profiles", profile.Label, profile.RootField)
	}
}

// TestInterfaceTargets checks the compiler-facing view: a labelled interface is
// one label over several tables; an unlabelled one alternates over its
// implementors' labels. Both report every table a vertex could come from, which
// is what decides where an isomorphism guard is needed.
func TestInterfaceTargets(t *testing.T) {
	doc, err := sdl.Parse(interfaceSDL)
	if err != nil {
		t.Fatalf("sdl.Parse: %v", err)
	}
	cases := []struct {
		root   string
		labels []string
		tables []string
	}{
		{"actors", []string{"actor"}, []string{"bots", "persons"}},
		{"profiles", []string{"bot", "person"}, []string{"bots", "persons"}},
		{"persons", []string{"person"}, []string{"persons"}},
		{"bots", []string{"bot"}, []string{"bots"}},
	}
	for _, tc := range cases {
		target := doc.RootTarget(tc.root)
		if target == nil {
			t.Errorf("root field %q resolves to nothing", tc.root)
			continue
		}
		if strings.Join(target.Labels, "|") != strings.Join(tc.labels, "|") {
			t.Errorf("%s labels = %v, want %v", tc.root, target.Labels, tc.labels)
		}
		if strings.Join(target.Tables, ",") != strings.Join(tc.tables, ",") {
			t.Errorf("%s tables = %v, want %v", tc.root, target.Tables, tc.tables)
		}
	}
	if doc.RootTarget("widgets") != nil {
		t.Error("unknown root field must resolve to nil")
	}
	if got := strings.Join(doc.RootFields(), ","); got != "actors,bots,persons,profiles" {
		t.Errorf("RootFields = %q", got)
	}
}

func TestInterfaceParseErrors(t *testing.T) {
	cases := map[string]string{
		"interface without a key": `interface Actor @node(label: "actor") { name: String! }
type Person implements Actor @node(label: "person") { id: ID! name: String! }`,

		"implementor is not a @node": `interface Actor @node(label: "actor") { id: ID! name: String! }
type Person implements Actor @node(label: "person") { id: ID! name: String! }
type Ghost implements Actor { id: ID! name: String! }`,

		"relationship directive disagrees": `interface Actor @node(label: "actor") {
  id: ID!
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}
type Person implements Actor @node(label: "person") {
  id: ID!
  follows: [Person!]! @relationship(type: "follows", direction: IN)
}`,

		"relationship targets an interface": `interface Actor @node(label: "actor") { id: ID! name: String! }
type Person implements Actor @node(label: "person") {
  id: ID!
  name: String!
  knows: [Actor!]! @relationship(type: "knows", direction: OUT)
}`,

		"root field collision": `interface Actor @node(label: "person") { id: ID! name: String! }
type Person implements Actor @node(label: "person") { id: ID! name: String! }`,
	}
	for name, src := range cases {
		if _, err := sdl.Parse(src); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
}

// TestUnimplementedInterfaceIgnored proves an interface no @node type implements
// is GraphQL-only: it maps to nothing and is not queryable.
func TestUnimplementedInterfaceIgnored(t *testing.T) {
	doc, err := sdl.Parse(`interface Loose { id: ID! }
type Person @node(label: "person") { id: ID! name: String! }`)
	if err != nil {
		t.Fatalf("sdl.Parse: %v", err)
	}
	if len(doc.Interfaces) != 0 {
		t.Errorf("interfaces = %d, want 0", len(doc.Interfaces))
	}
	if doc.RootTarget("looses") != nil {
		t.Error("an unimplemented interface must not be queryable")
	}
}

// TestParseColumnDirectives covers the M6 mapping directives: a renamed column,
// an overridden type, uniqueness and a per-field index (SPEC.md §7 → M6).
func TestParseColumnDirectives(t *testing.T) {
	doc, err := sdl.Parse(`type Product @node(label: "product") {
  id: ID!
  sku: String! @unique
  title: String! @column(name: "name")
  price: Float! @column(type: "numeric(10,2)")
  category: String! @index(name: "by_category", using: "hash")
  notes: String @index
}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	n := doc.NodeByType("Product")
	if n == nil {
		t.Fatal("Product is not a mapped node")
	}
	byName := map[string]*sdl.Field{}
	for _, f := range n.Fields {
		byName[f.Name] = f
	}

	if got := byName["title"].ColumnName(); got != "name" {
		t.Errorf("title maps to column %q, want %q", got, "name")
	}
	if got := byName["sku"].ColumnName(); got != "sku" {
		t.Errorf("a field without @column(name:) keeps its own name, got %q", got)
	}
	if got := byName["price"].ColumnType; got != "numeric(10,2)" {
		t.Errorf("price type override = %q, want numeric(10,2)", got)
	}
	if !byName["sku"].Unique {
		t.Error("sku carries @unique but Unique is false")
	}
	if byName["title"].Unique {
		t.Error("title has no @unique but Unique is true")
	}
	idx := byName["category"].Index
	if idx == nil || idx.Name != "by_category" || idx.Using != "hash" {
		t.Errorf("category index = %+v, want name=by_category using=hash", idx)
	}
	if bare := byName["notes"].Index; bare == nil || bare.Name != "" || bare.Using != "" {
		t.Errorf("bare @index = %+v, want an empty spec the generator defaults", bare)
	}
}

// TestParseRejectsMisplacedMappingDirectives proves the M6 directives are
// rejected where they could have no effect, rather than silently ignored
// (SPEC.md §10).
func TestParseRejectsMisplacedMappingDirectives(t *testing.T) {
	cases := map[string]string{
		"on a relationship": `type Person @node(label: "person") {
  id: ID!
  follows: [Person!]! @relationship(type: "follows", direction: OUT) @unique
}`,
		"on an ignored field": `type Person @node(label: "person") {
  id: ID!
  nickname: String @ignore @column(name: "nick")
}`,
		"type override on the key": `type Person @node(label: "person") {
  id: ID! @column(type: "text")
}`,
		"unique on the key": `type Person @node(label: "person") {
  id: ID! @unique
}`,
		"colliding column names": `type Person @node(label: "person") {
  id: ID!
  name: String!
  title: String! @column(name: "name")
}`,
	}
	for label, src := range cases {
		if _, err := sdl.Parse(src); err == nil {
			t.Errorf("%s: expected a parse error, got none", label)
		}
	}
}

// constraintSDL exercises the M7 directive surface at once (SPEC.md §7 → M7):
// a natural key over two properties, table-level and column-level checks, a
// column default, and rename hints on both a type and a field.
const constraintSDL = `type Person @node(label: "person")
    @key(fields: ["email", "tenant"])
    @check(expr: "email <> ''")
    @check(expr: "char_length(tenant) > 0")
    @renamedFrom(name: "User") {
  id: ID!
  email: String! @renamedFrom(name: "mail")
  tenant: String!
  status: String! @default(value: "'active'") @check(expr: "status IN ('active', 'banned')")
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}`

func TestParseConstraintDirectives(t *testing.T) {
	doc, err := sdl.Parse(constraintSDL)
	require.NoError(t, err)

	n := doc.NodeByType("Person")
	require.NotNil(t, n)

	assert.Equal(t, []string{"email", "tenant"}, n.NaturalKey,
		"the natural key keeps the declared column order; the UNIQUE constraint is emitted in it")
	assert.Equal(t, []string{"email <> ''", "char_length(tenant) > 0"}, n.Checks,
		"table-level checks keep declaration order, so their derived constraint names are stable")
	assert.Equal(t, "User", n.RenamedFrom)

	byName := map[string]*sdl.Field{}
	for _, f := range n.Fields {
		byName[f.Name] = f
	}
	assert.Equal(t, "mail", byName["email"].RenamedFrom)
	assert.Equal(t, "'active'", byName["status"].Default)
	assert.Equal(t, "status IN ('active', 'banned')", byName["status"].Check)

	// design D1: the natural key sits *alongside* the surrogate key. The id
	// field is untouched, and it is still the identity edges reference.
	id := byName["id"]
	require.NotNil(t, id)
	assert.Equal(t, "ID", id.TypeName)
	assert.True(t, id.NonNull)
	assert.Empty(t, id.Default)
	assert.Empty(t, id.Check)
	assert.NotContains(t, n.NaturalKey, "id")
}

// TestParseNaturalKeyErrors covers the rule that a natural key names stored
// scalar columns of its own type: anything else has nothing to constrain, and
// the error says which field and which type.
func TestParseNaturalKeyErrors(t *testing.T) {
	cases := map[string]struct{ src, want string }{
		"unknown field": {`type Person @node(label: "person") @key(fields: ["email"]) {
  id: ID!
  name: String!
}`, `@key names field "email", which Person does not declare`},

		"relationship": {`type Person @node(label: "person") @key(fields: ["follows"]) {
  id: ID!
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}`, `Person.follows: @key names it, but it is a relationship`},

		"ignored field": {`type Person @node(label: "person") @key(fields: ["nickname"]) {
  id: ID!
  nickname: String @ignore
}`, `Person.nickname: @key names it, but it is @ignore`},

		"empty key": {`type Person @node(label: "person") @key(fields: []) {
  id: ID!
  name: String!
}`, `@key(fields:) is empty`},

		"repeated field": {`type Person @node(label: "person") @key(fields: ["name", "name"]) {
  id: ID!
  name: String!
}`, `@key names field "name" twice`},

		"two keys": {`type Person @node(label: "person") @key(fields: ["a"]) @key(fields: ["b"]) {
  id: ID!
  a: String!
  b: String!
}`, `only one is allowed`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := sdl.Parse(tc.src)
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

// TestParseRenameHintContradictions covers design D2's one error case: a hint
// naming something the same SDL still declares describes two objects, not one
// renamed one.
func TestParseRenameHintContradictions(t *testing.T) {
	cases := map[string]struct{ src, want string }{
		"field the SDL still declares": {`type Person @node(label: "person") {
  id: ID!
  email: String!
  contact: String! @renamedFrom(name: "email")
}`, `Person.contact: @renamedFrom(name: "email"), but Person still declares the field email`},

		"column another field still maps to": {`type Person @node(label: "person") {
  id: ID!
  address: String! @column(name: "email")
  contact: String! @renamedFrom(name: "email")
}`, `Person.address still maps to column "email"`},

		"a field renamed from itself": {`type Person @node(label: "person") {
  id: ID!
  email: String! @renamedFrom(name: "email")
}`, `but Person still declares the field email`},

		"type the SDL still declares": {`type User @node(label: "user") { id: ID! }
type Person @node(label: "person") @renamedFrom(name: "User") { id: ID! }`,
			`Person: @renamedFrom(name: "User"), but the SDL still declares the type User`},

		"interface the SDL still declares": {`interface Actor @node(label: "actor") { id: ID! }
type Person implements Actor @node(label: "person") @renamedFrom(name: "Actor") { id: ID! }`,
			`still declares the interface Actor`},

		"table another type still maps to": {`type Ghost @node(label: "ghost", table: "users") { id: ID! }
type Person @node(label: "person") @renamedFrom(name: "users") { id: ID! }`,
			`Ghost still maps to table "users"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := sdl.Parse(tc.src)
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

// TestRenameHintNamingSomethingAbsentIsAllowed is the first of the two
// "not an error" cases, and the one a later refactor is most likely to break by
// making rename validation symmetric. A hint whose old name is nowhere to be
// found is a no-op, not a mistake: the hint stays in the SDL after its rename
// has been applied, and that same SDL has to keep parsing and generating —
// every subsequent delta re-reads it (design D2).
func TestRenameHintNamingSomethingAbsentIsAllowed(t *testing.T) {
	// This is exactly the state of constraintSDL after its rename landed:
	// nothing here is called mail or User any more.
	src := `type Person @node(label: "person") @renamedFrom(name: "User") {
  id: ID!
  tenant: String! @renamedFrom(name: "mail")
  contact: String! @column(name: "email") @renamedFrom(name: "email")
}`
	doc, err := sdl.Parse(src)
	require.NoError(t, err, "a hint that matches nothing must parse; the differ decides it is a no-op")

	n := doc.NodeByType("Person")
	require.NotNil(t, n)
	assert.Equal(t, "User", n.RenamedFrom, "the hint survives parsing so the differ can still try to match it")

	// The third field is the subtle case: it declares the column "email" *and*
	// claims to have been renamed from "email". That is a GraphQL-level rename
	// with no physical rename behind it, so it is allowed — the contradiction
	// rule must exempt the field from its own column.
	byName := map[string]*sdl.Field{}
	for _, f := range n.Fields {
		byName[f.Name] = f
	}
	assert.Equal(t, "email", byName["contact"].ColumnName())
	assert.Equal(t, "email", byName["contact"].RenamedFrom)
}

// TestCheckExpressionIsNotParsed is the second "not an error" case: gopgql does
// not own an SQL expression parser, so a check expression is carried verbatim
// and PostgreSQL rejects a bad one when the migration is applied — which is the
// right place and the right error (design, Non-Goals).
func TestCheckExpressionIsNotParsed(t *testing.T) {
	const nonsense = "this is not (valid SQL at all"
	doc, err := sdl.Parse(`type Person @node(label: "person") @check(expr: "` + nonsense + `") {
  id: ID!
  age: Int! @check(expr: "age >>> ((")
}`)
	require.NoError(t, err, "an unparseable expression is the database's error, not the SDL's")

	n := doc.NodeByType("Person")
	require.NotNil(t, n)
	assert.Equal(t, []string{nonsense}, n.Checks, "the expression reaches the generator byte for byte")
	assert.Equal(t, "age >>> ((", field(n, "age").Check)
}

// TestParseConstraintDirectiveErrors covers the rules that do not depend on the
// SQL: an expression or a default has to say something, and each of these
// directives belongs on a field that maps to a column.
func TestParseConstraintDirectiveErrors(t *testing.T) {
	cases := map[string]struct{ src, want string }{
		"empty field check": {`type Person @node(label: "person") {
  id: ID!
  name: String! @check(expr: "")
}`, `Person.name: @check(expr:) is empty`},

		"blank type check": {`type Person @node(label: "person") @check(expr: "   ") {
  id: ID!
}`, `Person: @check(expr:) is empty`},

		"empty default": {`type Person @node(label: "person") {
  id: ID!
  name: String! @default(value: "")
}`, `Person.name: @default(value:) is empty`},

		"empty rename hint": {`type Person @node(label: "person") {
  id: ID!
  name: String! @renamedFrom(name: "")
}`, `Person.name: @renamedFrom(name:) is empty`},

		// gqlparser enforces single use at a field location itself; the model
		// holds one check per field, so this must stay an error either way.
		"two checks on one field": {`type Person @node(label: "person") {
  id: ID!
  name: String! @check(expr: "name <> ''") @check(expr: "length(name) < 10")
}`, `The directive check can only be used once at this location`},

		"default on a relationship": {`type Person @node(label: "person") {
  id: ID!
  follows: [Person!]! @relationship(type: "follows", direction: OUT) @default(value: "1")
}`, `it is a relationship and maps to no column`},

		"check on an ignored field": {`type Person @node(label: "person") {
  id: ID!
  nickname: String @ignore @check(expr: "nickname <> ''")
}`, `it is @ignore and maps to no column`},

		"default on the surrogate key": {`type Person @node(label: "person") {
  id: ID! @default(value: "gen_random_uuid()")
}`, `Person.id already defaults to a generated uuid`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := sdl.Parse(tc.src)
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

// TestConstraintDirectivesAreAdditive holds the M7 model to the claim its spec
// makes: an SDL using none of the four new directives parses exactly as it did
// before they existed. Asserted rather than assumed — it is the property that
// makes widening the directive surface safe.
func TestConstraintDirectivesAreAdditive(t *testing.T) {
	for name, src := range map[string]string{"worked example": exampleSDL, "interfaces": interfaceSDL} {
		t.Run(name, func(t *testing.T) {
			doc, err := sdl.Parse(src)
			require.NoError(t, err)

			for _, n := range doc.Nodes {
				assert.Nil(t, n.NaturalKey, "%s: no @key means no natural key, not an empty one", n.TypeName)
				assert.Empty(t, n.Checks, "%s", n.TypeName)
				assert.Empty(t, n.RenamedFrom, "%s", n.TypeName)
				for _, f := range n.Fields {
					assert.Empty(t, f.Default, "%s.%s", n.TypeName, f.Name)
					assert.Empty(t, f.Check, "%s.%s", n.TypeName, f.Name)
					assert.Empty(t, f.RenamedFrom, "%s.%s", n.TypeName, f.Name)
				}
			}
			for _, iface := range doc.Interfaces {
				for _, f := range iface.Fields {
					assert.Empty(t, f.Default, "%s.%s", iface.TypeName, f.Name)
					assert.Empty(t, f.Check, "%s.%s", iface.TypeName, f.Name)
					assert.Empty(t, f.RenamedFrom, "%s.%s", iface.TypeName, f.Name)
				}
			}
		})
	}

	// The pre-M7 model of the worked example, unchanged.
	doc, err := sdl.Parse(exampleSDL)
	require.NoError(t, err)
	require.Len(t, doc.Nodes, 1)
	n := doc.Nodes[0]
	assert.Equal(t, "Person", n.TypeName)
	assert.Equal(t, "person", n.Label)
	assert.Equal(t, "persons", n.Table)
	assert.Equal(t, []string{"id", "name", "email", "follows", "followedBy"}, fieldNames(n))
	assert.Equal(t, []string{"persons"}, doc.RootFields())

	target := doc.RootTarget("persons")
	require.NotNil(t, target)
	assert.Equal(t, []string{"person"}, target.Labels)
	assert.Equal(t, []string{"persons"}, target.Tables)
}

func fieldNames(n *sdl.Node) []string {
	out := make([]string, 0, len(n.Fields))
	for _, f := range n.Fields {
		out = append(out, f.Name)
	}
	return out
}
