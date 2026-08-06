package sdlsource_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/internal/sdlsource"
)

// write puts source in dir under name and returns the path.
func write(t *testing.T, dir, name, source string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
	return path
}

// TestLoadKeepsTheOrderTheFlagsWereGivenIn: --sdl is repeatable, and the order
// is the operator's, not the filesystem's.
func TestLoadKeepsTheOrderTheFlagsWereGivenIn(t *testing.T) {
	dir := t.TempDir()
	// Named so that alphabetical order and given order disagree: a loader that
	// quietly sorted everything would pass a test whose files sort the way they
	// were passed.
	second := write(t, dir, "a_second.graphql", "second\n")
	first := write(t, dir, "z_first.graphql", "first\n")

	src, err := sdlsource.Load([]string{first, second})
	require.NoError(t, err)

	assert.Equal(t, []string{first, second}, src.Paths)
	assert.Equal(t, "first\nsecond\n", src.Text)
	require.Len(t, src.Sources, 2)
	assert.Equal(t, first, src.Sources[0].Name,
		"each document keeps its own path, so an error names the file to open")
}

// TestLoadSortsADirectory: a directory's order comes from the file names, never
// from os.ReadDir — an SDL whose order depended on the filesystem would generate
// a different migration on a different machine.
func TestLoadSortsADirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "20_second.graphql", "second\n")
	write(t, dir, "10_first.graphql", "first\n")
	write(t, dir, "30_third.graphql", "third\n")
	// Not *.graphql, and a subdirectory: neither is read.
	write(t, dir, "notes.md", "ignored\n")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o750))
	write(t, filepath.Join(dir, "nested"), "deep.graphql", "deep\n")

	src, err := sdlsource.Load([]string{dir})
	require.NoError(t, err)

	assert.Equal(t, []string{
		filepath.Join(dir, "10_first.graphql"),
		filepath.Join(dir, "20_second.graphql"),
		filepath.Join(dir, "30_third.graphql"),
	}, src.Paths, "sorted by name, and not searched recursively")
	assert.Equal(t, "first\nsecond\nthird\n", src.Text)
}

// TestLoadTerminatesEveryDocument: a file with no trailing newline must not run
// its last line into the next file's first one, or concatenating would mean
// something the separate documents do not.
func TestLoadTerminatesEveryDocument(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "a.graphql", "type A")
	b := write(t, dir, "b.graphql", "type B")

	src, err := sdlsource.Load([]string{a, b})
	require.NoError(t, err)
	assert.Equal(t, "type A\ntype B\n", src.Text)
}

// TestLoadRefusesTheSameFileTwice: gqlparser's own symptom is every type in the
// file reported as redeclared, which reads as a broken schema rather than as a
// repeated flag.
func TestLoadRefusesTheSameFileTwice(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "a.graphql", "type A\n")

	_, err := sdlsource.Load([]string{path, filepath.Join(dir, ".", "a.graphql")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "same file")
}

func TestLoadReportsWhatItCouldNotRead(t *testing.T) {
	t.Run("no paths at all", func(t *testing.T) {
		_, err := sdlsource.Load(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no schema")
	})

	t.Run("a path that does not exist", func(t *testing.T) {
		_, err := sdlsource.Load([]string{filepath.Join(t.TempDir(), "absent.graphql")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read schema")
	})

	t.Run("a directory holding no *.graphql", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "readme.md", "nothing here\n")
		_, err := sdlsource.Load([]string{dir})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no *.graphql documents",
			"an empty directory must not read as an empty schema")
	})
}

// TestEnvPathsSplitsOnThePlatformSeparator: GOPGQL_SDL carries one path or
// several, and the separator is the one the platform already uses for a path
// list — the one that cannot collide with a path.
func TestEnvPathsSplitsOnThePlatformSeparator(t *testing.T) {
	sep := string(filepath.ListSeparator)

	t.Setenv(sdlsource.EnvVar, "one.graphql")
	assert.Equal(t, []string{"one.graphql"}, sdlsource.EnvPaths(sdlsource.EnvVar))

	t.Setenv(sdlsource.EnvVar, "one.graphql"+sep+"two.graphql")
	assert.Equal(t, []string{"one.graphql", "two.graphql"}, sdlsource.EnvPaths(sdlsource.EnvVar))

	// An empty element is dropped rather than becoming an empty path, which
	// would fail later with a message about "" instead of about the variable.
	t.Setenv(sdlsource.EnvVar, sep+"one.graphql"+sep)
	assert.Equal(t, []string{"one.graphql"}, sdlsource.EnvPaths(sdlsource.EnvVar))

	t.Setenv(sdlsource.EnvVar, "")
	assert.Empty(t, sdlsource.EnvPaths(sdlsource.EnvVar))
}

// TestPathListAppends: the flag has to accumulate, which is the entire reason
// it is a flag.Value rather than a string.
func TestPathListAppends(t *testing.T) {
	var p sdlsource.PathList
	require.NoError(t, p.Set("a.graphql"))
	require.NoError(t, p.Set("schema/"))
	assert.Equal(t, sdlsource.PathList{"a.graphql", "schema/"}, p)
	assert.Error(t, p.Set(""), "an empty --sdl is a mistake, not a path")
}

// TestDisplayNamesEveryFile: a message about the schema as a whole has to say
// what was read, or "already up to date with schema.graphql" would name one of
// several files.
func TestDisplayNamesEveryFile(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "a.graphql", "type A\n")
	b := write(t, dir, "b.graphql", "type B\n")

	one, err := sdlsource.Load([]string{a})
	require.NoError(t, err)
	assert.Equal(t, a, one.Display())

	two, err := sdlsource.Load([]string{a, b})
	require.NoError(t, err)
	assert.Contains(t, two.Display(), a)
	assert.Contains(t, two.Display(), b)
	assert.Contains(t, two.Display(), "2 files")
}
