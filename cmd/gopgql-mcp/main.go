// Command gopgql-mcp serves one SDL schema and one PostgreSQL database over
// the Model Context Protocol.
//
// The transport is configurable:
//
//	gopgql-mcp --sdl schema.graphql --dsn postgres://…                    # stdio
//	gopgql-mcp --sdl schema.graphql --transport http --addr :8080         # HTTP
//
// stdio is the default because that is how an agent spawns a server it owns:
// one process per client, on its stdin/stdout. HTTP is for a server that
// outlives its clients — a container in a compose stack, say — where several
// agents connect to one long-running process over the streamable HTTP
// transport at --path (default /mcp). /healthz answers a container healthcheck.
//
// GOPGQL_SDL, GOPGQL_DSN, GOPGQL_TRANSPORT, GOPGQL_ADDR and GOPGQL_PATH are the
// environment equivalents; a flag wins over the environment. The DSN is better
// supplied through the environment — an agent's MCP configuration is not a good
// place for a password.
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
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/mcp"
	"github.com/lega4e/gopgql/sdl"
)

// Build information, stamped by the release pipeline. version is also the
// implementation version the server reports over MCP. All three are overridable
// at build time with -ldflags "-X main.version=… -X main.commit=… -X
// main.date=…"; the defaults are what a plain `go build` leaves behind.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// versionLine renders the build information as the single line --version
// prints. gopgql prints the same shape, so a bug report naming either binary
// identifies one build.
func versionLine() string {
	return fmt.Sprintf("gopgql-mcp %s (commit %s, built %s)", version, commit, date)
}

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
	transport := fs.String("transport", "", `transport: "stdio" (default) or "http" (env GOPGQL_TRANSPORT)`)
	addr := fs.String("addr", "", `listen address for the http transport, default ":8080" (env GOPGQL_ADDR)`)
	path := fs.String("path", "", `URL path for the http transport, default "/mcp" (env GOPGQL_PATH)`)
	showVersion := fs.Bool("version", false, "print the version, commit and build date, then exit")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	// Answered before the environment is resolved and before anything is
	// validated, so `gopgql-mcp --version` works with no schema, no DSN and no
	// database — which is how a container image is smoke-tested.
	if *showVersion {
		fmt.Println(versionLine())
		return nil
	}

	// A flag wins over the environment.
	if *sdlPath == "" {
		*sdlPath = os.Getenv("GOPGQL_SDL")
	}
	if *dsn == "" {
		*dsn = os.Getenv("GOPGQL_DSN")
	}
	if *transport == "" {
		*transport = envOr("GOPGQL_TRANSPORT", transportStdio)
	}
	if *addr == "" {
		*addr = envOr("GOPGQL_ADDR", ":8080")
	}
	if *path == "" {
		*path = envOr("GOPGQL_PATH", "/mcp")
	}
	if *transport != transportStdio && *transport != transportHTTP {
		return fmt.Errorf("unknown transport %q; supported transports are %q and %q", *transport, transportStdio, transportHTTP)
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

	srv := mcp.New(doc, string(source), exec.PgxQuerier(pool), mcp.WithVersion(version))

	if *transport == transportHTTP {
		return serveHTTP(ctx, srv, *addr, *path)
	}
	// A closed stdin is how an MCP client says goodbye, and a cancelled context
	// is how a signal does; neither is a failure to report.
	if err := srv.Run(ctx, &mcpsdk.StdioTransport{}); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// The transports --transport accepts.
const (
	transportStdio = "stdio"
	transportHTTP  = "http"
)

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// serveHTTP serves the streamable HTTP transport until the context is
// cancelled, then drains in-flight requests.
//
// Every session gets the same *mcp.Server: the tools are stateless over one
// schema and one pool, so there is nothing per-client to keep apart.
func serveHTTP(ctx context.Context, srv *mcp.Server, addr, path string) error {
	handler := mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv.MCPServer() }, nil)

	mux := http.NewServeMux()
	mux.Handle(path, handler)
	// A container healthcheck needs something cheaper than an MCP handshake,
	// and one that does not open a session.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	done := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "gopgql-mcp: listening on %s%s\n", addr, path)
		done <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}
