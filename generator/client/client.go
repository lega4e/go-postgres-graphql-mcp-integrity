// Package client generates a typed Go client from an SDL document and a
// directory of named GraphQL operations.
//
// Every operation is compiled **at generate time**, through the same pure
// compiler a request would have gone through, and what is emitted is the result:
// the SQL as a const, the projection as a package-level var, and one method per
// operation. Nothing is compiled, parsed or reflected over at run time. A query
// that cannot compile — an unknown root field, a selection past the depth
// ceiling, a scalar with no mapping — fails `gopgql generate client`, which is
// the difference between a build error and a 500.
//
// # The handle is the second parameter of every method
//
// That is the whole point of the package, not a detail of it. A generated method
// never opens a connection and the client holds no pool: the caller passes a
// handle it owns — most usefully a transaction it is already inside — and the
// statement runs there. A client that opened its own connection could not
// participate in the caller's commit, and an operation that cannot be committed
// together with the caller's own bookkeeping cannot be exactly-once.
//
// # Results are assigned, not decoded
//
// The generator knows each operation's selection set exactly, so it emits field
// assignments. Nothing at run time inspects a struct tag or a Go type to decide
// where a column goes. That keeps the failure — a column that came back as
// something the field cannot hold — at a named field rather than in a decoder,
// and it keeps this package independent of whichever result-shaping strategy is
// in use.
package client

import (
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/sdl"
)

// Options configure a generation.
type Options struct {
	// Package is the emitted package name. Defaults to "gopgqlclient".
	Package string
	// GraphName is the property graph the compiled queries target. Defaults to
	// the generator's, exactly as elsewhere.
	GraphName string
	// MaxDepth is the compiler's traversal-depth ceiling; zero means its
	// default. A depth violation is reported here rather than at run time.
	MaxDepth int
}

// DefaultPackage is the package name used when Options.Package is empty.
const DefaultPackage = "gopgqlclient"

// Source is one operation document: the file it came from and its text.
type Source struct {
	// Path is the file's path, used in error messages so a duplicate operation
	// name can name both files.
	Path string
	// Content is the document text.
	Content string
}

// Load reads the operation documents in dir.
//
// The contract is spelled out because two implementations of "a directory of
// operations" would otherwise differ in exactly the ways that matter: only
// `*.graphql` files, **no subdirectory traversal**, and files read in sorted
// path order so a generation does not depend on the order the filesystem
// happens to hand entries back.
func Load(dir string) ([]Source, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("client: read operations dir %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".graphql" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("client: no *.graphql operation documents in %s", dir)
	}

	out := make([]Source, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("client: read %s: %w", path, err)
		}
		out = append(out, Source{Path: path, Content: string(data)})
	}
	return out, nil
}

// File is one emitted Go file.
type File struct {
	// Name is the file's base name.
	Name string
	// Content is its gofmt-ed source.
	Content []byte
}

// Generate compiles every operation and renders the client.
//
// The result is a fixed pair of files in a fixed order, and every list inside
// them is sorted or in declaration order — never in map order — so generating
// twice from one input produces byte-identical bytes.
func Generate(doc *sdl.Document, sources []Source, opts Options) ([]File, error) {
	if opts.Package == "" {
		opts.Package = DefaultPackage
	}
	copts := []compiler.Option{}
	if opts.GraphName != "" {
		copts = append(copts, compiler.WithGraphName(opts.GraphName))
	}
	if opts.MaxDepth > 0 {
		copts = append(copts, compiler.WithMaxDepth(opts.MaxDepth))
	}
	c := compiler.New(doc, copts...)

	ops, err := collect(sources)
	if err != nil {
		return nil, err
	}
	// Sorted by name, so the emitted file's order depends on the operations and
	// not on which file each happened to live in.
	sort.Slice(ops, func(i, j int) bool { return ops[i].Name < ops[j].Name })

	var b strings.Builder
	writeHeader(&b, opts.Package)
	b.WriteString(runtimeSource)

	for _, op := range ops {
		if err := renderOperation(&b, c, op); err != nil {
			return nil, err
		}
	}

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		// A generator that emits source Go cannot parse has a bug in it, and the
		// unformatted text is the only useful evidence.
		return nil, fmt.Errorf("client: generated source does not parse: %w\n\n%s", err, b.String())
	}
	return []File{{Name: "gopgql_client.go", Content: src}}, nil
}

// Write renders the files into dir.
func Write(dir string, files []File) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("client: create dir: %w", err)
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		path := filepath.Join(dir, f.Name)
		if err := os.WriteFile(path, f.Content, 0o644); err != nil {
			return nil, fmt.Errorf("client: write %s: %w", path, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// operation is one named operation ready to compile.
type operation struct {
	// Name is the operation's name, and the exported method name.
	Name string
	// Path is the file it came from.
	Path string
	// Source is the single-operation document text handed to the compiler.
	Source string
	// Op is the parsed operation, for its variable definitions and kind.
	Op *ast.OperationDefinition
}

// collect parses every document and returns its named operations.
//
// An anonymous operation is an error: the operation's name *is* the method's
// name, so there is nothing to call it. A name used twice is an error naming
// both files, because silently keeping one would drop a method that a caller
// wrote an operation for.
func collect(sources []Source) ([]operation, error) {
	var out []operation
	seen := map[string]string{}
	for _, src := range sources {
		doc, gqlErr := parser.ParseQuery(&ast.Source{Name: src.Path, Input: src.Content})
		if gqlErr != nil {
			return nil, fmt.Errorf("client: parse %s: %w", src.Path, gqlErr)
		}
		for i, op := range doc.Operations {
			if op.Name == "" {
				return nil, fmt.Errorf("client: %s declares an anonymous operation; "+
					"an operation's name is the generated method's name", src.Path)
			}
			if prev, dup := seen[op.Name]; dup {
				return nil, fmt.Errorf("client: the operation %q is declared in both %s and %s",
					op.Name, prev, src.Path)
			}
			seen[op.Name] = src.Path

			// The compiler takes a document holding exactly one operation, and a
			// file may hold several, so each is sliced back out of the original
			// text: from its own start to the next one's, or to the end. Slicing
			// beats re-printing the AST, which would be a second GraphQL
			// grammar to keep in step with gqlparser's, and it beats counting
			// braces, which a string argument containing one would defeat.
			end := len(src.Content)
			if i+1 < len(doc.Operations) && doc.Operations[i+1].Position != nil {
				end = doc.Operations[i+1].Position.Start
			}
			out = append(out, operation{
				Name:   op.Name,
				Path:   src.Path,
				Source: slice(src.Content, op, end),
				Op:     op,
			})
		}
	}
	return out, nil
}

// slice returns content[op.start:end], or the whole document when gqlparser
// recorded no position (which it always does; the guard keeps a nil from
// becoming a panic).
func slice(content string, op *ast.OperationDefinition, end int) string {
	if op.Position == nil {
		return content
	}
	start := op.Position.Start
	if start < 0 || start > len(content) || end < start || end > len(content) {
		return content
	}
	return content[start:end]
}
