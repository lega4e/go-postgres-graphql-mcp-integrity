package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/sdl"
)

const clientSchema = `
type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
}
type Mutation {
  appendEvent(
    streamId: String! @column(name: "stream_id")
    payload: JSON!
    queue: String = "agent"
  ): Boolean! @function(schema: "dbos", name: "append_event", returns: VOID)
  startAgentRun(
    agentDigest: String! @column(name: "agent_digest")
  ): String! @function(schema: "dbos", name: "enqueue_workflow")
}
`

const clientOps = `
query ListPeople($name: String!) {
  persons(name: $name) {
    name
    email
    follows { name }
  }
}

mutation AppendEvent($streamId: String!, $payload: JSON!) {
  appendEvent(streamId: $streamId, payload: $payload)
}

mutation StartAgentRun($agentDigest: String!) {
  startAgentRun(agentDigest: $agentDigest)
}
`

func generate(t *testing.T) string {
	t.Helper()
	doc, err := sdl.Parse(clientSchema)
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ops.graphql"), []byte(clientOps), 0o644))

	sources, err := Load(dir)
	require.NoError(t, err)
	files, err := Generate(doc, sources, Options{Package: "testclient"})
	require.NoError(t, err)
	require.Len(t, files, 1)
	return string(files[0].Content)
}

// The signature is the requirement: every generated method takes the handle as
// its second parameter, so the operation runs in whatever transaction the caller
// already holds. A client that opened its own connection could not participate
// in the caller's commit.
func TestEveryMethodTakesTheCallersHandle(t *testing.T) {
	src := generate(t)

	assert.Contains(t, src,
		"func (c *Client) AppendEvent(ctx context.Context, h exec.Handle, in AppendEventInput) error")
	assert.Contains(t, src,
		"func (c *Client) StartAgentRun(ctx context.Context, h exec.Handle, in StartAgentRunInput) (string, error)")
	assert.Contains(t, src,
		"func (c *Client) ListPeople(ctx context.Context, h exec.Handle, in ListPeopleInput) ([]ListPeoplePerson, error)")

	assert.Contains(t, src, "type Client struct{}")
	assert.NotContains(t, src, "pgxpool",
		"the generated client opens no connection and holds no pool")
	assert.NotContains(t, src, "OpenReadOnly")
}

// The SQL is baked at generate time and the bind parameters are wired from the
// input struct, in the compiler's own $-order. Nothing is compiled at run time.
func TestSQLIsBakedAndArgumentsAreWired(t *testing.T) {
	src := generate(t)

	assert.Contains(t, src,
		`const appendEventSQL = "SELECT dbos.append_event(stream_id => $1, payload => $2, queue => $3)"`)
	assert.Contains(t, src, `Args:     []any{in.StreamId, in.Payload, "agent"}`,
		"a variable becomes an input field; the SDL's GraphQL default becomes a literal")
	assert.Contains(t, src, "const listPeopleSQL = ")
	assert.Contains(t, src, "var listPeopleProjection = compiler.Projection{")
	assert.NotContains(t, src, "CompileQuery(", "no operation is compiled at run time")
	assert.NotContains(t, src, "CompileMutation(")
}

// Results are assigned field by field from what exec returned, generated. A
// nullable field is a pointer, so a NULL and a zero value stay tellable apart.
func TestResultsAreAssignedNotDecoded(t *testing.T) {
	src := generate(t)

	assert.Contains(t, src, "type ListPeoplePerson struct {")
	assert.Contains(t, src, "\tName    string")
	assert.Contains(t, src, "\tEmail   *string")
	assert.Contains(t, src, "\tFollows []ListPeoplePersonFollows")
	assert.Contains(t, src, `o.Name, err = gopgqlValue(at+".name", row["name"], gopgqlAsString)`)
	assert.Contains(t, src, `o.Email, err = gopgqlPointer(at+".email", row["email"], gopgqlAsString)`)
	assert.NotContains(t, src, "reflect.")
	assert.NotContains(t, src, "json:\"", "nothing is decoded through a struct tag")
}

// A void function's method returns only whether the call succeeded, which is
// the shape the consumer asked for.
func TestVoidMutationReturnsOnlyAnError(t *testing.T) {
	src := generate(t)

	assert.Contains(t, src, "Returns:  sdl.ReturnVoid")
	assert.Contains(t, src, "Returns:  sdl.ReturnScalar")
	assert.Contains(t, src, "\t_, err := exec.Call(ctx, h, &compiler.CompiledCall{")
}

// numericSchema is every scalar whose canonical response form is not the Go type
// pgx scans it as, including the only way `numeric` is reachable at all: a
// @column(type:) override on a Float.
const numericSchema = `
type Reading @node(label: "reading") {
  id: ID!
  count: Int!
  approx: Float!
  exact: Float! @column(type: "numeric(10,2)")
  taken: DateTime!
}
`

