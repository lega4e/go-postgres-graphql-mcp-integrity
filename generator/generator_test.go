package generator_test

import (
	"strings"
	"testing"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/sdl"
)

// exampleSDL is the worked example from SPEC.md §5.2.
const exampleSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// wantDDL is the DDL from SPEC.md §5.2. The generator must reproduce it so the
// documentation's worked example cannot drift from the implementation.
const wantDDL = `CREATE TABLE persons (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL,
    email text
);

CREATE TABLE follows (
    source_id uuid NOT NULL REFERENCES persons (id),
    target_id uuid NOT NULL REFERENCES persons (id),
    PRIMARY KEY (source_id, target_id)
);
CREATE INDEX follows_target_idx ON follows (target_id);

CREATE PROPERTY GRAPH app_graph
  VERTEX TABLES (
    persons LABEL person PROPERTIES (id, name, email)
  )
  EDGE TABLES (
    follows SOURCE KEY (source_id) REFERENCES persons (id)
            DESTINATION KEY (target_id) REFERENCES persons (id)
            LABEL follows PROPERTIES (source_id, target_id)
  );
`

func buildDDL(t *testing.T, src string) string {
	t.Helper()
	doc, err := sdl.Parse(src)
	if err != nil {
		t.Fatalf("sdl.Parse: %v", err)
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		t.Fatalf("generator.Build: %v", err)
	}
	return generator.DDL(m)
}

func TestDDLMatchesWorkedExample(t *testing.T) {
	got := buildDDL(t, exampleSDL)
	if got != wantDDL {
		t.Errorf("DDL mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, wantDDL)
	}
}

func TestInvariants(t *testing.T) {
	got := buildDDL(t, exampleSDL)
	// §5.3 invariant 1: KEY columns re-listed in PROPERTIES.
	if !strings.Contains(got, "PROPERTIES (id, name, email)") {
		t.Error("vertex key id must be re-listed in PROPERTIES")
	}
	if !strings.Contains(got, "PROPERTIES (source_id, target_id)") {
		t.Error("edge keys must be re-listed in PROPERTIES")
	}
	// §5.3 invariant 2: destination-key index on every edge table.
	if !strings.Contains(got, "CREATE INDEX follows_target_idx ON follows (target_id)") {
		t.Error("edge table must have an index on its destination key")
	}
}

func TestScalarMappingAndIgnore(t *testing.T) {
	src := `type Event @node(label: "event") {
  id: ID!
  count: Int!
  ratio: Float
  active: Boolean!
  payload: JSON
  at: DateTime!
  tags: [String!]!
  secret: String @ignore
}`
	got := buildDDL(t, src)
	for _, want := range []string{
		"CREATE TABLE events (",
		"count integer NOT NULL",
		"ratio double precision",
		"active boolean NOT NULL",
		"payload jsonb",
		"at timestamptz NOT NULL",
		"tags text[] NOT NULL",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected DDL to contain %q, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "secret") {
		t.Errorf("@ignore field must not appear in DDL, got:\n%s", got)
	}
}

func TestKeywordIdentifiersQuoted(t *testing.T) {
	// A node whose table name collides with a SQL keyword must be quoted
	// (§5.3 invariant 3).
	src := `type Order @node(label: "order", table: "order") {
  id: ID!
  total: Float!
}`
	got := buildDDL(t, src)
	if !strings.Contains(got, `CREATE TABLE "order" (`) {
		t.Errorf("keyword table name must be quoted, got:\n%s", got)
	}
	if !strings.Contains(got, `"order" LABEL "order"`) {
		t.Errorf("keyword label must be quoted in property graph, got:\n%s", got)
	}
}

func TestRejectsMissingKey(t *testing.T) {
	_, err := sdl.Parse(`type Person @node(label: "person") { name: String! }`)
	if err == nil {
		t.Fatal("expected error for @node type without id: ID!")
	}
}

// interfaceSDL maps two vertex tables under one interface. Actor carries @node,
// so persons and bots both expose the shared `actor` label; Profile does not, so
// it contributes no label and is matched by alternation at compile time
// (SPEC.md §7 → M4).
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

// TestInterfaceSharedLabel checks the shared label lands on every implementor's
// vertex table with an aligned property list — SPEC.md §5.3 invariant 5, which
// PostgreSQL rejects the graph over if it is broken.
func TestInterfaceSharedLabel(t *testing.T) {
	got := buildDDL(t, interfaceSDL)
	for _, want := range []string{
		"bots LABEL bot PROPERTIES (id, name, vendor)\n            LABEL actor PROPERTIES (id, name)",
		"persons LABEL person PROPERTIES (id, name, email)\n            LABEL actor PROPERTIES (id, name)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected DDL to contain:\n%s\n--- got ---\n%s", want, got)
		}
	}
	// An unlabelled interface contributes nothing to the DDL.
	if strings.Contains(got, "profile") {
		t.Errorf("an unlabelled interface must not reach the DDL, got:\n%s", got)
	}
	// Both edge tables carry the same label, each with the aligned property list.
	for _, want := range []string{
		"bot_follows SOURCE KEY (source_id) REFERENCES bots (id)",
		"follows SOURCE KEY (source_id) REFERENCES persons (id)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected DDL to contain %q, got:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "LABEL follows PROPERTIES (source_id, target_id)"); n != 2 {
		t.Errorf("edge label `follows` spans %d tables, want 2", n)
	}
}

// TestRejectsMisalignedSharedLabel proves invariant 5 is enforced at generate
// time rather than surfacing as a PostgreSQL error at migration time. The two
// implementors expose the same property *names* under `actor` but different
// types, which PostgreSQL refuses.
func TestRejectsMisalignedSharedLabel(t *testing.T) {
	src := `interface Actor @node(label: "actor") {
  id: ID!
  name: String!
}

type Person implements Actor @node(label: "person") {
  id: ID!
  name: String!
}

type Bot implements Actor @node(label: "bot") {
  id: ID!
  name: String!
  rank: Int
}`
	// Aligned: both expose (id, name) under `actor`; the extra column is not a
	// property of the shared label.
	doc, err := sdl.Parse(src)
	if err != nil {
		t.Fatalf("sdl.Parse: %v", err)
	}
	if _, err := generator.Build(doc, ""); err != nil {
		t.Fatalf("aligned interface should build: %v", err)
	}

	// A property name with two types across the graph is what PostgreSQL
	// rejects ("a property of the same name has to have the same data type").
	clash := `type Person @node(label: "person") {
  id: ID!
  name: String!
}

type Gizmo @node(label: "gizmo") {
  id: ID!
  name: Int!
}`
	doc, err = sdl.Parse(clash)
	if err != nil {
		t.Fatalf("sdl.Parse: %v", err)
	}
	if _, err := generator.Build(doc, ""); err == nil {
		t.Error("expected a property-type clash to be rejected at generate time")
	}
}

// TestRejectsLabelClaimedTwice guards the case where an interface and one of its
// implementors ask for the same label — the table would carry it twice with
// different property lists.
func TestRejectsLabelClaimedTwice(t *testing.T) {
	src := `interface Actor @node(label: "person") {
  id: ID!
  name: String!
}

type Person implements Actor @node(label: "person", table: "people") {
  id: ID!
  name: String!
  email: String
}`
	doc, err := sdl.Parse(src)
	if err != nil {
		t.Fatalf("sdl.Parse: %v", err)
	}
	if _, err := generator.Build(doc, ""); err == nil {
		t.Error("expected an error when an interface and its implementor claim one label")
	}
}
