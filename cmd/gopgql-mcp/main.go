// Command gopgql-mcp serves one SDL schema and one PostgreSQL database over
// the Model Context Protocol, on stdio.
//
//	gopgql-mcp --sdl schema.graphql --dsn postgres://user:pass@host/db
//
// GOPGQL_SDL and GOPGQL_DSN are the environment equivalents; a flag wins over
// the environment. The DSN is better supplied through the environment — an
// agent's MCP configuration is not a good place for a password.
//
// The server exposes two tools, `introspect` and `query`. It applies no
// migrations and executes no writes: its pool is opened with
// default_transaction_read_only=on, so even a statement that tried to write
// would be refused by the database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/mcp"
	"github.com/lega4e/gopgql/sdl"
)

// version is the implementation version the server reports. It is overridable
// at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "gopgql-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	fs := flag.NewFlagSet("gopgql-mcp", flag.ContinueOnError)
	sdlPath := fs.String("sdl", "", "path to the SDL schema (env GOPGQL_SDL)")
	dsn := fs.String("dsn", "", "PostgreSQL connection string (env GOPGQL_DSN)")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	// A flag wins over the environment.
	if *sdlPath == "" {
		*sdlPath = os.Getenv("GOPGQL_SDL")
	}
	if *dsn == "" {
		*dsn = os.Getenv("GOPGQL_DSN")
	}
	if *sdlPath == "" {
		return errors.New("no schema: pass --sdl or set GOPGQL_SDL")
	}
	if *dsn == "" {
		return errors.New("no database: pass --dsn or set GOPGQL_DSN")
	}

	// Parse and validate the schema before connecting: a half-loaded schema
	// would serve tools that cannot answer.
	source, err := os.ReadFile(*sdlPath)
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	doc, err := sdl.Parse(string(source))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Every session starts read-only, so a statement that would write is
	// refused by the database rather than by convention (design D4). The pool
	// is pinged here, so an unreachable database is reported at startup instead
	// of failing every tool call.
	pool, err := exec.OpenReadOnly(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	srv := mcp.New(doc, string(source), pool, mcp.WithVersion(version))
	// A closed stdin is how an MCP client says goodbye, and a cancelled context
	// is how a signal does; neither is a failure to report.
	if err := srv.Run(ctx, &mcpsdk.StdioTransport{}); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
