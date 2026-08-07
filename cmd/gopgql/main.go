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
	"path/filepath"
	"text/tabwriter"

	// Registers the "pgx" database/sql driver goose runs through.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/lega4e/gopgql/conform"
	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/generator/client"
	"github.com/lega4e/gopgql/internal/sdlsource"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/schema"
	"github.com/lega4e/gopgql/sdl"
)

const usage = `gopgql — generate and apply migrations from a GraphQL SDL schema.

Usage:
  gopgql generate --sdl <path> [--sdl <path>…] --dir <dir> [--name <suffix>]
                  [--graph <name>] [--json-type <type>] [--no-tables] [--no-graph]
  gopgql generate client --sdl <path> [--sdl <path>…] --operations <dir> --out <dir>
                  [--package <name>] [--graph <name>]
  gopgql migrate  --dsn <url> [--sdl <path>…] [--dir <dir>] [--name <suffix>]
                  [--graph <name>] [--json-type <type>] [--no-tables] [--no-graph]
  gopgql conform  --sdl <path> [--sdl <path>…] --dsn <url> [--graph <name>]

Commands:
  generate   Write the next migration for the schema into --dir.
  generate client
             Write a typed Go client for --sdl and the named GraphQL operations
             in --operations. Every operation is compiled here, so a query that
             cannot compile fails this command and never a request. Every
             generated method takes an exec.Handle as its second parameter: the
             client opens no connection and holds no pool, so an operation runs
             in whatever transaction the caller is already in.
  migrate    Generate (when --sdl is given) and apply --dir to --dsn.
  conform    Report how the database's property graph differs from the SDL.
  version    Print the version, commit and build date (also --version, -v).

Flags:
  --sdl    Path to an SDL schema file, or to a directory whose *.graphql files
           are read in sorted order (not recursive). Repeatable: every document
           is parsed as one schema, so a property graph can span PostgreSQL
           schemas declared in separate files, and splitting a schema across
           files produces exactly what concatenating them would.
           GOPGQL_SDL holds one path or several, separated the way the platform
           separates a path list (':' on Unix, ';' on Windows).
  --json-type  Physical type for the JSON scalar: "jsonb" (default) or "json".
           jsonb is indexable and queryable; json is what round-trips a document
           byte-identically, because jsonb sorts keys, drops insignificant
           whitespace and keeps only the last of a duplicated key. This is the
           default for every JSON column; @column(type: ...) still wins on the
           column that carries it. (env GOPGQL_JSON_TYPE)
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
           apply; a generation is not atomic, so an interrupted run may leave
           the graph down — re-run it and it continues from where it stopped.
  --no-tables  Skip the tables half — someone else owns the tables, and the
               SDL describes only the slice surfaced as a graph. Nothing about
               a table is read, diffed or emitted. (env GOPGQL_NO_TABLES)
  --no-graph   Skip the property-graph half. (env GOPGQL_NO_GRAPH)
           The flags scope a directory's first generation; after that the
           directory's own history decides which halves it manages, and a flag
           that contradicts it is an error. They scope what is *generated* —
           applying is always the whole directory.
  --operations  Directory of *.graphql operation documents, for generate
           client. Not searched recursively. (env GOPGQL_OPERATIONS)
  --out    Directory the generated package is written to, for generate client.
           (env GOPGQL_CLIENT_OUT)
  --package  Package name for the generated code. Default "gopgqlclient".
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

// commonFlags are the flags several subcommands share, registered once so their
// spellings, their GOPGQL_* fallbacks and their flag-wins-over-env precedence
// cannot drift apart between subcommands.
type commonFlags struct {
	sdlPaths *sdlsource.PathList
	dsn      *string
	dir      *string
	name     *string
	graph    *string
	jsonType *string
	noTables *bool
	noGraph  *bool
}

// newFlagSet builds a flag set for one subcommand and registers the shared flags
// on it.
//
// One set per subcommand, rather than one shared by all of them, for two
// reasons. Go's flag package stops at the first non-flag argument, so a
// two-word subcommand parsed against a single shared set would parse *zero*
// flags — `gopgql generate client --sdl x` silently generating from nothing.
// And `generate client`'s own flags (--operations, --out, --package) belong to
// it alone; on a shared set they would be offered to `migrate` and `conform`,
// which have no use for them.
func newFlagSet(command string) (*flag.FlagSet, *commonFlags) {
	fs := flag.NewFlagSet("gopgql "+command, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	f := &commonFlags{
		sdlPaths: &sdlsource.PathList{},
		dsn:      fs.String("dsn", "", "PostgreSQL connection string (env GOPGQL_DSN)"),
		dir:      fs.String("dir", "", `migration directory (env GOPGQL_MIGRATIONS, default "migrations")`),
		name:     fs.String("name", "schema", "descriptive suffix for a generated delta"),
		graph:    fs.String("graph", "", "property-graph name (default: the generator's)"),
		jsonType: fs.String("json-type", "", `physical type for the JSON scalar: "jsonb" (default) or "json" (env GOPGQL_JSON_TYPE)`),
		noTables: fs.Bool("no-tables", false, "skip the tables half (someone else owns the tables)"),
		noGraph:  fs.Bool("no-graph", false, "skip the property-graph half"),
	}
	fs.Var(f.sdlPaths, "sdl", sdlsource.FlagUsage)
	return fs, f
}

// parse parses argv and folds the environment in underneath the flags.
//
// It reports flag.ErrHelp as success: -h/--help has already printed the usage
// text through fs.Usage, and reporting it as a failure would make `gopgql
// conform --help` exit non-zero — which, for a command whose exit status *is*
// its result, reads as drift rather than as help.
func (c *commonFlags) parse(fs *flag.FlagSet, argv []string) error {
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelp
		}
		return err
	}

	// A flag wins over the environment.
	if len(*c.sdlPaths) == 0 {
		*c.sdlPaths = sdlsource.EnvPaths(sdlsource.EnvVar)
	}
	if *c.jsonType == "" {
		*c.jsonType = os.Getenv("GOPGQL_JSON_TYPE")
	}
	if *c.dsn == "" {
		*c.dsn = os.Getenv("GOPGQL_DSN")
	}
	if *c.dir == "" {
		*c.dir = os.Getenv("GOPGQL_MIGRATIONS")
	}
	if *c.dir == "" {
		*c.dir = "migrations"
	}
	if !*c.noTables && os.Getenv("GOPGQL_NO_TABLES") != "" {
		*c.noTables = true
	}
	if !*c.noGraph && os.Getenv("GOPGQL_NO_GRAPH") != "" {
		*c.noGraph = true
	}
	if *c.noTables && *c.noGraph {
		return errors.New("--no-tables and --no-graph together leave nothing to do")
	}
	return nil
}

