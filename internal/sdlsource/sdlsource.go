// Package sdlsource resolves the --sdl paths a command line carries into the
// SDL documents to parse.
//
// It is the one place that knows what --sdl accepts — a file, several files, or
// a directory of *.graphql — so gopgql and gopgql-mcp cannot disagree about it.
// The reading lives here rather than in package sdl because sdl has no IO and
// compiles to WASM, where none of this means anything.
package sdlsource

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lega4e/gopgql/sdl"
)

// Ext is the file extension a directory is searched for.
const Ext = ".graphql"

// EnvVar is the environment variable --sdl falls back to.
const EnvVar = "GOPGQL_SDL"

// FlagUsage is the --sdl help text, shared so both binaries describe the same
// flag the same way.
const FlagUsage = "path to an SDL schema file or a directory of *.graphql; repeatable (env " + EnvVar + ")"

// PathList collects a repeatable path flag. Go's flag package has no repeatable
// string flag of its own; a flag.Value that appends is the whole of what is
// missing.
type PathList []string

func (p *PathList) String() string { return strings.Join(*p, string(filepath.ListSeparator)) }

// Set appends one occurrence of the flag.
func (p *PathList) Set(v string) error {
	if v == "" {
		return fmt.Errorf("empty path")
	}
	*p = append(*p, v)
	return nil
}

// EnvPaths splits an environment variable holding one or more paths.
//
// The separator is the platform's own — ':' on Unix, ';' on Windows — because
// that is the separator every path-list variable on the platform already uses,
// and it is the one that cannot collide with a path (a Windows drive letter's
// colon would).
func EnvPaths(name string) []string {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range filepath.SplitList(v) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Schema is a resolved --sdl: the documents to parse and the text they make
// together.
type Schema struct {
	// Sources are the documents in the order they will be parsed: the order the
	// paths were given, and within a directory, sorted by file name.
	Sources []sdl.Source
	// Paths are the files the sources were read from, in the same order.
	Paths []string
	// Text is the sources concatenated, each ending in a newline. It is the
	// document a single --sdl would have held, and parsing it produces the same
	// model — which is what makes splitting a schema across files a purely
	// editorial decision (gopgql#54).
	Text string
}

// Display names the schema for a message about it as a whole: the single path
// when there is one, and "n files (first, …)" when there are several.
func (s *Schema) Display() string {
	switch len(s.Paths) {
	case 0:
		return "(no schema)"
	case 1:
		return s.Paths[0]
	default:
		return fmt.Sprintf("%d files (%s)", len(s.Paths), strings.Join(s.Paths, ", "))
	}
}

// Load reads every path into the documents to parse.
//
// A path naming a directory expands to the *.graphql files directly inside it,
// sorted by name — not searched recursively, matching --operations. The sort is
// what makes a directory reproducible: os.ReadDir's order is not guaranteed, and
// a generator whose output depends on it would emit a different migration on a
// different machine.
func Load(paths []string) (*Schema, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("no schema given")
	}

	files, err := expand(paths)
	if err != nil {
		return nil, err
	}

	// The same file twice is a mistake with a confusing symptom — gqlparser
	// reports every type in it as redeclared — so it is named here instead.
	seen := map[string]string{}
	out := &Schema{}
	var text strings.Builder
	for _, path := range files {
		key := path
		if abs, err := filepath.Abs(path); err == nil {
			key = filepath.Clean(abs)
		}
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("read schema: %s and %s are the same file; --sdl takes each document once", prev, path)
		}
		seen[key] = path

		data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own argument
		if err != nil {
			return nil, fmt.Errorf("read schema: %w", err)
		}
		body := string(data)
		out.Sources = append(out.Sources, sdl.Source{Name: path, Input: body})
		out.Paths = append(out.Paths, path)

		// A file that does not end in a newline would otherwise run its last
		// line into the next file's first one, which is the one way
		// concatenating could mean something different from parsing separately.
		text.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			text.WriteString("\n")
		}
	}
	out.Text = text.String()
	return out, nil
}

// expand turns the given paths into the files to read, expanding directories.
func expand(paths []string) ([]string, error) {
	var files []string
	for _, path := range paths {
		if path == "" {
			return nil, fmt.Errorf("read schema: empty --sdl path")
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("read schema: %w", err)
		}
		if !info.IsDir() {
			files = append(files, path)
			continue
		}
		found, err := dirFiles(path)
		if err != nil {
			return nil, err
		}
		files = append(files, found...)
	}
	return files, nil
}

// dirFiles lists the *.graphql files directly inside dir, sorted by name.
func dirFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != Ext {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("read schema: no *%s documents in %s", Ext, dir)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, filepath.Join(dir, name))
	}
	return out, nil
}
