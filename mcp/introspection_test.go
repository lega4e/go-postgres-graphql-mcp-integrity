package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lega4e/gopgql/sdl"
)

const testSDL = `type Person implements Actor @node(label: "Person") {
  id: ID!
  name: String!
  nickname: String
  follows: [Person!]! @relationship(type: "FOLLOWS", direction: OUT)
  secret: String @ignore
}

interface Actor {
  id: ID!
  name: String!
}
`

// newTestServer builds a server with no database: every assertion below is
// about introspection, which is answered from the schema alone.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	doc, err := sdl.Parse(testSDL)
	if err != nil {
		t.Fatalf("parse SDL: %v", err)
	}
	return New(doc, testSDL, nil)
}

func introspect(t *testing.T, s *Server, query string) map[string]any {
	t.Helper()
	out, err := s.Query(context.Background(), query, nil, FormatJSON)
	if err != nil {
		t.Fatalf("introspection query failed: %v\n%s", err, query)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("introspection result is not JSON: %v\n%s", err, out)
	}
	return data
}

func TestSchemaMetaFields(t *testing.T) {
	s := newTestServer(t)
	data := introspect(t, s, `{ __schema { queryType { name } } }`)

	schema, ok := data["__schema"].(map[string]any)
	if !ok {
		t.Fatalf("no __schema in %v", data)
	}
	qt, _ := schema["queryType"].(map[string]any)
	if qt["name"] != "Query" {
		t.Fatalf("queryType.name = %v, want Query", qt["name"])
	}
}

