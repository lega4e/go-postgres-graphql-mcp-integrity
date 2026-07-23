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
