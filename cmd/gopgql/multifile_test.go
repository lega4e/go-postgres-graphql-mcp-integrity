package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two halves of gopgql#54 at the level a consumer meets them: the command
// line.
//
// AgentIQ wrote an ~80-line tool (tools/sdlmerge) whose only job was to
// concatenate SDL files before every generate, because --sdl took one path. What
// deletes that tool is not "gopgql can read two files" but the stronger claim
// that reading two files means exactly what concatenating them meant — otherwise
// the workaround has to stay as the thing whose output is trusted.

// The AgentIQ shape: a read-only PostgreSQL schema owned by DBOS, an owned one,
// and a relationship crossing between them. Two files, because the boundary
// between "what this service migrates" and "what it only reads" is the most
// important fact about the repository, and one property graph, because a
// property graph is what joins them.
const (
	readOnlyPart = `type Workflow @node(label: "workflow", table: "workflow_status", schema: "dbos") @readonly {
  id: ID! @column(name: "workflow_uuid")
  status: String!
  sessions: [Session!]! @relationship(type: "HAS_SESSION", direction: OUT,
                                      table: "session", schema: "agentiq",
                                      sourceKey: ["workflow_uuid"], destKey: ["id"])
}
`
	ownedPart = `type Session @node(label: "session", table: "session", schema: "agentiq") {
  id: ID!
  transcript: JSON
}
`
)

// writeFile puts source in dir under name and returns the path.
func writeFile(t *testing.T, dir, name, source string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
	return path
}

// generatedDir reads every file a migration directory holds, keyed by base name.
// The whole directory is compared, so a generation that wrote an extra file — or
// one fewer — fails as loudly as one that wrote different bytes.
func generatedDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "read %s", dir)
	out := map[string]string{}
	for _, e := range entries {
		body, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // a temp dir the test just wrote
		require.NoError(t, err)
		out[e.Name()] = string(body)
	}
	require.NotEmpty(t, out, "a generation that wrote nothing proves nothing")
	return out
}

// TestMultipleFilesEqualTheirConcatenation is the acceptance property, tested as
// a property: generate from two files, generate from the two files joined, and
// compare the bytes of everything written.
func TestMultipleFilesEqualTheirConcatenation(t *testing.T) {
	root := t.TempDir()

	splitDir := filepath.Join(root, "split-migrations")
	require.NoError(t, run([]string{"generate",
		"--sdl", writeFile(t, root, "00-dbos.graphql", readOnlyPart),
		"--sdl", writeFile(t, root, "10-agentiq.graphql", ownedPart),
		"--dir", splitDir, "--name", "schema",
	}))

	joinedDir := filepath.Join(root, "joined-migrations")
	require.NoError(t, run([]string{"generate",
		"--sdl", writeFile(t, root, "joined.graphql", readOnlyPart+ownedPart),
		"--dir", joinedDir, "--name", "schema",
	}))

	assert.Equal(t, generatedDir(t, joinedDir), generatedDir(t, splitDir),
		"a multi-file invocation must produce the same output as the concatenation of those files; "+
			"anything less and a consumer has to keep concatenating the files itself")
}

// TestTheGraphSpansSchemasDeclaredInSeparateFiles is the requirement the flag
// exists for. The two PostgreSQL schemas are declared in two files, and the one
// CREATE PROPERTY GRAPH has to name both.
func TestTheGraphSpansSchemasDeclaredInSeparateFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "migrations")
	require.NoError(t, run([]string{"generate",
		"--sdl", writeFile(t, root, "00-dbos.graphql", readOnlyPart),
		"--sdl", writeFile(t, root, "10-agentiq.graphql", ownedPart),
		"--dir", dir,
	}))

	var graph string
	for name, body := range generatedDir(t, dir) {
		if filepath.Ext(name) == ".sql" && strings.Contains(body, "CREATE PROPERTY GRAPH") {
			graph = body
		}
	}
	require.NotEmpty(t, graph, "no CREATE PROPERTY GRAPH was written")

	assert.Contains(t, graph, "dbos.workflow_status", "the read-only file's table")
	assert.Contains(t, graph, "agentiq.session", "the owned file's table")
	assert.Contains(t, graph, "HAS_SESSION",
		"the edge joining them, declared in one file and resolved against the other")
}

// TestADirectoryIsReadInSortedOrder: the directory form has to be reproducible,
// and it has to mean the same thing as naming its files.
func TestADirectoryIsReadInSortedOrder(t *testing.T) {
	root := t.TempDir()
	schemaDir := filepath.Join(root, "schema")
	require.NoError(t, os.Mkdir(schemaDir, 0o750))
	writeFile(t, schemaDir, "00-dbos.graphql", readOnlyPart)
	writeFile(t, schemaDir, "10-agentiq.graphql", ownedPart)
	// Neither is read: only *.graphql, and only the top level.
	writeFile(t, schemaDir, "README.md", "notes\n")

	dirForm := filepath.Join(root, "dir-migrations")
	require.NoError(t, run([]string{"generate", "--sdl", schemaDir, "--dir", dirForm}))

	named := filepath.Join(root, "named-migrations")
	require.NoError(t, run([]string{"generate",
		"--sdl", filepath.Join(schemaDir, "00-dbos.graphql"),
		"--sdl", filepath.Join(schemaDir, "10-agentiq.graphql"),
		"--dir", named,
	}))

	assert.Equal(t, generatedDir(t, named), generatedDir(t, dirForm),
		"--sdl <dir> is --sdl of its *.graphql files in sorted order, and nothing else")
}

