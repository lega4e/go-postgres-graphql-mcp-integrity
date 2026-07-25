package sdl_test

import (
	"strings"
	"testing"

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
