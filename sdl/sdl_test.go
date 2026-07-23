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
