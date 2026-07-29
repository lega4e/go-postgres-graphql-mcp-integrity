package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote. The version line goes to stdout by design, unlike this command's
// diagnostics — a `--version` answer piped into a script is the point.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()

	fn()
	require.NoError(t, w.Close())
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(out)
}

// TestVersionDefaults pins the values an unstamped `go build` leaves behind.
// The release pipeline overrides exactly these three symbols, so a rename here
// silently produces binaries that report "dev" for every release.
func TestVersionDefaults(t *testing.T) {
	assert.Equal(t, "dev", version)
	assert.Equal(t, "none", commit)
	assert.Equal(t, "unknown", date)
}

// TestVersionNeedsNoSchemaOrDatabase is the property that makes the flag usable
// as an image smoke test: it is answered before the environment is resolved, so
// it succeeds where every other invocation of this server would fail. Both flag
// spellings are covered because the stdlib flag package accepts both and a
// Dockerfile HEALTHCHECK may use either.
func TestVersionNeedsNoSchemaOrDatabase(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{name: "double dash", argv: []string{"--version"}},
		{name: "single dash", argv: []string{"-version"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Not merely absent from the test's own environment — explicitly
			// empty, so an exported GOPGQL_DSN in a developer's shell cannot
			// make this pass for the wrong reason.
			t.Setenv("GOPGQL_SDL", "")
			t.Setenv("GOPGQL_DSN", "")

			var err error
			out := captureStdout(t, func() { err = run(tc.argv) })
			require.NoError(t, err)

			line := strings.TrimSpace(out)
			assert.Contains(t, line, "gopgql-mcp", "the line has to say which binary it came from")
			// A build that stamped only some of the three is a build nobody can
			// trace, so each one is asserted where it appears.
			assert.Contains(t, line, version)
			assert.Contains(t, line, "commit "+commit)
			assert.Contains(t, line, "built "+date)
		})
	}
}

// TestNoVersionFlagStillValidates guards the flag's blast radius: adding it must
// not turn a normal invocation into an early return. With no schema the server
// must still refuse to start, exactly as before.
func TestNoVersionFlagStillValidates(t *testing.T) {
	t.Setenv("GOPGQL_SDL", "")
	t.Setenv("GOPGQL_DSN", "")

	err := run(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--sdl")
}
