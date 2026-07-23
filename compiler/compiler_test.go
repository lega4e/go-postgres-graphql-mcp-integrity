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
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
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
	want := `SELECT name
FROM GRAPH_TABLE (app_graph
  MATCH (v IS person)
  COLUMNS (v.name AS name)
)`
	if sql != want {
		t.Errorf("SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", sql, want)
	}
	if len(args) != 0 {
		t.Errorf("expected no bind params in M1, got %v", args)
	}
}

func TestCompileMultipleProperties(t *testing.T) {
	c := newCompiler(t)
	cq, err := c.CompileQuery(`{ persons { id name email } }`, nil)
	if err != nil {
		t.Fatalf("CompileQuery: %v", err)
	}
	want := `SELECT id, name, email
FROM GRAPH_TABLE (app_graph
  MATCH (v IS person)
  COLUMNS (v.id AS id, v.name AS name, v.email AS email)
)`
	if cq.SQL != want {
		t.Errorf("SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", cq.SQL, want)
	}
	if cq.Projection.ResponseKey != "persons" {
		t.Errorf("response key = %q, want persons", cq.Projection.ResponseKey)
	}
	if len(cq.Projection.Fields) != 3 {
		t.Errorf("projection fields = %d, want 3", len(cq.Projection.Fields))
	}
}

func TestCompileAlias(t *testing.T) {
	c := newCompiler(t)
	cq, err := c.CompileQuery(`{ persons { fullName: name } }`, nil)
	if err != nil {
		t.Fatalf("CompileQuery: %v", err)
	}
	want := `SELECT "fullName"
FROM GRAPH_TABLE (app_graph
  MATCH (v IS person)
  COLUMNS (v.name AS "fullName")
)`
	if cq.SQL != want {
		t.Errorf("SQL mismatch:\n--- got ---\n%s\n--- want ---\n%s", cq.SQL, want)
	}
	if cq.Projection.Fields[0].ResponseKey != "fullName" {
		t.Errorf("alias response key = %q, want fullName", cq.Projection.Fields[0].ResponseKey)
	}
}

func TestCompileRejects(t *testing.T) {
	c := newCompiler(t)
	cases := map[string]string{
		"nesting":       `{ persons { follows { name } } }`,
		"arguments":     `{ persons(name: "x") { name } }`,
		"variables":     `query($n: String) { persons { name } }`,
		"unknown root":  `{ widgets { name } }`,
		"unknown field": `{ persons { nope } }`,
		"relationship":  `{ persons { follows } }`,
		"two roots":     `{ persons { name } persons { email } }`,
		"empty":         `{ persons { } }`,
	}
	for name, op := range cases {
		if _, _, err := c.Compile(op, nil); err == nil {
			t.Errorf("%s: expected error, got nil for %q", name, op)
		}
	}
}
