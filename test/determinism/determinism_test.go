// Package determinism_test asserts that gopgql's two generators — migrations
// and the typed Go client — produce byte-identical output from one input, carry
// nothing about the machine that ran them, and need no database.
//
// It is the only suite in test/ that boots no container, deliberately: what it
// checks is what a consumer's CI checks with `go generate ./... && git diff
// --exit-code`, and a version of that check which needed a live PostgreSQL would
// not be the same check.
//
// It runs as an ordinary test with no container, because that is the point — a
// consumer's CI runs `go generate ./... && git diff --exit-code`, and a check
// that could only be made against a live PostgreSQL would not be the same check.
package determinism_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/generator/client"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/sdl"
)

// determinismSchema is deliberately wide: several types, an interface, an
// unmanaged table, indexes, constraints and a Mutation. Every one of those is
// held in a map somewhere on the way through, and map iteration order is exactly
// what this file exists to catch.
const determinismSchema = `
interface Actor @node(label: "actor") {
  id: ID!
  handle: String!
}
type Person implements Actor @node(label: "person") @key(fields: ["handle"]) {
  id: ID!
  handle: String! @index(using: "btree")
  email: String @unique
  score: Int! @default(value: "0") @check(expr: "score >= 0")
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  wrote: [Post!]! @relationship(type: "wrote", direction: OUT)
}
type Bot implements Actor @node(label: "bot") {
  id: ID!
  handle: String!
  vendor: String!
}
type Post @node(label: "post") @check(expr: "title <> ''") {
  id: ID!
  title: String!
  seq: Int! @column(name: "offset")
}
type Stream @node(label: "stream", table: "streams", schema: "dbos") @readonly {
  id: ID!
  topic: String!
  seq: Int! @column(name: "offset")
}
type Mutation {
  startAgentRun(
    agentDigest: String! @column(name: "agent_digest")
    userId: String! @column(name: "user_id")
    queue: String = "agent"
    priority: Int
  ): String! @function(schema: "dbos", name: "enqueue_workflow")
  sendMessage(destination: String!): Boolean!
    @function(schema: "dbos", name: "send_message", returns: VOID)
}
`

const determinismOps = `
query ListActors($handle: String!) {
  actors(handle: $handle) { handle }
}

query ListPeople($handle: String!) {
  persons(handle: $handle) {
    handle
    email
    score
    follows { handle }
    wrote { title seq }
  }
}

mutation StartAgentRun($agentDigest: String!, $userId: String!, $priority: Int) {
  startAgentRun(agentDigest: $agentDigest, userId: $userId, priority: $priority)
}

mutation SendMessage($destination: String!) {
  sendMessage(destination: $destination)
}
`

// generateBoth runs the two generators — migrations and the typed client — into
// a fresh directory and returns every file's path (relative) and bytes.
func generateBoth(t *testing.T) map[string][]byte {
	t.Helper()
	doc, err := sdl.Parse(determinismSchema)
	require.NoError(t, err)
	model, err := generator.Build(doc, "")
	require.NoError(t, err)

	root := t.TempDir()
	migrations := filepath.Join(root, "migrations")
	_, err = migrate.Generate(migrations, model, "schema", migrate.Halves{})
	require.NoError(t, err)

	opsDir := filepath.Join(root, "operations")
	require.NoError(t, os.MkdirAll(opsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(opsDir, "ops.graphql"), []byte(determinismOps), 0o644))

	sources, err := client.Load(opsDir)
	require.NoError(t, err)
	files, err := client.Generate(doc, sources, client.Options{Package: "gen"})
	require.NoError(t, err)
	_, err = client.Write(filepath.Join(root, "gen"), files)
	require.NoError(t, err)

	out := map[string][]byte{}
	require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, "operations"+string(filepath.Separator)) {
			return nil // the input, not an artifact
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	}))
	require.NotEmpty(t, out)
	return out
}

// TestGenerationIsByteIdentical is the check that catches map iteration reaching
// output. Two runs from one input, into two directories, must produce the same
// trees byte for byte — because a consumer's CI is `go generate ./... && git
// diff --exit-code`, and a generation that reorders anything fails it.
//
// Go randomises map iteration order per range, so repeating the generation in
// one process is a real trial rather than a formality.
func TestGenerationIsByteIdentical(t *testing.T) {
	first := generateBoth(t)
	for i := range 4 {
		again := generateBoth(t)

		firstNames := sortedKeys(first)
		againNames := sortedKeys(again)
		require.Equal(t, firstNames, againNames, "run %d produced a different set of files", i+2)
		for _, name := range firstNames {
			assert.Equal(t, string(first[name]), string(again[name]),
				"run %d produced different bytes for %s", i+2, name)
		}
	}
}

