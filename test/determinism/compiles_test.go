package determinism_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/generator/client"
	"github.com/lega4e/gopgql/sdl"
)

// TestGeneratedClientCompiles builds the generated package for real.
//
// Every other assertion about the client is made on its text, and text that
// looks right is not the same as code that compiles: a wrong Go type for a
// scalar, a decoder that does not match the field it is assigned to, or a nested
// result type named one way and referred to another all read perfectly and fail
// at `go build`. The generator runs its output through go/format, which parses
// but does not type-check, so this is the only check that would catch them.
//
// It builds in a throwaway module that replaces gopgql with the working tree, so
// what is compiled against is this branch rather than a published version.
func TestGeneratedClientCompiles(t *testing.T) {
	doc, err := sdl.Parse(determinismSchema)
	require.NoError(t, err)

	work := t.TempDir()
	opsDir := filepath.Join(work, "operations")
	require.NoError(t, os.MkdirAll(opsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(opsDir, "ops.graphql"), []byte(determinismOps), 0o644))

	sources, err := client.Load(opsDir)
	require.NoError(t, err)
	files, err := client.Generate(doc, sources, client.Options{Package: "gen"})
	require.NoError(t, err)

	pkgDir := filepath.Join(work, "gen")
	_, err = client.Write(pkgDir, files)
	require.NoError(t, err)

	root := repoRoot(t)
	gomod := fmt.Sprintf(`module gopgqlgen.test

go 1.25.0

require github.com/lega4e/gopgql v0.0.0

replace github.com/lega4e/gopgql => %s
`, root)
	require.NoError(t, os.WriteFile(filepath.Join(work, "go.mod"), []byte(gomod), 0o644))

	// A `use` line would be simpler, but the working tree may itself be inside a
	// workspace; copying the repo's go.sum keeps module resolution to exactly
	// what this branch already resolves.
	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(work, "go.sum"), sum, 0o644))

	build := exec.Command("go", "build", "./...")
	build.Dir = work
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")
	out, err := build.CombinedOutput()
	require.NoError(t, err, "the generated client must compile:\n%s", out)

	vet := exec.Command("go", "vet", "./...")
	vet.Dir = work
	vet.Env = build.Env
	out, err = vet.CombinedOutput()
	require.NoError(t, err, "the generated client must vet cleanly:\n%s", out)
}