// The decoders a numeric leaf is assigned through, and the import they need.
//
// json.Number is the canonical form of every Int, Float and numeric leaf, so a
// generated client that could not name the type could not read its own results —
// which is what issue #51 was.
func TestNumericLeavesAreDecodedThroughTheNumberDecoders(t *testing.T) {
	doc, err := sdl.Parse(numericSchema)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ops.graphql"),
		[]byte("query Readings { readings { count approx exact taken } }"), 0o644))
	sources, err := Load(dir)
	require.NoError(t, err)
	files, err := Generate(doc, sources, Options{Package: "testclient"})
	require.NoError(t, err)
	src := string(files[0].Content)

	assert.Contains(t, src, "\t\"encoding/json\"\n", "json.Number is named in every generated client")
	assert.Contains(t, src, `o.Count, err = gopgqlValue(at+".count", row["count"], gopgqlAsInt64)`)
	assert.Contains(t, src, `o.Approx, err = gopgqlValue(at+".approx", row["approx"], gopgqlAsFloat64)`)
	assert.Contains(t, src, `o.Exact, err = gopgqlValue(at+".exact", row["exact"], gopgqlAsFloat64)`,
		"a numeric-backed Float is a float64 field decoded like any other Float")
	assert.Contains(t, src, `o.Taken, err = gopgqlValue(at+".taken", row["taken"], gopgqlAsTime)`)
	assert.Contains(t, src, "Scalar: compiler.ScalarNumeric",
		"the baked projection keeps the override's canonical form, which is what the shaper reads")
}

// Numeric is a ScalarKind, not a GraphQL scalar: compiler.classifyScalar reaches
// it only through @column(type: "numeric"), never from a type name. Written as a
// type name it is refused here, with the field named — the outcome a schema
// author can act on, and the reason scalarGo has no "Numeric" key to look up.
func TestNumericWrittenAsAGraphQLTypeIsRefusedAtGenerateTime(t *testing.T) {
	doc, err := sdl.Parse(`
scalar Numeric
type Product @node(label: "product") {
  id: ID!
  price: Numeric!
}
`)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ops.graphql"),
		[]byte("query Products { products { price } }"), 0o644))
	sources, err := Load(dir)
	require.NoError(t, err)

	_, err = Generate(doc, sources, Options{})
	require.ErrorContains(t, err, "Product.price is Numeric, which has no scalar mapping")
}

func TestGeneratedFileCarriesTheGeneratedHeader(t *testing.T) {
	src := generate(t)
	assert.True(t, strings.HasPrefix(src, "// Code generated by gopgql. DO NOT EDIT.\n"),
		"Go's own convention is what makes tooling skip the file and a reviewer stop reading it")
}

// A compile error stays a generate-time error. An unknown root field, a
// selection past the depth ceiling and an unknown mutation all fail here rather
// than on a request.
func TestCompileErrorsFailGeneration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ops     string
		wantErr string
	}{
		{"unknown root field", `query Q { nope { name } }`, "unknown root field"},
		{"unknown mutation", `mutation M { nope(a: "x") }`, "unknown mutation field"},
		{"anonymous operation", `query { persons { name } }`, "anonymous operation"},
		{"depth ceiling", `query Q {
			persons { follows { follows { follows { follows { name } } } } }
		}`, "past MaxDepth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := sdl.Parse(clientSchema)
			require.NoError(t, err)
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "ops.graphql"), []byte(tc.ops), 0o644))
			sources, err := Load(dir)
			require.NoError(t, err)
			_, err = Generate(doc, sources, Options{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestDuplicateOperationNameNamesBothFiles(t *testing.T) {
	doc, err := sdl.Parse(clientSchema)
	require.NoError(t, err)

	dir := t.TempDir()
	op := "query ListPeople($name: String!) { persons(name: $name) { name } }"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.graphql"), []byte(op), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.graphql"), []byte(op), 0o644))

	sources, err := Load(dir)
	require.NoError(t, err)
	_, err = Generate(doc, sources, Options{})
	require.ErrorContains(t, err, "a.graphql")
	assert.ErrorContains(t, err, "b.graphql")
}

// Load's contract, stated as a test because two implementations of "a directory
// of operations" would otherwise differ exactly here.
func TestLoadReadsOnlyGraphQLFilesAndDoesNotRecurse(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.graphql"), []byte("query B { persons { name } }"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.graphql"), []byte("query A { persons { name } }"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.md"), []byte("ignored"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nested"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nested", "c.graphql"), []byte("query C { persons { name } }"), 0o644))

	sources, err := Load(dir)
	require.NoError(t, err)
	require.Len(t, sources, 2)
	assert.Equal(t, "a.graphql", filepath.Base(sources[0].Path), "files are read in sorted order")
	assert.Equal(t, "b.graphql", filepath.Base(sources[1].Path))
}
