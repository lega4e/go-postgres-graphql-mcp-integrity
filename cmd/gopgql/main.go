// Command gopgql is the schema-side CLI: it turns an SDL document into goose
// migrations, applies them, and checks that the database still matches
// (SPEC.md §4.1).
//
//	gopgql generate --sdl schema.graphql --dir migrations
//	gopgql migrate  --sdl schema.graphql --dsn postgres://…
//	gopgql conform  --sdl schema.graphql --dsn postgres://…
//
// `generate` folds whatever migrations already exist in --dir, diffs them
// against the schema the SDL describes, and writes the next migration —
// 0001_init.sql on an empty directory, NNNN_<name>.sql thereafter, nothing at
// all when the two already agree.
//
// `migrate` does that and then applies the directory with goose. It is what an
// init container runs: with an ephemeral --dir it regenerates 0001_init.sql
// every time, and goose skips the versions it has already applied, so running
// it repeatedly against the same database is a no-op.
//
// `conform` reads the property graph back out of the database and reports how
// it differs from the SDL. Everything else here reasons from the SDL alone and
// so cannot notice that the database stopped agreeing with it (SPEC.md §3.1);
// this is the check that does. Its answer is in its exit status — 2 for drift,
// separate from 1 for a check that could not run — so it gates a pipeline
// without a wrapper script interpreting its output (design D5).
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	// Registers the "pgx" database/sql driver goose runs through.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/lega4e/gopgql/conform"
	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/schema"
	"github.com/lega4e/gopgql/sdl"
)

const usage = `gopgql — generate and apply migrations from a GraphQL SDL schema.

Usage:
  gopgql generate --sdl <file> --dir <dir> [--name <suffix>] [--graph <name>]
  gopgql migrate  --dsn <url> [--sdl <file>] [--dir <dir>] [--name <suffix>] [--graph <name>]
  gopgql conform  --sdl <file> --dsn <url> [--graph <name>]

Commands:
  generate   Write the next migration for the schema into --dir.
  migrate    Generate (when --sdl is given) and apply --dir to --dsn.
  conform    Report how the database's property graph differs from the SDL.
  version    Print the version, commit and build date (also --version, -v).

Flags:
  --sdl    Path to the SDL schema.                     (env GOPGQL_SDL)
  --dsn    PostgreSQL connection string.               (env GOPGQL_DSN)
  --dir    Migration directory. Default "migrations".  (env GOPGQL_MIGRATIONS)
  --name   Descriptive suffix for a generated delta. Default "schema".
  --graph  Property-graph name. Default is the generator's.
A flag wins over its environment variable.

Exit status:
  0  Success — for conform, the database matches the SDL.
  1  The command could not run: bad flags, a schema that would not parse, a
     database it could not reach, or a property graph it could not find.
  2  conform only: the database was read, and it has drifted.

conform compares the property graph — which elements exist, the labels they
carry, and the properties they expose. It does not compare column defaults,
CHECK or UNIQUE constraints, indexes or column types, because PostgreSQL does
not record those in the graph catalogs.
`

// Exit statuses. The split between them is the point of the conform
// subcommand: a pipeline that fails needs to know whether the answer was "no"
// or whether there was no answer at all, and the operator's next move is
// completely different in the two cases — edit the SDL or the database on
// drift, fix the connection or apply the migrations on a failure to run.
// Collapsing both into 1 would force a CI step to parse the message, which is
// the mistake the structured findings exist to avoid.
const (
	exitFailure = 1
	exitDrift   = 2
)

// Build information, stamped by the release pipeline. Overridden at build time
// via -ldflags "-X main.version=… -X main.commit=… -X main.date=…"; the
// defaults are what a plain `go build` leaves behind.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// versionLine renders the build information as the single line the version
// query prints. gopgql-mcp prints the same shape, so a bug report naming either
// binary identifies one build.
func versionLine() string {
	return fmt.Sprintf("gopgql %s (commit %s, built %s)", version, commit, date)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "gopgql: %v\n", err)
		os.Exit(exitCode(err))
	}
}

// driftError reports that conform ran to completion and the database did not
// match the SDL. It is a distinct type rather than a message because
// [exitCode] must recognise it structurally — matching on prose is exactly
// what the exit status exists to spare every caller.
type driftError struct {
	graph    string
	sdlPath  string
	findings int
}

