// Command gopgql is the schema-side CLI: it turns an SDL document into goose
// migrations, applies them, and checks that the database still matches
// (SPEC.md §4.1).
//
//	gopgql generate --sdl schema.graphql --dir migrations
//	gopgql migrate  --sdl schema.graphql --dsn postgres://…
//	gopgql conform  --sdl schema.graphql --dsn postgres://…
//
// `generate` folds whatever migrations already exist in --dir, diffs them
// against the schema the SDL describes, and writes the generation that closes
// the gap: consecutive single-purpose migrations, each doing exactly one thing,
// into --dir itself. Nothing at all when the two already agree.
//
// `migrate` does that and then applies the directory with goose — a plain
// forward apply in version order, with no ordering of gopgql's own. It is what
// an init container runs: with an ephemeral --dir it regenerates the whole
// history every time, and goose skips the versions it has already applied, so
// running it repeatedly against the same database is a no-op.
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
	"strconv"
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
           One directory, one goose history, one goose_db_version table. No
           migration ever mixes table DDL with property-graph DDL: one edit of
           the SDL emits consecutive single-purpose migrations, applied in that
           order —
             0003_add_email_graph_down.sql  DROP PROPERTY GRAPH IF EXISTS …
             0004_add_email_tables.sql      ALTER TABLE …
             0005_add_email_graph.sql       CREATE PROPERTY GRAPH …
           The graph comes down first because PostgreSQL refuses to alter a
           column a live property graph exposes, and goes back up last over the
           tables of its own generation. gopgql migrate is that plain forward
           apply; a generation is not atomic, so an apply that stops partway
           leaves the graph down, which gopgql conform reports as "property
           graph not found". How to recover depends on why it stopped:
             interrupted (a crash, ^C)  re-run it; it continues from where it
                                        stopped and the graph comes back up.
             the _tables migration      re-running fails identically — the DDL
             failed (NOT NULL over      is the problem. The teardown in front
             existing rows, a type      of it has committed, so replaying from
             change PG refuses)         zero is no better. goose down one step
                                        restores the graph; then fix the SDL.
  --no-tables  Skip the tables half — someone else owns the tables, and the
               SDL describes only the slice surfaced as a graph. Nothing about
               a table is read, diffed or emitted. (env GOPGQL_NO_TABLES)
  --no-graph   Skip the property-graph half. (env GOPGQL_NO_GRAPH)
           The flags scope a directory's first generation; after that the
           directory's own history decides which halves it manages, and a flag
           that contradicts it is an error. They scope what is *generated* —
           applying is always the whole directory.
  --name   Descriptive suffix for a generated delta. Default "schema".
  --graph  Property-graph name. Default is the generator's.
A flag wins over its environment variable. GOPGQL_NO_TABLES and GOPGQL_NO_GRAPH
take a boolean — true/false, 1/0, t/f — and an unset or empty one is false;
anything else is an error rather than a guessed default.

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
	// The boolean pair is read the same way, except that their values have to be
	// parsed rather than merely tested for emptiness — see [envBool].
	noTablesEnv, err := envBool("GOPGQL_NO_TABLES")
	if err != nil {
		return err
	}
	noGraphEnv, err := envBool("GOPGQL_NO_GRAPH")
	if err != nil {
		return err
	}
	*noTables = *noTables || noTablesEnv
	*noGraph = *noGraph || noGraphEnv
	if *noTables && *noGraph {
		return errors.New("--no-tables and --no-graph together leave nothing to do")
	}
	halves := migrate.Halves{NoTables: *noTables, NoGraph: *noGraph}

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
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", command)
	}
}

// envBool reads a boolean environment variable. Unset or empty is false.
//
// It parses the value rather than testing it for emptiness, because these two
// variables are documented surface and `false` is exactly what a compose file or
// a Helm values block writes for "off". Treating any non-empty string as true
// made GOPGQL_NO_TABLES=false turn the tables half *off* — the opposite of what
// it says.
//
// Anything strconv.ParseBool does not recognise is an error rather than a silent
// default, because both defaults are wrong to guess at: choosing false ignores
// an operator who meant to disable a half, and choosing true disables one they
// never asked to.
func envBool(name string) (bool, error) {
	raw, ok := os.LookupEnv(name)
	if !ok || raw == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s=%q is not a boolean: use true/false, 1/0, t/f, TRUE/FALSE", name, raw)
	}
	return v, nil
}

