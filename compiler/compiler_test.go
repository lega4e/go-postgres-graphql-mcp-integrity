package compiler_test

import (
	"testing"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/sdl"
)

const exampleSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  age: Int
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

func newCompiler(t *testing.T) *compiler.Compiler {
	t.Helper()
	doc, err := sdl.Parse(exampleSDL)
	if err != nil {
		t.Fatalf("sdl.Parse: %v", err)
	}
	return compiler.New(doc)
}

func TestCompileSingleVertex(t *testing.T) {
	c := newCompiler(t)
	sql, args, err := c.Compile(`{ persons { name } }`, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := `SELECT v0_k, v0_c0
FROM GRAPH_TABLE (app_graph
  MATCH (v0 IS person)
  COLUMNS (v0.id AS v0_k, v0.name AS v0_c0)
)`
	if sql != want {
		t.Errorf("SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", sql, want)
	}
	if len(args) != 0 {
		t.Errorf("expected no bind params, got %v", args)
	}
}

func TestCompileMultipleProperties(t *testing.T) {
	c := newCompiler(t)
	cq, err := c.CompileQuery(`{ persons { id name email } }`, nil)
	if err != nil {
		t.Fatalf("CompileQuery: %v", err)
	}
	want := `SELECT v0_k, v0_c0, v0_c1, v0_c2
FROM GRAPH_TABLE (app_graph
  MATCH (v0 IS person)
  COLUMNS (v0.id AS v0_k, v0.id AS v0_c0, v0.name AS v0_c1, v0.email AS v0_c2)
)`
	if cq.SQL != want {
		t.Errorf("SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", cq.SQL, want)
	}
	if cq.Projection.Root.ResponseKey != "persons" {
		t.Errorf("response key = %q, want persons", cq.Projection.Root.ResponseKey)
	}
	if len(cq.Projection.Root.Fields) != 3 {
		t.Errorf("projection fields = %d, want 3", len(cq.Projection.Root.Fields))
	}
}

func TestCompileAlias(t *testing.T) {
	c := newCompiler(t)
	cq, err := c.CompileQuery(`{ persons { fullName: name } }`, nil)
	if err != nil {
		t.Fatalf("CompileQuery: %v", err)
	}
	want := `SELECT v0_k, v0_c0
FROM GRAPH_TABLE (app_graph
  MATCH (v0 IS person)
  COLUMNS (v0.id AS v0_k, v0.name AS v0_c0)
)`
	if cq.SQL != want {
		t.Errorf("SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", cq.SQL, want)
	}
	if cq.Projection.Root.Fields[0].ResponseKey != "fullName" {
		t.Errorf("alias response key = %q, want fullName", cq.Projection.Root.Fields[0].ResponseKey)
	}
}

// TestCompileNestedWithVariable exercises the M3 exit query: a one-hop OUT
// traversal filtered by a bound variable.
func TestCompileNestedWithVariable(t *testing.T) {
	c := newCompiler(t)
	cq, err := c.CompileQuery(`{ persons(name: $n) { follows { name } } }`, map[string]any{"n": "Alice"})
	if err != nil {
		t.Fatalf("CompileQuery: %v", err)
	}
	want := `SELECT v0_k, v1_k, v1_c0
FROM GRAPH_TABLE (app_graph
  MATCH (v0 IS person) -[e0 IS follows]-> (v1 IS person)
  WHERE v0.name = $1
  COLUMNS (v0.id AS v0_k, v1.id AS v1_k, v1.name AS v1_c0)
)`
	if cq.SQL != want {
		t.Errorf("SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", cq.SQL, want)
	}
	if len(cq.Args) != 1 || cq.Args[0] != "Alice" {
		t.Errorf("args = %v, want [Alice]", cq.Args)
	}
	root := cq.Projection.Root
	if len(root.Children) != 1 {
		t.Fatalf("root children = %d, want 1", len(root.Children))
	}
	if root.Children[0].ResponseKey != "follows" {
		t.Errorf("child response key = %q, want follows", root.Children[0].ResponseKey)
	}
}

// TestCompileInDirection proves an IN relationship emits the reversed arrow.
func TestCompileInDirection(t *testing.T) {
	c := newCompiler(t)
	cq, err := c.CompileQuery(`{ persons { followedBy { name } } }`, nil)
	if err != nil {
		t.Fatalf("CompileQuery: %v", err)
	}
	want := `SELECT v0_k, v1_k, v1_c0
FROM GRAPH_TABLE (app_graph
  MATCH (v0 IS person) <-[e0 IS follows]- (v1 IS person)
  COLUMNS (v0.id AS v0_k, v1.id AS v1_k, v1.name AS v1_c0)
)`
	if cq.SQL != want {
		t.Errorf("SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", cq.SQL, want)
	}
}

// TestCompileLiteralArgument proves a literal argument is bound, not
// interpolated.
func TestCompileLiteralArgument(t *testing.T) {
	c := newCompiler(t)
	sql, args, err := c.Compile(`{ persons(name: "Bob", age: 41) { name } }`, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := `SELECT v0_k, v0_c0
FROM GRAPH_TABLE (app_graph
  MATCH (v0 IS person)
  WHERE v0.name = $1 AND v0.age = $2
  COLUMNS (v0.id AS v0_k, v0.name AS v0_c0)
)`
	if sql != want {
		t.Errorf("SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", sql, want)
	}
	if len(args) != 2 || args[0] != "Bob" || args[1] != int64(41) {
		t.Errorf("args = %#v, want [Bob 41]", args)
	}
}

// TestCompileParentScalarAndChild covers a root scalar plus a nested list — the
// dedup case shape must collapse.
func TestCompileParentScalarAndChild(t *testing.T) {
	c := newCompiler(t)
	cq, err := c.CompileQuery(`{ persons { name follows { name } } }`, nil)
	if err != nil {
		t.Fatalf("CompileQuery: %v", err)
	}
	want := `SELECT v0_k, v0_c0, v1_k, v1_c0
FROM GRAPH_TABLE (app_graph
  MATCH (v0 IS person) -[e0 IS follows]-> (v1 IS person)
  COLUMNS (v0.id AS v0_k, v0.name AS v0_c0, v1.id AS v1_k, v1.name AS v1_c0)
)`
	if cq.SQL != want {
		t.Errorf("SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", cq.SQL, want)
	}
}

func TestCompileVariableDefault(t *testing.T) {
	doc, err := sdl.Parse(exampleSDL)
	if err != nil {
		t.Fatalf("sdl.Parse: %v", err)
	}
	c := compiler.New(doc)
	// No value supplied for $n, but the operation declares a default.
	_, args, err := c.Compile(`query($n: String = "Zoe") { persons(name: $n) { name } }`, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(args) != 1 || args[0] != "Zoe" {
		t.Errorf("args = %v, want [Zoe] from the variable default", args)
	}
}

func TestCompileRejects(t *testing.T) {
	c := newCompiler(t)
	cases := map[string]struct {
		op   string
		vars map[string]any
	}{
		"multi-hop":            {`{ persons { follows { follows { name } } } }`, nil},
		"two relationships":    {`{ persons { follows { name } followedBy { name } } }`, nil},
		"unknown root":         {`{ widgets { name } }`, nil},
		"unknown field":        {`{ persons { nope } }`, nil},
		"relationship no body": {`{ persons { follows } }`, nil},
		"two roots":            {`{ persons { name } persons { email } }`, nil},
		"empty":                {`{ persons { } }`, nil},
		"missing variable":     {`{ persons(name: $n) { name } }`, nil},
		"argument on unknown":  {`{ persons(bogus: "x") { name } }`, nil},
		"mutation":             {`mutation { persons { name } }`, nil},
	}
	for name, tc := range cases {
		if _, _, err := c.Compile(tc.op, tc.vars); err == nil {
			t.Errorf("%s: expected error, got nil for %q", name, tc.op)
		}
	}
}