// TestGeneratingTwiceFromSeveralFilesWritesNothingTheSecondTime: the multi-file
// path must not reintroduce the silent no-op in either direction — it has to
// write on the first run and stay quiet on the second.
func TestGeneratingTwiceFromSeveralFilesWritesNothingTheSecondTime(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "migrations")
	argv := []string{"generate",
		"--sdl", writeFile(t, root, "00-dbos.graphql", readOnlyPart),
		"--sdl", writeFile(t, root, "10-agentiq.graphql", ownedPart),
		"--dir", dir,
	}

	require.NoError(t, run(argv))
	first := generatedDir(t, dir)

	require.NoError(t, run(argv))
	assert.Equal(t, first, generatedDir(t, dir),
		"a schema that has not changed proposes nothing, however many files it is in")
}

// TestGlobalJSONTypeReachesTheDDL: the global setting is only real if it lands
// in the generated DDL, and only useful if the per-column escape still wins.
func TestGlobalJSONTypeReachesTheDDL(t *testing.T) {
	const src = `type Doc @node(label: "doc") {
  id: ID!
  verbatim: JSON
  indexed: JSON @column(type: "jsonb")
}
`
	for _, tc := range []struct {
		name    string
		argv    []string
		env     string
		wantCol string
	}{
		{name: "default", wantCol: "verbatim jsonb"},
		{name: "flag", argv: []string{"--json-type", "json"}, wantCol: "verbatim json"},
		{name: "environment", env: "json", wantCol: "verbatim json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("GOPGQL_JSON_TYPE", tc.env)
			}
			root := t.TempDir()
			dir := filepath.Join(root, "migrations")
			argv := append([]string{"generate", "--sdl", writeFile(t, root, "s.graphql", src), "--dir", dir}, tc.argv...)
			require.NoError(t, run(argv))

			tables := tablesBody(t, dir)
			assert.Contains(t, tables, tc.wantCol)
			assert.Contains(t, tables, "indexed jsonb",
				`@column(type:) still overrides the global default per column`)
		})
	}
}

// TestChangingTheGlobalJSONTypeMigratesTheDirectory: the setting has to work on
// a directory that already exists, or it is a greenfield-only setting — and the
// schemas that need it most are the deployed ones.
func TestChangingTheGlobalJSONTypeMigratesTheDirectory(t *testing.T) {
	const src = `type Doc @node(label: "doc") {
  id: ID!
  payload: JSON
}
`
	root := t.TempDir()
	dir := filepath.Join(root, "migrations")
	sdlPath := writeFile(t, root, "s.graphql", src)

	require.NoError(t, run([]string{"generate", "--sdl", sdlPath, "--dir", dir, "--name", "init"}))
	require.Contains(t, tablesBody(t, dir), "payload jsonb")
	before := len(generatedDir(t, dir))

	require.NoError(t, run([]string{"generate", "--sdl", sdlPath, "--dir", dir,
		"--name", "json", "--json-type", "json"}))
	after := generatedDir(t, dir)
	require.Greater(t, len(after), before,
		"changing the JSON type must write a migration, not report the directory up to date")

	var alter string
	for name, body := range after {
		if strings.Contains(body, "ALTER COLUMN payload TYPE json") {
			alter = name
		}
	}
	assert.NotEmpty(t, alter, "the delta has to move the column, and keep its rows: %v", sortedKeys(after))
}

// TestRepeatedSDLIsRefused: naming one file twice is a mistake whose natural
// symptom — every type in it reported as redeclared — reads as a broken schema.
func TestRepeatedSDLIsRefused(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "s.graphql", ownedPart)
	err := run([]string{"generate", "--sdl", path, "--sdl", path,
		"--dir", filepath.Join(root, "migrations")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "same file")
}

// TestUsageDocumentsTheRepeatableFlagAndTheJSONType: both halves of gopgql#54
// have to be discoverable from --help, which is where an operator looks first.
func TestUsageDocumentsTheRepeatableFlagAndTheJSONType(t *testing.T) {
	assert.Contains(t, usage, "--json-type")
	assert.Contains(t, usage, "GOPGQL_JSON_TYPE")
	assert.Contains(t, usage, "Repeatable")
	assert.Contains(t, usage, "are read in sorted order (not recursive)")
	assert.Contains(t, usage, "@column(type: ...) still wins",
		"the escape from the global default has to be named alongside it")
}

// tablesBody returns the body of the migration holding the table DDL.
func tablesBody(t *testing.T, dir string) string {
	t.Helper()
	files := generatedDir(t, dir)
	for _, name := range sortedKeys(files) {
		if strings.Contains(files[name], "CREATE TABLE") {
			return files[name]
		}
	}
	t.Fatalf("no CREATE TABLE in %s: %v", dir, sortedKeys(files))
	return ""
}

// sortedKeys names a directory's files in a stable order, for a failure message
// and for picking the first match deterministically.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