func (c *commonFlags) halves() migrate.Halves {
	return migrate.Halves{NoTables: *c.noTables, NoGraph: *c.noGraph}
}

// buildOptions are the generator settings the flags carry.
func (c *commonFlags) buildOptions() generator.Options {
	return generator.Options{GraphName: *c.graph, JSONType: *c.jsonType}
}

// errHelp signals that the usage text was printed and the process should exit
// zero. It never reaches the user: run swallows it.
var errHelp = errors.New("help requested")

func run(argv []string) error {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}
	command, rest := argv[0], argv[1:]

	// A second word is read only for `generate`, which is the only subcommand
	// that has one.
	if command == "generate" && len(rest) > 0 && rest[0] == "client" {
		err := generateClient(rest[1:])
		if errors.Is(err, errHelp) {
			return nil
		}
		return err
	}

	switch command {
	case "generate":
		fs, f := newFlagSet(command)
		if err := f.parse(fs, rest); err != nil {
			return skipHelp(err)
		}
		if len(*f.sdlPaths) == 0 {
			return errors.New("generate needs a schema: pass --sdl or set GOPGQL_SDL")
		}
		return generate(*f.sdlPaths, *f.dir, *f.name, f.buildOptions(), f.halves())

	case "migrate":
		fs, f := newFlagSet(command)
		if err := f.parse(fs, rest); err != nil {
			return skipHelp(err)
		}
		if *f.dsn == "" {
			return errors.New("migrate needs a database: pass --dsn or set GOPGQL_DSN")
		}
		if len(*f.sdlPaths) > 0 {
			if err := generate(*f.sdlPaths, *f.dir, *f.name, f.buildOptions(), f.halves()); err != nil {
				return err
			}
		}
		return apply(*f.dir, *f.dsn)

	case "conform":
		fs, f := newFlagSet(command)
		if err := f.parse(fs, rest); err != nil {
			return skipHelp(err)
		}
		// Both sides are required: the check is a comparison, and with either
		// half missing there is nothing to compare rather than a default to
		// fall back on.
		if len(*f.sdlPaths) == 0 {
			return errors.New("conform needs a schema: pass --sdl or set GOPGQL_SDL")
		}
		if *f.dsn == "" {
			return errors.New("conform needs a database: pass --dsn or set GOPGQL_DSN")
		}
		return conformCheck(context.Background(), *f.sdlPaths, *f.dsn, f.buildOptions())

	// --version and -v are accepted alongside the subcommand because that is
	// what a reader reaches for first, and neither form collides with anything:
	// no subcommand's flag set defines -v, and a flag in this position is the
	// command. The line goes to stdout — it is the command's output, not a
	// diagnostic.
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

// skipHelp turns the help sentinel into success, so `gopgql generate --help`
// exits zero.
func skipHelp(err error) error {
	if errors.Is(err, errHelp) {
		return nil
	}
	return err
}

// generateClient writes the typed Go client for an SDL and a directory of named
// GraphQL operation documents.
//
// Every operation is compiled here, now, through the same pure compiler a
// request would use — so an unknown root field, a selection past the depth
// ceiling or a scalar with no mapping fails this command rather than a request.
// Nothing contacts a database: --dsn is not a flag of this subcommand because
// there is nothing it could be used for.
func generateClient(argv []string) error {
	// This subcommand registers --sdl and --graph itself rather than through
	// newFlagSet: it takes neither --dir nor --dsn nor the halves flags, and a
	// flag a subcommand cannot act on is worse than one it does not offer.
	fs := flag.NewFlagSet("gopgql generate client", flag.ContinueOnError)
	sdlPaths := &sdlsource.PathList{}
	fs.Var(sdlPaths, "sdl", sdlsource.FlagUsage)
	opsDir := fs.String("operations", "", "directory of *.graphql operation documents (env GOPGQL_OPERATIONS)")
	out := fs.String("out", "", "directory to write the generated package into (env GOPGQL_CLIENT_OUT)")
	pkg := fs.String("package", client.DefaultPackage, "package name for the generated code")
	graph := fs.String("graph", "", "property-graph name (default: the generator's)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelp
		}
		return err
	}

	// A flag wins over the environment, matching every other subcommand.
	if len(*sdlPaths) == 0 {
		*sdlPaths = sdlsource.EnvPaths(sdlsource.EnvVar)
	}
	if *opsDir == "" {
		*opsDir = os.Getenv("GOPGQL_OPERATIONS")
	}
	if *out == "" {
		*out = os.Getenv("GOPGQL_CLIENT_OUT")
	}
	switch {
	case len(*sdlPaths) == 0:
		return errors.New("generate client needs a schema: pass --sdl or set GOPGQL_SDL")
	case *opsDir == "":
		return errors.New("generate client needs operations: pass --operations or set GOPGQL_OPERATIONS")
	case *out == "":
		return errors.New("generate client needs an output directory: pass --out or set GOPGQL_CLIENT_OUT")
	}

	src, err := sdlsource.Load(*sdlPaths)
	if err != nil {
		return err
	}

	// Writing the generated package over its own inputs would destroy them, and
	// the failure would look like a bad generation rather than a lost file.
	// Every schema file is checked, not just the first: --sdl is repeatable, and
	// the one that would be overwritten is as likely to be the last.
	outAbs, opsAbs := abs(*out), abs(*opsDir)
	if outAbs == opsAbs {
		return fmt.Errorf("--out and --operations are the same directory (%s); "+
			"the generated package would be written over the operations it was generated from", *out)
	}
	for _, p := range src.Paths {
		if outAbs == abs(filepath.Dir(p)) {
			return fmt.Errorf("--out is the directory holding %s (%s); "+
				"write the generated package somewhere of its own", p, *out)
		}
	}

	doc, err := sdl.ParseSources(src.Sources...)
	if err != nil {
		return err
	}
	sources, err := client.Load(*opsDir)
	if err != nil {
		return err
	}
	files, err := client.Generate(doc, sources, client.Options{Package: *pkg, GraphName: *graph})
	if err != nil {
		return err
	}
	paths, err := client.Write(*out, files)
	if err != nil {
		return err
	}
	for _, p := range paths {
		fmt.Printf("gopgql: wrote %s\n", p)
	}
	return nil
}