func (e *driftError) Error() string {
	return fmt.Sprintf("property graph %q has drifted from %s: %s",
		e.graph, e.sdlPath, countFindings(e.findings))
}

// exitCode maps a failed run to a status. Only conform ever yields a
// *driftError, so every other subcommand keeps exiting 1 exactly as before.
func exitCode(err error) int {
	var drift *driftError
	if errors.As(err, &drift) {
		return exitDrift
	}
	return exitFailure
}

func run(argv []string) error {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}
	command, rest := argv[0], argv[1:]

	fs := flag.NewFlagSet("gopgql "+command, flag.ContinueOnError)
	sdlPath := fs.String("sdl", "", "path to the SDL schema (env GOPGQL_SDL)")
	dsn := fs.String("dsn", "", "PostgreSQL connection string (env GOPGQL_DSN)")
	dir := fs.String("dir", "", `migration directory (env GOPGQL_MIGRATIONS, default "migrations")`)
	name := fs.String("name", "schema", "descriptive suffix for a generated delta")
	graph := fs.String("graph", "", "property-graph name (default: the generator's)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(rest); err != nil {
		// -h/--help has already printed the usage text through fs.Usage.
		// Reporting it as a failure would make `gopgql conform --help` exit
		// non-zero, and for a command whose exit status *is* its result that
		// reads as drift rather than as help.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	// A flag wins over the environment.
	if *sdlPath == "" {
		*sdlPath = os.Getenv("GOPGQL_SDL")
	}
	if *dsn == "" {
		*dsn = os.Getenv("GOPGQL_DSN")
	}
	if *dir == "" {
		*dir = os.Getenv("GOPGQL_MIGRATIONS")
	}
	if *dir == "" {
		*dir = "migrations"
	}

	switch command {
	case "generate":
		if *sdlPath == "" {
			return errors.New("generate needs a schema: pass --sdl or set GOPGQL_SDL")
		}
		return generate(*sdlPath, *dir, *name, *graph)
	case "migrate":
		if *dsn == "" {
			return errors.New("migrate needs a database: pass --dsn or set GOPGQL_DSN")
		}
		if *sdlPath != "" {
			if err := generate(*sdlPath, *dir, *name, *graph); err != nil {
				return err
			}
		}
		return apply(*dir, *dsn)
	case "conform":
		// Both sides are required: the check is a comparison, and with either
		// half missing there is nothing to compare rather than a default to
		// fall back on.
		if *sdlPath == "" {
			return errors.New("conform needs a schema: pass --sdl or set GOPGQL_SDL")
		}
		if *dsn == "" {
			return errors.New("conform needs a database: pass --dsn or set GOPGQL_DSN")
		}
		return conformCheck(context.Background(), *sdlPath, *dsn, *graph)
	// --version and -v are accepted alongside the subcommand because that is
	// what a reader reaches for first, and neither form collides with anything:
	// the flag set defines no -v, and a flag in this position is the command.
	// The line goes to stdout — it is the command's output, not a diagnostic.
	case "version", "--version", "-v":
		fmt.Println(versionLine())
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", command)
	}
}

// generate writes the next migration for the SDL's schema into dir.
func generate(sdlPath, dir, name, graph string) error {
	model, err := build(sdlPath, graph)
	if err != nil {
		return err
	}
	path, err := migrate.Generate(dir, model, name)
	if err != nil {
		return err
	}
	if path == "" {
		fmt.Printf("gopgql: %s is already up to date with %s\n", dir, sdlPath)
		return nil
	}
	fmt.Printf("gopgql: wrote %s\n", path)
	return nil
}

// build parses and validates the SDL and returns the physical schema model.
func build(sdlPath, graph string) (*schema.Schema, error) {
	source, err := os.ReadFile(sdlPath)
	if err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	doc, err := sdl.Parse(string(source))
	if err != nil {
		return nil, err
	}
	return generator.Build(doc, graph)
}

