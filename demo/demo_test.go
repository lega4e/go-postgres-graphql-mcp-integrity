package demo

import (
	"strings"
	"testing"
)

// wantExampleDDL is the DDL from SPEC.md §5.2. The M0 preview generator must
// reproduce it from ExampleSDL so the docs playground never drifts from the
// specification's worked example.
const wantExampleDDL = `CREATE TABLE persons (
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

func TestGenerateWorkedExample(t *testing.T) {
	got, err := Generate(ExampleSDL)
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if got.DDL != wantExampleDDL {
		t.Errorf("DDL mismatch:\n--- got ---\n%s\n--- want ---\n%s", got.DDL, wantExampleDDL)
	}
	wantQuery := `SELECT id, name, email
FROM GRAPH_TABLE (app_graph
  MATCH (v IS person)
  COLUMNS (v.id AS id, v.name AS name, v.email AS email)
)
ORDER BY name;`
	if got.Query != wantQuery {
		t.Errorf("query mismatch:\n--- got ---\n%s\n--- want ---\n%s", got.Query, wantQuery)
	}
}

func TestGenerateInvariants(t *testing.T) {
	got, err := Generate(ExampleSDL)
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	// §5.3 invariant 1: every KEY column also appears in PROPERTIES.
	if !strings.Contains(got.DDL, "PROPERTIES (source_id, target_id)") {
		t.Error("edge KEY columns must be re-listed in PROPERTIES")
	}
	if !strings.Contains(got.DDL, "PROPERTIES (id, name, email)") {
		t.Error("vertex KEY column id must be re-listed in PROPERTIES")
	}
	// §5.3 invariant 2: every edge table has an index on its destination key.
	if !strings.Contains(got.DDL, "CREATE INDEX follows_target_idx ON follows (target_id)") {
		t.Error("edge table must have an index on its destination key column")
	}
}

func TestGenerateNoNode(t *testing.T) {
	_, err := Generate(`type Person { id: ID! }`)
	if err == nil {
		t.Fatal("expected error for SDL without a @node type, got nil")
	}
}

func TestGenerateIgnoreAndScalars(t *testing.T) {
	sdl := `type Event @node(label: "event") {
  id: ID!
  count: Int!
  ratio: Float
  active: Boolean!
  payload: JSON
  at: DateTime!
  tags: [String!]!
  secret: String @ignore
}`
	got, err := Generate(sdl)
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	for _, want := range []string{
		"CREATE TABLE events (",
		"count integer NOT NULL",
		"ratio double precision",
		"active boolean NOT NULL",
		"payload jsonb",
		"at timestamptz NOT NULL",
		"tags text[] NOT NULL",
	} {
		if !strings.Contains(got.DDL, want) {
			t.Errorf("expected DDL to contain %q, got:\n%s", want, got.DDL)
		}
	}
	if strings.Contains(got.DDL, "secret") {
		t.Errorf("@ignore field must not appear in DDL, got:\n%s", got.DDL)
	}
}

func TestPluralize(t *testing.T) {
	cases := map[string]string{
		"person":  "persons",
		"company": "companies",
		"box":     "boxes",
		"class":   "classes",
		"day":     "days",
	}
	for in, want := range cases {
		if got := pluralize(in); got != want {
			t.Errorf("pluralize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIdentQuotesKeywords(t *testing.T) {
	if got := ident("order"); got != `"order"` {
		t.Errorf("ident(order) = %q, want quoted", got)
	}
	if got := ident("name"); got != "name" {
		t.Errorf("ident(name) = %q, want unquoted", got)
	}
}
