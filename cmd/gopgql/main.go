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
	"path/filepath"
	"slices"
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
                  [--no-tables] [--no-graph]
  gopgql migrate  --dsn <url> [--sdl <file>] [--dir <dir>] [--name <suffix>] [--graph <name>]
                  [--no-tables] [--no-graph]
  gopgql conform  --sdl <file> --dsn <url> [--graph <name>]

Commands:
  generate   Write the next migration for the schema into --dir.
  migrate    Generate (when --sdl is given) and apply --dir to --dsn.
  conform    Report how the database's property graph differs from the SDL.

Flags:
  --sdl    Path to the SDL schema.                     (env GOPGQL_SDL)
  --dsn    PostgreSQL connection string.               (env GOPGQL_DSN)
  --dir    Migration directory. Default "migrations".  (env GOPGQL_MIGRATIONS)
           Holds two subdirectories:
             <dir>/tables/  CREATE TABLE and its indexes
             <dir>/graph/   CREATE PROPERTY GRAPH
           They are applied in lockstep, one generation at a time —
           tables/0001, graph/0001, tables/0002, graph/0002, … — so every
           graph migration lands on the tables of its own generation.
  --no-tables  Skip the tables half — someone else owns the tables, and the
               SDL describes only the slice surfaced as a graph. Nothing about
               a table is read, diffed or emitted. (env GOPGQL_NO_TABLES)
  --no-graph   Skip the property-graph half. (env GOPGQL_NO_GRAPH)
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
	noTables := fs.Bool("no-tables", false, "skip the tables half (someone else owns the tables)")
	noGraph := fs.Bool("no-graph", false, "skip the property-graph half")
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
	if !*noTables && os.Getenv("GOPGQL_NO_TABLES") != "" {
		*noTables = true
	}
	if !*noGraph && os.Getenv("GOPGQL_NO_GRAPH") != "" {
		*noGraph = true
	}
	if *noTables && *noGraph {
		return errors.New("--no-tables and --no-graph together leave nothing to do")
	}
	halves := selectedHalves(*noTables, *noGraph)

	switch command {
	case "generate":
		if *sdlPath == "" {
			return errors.New("generate needs a schema: pass --sdl or set GOPGQL_SDL")
		}
		return generate(*sdlPath, *dir, *name, *graph, halves)
	case "migrate":
		if *dsn == "" {
			return errors.New("migrate needs a database: pass --dsn or set GOPGQL_DSN")
		}
		if *sdlPath != "" {
			if err := generate(*sdlPath, *dir, *name, *graph, halves); err != nil {
				return err
			}
		}
		return applyHalves(*dir, halves, *dsn)
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
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", command)
	}
}

// half is one of the two independently generated and applied parts of a
// schema. Which one a migration directory holds is decided by its path and
// nothing else — no marker is written into the files and none is read back, so
// a directory cannot disagree with itself about what it owns.
type half struct {
	subdir   string
	generate func(dir string, desired *schema.Schema, name string, version int) (string, error)
}

// selectedHalves returns the halves to generate and apply, in dependency order
// within a generation: tables before the graph that references them.
func selectedHalves(noTables, noGraph bool) []half {
	var hs []half
	if !noTables {
		hs = append(hs, half{subdir: migrate.TablesDir, generate: migrate.GenerateTables})
	}
	if !noGraph {
		hs = append(hs, half{subdir: migrate.GraphDir, generate: migrate.GenerateGraph})
	}
	return hs
}

