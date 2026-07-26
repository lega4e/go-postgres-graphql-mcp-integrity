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
  gopgql migrate  --dsn <url> [--sdl <file>] [--dir <dir>] [--name <suffix>] [--graph <name>]

Commands:
  generate   Write the next migration for the schema into --dir.
  migrate    Generate (when --sdl is given) and apply --dir to --dsn.

Flags:
  --sdl    Path to the SDL schema.                     (env GOPGQL_SDL)
  --dsn    PostgreSQL connection string.               (env GOPGQL_DSN)
  --dir    Migration directory. Default "migrations".  (env GOPGQL_MIGRATIONS)
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