// timestampish matches the shapes that make a generated artifact differ between
// two machines or two minutes. A generated file containing any of them cannot
// pass `git diff --exit-code` in somebody else's CI.
var timestampish = []struct {
	what string
	re   *regexp.Regexp
}{
	{"an ISO-8601 timestamp", regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}`)},
	{"a unix-epoch-sized number", regexp.MustCompile(`\b1[6-9]\d{8}\b`)},
	{"an absolute path", regexp.MustCompile(`(?m)(^|[^\w/])/(Users|home|tmp|var|private)/`)},
	{"a Go build path", regexp.MustCompile(`/go/pkg/mod/`)},
}

func TestGeneratedArtifactsCarryNoEnvironment(t *testing.T) {
	files := generateBoth(t)

	host, err := os.Hostname()
	require.NoError(t, err)
	user := os.Getenv("USER")

	for _, name := range sortedKeys(files) {
		body := string(files[name])
		for _, probe := range timestampish {
			assert.NotRegexp(t, probe.re, body, "%s contains %s", name, probe.what)
		}
		if host != "" {
			assert.NotContains(t, body, host, "%s names the machine it was generated on", name)
		}
		if user != "" && len(user) > 2 {
			assert.NotContains(t, body, user, "%s names the user who generated it", name)
		}
	}
}

// Migration filenames are sequence-numbered, never timestamped (SPEC.md §3.0).
// A timestamped name is deterministic-looking and is not: two developers
// generating the same delta would produce two files.
func TestMigrationFilenamesAreSequenceNumbered(t *testing.T) {
	files := generateBoth(t)

	nameRe := regexp.MustCompile(`^migrations/\d{4}_[a-z0-9_]+\.sql$`)
	seen := 0
	for _, name := range sortedKeys(files) {
		if !strings.HasPrefix(name, "migrations/") {
			continue
		}
		seen++
		assert.Regexp(t, nameRe, name)
	}
	assert.Positive(t, seen, "the fixture must actually generate migrations")
}

// TestGenerationNeedsNoDatabase runs both generators in a subprocess whose
// environment names no database at all, and whose PGHOST points at a socket
// directory that does not exist — so a stray connection attempt fails loudly
// rather than finding a local server and passing by accident.
func TestGenerationNeedsNoDatabase(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "gopgql")
	build := exec.Command("go", "build", "-o", bin, "./cmd/gopgql")
	build.Dir = repoRoot(t)
	out, err := build.CombinedOutput()
	require.NoError(t, err, "build gopgql: %s", out)

	work := t.TempDir()
	sdlPath := filepath.Join(work, "schema.graphql")
	require.NoError(t, os.WriteFile(sdlPath, []byte(determinismSchema), 0o644))
	opsDir := filepath.Join(work, "operations")
	require.NoError(t, os.MkdirAll(opsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(opsDir, "ops.graphql"), []byte(determinismOps), 0o644))

	// A deliberately hostile environment: no DSN, and a PGHOST that cannot
	// resolve to anything.
	env := append(os.Environ(),
		"GOPGQL_DSN=",
		"PGHOST="+filepath.Join(work, "no-such-socket-dir"),
		"PGPORT=1",
		"PGDATABASE=",
		"PGUSER=",
	)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"generate", []string{"generate", "--sdl", sdlPath, "--dir", filepath.Join(work, "migrations")}},
		{"generate client", []string{"generate", "client",
			"--sdl", sdlPath, "--operations", opsDir, "--out", filepath.Join(work, "gen")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bin, tc.args...)
			cmd.Dir = work
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s must succeed with no database reachable: %s", tc.name, out)
			assert.Contains(t, string(out), "wrote")
		})
	}
}

// repoRoot walks up from the test's directory to the module root, so the
// subprocess build above does not depend on where `go test` was invoked.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "no go.mod above %s", dir)
		dir = parent
	}
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slicesSort(out)
	return out
}

// slicesSort keeps the import list free of a package used once.
func slicesSort(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