// generate writes the next migration for each selected half into its own
// subdirectory of dir.
func generate(sdlPath, dir, name, graph string, halves []half) error {
	model, err := build(sdlPath, graph)
	if err != nil {
		return err
	}
	// One version for both halves, read before either is written: the number is
	// the generation, not this half's file count (migrate.NextVersion).
	version, err := migrate.NextVersion(dir)
	if err != nil {
		return err
	}
	for _, h := range halves {
		sub := filepath.Join(dir, h.subdir)
		path, err := h.generate(sub, model, name, version)
		if err != nil {
			return err
		}
		if path == "" {
			fmt.Printf("gopgql: %s is already up to date with %s\n", sub, sdlPath)
			continue
		}
		fmt.Printf("gopgql: wrote %s\n", path)
	}
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

// applyHalves applies the selected halves in lockstep, one generation at a
// time:
//
//	tables/0001 → graph/0001 → tables/0002 → graph/0002 → …
//
// Not all of one half and then all of the other. A graph migration describes
// the columns of its own generation, so replaying graph/0001 once the tables
// have already moved on to 0002 runs a historical graph definition against a
// current schema: it names a column 0002 dropped or renamed, and PostgreSQL
// refuses it. Each generation of the graph has to land on the tables it was
// generated against (gopgql#38).
//
// The graph is still taken down before the tables move, but per generation
// rather than once at the front. What blocks tables/<n> is the live graph
// graph/<n-1> built — PostgreSQL will not drop or retype a column a live
// property graph exposes — and graph/<n> puts the right one back immediately
// after. So a generation with table work reads
//
//	graph/<n-1> down → tables/<n> up → graph/<n> up
//
// and a generation with no table work does not touch the graph at all. That is
// what makes re-running `gopgql migrate` against an already-migrated database
// the no-op it is documented to be: with nothing pending, nothing is dropped.
//
// The halves need not be the same length. A generation one half skipped is
// simply not a step for that half; when the tables half outruns the graph half
// the graph is rebuilt at the end (restoreGraph). With one half turned off
// there is nothing to interleave and the other applies in version order.
func applyHalves(dir string, halves []half, dsn string) error {
	var tablesDir, graphDir string
	for _, h := range halves {
		switch h.subdir {
		case migrate.TablesDir:
			tablesDir = filepath.Join(dir, h.subdir)
		case migrate.GraphDir:
			graphDir = filepath.Join(dir, h.subdir)
		}
	}
	switch {
	case tablesDir == "":
		return applyAll(graphDir, migrate.GraphDir, dsn)
	case graphDir == "":
		return applyAll(tablesDir, migrate.TablesDir, dsn)
	default:
		return applyInterleaved(tablesDir, graphDir, dsn)
	}
}

// applyInterleaved is applyHalves' both-halves case: the lockstep walk over the
// generations the two directories hold between them.
func applyInterleaved(tablesDir, graphDir, dsn string) error {
	ctx := context.Background()
	if err := checkDir(tablesDir); err != nil {
		return err
	}
	if err := checkDir(graphDir); err != nil {
		return err
	}
	tableVersions, err := migrate.Versions(tablesDir)
	if err != nil {
		return err
	}
	graphVersions, err := migrate.Versions(graphDir)
	if err != nil {
		return err
	}

	db, err := connect(dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	tablesAt, err := halfVersion(ctx, db, migrate.TablesDir)
	if err != nil {
		return err
	}
	graphAt, err := halfVersion(ctx, db, migrate.GraphDir)
	if err != nil {
		return err
	}

	// graphDown stays true from the moment the graph is taken down until a
	// graph migration puts one back, so the walk drops at most once per run of
	// consecutive table-only generations — and never on a database with
	// nothing pending.
	graphDown := false
	for _, v := range mergeVersions(tableVersions, graphVersions) {
		if slices.Contains(tableVersions, v) && v > tablesAt {
			if !graphDown {
				dropped, err := releaseGraph(ctx, db, graphDir, graphAt)
				if err != nil {
					return err
				}
				graphDown = dropped
			}
			if err := upTo(ctx, db, tablesDir, migrate.TablesDir, v); err != nil {
				return err
			}
			tablesAt = v
		}
		if slices.Contains(graphVersions, v) && v > graphAt {
			if err := upTo(ctx, db, graphDir, migrate.GraphDir, v); err != nil {
				return err
			}
			graphAt = v
			graphDown = false
		}
	}
	if graphDown {
		return restoreGraph(ctx, db, graphDir, graphAt)
	}
	return nil
}

// applyAll applies a whole directory in version order. It is the single-half
// case: with the other half turned off there is no second half to interleave
// with, and no property graph of gopgql's to take down first.
func applyAll(dir, subdir, dsn string) error {
	if err := checkDir(dir); err != nil {
		return err
	}
	db, err := connect(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	goose.SetTableName(migrate.VersionTable(subdir))
	if err := goose.UpContext(context.Background(), db, dir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	fmt.Printf("gopgql: applied %s\n", dir)
	return nil
}

// mergeVersions is the ascending union of the two halves' version lists: every
// generation the lockstep walk has to consider, from either directory.
func mergeVersions(a, b []int64) []int64 {
	out := make([]int64, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	slices.Sort(out)
	return slices.Compact(out)
}

// releaseGraph takes down the property graph the graph half has built so far,
// so the tables half can alter the columns it exposes. It reports whether there
// was one to take down.
//
// It is deliberately not a goose rollback. Rolling the graph directory back to
// zero and replaying it would re-run historical CREATE PROPERTY GRAPH
// statements against tables that have since moved on. Only the graph that is
// live right now has to go — the one folded from graph migrations 1…at, which
// is why the fold is bounded by the applied version rather than reading the
// whole directory.
func releaseGraph(ctx context.Context, db *sql.DB, graphDir string, at int64) (bool, error) {
	folded, err := migrate.FoldUpTo(graphDir, at)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", graphDir, err)
	}
	if folded == nil || folded.GraphName == "" {
		return false, nil
	}
	if _, err := db.ExecContext(ctx, migrate.DropGraphSQL(folded.GraphName)); err != nil {
		return false, fmt.Errorf("release the property graph before altering its tables: %w", err)
	}
	return true, nil
}

// restoreGraph rebuilds the property graph when the tables half had a
// generation the graph half does not — tables/0003 against a graph directory
// that ends at 0002, which happens whenever a table changes in a way the graph
// statement does not mention (a new index, a column no label exposes). The
// graph came down so the tables could move and nothing followed to put it back,
// so the applier recreates the graph the graph half still describes.
//
// Without this the graph would silently disappear from a database whose table
// half simply ran ahead.
func restoreGraph(ctx context.Context, db *sql.DB, graphDir string, at int64) error {
	folded, err := migrate.FoldUpTo(graphDir, at)
	if err != nil {
		return fmt.Errorf("read %s: %w", graphDir, err)
	}
	if folded == nil || folded.GraphName == "" {
		return nil
	}
	if _, err := db.ExecContext(ctx, migrate.CreateGraphSQL(folded)); err != nil {
		return fmt.Errorf("rebuild the property graph after the tables moved: %w", err)
	}
	fmt.Printf("gopgql: rebuilt property graph %s\n", folded.GraphName)
	return nil
}

// halfVersion reads the version a half's own goose table records.
//
// Each half needs its own table: both directories start at 0001, so with
// goose's single shared table the tables half records version 1 and the graph
// half's 0001 is then considered already applied and silently skipped — the
// database ends up with tables and no property graph, and nothing reports a
// problem.
func halfVersion(ctx context.Context, db *sql.DB, subdir string) (int64, error) {
	goose.SetTableName(migrate.VersionTable(subdir))
	v, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		if errors.Is(err, goose.ErrNoNextVersion) {
			return 0, nil
		}
		return 0, fmt.Errorf("read the %s half's migration version: %w", subdir, err)
	}
	if v < 0 {
		return 0, nil
	}
	return v, nil
}

// upTo applies dir up to and including version — one generation of one half.
func upTo(ctx context.Context, db *sql.DB, dir, subdir string, version int64) error {
	goose.SetTableName(migrate.VersionTable(subdir))
	if err := goose.UpToContext(ctx, db, dir, version); err != nil {
		return fmt.Errorf("goose up %s to %04d: %w", dir, version, err)
	}
	fmt.Printf("gopgql: applied %s up to %04d\n", dir, version)
	return nil
}

// checkDir reports a missing migration directory as the actionable error it is.
func checkDir(dir string) error {
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("migration directory %s: %w", dir, err)
	}
	return nil
}

// connect opens the database goose will run through and proves it answers.
func connect(dsn string) (*sql.DB, error) {
	if err := goose.SetDialect("postgres"); err != nil {
		return nil, fmt.Errorf("goose dialect: %w", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	return db, nil
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
