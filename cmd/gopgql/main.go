// Command gopgql is the schema-side CLI: it turns an SDL document into goose
// migrations and applies them (SPEC.md §4.1).
//
//	gopgql generate --sdl schema.graphql --dir migrations
//	gopgql migrate  --sdl schema.graphql --dsn postgres://…
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
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	// Registers the "pgx" database/sql driver goose runs through.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

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

Commands:
  generate   Write the next migration for the schema into --dir.
  migrate    Generate (when --sdl is given) and apply --dir to --dsn.

Flags:
  --sdl    Path to the SDL schema.                     (env GOPGQL_SDL)
  --dsn    PostgreSQL connection string.               (env GOPGQL_DSN)
  --dir    Migration directory. Default "migrations".  (env GOPGQL_MIGRATIONS)
           Holds two subdirectories, generated and applied in this order:
             <dir>/tables/  CREATE TABLE and its indexes
             <dir>/graph/   CREATE PROPERTY GRAPH
           Tables are applied first: the graph references them, so applying
           the graph half against absent tables is refused by the database.
  --no-tables  Skip the tables half — someone else owns the tables, and the
               SDL describes only the slice surfaced as a graph. Nothing about
               a table is read, diffed or emitted. (env GOPGQL_NO_TABLES)
  --no-graph   Skip the property-graph half. (env GOPGQL_NO_GRAPH)
  --name   Descriptive suffix for a generated delta. Default "schema".
  --graph  Property-graph name. Default is the generator's.
A flag wins over its environment variable.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "gopgql: %v\n", err)
		os.Exit(1)
	}
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
	generate func(dir string, desired *schema.Schema, name string) (string, error)
}

// selectedHalves returns the halves to generate and apply, always in dependency
// order: tables before the graph that references them.
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
	for _, h := range halves {
		sub := filepath.Join(dir, h.subdir)
		path, err := h.generate(sub, model, name)
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

// applyHalves applies the selected halves in the only order that works.
//
// It is not simply "tables, then graph". A property graph depends on the
// columns it exposes, so PostgreSQL refuses to alter or drop one while the
// graph is up — the tables half cannot land its own delta underneath a live
// graph. So when both halves are in play the graph is taken down first, the
// tables are brought up, and the graph is rebuilt on top:
//
//	graph down → tables up → graph up
//
// On a fresh database the first step is a no-op. Rebuilding the graph every
// time costs nothing: graphs are metadata and every delta already drops and
// recreates them (SPEC.md §7 → M2).
//
// A project that owns only one half never hits this, and applies just its own.
func applyHalves(dir string, halves []half, dsn string) error {
	var tables, graph *half
	for i := range halves {
		switch halves[i].subdir {
		case migrate.TablesDir:
			tables = &halves[i]
		case migrate.GraphDir:
			graph = &halves[i]
		}
	}

	if tables != nil && graph != nil {
		name, err := graphName(filepath.Join(dir, graph.subdir))
		if err != nil {
			return err
		}
		if name != "" {
			if err := dropGraph(name, dsn); err != nil {
				return fmt.Errorf("release the property graph before altering its tables: %w", err)
			}
		}
	}
	for _, h := range halves {
		if err := apply(filepath.Join(dir, h.subdir), h.subdir, dsn); err != nil {
			return err
		}
	}
	return nil
}

// graphName folds the graph directory to find out what the graph is called.
// An empty result means the directory declares no graph yet, so there is
// nothing to release.
func graphName(dir string) (string, error) {
	if _, err := os.Stat(dir); err != nil {
		return "", nil
	}
	folded, err := migrate.Fold(dir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", dir, err)
	}
	if folded == nil {
		return "", nil
	}
	return folded.GraphName, nil
}

// dropGraph releases the property graph's dependency on its tables.
//
// It is deliberately not a goose rollback. Rolling the graph directory back to
// zero and replaying it would re-run historical CREATE PROPERTY GRAPH
// statements against tables that have since moved on — a graph from three
// migrations ago naming a column that no longer exists. Only the current graph
// needs to go, and the graph half's own next migration recreates the right one.
func dropGraph(name, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), migrate.DropGraphSQL(name)); err != nil {
		return err
	}
	return nil
}

// apply runs the migrations in dir against the database.
func apply(dir, subdir, dsn string) error {
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

	// Each half records itself in its own version table: both start at 0001,
	// so a shared one would make goose skip the second half's initial
	// migration as already applied.
	goose.SetTableName(migrate.VersionTable(subdir))
	if err := goose.UpContext(context.Background(), db, dir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	fmt.Printf("gopgql: applied %s\n", dir)
	return nil
}
