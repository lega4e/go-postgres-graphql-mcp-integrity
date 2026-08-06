package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/conform"
	"github.com/lega4e/gopgql/generator"
)

// minimalSDL is the smallest schema generator.Build accepts: one @node type
// with the surrogate id every vertex needs (SPEC.md §5.1).
const minimalSDL = `
type Person @node(label: "person") {
  id: ID!
  name: String!
}
`

// unreachableDSN points at a port nothing listens on, so a connection attempt
// is refused rather than hanging. It is a loopback address on purpose: the
// test must not depend on the network resolving or timing out.
const unreachableDSN = "postgres://gopgql@127.0.0.1:1/gopgql?sslmode=disable&connect_timeout=1"

// writeSDL puts a schema in a temp file and returns its path.
func writeSDL(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.graphql")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
	return path
}

// cells splits a rendered table row into its values, discarding the alignment
// padding. Asserting on cells rather than on the exact string keeps the tests
// about the content of a finding, so re-tuning the column widths does not fail
// them.
func cells(line string) []string { return strings.Fields(line) }

// TestExitCodeSeparatesDriftFromFailure pins the contract the conform
// subcommand exists to provide: a pipeline can tell "the database drifted"
// from "the check never ran" by status alone.
func TestExitCodeSeparatesDriftFromFailure(t *testing.T) {
	drift := &driftError{graph: "app_graph", sdlPath: "schema.graphql", findings: 2}

	assert.Equal(t, exitDrift, exitCode(drift))
	// Wrapping must not lose the classification — callers up the stack add
	// context to errors routinely.
	assert.Equal(t, exitDrift, exitCode(fmt.Errorf("conform: %w", drift)))

	assert.Equal(t, exitFailure, exitCode(errors.New("connect to the database: refused")))
	assert.Equal(t, exitFailure, exitCode(fmt.Errorf("%w: %q", conform.ErrGraphNotFound, "app_graph")))
}

// TestDriftErrorNamesGraphSchemaAndCount checks the one-line summary an
// operator sees on stderr identifies which graph and which SDL disagreed.
func TestDriftErrorNamesGraphSchemaAndCount(t *testing.T) {
	one := &driftError{graph: "app_graph", sdlPath: "schema.graphql", findings: 1}
	assert.Equal(t, `property graph "app_graph" has drifted from schema.graphql: 1 finding`, one.Error())

	many := &driftError{graph: "app_graph", sdlPath: "schema.graphql", findings: 3}
	assert.Equal(t, `property graph "app_graph" has drifted from schema.graphql: 3 findings`, many.Error())
}

// TestRenderFindingsKeepsEveryFieldOnOneLine asserts the rendering does not
// drop the kind — the field a reader acts on — nor conflate "nothing there"
// with an empty cell, for any of the five kinds.
func TestRenderFindingsKeepsEveryFieldOnOneLine(t *testing.T) {
	report := conform.Report{Findings: []conform.Finding{
		{Kind: conform.MissingElement, Element: "company", Want: "company"},
		{Kind: conform.LabelMismatch, Element: "person", Want: "actor, person", Got: "human"},
		{Kind: conform.MissingProperty, Element: "person", Property: "email", Want: "email"},
		{Kind: conform.UnexpectedProperty, Element: "person", Property: "ssn", Got: "ssn"},
		{Kind: conform.UnexpectedElement, Element: "robot", Got: "robot"},
	}}

	var out strings.Builder
	renderFindings(&out, report)

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	require.Len(t, lines, 1+len(report.Findings), "a header plus one line per finding")
	assert.Equal(t, []string{"KIND", "ELEMENT", "PROPERTY", "SDL", "DATABASE"}, cells(lines[0]))

	// "actor, person" carries a space, so compare that row whole rather than
	// by whitespace-split cells.
	assert.Equal(t, []string{"MissingElement", "company", "-", "company", "-"}, cells(lines[1]))
	assert.Regexp(t, `^LabelMismatch\s+person\s+-\s+actor, person\s+human$`, lines[2])
	assert.Equal(t, []string{"MissingProperty", "person", "email", "email", "-"}, cells(lines[3]))
	assert.Equal(t, []string{"UnexpectedProperty", "person", "ssn", "-", "ssn"}, cells(lines[4]))
	assert.Equal(t, []string{"UnexpectedElement", "robot", "-", "-", "robot"}, cells(lines[5]))
}

// TestRenderFindingsAlignsColumns guards the readability claim: every row's
// cells start at the same offset, which is the whole reason for the table.
func TestRenderFindingsAlignsColumns(t *testing.T) {
	report := conform.Report{Findings: []conform.Finding{
		{Kind: conform.MissingProperty, Element: "person", Property: "email", Want: "email"},
		{Kind: conform.UnexpectedProperty, Element: "organisation_membership", Property: "ssn", Got: "ssn"},
	}}

	var out strings.Builder
	renderFindings(&out, report)

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	require.Len(t, lines, 3)
	offset := strings.Index(lines[0], "ELEMENT")
	require.Positive(t, offset)
	elements := []string{"person", "organisation_membership"}
	for i, line := range lines[1:] {
		assert.Equal(t, offset, strings.Index(line, elements[i]),
			"the element column starts at the header's offset in %q", line)
	}
}