// abs resolves a path for comparison, falling back to the path itself when the
// working directory cannot be read — a comparison that then only ever
// under-reports, never wrongly refuses.
func abs(path string) string {
	p, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return filepath.Clean(p)
}

// generate writes the generation the SDL calls for into dir.
func generate(sdlPaths []string, dir, name string, opts generator.Options, halves migrate.Halves) error {
	model, schemaSrc, err := build(sdlPaths, opts)
	if err != nil {
		return err
	}
	paths, err := migrate.Generate(dir, model, name, halves)
	if err != nil {
		return disownGuidance(err, halves)
	}
	if len(paths) == 0 {
		fmt.Printf("gopgql: %s is already up to date with %s\n", dir, schemaSrc.Display())
		return nil
	}
	for _, p := range paths {
		fmt.Printf("gopgql: wrote %s\n", p)
	}
	return nil
}

// The guidance printed when a turned-off half contradicts the directory's own
// history (design D4a). The refusal itself says what the contradiction is; this
// says what to do instead, which differs per half — and for --no-graph the
// legitimate reason to want it is to get rid of the graph, so the message names
// the deliberate way to do that.
const (
	graphDisownGuidance = "Which halves a directory manages is fixed by its first generation.\n" +
		"To drop the property graph, generate from a desired schema that declares no\n" +
		"graph: that emits the graph-teardown migration and no rebuild, so the drop is\n" +
		"recorded in the history and reviewable in the diff."

	tablesDisownGuidance = "Which halves a directory manages is fixed by its first generation.\n" +
		"To hand the tables to another tool from now on, generate the graph half into a\n" +
		"fresh --dir: suppressing table DDL in a directory that creates tables would\n" +
		"leave the graph half naming columns nothing creates."
)