func TestTypeByName(t *testing.T) {
	s := newTestServer(t)
	data := introspect(t, s, `{ __type(name: "Person") { name kind fields { name } } }`)

	typ, ok := data["__type"].(map[string]any)
	if !ok {
		t.Fatalf("no __type in %v", data)
	}
	if typ["kind"] != "OBJECT" {
		t.Fatalf("kind = %v, want OBJECT", typ["kind"])
	}
	names := map[string]bool{}
	for _, f := range typ["fields"].([]any) {
		names[f.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"id", "name", "nickname", "follows"} {
		if !names[want] {
			t.Errorf("Person is missing the field %q: %v", want, names)
		}
	}
	// @ignore maps to no column and no relationship, so a query selecting it
	// would not compile; introspection must not advertise it.
	if names["secret"] {
		t.Error("an @ignore field must not appear in introspection")
	}
}

func TestUnknownTypeIsNull(t *testing.T) {
	s := newTestServer(t)
	data := introspect(t, s, `{ __type(name: "Nope") { name } }`)
	if v, ok := data["__type"]; !ok || v != nil {
		t.Fatalf("__type on an unknown name = %v, want null", data["__type"])
	}
}

func TestFullIntrospectionQueryIsWellFormed(t *testing.T) {
	s := newTestServer(t)
	data := introspect(t, s, FullIntrospectionQuery)

	schema := data["__schema"].(map[string]any)
	if schema["mutationType"] != nil || schema["subscriptionType"] != nil {
		t.Error("the mapped graph is read-only; there is no mutation or subscription root")
	}
	if len(schema["directives"].([]any)) == 0 {
		t.Error("no directives reported")
	}

	types := map[string]map[string]any{}
	for _, raw := range schema["types"].([]any) {
		typ := raw.(map[string]any)
		types[typ["name"].(string)] = typ
	}
	for _, want := range []string{"Query", "Person", "Actor", "String", "__Type"} {
		if _, ok := types[want]; !ok {
			t.Errorf("__schema.types is missing %q", want)
		}
	}
	if types["Actor"]["kind"] != "INTERFACE" {
		t.Errorf("Actor kind = %v, want INTERFACE", types["Actor"]["kind"])
	}
	if len(types["Actor"]["possibleTypes"].([]any)) != 1 {
		t.Errorf("Actor possibleTypes = %v, want Person", types["Actor"]["possibleTypes"])
	}

	// `follows: [Person!]!` must come back as the wrapper chain the
	// specification defines: NON_NULL → LIST → NON_NULL → Person.
	var follows map[string]any
	for _, raw := range types["Person"]["fields"].([]any) {
		f := raw.(map[string]any)
		if f["name"] == "follows" {
			follows = f
		}
	}
	if follows == nil {
		t.Fatal("Person.follows is missing")
	}
	ref := follows["type"].(map[string]any)
	if ref["kind"] != "NON_NULL" || ref["name"] != nil {
		t.Fatalf("outer type ref = %v, want an unnamed NON_NULL", ref)
	}
	list := ref["ofType"].(map[string]any)
	if list["kind"] != "LIST" {
		t.Fatalf("second type ref = %v, want LIST", list)
	}
	inner := list["ofType"].(map[string]any)
	if inner["kind"] != "NON_NULL" {
		t.Fatalf("third type ref = %v, want NON_NULL", inner)
	}
	named := inner["ofType"].(map[string]any)
	if named["kind"] != "OBJECT" || named["name"] != "Person" {
		t.Fatalf("innermost type ref = %v, want the Person object", named)
	}
}

// A newer graphql-js IntrospectionQuery selects isOneOf; an unknown field is a
// hard error in the executor, so leaving it out would fail the whole query.
func TestIsOneOfIsAnswered(t *testing.T) {
	s := newTestServer(t)
	data := introspect(t, s, `{ __type(name: "Person") { name isOneOf } __schema { types { name isOneOf } } }`)

	typ := data["__type"].(map[string]any)
	if v, ok := typ["isOneOf"]; !ok || v != nil {
		t.Errorf("isOneOf on an object type = %v, want null", typ["isOneOf"])
	}
	if len(data["__schema"].(map[string]any)["types"].([]any)) == 0 {
		t.Error("isOneOf must not break the types listing")
	}
}

func TestQueryRootReportsWhatCompiles(t *testing.T) {
	s := newTestServer(t)
	data := introspect(t, s, `{ __type(name: "Query") { fields { name args { name type { kind name } } type { kind ofType { kind ofType { kind name } } } } } }`)

	fields := data["__type"].(map[string]any)["fields"].([]any)
	roots := map[string]map[string]any{}
	for _, raw := range fields {
		f := raw.(map[string]any)
		roots[f["name"].(string)] = f
	}
	// One root per @node table and one per interface (sdl.Document.RootFields).
	for _, want := range s.doc.RootFields() {
		if _, ok := roots[want]; !ok {
			t.Errorf("Query is missing the root field %q", want)
		}
	}
	// Root fields filter by equality on a scalar property.
	args := map[string]bool{}
	for _, raw := range roots["Persons"]["args"].([]any) {
		args[raw.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"id", "name", "nickname"} {
		if !args[want] {
			t.Errorf("Persons is missing the filter argument %q: %v", want, args)
		}
	}
	if args["follows"] || args["secret"] {
		t.Errorf("only scalar properties are filterable: %v", args)
	}
}

func TestIntrospectToolModes(t *testing.T) {
	s := newTestServer(t)

	t.Run("overview omits field definitions", func(t *testing.T) {
		out, err := s.Introspect("", false, "")
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(out), &data); err != nil {
			t.Fatal(err)
		}
		schema := data["__schema"].(map[string]any)
		if len(schema["queryType"].(map[string]any)["fields"].([]any)) == 0 {
			t.Error("the overview must name the queryable root fields")
		}
		for _, raw := range schema["types"].([]any) {
			typ := raw.(map[string]any)
			if _, ok := typ["fields"]; ok {
				t.Fatalf("the overview must omit type field definitions, got %v", typ)
			}
		}
		if !strings.Contains(out, "Person") {
			t.Error("the overview must name every mapped type")
		}
	})

	t.Run("type detail", func(t *testing.T) {
		out, err := s.Introspect("Person", false, "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `"follows"`) {
			t.Errorf("the type detail must list the type's fields:\n%s", out)
		}
	})

	t.Run("full", func(t *testing.T) {
		out, err := s.Introspect("", true, "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `"__Schema"`) {
			t.Errorf("the full result must carry the whole type system:\n%s", out)
		}
	})

	t.Run("sdl", func(t *testing.T) {
		out, err := s.Introspect("", false, FormatSDL)
		if err != nil {
			t.Fatal(err)
		}
		if out != testSDL {
			t.Errorf("the SDL format must return the document verbatim:\n%s", out)
		}
	})

	t.Run("unknown format", func(t *testing.T) {
		if _, err := s.Introspect("", false, "yaml"); err == nil {
			t.Error("an unknown format must be rejected")
		}
	})
}

func TestToolDescriptionsTeachIntrospection(t *testing.T) {
	for name, description := range map[string]string{
		ToolIntrospect: introspectDescription,
		ToolQuery:      queryDescription,
	} {
		if !strings.Contains(description, "__schema") || !strings.Contains(description, "__type") {
			t.Errorf("the %s description must name the introspection meta-fields:\n%s", name, description)
		}
	}
	if !strings.Contains(queryDescription, "{ __schema {") {
		t.Error("the query description must carry an introspection query an agent can send verbatim")
	}
}

func TestIntrospectionRejectsMarkdown(t *testing.T) {
	s := newTestServer(t)
	_, err := s.Query(context.Background(), `{ __schema { queryType { name } } }`, nil, FormatMarkdown)
	if err == nil || !strings.Contains(err.Error(), "nested") {
		t.Fatalf("markdown over an introspection result must be refused, got %v", err)
	}
}
