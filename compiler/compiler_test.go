package compiler_test

import (
	"errors"
	"strings"
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

// interfaceSDL maps two tables under one interface twice over: Actor carries
// @node, so its implementors share the `actor` label; Profile does not, so it is
// matched by alternation over `bot` and `person` (SPEC.md §7 → M4).
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

func newCompiler(t *testing.T) *compiler.Compiler {
	t.Helper()
	return compilerFor(t, exampleSDL)
}

func newInterfaceCompiler(t *testing.T) *compiler.Compiler {
	t.Helper()
	return compilerFor(t, interfaceSDL)
}

func compilerFor(t *testing.T, src string) *compiler.Compiler {
	t.Helper()
	doc, err := sdl.Parse(src)
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
  WHERE v0.name = $1 AND v0.id <> v1.id
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
  WHERE v0.id <> v1.id
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
  WHERE v0.id <> v1.id
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

// TestCompileThreeHop is the M4 exit shape: nesting keeps extending one
// pattern, and every pair of person positions is guarded against binding the
// same row (SPEC.md §7 → M4).
func TestCompileThreeHop(t *testing.T) {
	c := newCompiler(t)
	cq, err := c.CompileQuery(
		`{ persons(name: $n) { name follows { name follows { name follows { name } } } } }`,
		map[string]any{"n": "Alice"})
	if err != nil {
		t.Fatalf("CompileQuery: %v", err)
	}
	want := `SELECT v0_k, v0_c0, v1_k, v1_c0, v2_k, v2_c0, v3_k, v3_c0
FROM GRAPH_TABLE (app_graph
  MATCH (v0 IS person) -[e0 IS follows]-> (v1 IS person) -[e1 IS follows]-> ` +
		`(v2 IS person) -[e2 IS follows]-> (v3 IS person)
  WHERE v0.name = $1 AND v0.id <> v1.id AND v0.id <> v2.id AND v0.id <> v3.id ` +
		`AND v1.id <> v2.id AND v1.id <> v3.id AND v2.id <> v3.id
  COLUMNS (v0.id AS v0_k, v0.name AS v0_c0, v1.id AS v1_k, v1.name AS v1_c0, ` +
		`v2.id AS v2_k, v2.name AS v2_c0, v3.id AS v3_k, v3.name AS v3_c0)
)`
	if cq.SQL != want {
		t.Errorf("SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", cq.SQL, want)
	}

	// The projection tree is three levels deep under the root.
	sel := cq.Projection.Root
	for depth := 0; depth < 3; depth++ {
		if len(sel.Children) != 1 {
			t.Fatalf("level %d has %d children, want 1", depth, len(sel.Children))
		}
		sel = sel.Children[0]
	}
	if len(sel.Children) != 0 {
		t.Errorf("leaf level has %d children, want 0", len(sel.Children))
	}
}

// TestCompileDepthExceeded proves a fourth hop is refused at compile time with
// the typed error — no SQL is produced, so nothing can reach the database.
func TestCompileDepthExceeded(t *testing.T) {
	c := newCompiler(t)
	if c.MaxDepth() != compiler.DefaultMaxDepth {
		t.Fatalf("MaxDepth = %d, want the default %d", c.MaxDepth(), compiler.DefaultMaxDepth)
	}
	sql, args, err := c.Compile(`{ persons { follows { follows { follows { follows { name } } } } } }`, nil)
	if err == nil {
		t.Fatal("expected a depth error for a 4-hop selection, got nil")
	}
	var depthErr *compiler.DepthExceededError
	if !errors.As(err, &depthErr) {
		t.Fatalf("error is %T (%v), want *compiler.DepthExceededError", err, err)
	}
	if depthErr.MaxDepth != 3 || depthErr.Depth != 4 {
		t.Errorf("MaxDepth/Depth = %d/%d, want 3/4", depthErr.MaxDepth, depthErr.Depth)
	}
	if got := strings.Join(depthErr.Path, "."); got != "persons.follows.follows.follows.follows" {
		t.Errorf("Path = %q, want the full response-key trail", got)
	}
	if sql != "" || args != nil {
		t.Errorf("a rejected compilation must yield no SQL and no args, got %q / %v", sql, args)
	}
}

// TestCompileMaxDepthConfigured proves the ceiling is configurable: the same
// query that compiles at the default fails one notch lower (SPEC.md §6.2).
func TestCompileMaxDepthConfigured(t *testing.T) {
	doc, err := sdl.Parse(exampleSDL)
	if err != nil {
		t.Fatalf("sdl.Parse: %v", err)
	}
	const twoHop = `{ persons { follows { follows { name } } } }`

	if _, _, err := compiler.New(doc, compiler.WithMaxDepth(2)).Compile(twoHop, nil); err != nil {
		t.Fatalf("2 hops at MaxDepth 2: %v", err)
	}

	_, _, err = compiler.New(doc, compiler.WithMaxDepth(1)).Compile(twoHop, nil)
	var depthErr *compiler.DepthExceededError
	if !errors.As(err, &depthErr) {
		t.Fatalf("2 hops at MaxDepth 1: error is %T (%v), want *compiler.DepthExceededError", err, err)
	}
	if depthErr.MaxDepth != 1 {
		t.Errorf("MaxDepth = %d, want 1", depthErr.MaxDepth)
	}

	// Zero permits no traversal at all; a plain root selection still compiles.
	zero := compiler.New(doc, compiler.WithMaxDepth(-4))
	if zero.MaxDepth() != 0 {
		t.Errorf("negative MaxDepth = %d, want it clamped to 0", zero.MaxDepth())
	}
	if _, _, err := zero.Compile(`{ persons { name } }`, nil); err != nil {
		t.Errorf("root-only query at MaxDepth 0: %v", err)
	}
	_, _, err = zero.Compile(`{ persons { follows { name } } }`, nil)
	if !errors.As(err, &depthErr) {
		t.Fatalf("one hop at MaxDepth 0: error is %T (%v), want *compiler.DepthExceededError", err, err)
	}
	if got := err.Error(); !strings.Contains(got, "is 1 hop from the root") {
		t.Errorf("a single hop must not be reported as %q", got)
	}
}

// TestCompileSharedLabelInterface proves an interface carrying @node compiles to
// the single shared label its implementors' tables all expose.
func TestCompileSharedLabelInterface(t *testing.T) {
	c := newInterfaceCompiler(t)
	cq, err := c.CompileQuery(`{ actors { name } }`, nil)
	if err != nil {
		t.Fatalf("CompileQuery: %v", err)
	}
	want := `SELECT v0_k, v0_c0
FROM GRAPH_TABLE (app_graph
  MATCH (v0 IS actor)
  COLUMNS (v0.id AS v0_k, v0.name AS v0_c0)
)`
	if cq.SQL != want {
		t.Errorf("SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", cq.SQL, want)
	}
}

// TestCompileLabelAlternation proves an interface without @node compiles to a
// label expression alternating over its implementors' own labels.
func TestCompileLabelAlternation(t *testing.T) {
	c := newInterfaceCompiler(t)
	cq, err := c.CompileQuery(`{ profiles { name } }`, nil)
	if err != nil {
		t.Fatalf("CompileQuery: %v", err)
	}
	want := `SELECT v0_k, v0_c0
FROM GRAPH_TABLE (app_graph
  MATCH (v0 IS bot|person)
  COLUMNS (v0.id AS v0_k, v0.name AS v0_c0)
)`
	if cq.SQL != want {
		t.Errorf("SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", cq.SQL, want)
	}
}

// TestCompileInterfaceTraversalGuard proves a guard is emitted exactly where two
// positions could bind the same row: an interface spanning persons and bots
// overlaps a person target, so `v0.id <> v1.id` is required.
func TestCompileInterfaceTraversalGuard(t *testing.T) {
	c := newInterfaceCompiler(t)
	cq, err := c.CompileQuery(`{ actors { name follows { name } } }`, nil)
	if err != nil {
		t.Fatalf("CompileQuery: %v", err)
	}
	want := `SELECT v0_k, v0_c0, v1_k, v1_c0
FROM GRAPH_TABLE (app_graph
  MATCH (v0 IS actor) -[e0 IS follows]-> (v1 IS person)
  WHERE v0.id <> v1.id
  COLUMNS (v0.id AS v0_k, v0.name AS v0_c0, v1.id AS v1_k, v1.name AS v1_c0)
)`
	if cq.SQL != want {
		t.Errorf("SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", cq.SQL, want)
	}
}

// TestCompileNoGuardBetweenDisjointTables proves the guard is not emitted where
// it cannot matter: a bot and a person can never be the same row.
func TestCompileNoGuardBetweenDisjointTables(t *testing.T) {
	c := newInterfaceCompiler(t)
	cq, err := c.CompileQuery(`{ bots { name follows { name } } }`, nil)
	if err != nil {
		t.Fatalf("CompileQuery: %v", err)
	}
	if strings.Contains(cq.SQL, "<>") {
		t.Errorf("bots and persons are disjoint tables; no guard expected:\n%s", cq.SQL)
	}
}

func TestCompileRejects(t *testing.T) {
	c := newCompiler(t)
	cases := map[string]struct {
		op   string
		vars map[string]any
	}{
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