// generate writes the generation the SDL calls for into dir.
func generate(sdlPath, dir, name, graph string, halves migrate.Halves) error {
	model, err := build(sdlPath, graph)
	if err != nil {
		return err
	}
	paths, err := migrate.Generate(dir, model, name, halves)
	if err != nil {
		return disownGuidance(err)
	}
	if len(paths) == 0 {
		fmt.Printf("gopgql: %s is already up to date with %s\n", dir, sdlPath)
		return nil
	}
	for _, p := range paths {
		fmt.Printf("gopgql: wrote %s\n", p)
	}
	return nil
}

// The guidance printed when the requested halves contradict the directory's own
// history (design D4a). The refusal itself says what the contradiction is; this
// says what to do instead, which differs per case — and for --no-graph the
// legitimate reason to want it is to get rid of the graph, so the message names
// the deliberate way to do that.
const (
	graphDisownGuidance = "To drop the property graph, generate from a desired schema that declares no\n" +
		"graph: that emits the graph-teardown migration and no rebuild, so the drop is\n" +
		"recorded in the history and reviewable in the diff."

	tablesDisownGuidance = "To hand the tables to another tool from now on, generate the graph half into a\n" +
		"fresh --dir: suppressing table DDL in a directory that creates tables would\n" +
		"leave the graph half naming columns nothing creates."

	tablesAdoptGuidance = "To start managing the tables here too, generate both halves into a fresh\n" +
		"--dir. This history creates no tables, so there is no prior column to diff\n" +
		"against: every column would be emitted as a fresh ADD COLUMN against tables\n" +
		"that already have them, and it would fail only after the graph teardown had\n" +
		"committed — leaving the database with no property graph and this directory\n" +
		"unapplyable. Pass --no-tables to keep generating the graph half alone."
)

// disownGuidance appends the guidance for this refusal to it.
//
// The refusal is migrate's, because that is where the history is read; the
// guidance is the CLI's, because it is about flags. The branch reads the case
// off the error rather than off the flags, so it stays correct however the
// flags are combined — and the adopt case has no flag to read at all.
func disownGuidance(err error) error {
	var conflict *migrate.HalfConflictError
	if !errors.As(err, &conflict) {
		return err
	}
	var guidance string
	switch {
	case conflict.Adopting:
		// Only the tables half is ever refused in this direction: a graph is
		// derivable from the SDL alone, so adopting one is legitimate.
		guidance = tablesAdoptGuidance
	case conflict.Half == migrate.GraphHalf:
		guidance = graphDisownGuidance
	default:
		guidance = tablesDisownGuidance
	}
	return fmt.Errorf("%w\n\n%s", err, guidance)
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

// apply applies every pending migration in dir, in ascending version order.
//
// That is the whole of it: goose's ordinary forward apply against goose's own
// default version table. gopgql neither reorders the migrations nor decides that
// any of them is to be skipped, because the order is the file numbering and the
// numbering is chronological by construction (design D3). A turned-off half
// changes what `generate` writes and never what this applies — a flag that could
// skip part of an applied history is precisely the class of bug the per-half
// version tables created.
//
// A generation is several files and goose runs each in its own transaction, so an
// interrupted run can stop between the graph teardown and the rebuild, leaving a
// database whose tables have moved and which has no property graph. Re-running
// closes the window; queries against the graph fail loudly until then rather than
// returning wrong rows.
func apply(dir, dsn string) error {
	if err := checkDir(dir); err != nil {
		return err
	}
	db, err := connect(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := goose.UpContext(context.Background(), db, dir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	fmt.Printf("gopgql: applied %s\n", dir)
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