// TestConformRequiresBothSides checks the comparison refuses to start with
// half of it missing, and says which half.
func TestConformRequiresBothSides(t *testing.T) {
	// The subcommand falls back to the environment, so an inherited value
	// would make these assertions depend on the developer's shell.
	t.Setenv("GOPGQL_SDL", "")
	t.Setenv("GOPGQL_DSN", "")

	err := run([]string{"conform"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--sdl")
	assert.Equal(t, exitFailure, exitCode(err))

	err = run([]string{"conform", "--sdl", writeSDL(t, minimalSDL)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--dsn")
	assert.Equal(t, exitFailure, exitCode(err))
}

// TestConformReadsSDLFromTheEnvironment covers the environment fallbacks:
// conform resolves --sdl and --dsn exactly as generate and migrate do, so an
// init container that already exports them needs no extra flags.
func TestConformReadsSDLFromTheEnvironment(t *testing.T) {
	t.Setenv("GOPGQL_SDL", writeSDL(t, minimalSDL))
	t.Setenv("GOPGQL_DSN", "")

	err := run([]string{"conform"})
	require.Error(t, err)
	// The schema resolved, so the missing half is the database — proving the
	// SDL came from the environment rather than being absent.
	assert.Contains(t, err.Error(), "--dsn")
}

// TestConformUnreachableDatabaseIsNotDrift: both outcomes end the process
// non-zero, and the operator can tell which happened without reading the
// findings — because there are none, and the status is not exitDrift.
func TestConformUnreachableDatabaseIsNotDrift(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err := conformCheck(ctx, []string{writeSDL(t, minimalSDL)}, unreachableDSN, generator.Options{})
	require.Error(t, err)

	var drift *driftError
	assert.False(t, errors.As(err, &drift), "an unreachable database is not a verdict about the schema")
	assert.Equal(t, exitFailure, exitCode(err))
	assert.Contains(t, err.Error(), "conformance check did not run")
}

// TestConformRejectsAnUnreadableSchema keeps a bad SDL on the "did not run"
// side too: nothing was compared, so nothing can be said about drift.
func TestConformRejectsAnUnreadableSchema(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.graphql")

	err := conformCheck(context.Background(), []string{missing}, unreachableDSN, generator.Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conformance check did not run")
	assert.Equal(t, exitFailure, exitCode(err))
}

// TestUsageDocumentsConform: the subcommand, its flags, the exit statuses and
// the limit of what it compares all have to be discoverable from the usage
// text alone.
func TestUsageDocumentsConform(t *testing.T) {
	assert.Contains(t, usage, "gopgql conform  --sdl <path> [--sdl <path>…] --dsn <url> [--graph <name>]")
	assert.Contains(t, usage, "conform    Report how the database's property graph differs from the SDL.")
	assert.Contains(t, usage, "2  conform only")
	// The check must not read as broader than it is (package conform's doc).
	assert.Contains(t, usage, "It does not compare column defaults")
}

// TestHelpExitsZero guards a subtlety created by the exit-status contract: for
// a command whose status is its answer, `gopgql conform --help` failing would
// look like a result.
func TestHelpExitsZero(t *testing.T) {
	assert.NoError(t, run([]string{"conform", "--help"}))
	assert.NoError(t, run([]string{"help"}))
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote. The version line goes to stdout by design — it is the command's output
// rather than a diagnostic — so checking it means reading the real stream.
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

// TestVersionReportsBuildInfo covers every accepted form of the query. The
// values are injected with -ldflags at release time, so the assertions are
// about the shape of the line and about all three variables being read — never
// about an exact release string.
func TestVersionReportsBuildInfo(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{name: "subcommand", argv: []string{"version"}},
		{name: "long flag", argv: []string{"--version"}},
		{name: "short flag", argv: []string{"-v"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			out := captureStdout(t, func() { err = run(tc.argv) })
			require.NoError(t, err)

			line := strings.TrimSpace(out)
			assert.Contains(t, line, "gopgql", "the line has to say which binary it came from")
			// A build that stamped only some of the three is a build nobody can
			// trace, so each one is asserted where it appears.
			assert.Contains(t, line, version)
			assert.Contains(t, line, "commit "+commit)
			assert.Contains(t, line, "built "+date)
		})
	}
}

// TestUsageDocumentsVersion: the subcommand is only useful if it is
// discoverable without already knowing it exists.
func TestUsageDocumentsVersion(t *testing.T) {
	assert.Contains(t, usage, "version    Print the version, commit and build date")
}