// apply runs the migrations in dir against the database.
func apply(dir, dsn string) error {
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("migration directory %s: %w", dir, err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return fmt.Errorf("connect to the database: %w", err)
	}

	if err := goose.UpContext(context.Background(), db, dir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	fmt.Printf("gopgql: applied %s\n", dir)
	return nil
}

// conformCheck compares the property graph the database holds against the one
// the SDL describes, prints the differences, and reports drift as a
// *driftError so that the process exits 2 rather than 1.
//
// Every failure before the comparison is wrapped in "conformance check did not
// run". An unreachable database and a drifted one both end the process
// non-zero, so the first words of the message are what tell them apart. The
// wrapper is added here rather than left to each error's own text, so that a
// new failure mode cannot quietly start reading like a verdict.
func conformCheck(ctx context.Context, sdlPath, dsn, graph string) error {
	desired, err := build(sdlPath, graph)
	if err != nil {
		return fmt.Errorf("conformance check did not run: %w", err)
	}
	// desired.GraphName is the *resolved* name — generator.Build substitutes
	// its default for an empty --graph — so reporting and reflecting both use
	// it rather than re-deriving the default here and risking disagreement.
	graphName := desired.GraphName

	// Read-only, and pinged before anything else, so an unreachable database
	// fails here with a connection error instead of surfacing as an empty
	// reflection that would read as total drift.
	pool, err := exec.OpenReadOnly(ctx, dsn)
	if err != nil {
		return fmt.Errorf("conformance check did not run: %w", err)
	}
	defer pool.Close()

	actual, err := conform.Reflect(ctx, pool, graphName)
	if err != nil {
		// A missing graph is the other verdict-shaped non-verdict: comparing
		// against nothing would report every element as missing, when the
		// truth is usually that the migrations were never applied. conform
		// exports a sentinel precisely so this test is structural.
		if errors.Is(err, conform.ErrGraphNotFound) {
			return fmt.Errorf("conformance check did not run: %w — apply the "+
				"migrations first, or pass --graph if the graph has another name", err)
		}
		return fmt.Errorf("conformance check did not run: %w", err)
	}

	report := conform.Check(desired, actual)
	if report.OK() {
		fmt.Printf("gopgql: property graph %q conforms to %s\n", graphName, sdlPath)
		fmt.Print(coverageNote)
		return nil
	}

	// The findings go to stdout and the one-line summary rides out on stderr
	// as the error, so a redirect keeps the report and the terminal still says
	// what happened. Printing the summary on both would only duplicate it.
	renderFindings(os.Stdout, report)
	fmt.Print(coverageNote)
	return &driftError{graph: graphName, sdlPath: sdlPath, findings: len(report.Findings)}
}

// coverageNote is printed on both outcomes, not only on drift. "Conforms" is
// the sentence most likely to be over-read — the package doc is explicit that
// an empty report says nothing about defaults, constraints or indexes — and a
// caveat that appears only when something is already wrong is a caveat nobody
// reads.
const coverageNote = "\ngopgql: compared elements, labels and properties only; " +
	"defaults, constraints and indexes are not covered.\n"

// renderFindings prints the report as one aligned row per finding.
//
// It is a flat table rather than a per-element grouping on purpose. Each
// finding stays a single line carrying all five fields, so a reader can scan
// down the Kind column — the thing they act on — and a grep for an element
// name returns whole findings rather than rows orphaned from a group header.
// Grouping would also break the alignment: tabwriter sizes columns per
// contiguous block, so a heading between groups would let each group choose
// its own widths.
//
// Findings arrive sorted by element, so the elements still cluster.
func renderFindings(w io.Writer, r conform.Report) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tELEMENT\tPROPERTY\tSDL\tDATABASE")
	for _, f := range r.Findings {
		// SDL and DATABASE name the sides rather than echoing Want/Got,
		// because which side said what is the fact an operator needs and
		// "want"/"got" leaves them to remember the convention.
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			f.Kind, f.Element, dash(f.Property), dash(f.Want), dash(f.Got))
	}
	_ = tw.Flush()
}

// dash renders "nothing there" visibly. A blank cell in an aligned table is
// indistinguishable from a column that ran short, and "nothing there" is the
// substance of half the finding kinds.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// countFindings formats a finding count as English, so a report of exactly one
// difference does not read as "1 findings".
func countFindings(n int) string {
	if n == 1 {
		return "1 finding"
	}
	return fmt.Sprintf("%d findings", n)
}
