package compiler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/sdl"
)

const mutationSchema = `
type Person @node(label: "person") {
  id: ID!
  name: String!
}
type Mutation {
  startAgentRun(
    agentDigest: String! @column(name: "agent_digest")
    userId: String! @column(name: "user_id")
    queue: String = "agent"
    deduplicationId: String @column(name: "deduplication_id")
    priority: Int
  ): String! @function(schema: "dbos", name: "enqueue_workflow")
  sendMessage(destination: String!): Boolean!
    @function(schema: "dbos", name: "send_message", returns: VOID)
}
`

func mutationCompiler(t *testing.T) *Compiler {
	t.Helper()
	doc, err := sdl.Parse(mutationSchema)
	require.NoError(t, err)
	return New(doc)
}

// A call is a function call and nothing else: no graph name, no MATCH, no
// GRAPH_TABLE. That is what lets SPEC.md §2.2's "no mutation through a property
// graph" stand untouched while mutations exist — the two never meet.
func TestCompileMutationEmitsAPlainCall(t *testing.T) {
	c := mutationCompiler(t)

	cc, err := c.CompileMutation(`mutation { sendMessage(destination: "wf-1") }`, nil)
	require.NoError(t, err)
	assert.Equal(t, `SELECT dbos.send_message(destination => $1)`, cc.SQL)
	assert.Equal(t, []any{"wf-1"}, cc.Args)
	assert.Equal(t, sdl.ReturnVoid, cc.Returns)
	assert.NotContains(t, cc.SQL, "GRAPH_TABLE")
	assert.NotContains(t, cc.SQL, "MATCH")
}

// Named notation, not positional: the operation may write the arguments in any
// order and the call is the same, because each names its parameter.
func TestCompileMutationMapsArgumentsByName(t *testing.T) {
	c := mutationCompiler(t)

	declared, err := c.CompileMutation(
		`mutation { startAgentRun(agentDigest: "sha256:a", userId: "u-1") }`, nil)
	require.NoError(t, err)

	reversed, err := c.CompileMutation(
		`mutation { startAgentRun(userId: "u-1", agentDigest: "sha256:a") }`, nil)
	require.NoError(t, err)

	assert.Equal(t,
		`SELECT dbos.enqueue_workflow(agent_digest => $1, user_id => $2, queue => $3)`,
		declared.SQL)
	assert.Equal(t, declared.SQL, reversed.SQL,
		"argument order in the operation must not change the call")
	assert.Equal(t, declared.Args, reversed.Args)
	assert.Equal(t, []any{"sha256:a", "u-1", "agent"}, declared.Args)
}

// The SDL's own GraphQL default is applied here, explicitly. Nothing else
// applies it: the compiler parses the operation and never runs gqlparser's
// validator, so a default left to it would silently reach nothing at all — and
// `queue: String = "agent"` is the trap in the issue's own example.
func TestCompileMutationAppliesGraphQLArgumentDefaults(t *testing.T) {
	c := mutationCompiler(t)

	cc, err := c.CompileMutation(
		`mutation { startAgentRun(agentDigest: "sha256:a", userId: "u-1") }`, nil)
	require.NoError(t, err)
	assert.Contains(t, cc.SQL, "queue => $3")
	assert.Equal(t, "agent", cc.Args[2])
}

// An argument the operation document does not pass, and that declares no
// GraphQL default, is absent from the call — so the function's own SQL DEFAULT
// applies. gopgql emits no DEFAULT keyword and invents no value.
func TestCompileMutationOmitsUnpassedArguments(t *testing.T) {
	c := mutationCompiler(t)

	cc, err := c.CompileMutation(
		`mutation { startAgentRun(agentDigest: "sha256:a", userId: "u-1") }`, nil)
	require.NoError(t, err)
	assert.NotContains(t, cc.SQL, "priority")
	assert.NotContains(t, cc.SQL, "deduplication_id")
	assert.NotContains(t, cc.SQL, "DEFAULT")
	assert.Len(t, cc.Args, 3)
}

// NULL is not DEFAULT, and this is the difference proven rather than described:
// the same argument left out of the document and passed as an unset nullable
// variable produce two different statements. Only the first can reach the
// function's declared default.
func TestNullIsNotDefault(t *testing.T) {
	c := mutationCompiler(t)

	omitted, err := c.CompileMutation(
		`mutation { startAgentRun(agentDigest: "a", userId: "u") }`, nil)
	require.NoError(t, err)

	explicitNull, err := c.CompileMutation(
		`mutation Run($p: Int) { startAgentRun(agentDigest: "a", userId: "u", priority: $p) }`, nil)
	require.NoError(t, err)

	assert.NotContains(t, omitted.SQL, "priority",
		"omitted from the document: the parameter is not mentioned, so SQL DEFAULT applies")
	assert.Contains(t, explicitNull.SQL, "priority => $4",
		"passed as an unset nullable variable: the parameter is present and its value is NULL")
	assert.Equal(t, []any{"a", "u", "agent", nil}, explicitNull.Args)
}

// An unset variable declared non-null still fails: it has no value and no
// default, and NULL is not one.
func TestUnsetNonNullVariableStillFails(t *testing.T) {
	c := mutationCompiler(t)

	_, err := c.CompileMutation(
		`mutation Run($d: String!) { startAgentRun(agentDigest: $d, userId: "u") }`, nil)
	require.ErrorContains(t, err, "no value supplied for variable $d")
}

func TestCompileMutationRejections(t *testing.T) {
	c := mutationCompiler(t)

	for _, tc := range []struct {
		name    string
		op      string
		wantErr string
	}{
		{"a query", `query { persons { name } }`, "compile it with CompileQuery"},
		{"unknown field", `mutation { nope(a: "x") }`, "callable mutation fields are"},
		{"unknown argument", `mutation { sendMessage(nope: "x") }`, `has no argument "nope"`},
		{"two root fields",
			`mutation { sendMessage(destination: "a") startAgentRun(agentDigest: "b", userId: "c") }`,
			"exactly one root field"},
		{"subselection", `mutation { sendMessage(destination: "a") { id } }`, "cannot have a subselection"},
		{"explicit null on a non-null argument",
			`mutation { sendMessage(destination: null) }`, "cannot be passed as null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.CompileMutation(tc.op, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// CompileQuery still compiles queries only, and its refusal now points at the
// method that does compile a mutation instead of citing a read-only-ness that
// was never the reason a *call* could not be compiled.
func TestCompileQueryDirectsAMutationToCompileMutation(t *testing.T) {
	c := mutationCompiler(t)

	_, err := c.CompileQuery(`mutation { sendMessage(destination: "a") }`, nil)
	require.ErrorContains(t, err, "CompileMutation")
	assert.NotContains(t, err.Error(), "read-only")
}

// An unset nullable variable binds NULL in a query too — the same rule, since
// it is the same value resolution. Before mutations existed it failed, which
// made every optional argument unusable.
func TestUnsetNullableVariableBindsNullInAQuery(t *testing.T) {
	c := mutationCompiler(t)

	cq, err := c.CompileQuery(`query P($n: String) { persons(name: $n) { name } }`, nil)
	require.NoError(t, err)
	assert.Equal(t, []any{nil}, cq.Args)
}