// disownGuidance appends the per-half guidance to the sentinel refusal.
//
// The refusal is migrate's, because that is where the history is read; the
// guidance is the CLI's, because it is about flags. Which half was turned off is
// known here and nowhere else, so the branch belongs here too.
func disownGuidance(err error, halves migrate.Halves) error {
	if !errors.Is(err, migrate.ErrHalfDisowned) {
		return err
	}
	switch {
	case halves.NoGraph:
		return fmt.Errorf("%w\n\n%s", err, graphDisownGuidance)
	case halves.NoTables:
		return fmt.Errorf("%w\n\n%s", err, tablesDisownGuidance)
	default:
		return err
	}
}

// build parses and validates the SDL and returns the physical schema model,
// alongside the resolved sources so a message can name what was read.
//
// Every --sdl is parsed as one schema, which is what lets one property graph
// span two PostgreSQL schemas declared in two files (gopgql#54). Splitting them
// changes nothing about the result — the model is the same as it would be for
// the files concatenated — so the split is free to follow the ownership
// boundary rather than the generator's convenience.
func build(sdlPaths []string, opts generator.Options) (*schema.Schema, *sdlsource.Schema, error) {
	src, err := sdlsource.Load(sdlPaths)
	if err != nil {
		return nil, nil, err
	}
	doc, err := sdl.ParseSources(src.Sources...)
	if err != nil {
		return nil, nil, err
	}
	m, err := generator.BuildWith(doc, opts)
	if err != nil {
		return nil, nil, err
	}
	return m, src, nil
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
func conformCheck(ctx context.Context, sdlPaths []string, dsn string, opts generator.Options) error {
	desired, schemaSrc, err := build(sdlPaths, opts)
	if err != nil {
		return fmt.Errorf("conformance check did not run: %w", err)
	}
	sdlPath := schemaSrc.Display()
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

	actual, err := conform.Reflect(ctx, exec.PgxQuerier(pool), graphName)
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
