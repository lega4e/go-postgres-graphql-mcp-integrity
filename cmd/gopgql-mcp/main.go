// Command gopgql-mcp serves one SDL schema and one PostgreSQL database over
// the Model Context Protocol.
//
// The transport is configurable:
//
//	gopgql-mcp --sdl schema.graphql --dsn postgres://…                    # stdio
//	gopgql-mcp --sdl schema.graphql --transport http --addr :8080         # HTTP
//
// --sdl is repeatable and also takes a directory of *.graphql, read in sorted
// order; every document is served as one schema (gopgql#54).
//
// stdio is the default because that is how an agent spawns a server it owns:
// one process per client, on its stdin/stdout. HTTP is for a server that
// outlives its clients — a container in a compose stack, say — where several
// agents connect to one long-running process over the streamable HTTP
// transport at --path (default /mcp).
//
// The listener under that transport is github.com/lega4e/goga/serve, which
// serves /livez, /readyz, /healthz and /metrics on the same port beside it.
// /healthz is what a container healthcheck probes — it answers without opening
// an MCP session, as the hand-rolled one it replaces did — and none of the four
// is part of a request trace.
//
// GOPGQL_SDL, GOPGQL_DSN, GOPGQL_TRANSPORT, GOPGQL_ADDR and GOPGQL_PATH are the
// environment equivalents; a flag wins over the environment. The DSN is better
// supplied through the environment — an agent's MCP configuration is not a good
// place for a password.
//
// Both tools are traced. Nothing is exported unless the environment carries
// OpenTelemetry configuration — an OTLP exporter failing every second on a
// machine with no collector would write to the same stderr the server's
// diagnostics use — so set OTEL_EXPORTER_OTLP_ENDPOINT (or any other OTEL_
// variable) to turn it on. See internal/telemetry.
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

	"github.com/lega4e/goga/serve"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/internal/sdlsource"
	"github.com/lega4e/gopgql/internal/telemetry"
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
	sdlPaths := &sdlsource.PathList{}
	fs.Var(sdlPaths, "sdl", sdlsource.FlagUsage)
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
	if len(*sdlPaths) == 0 {
		*sdlPaths = sdlsource.EnvPaths(sdlsource.EnvVar)
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
	if len(*sdlPaths) == 0 {
		return errors.New("no schema: pass --sdl or set GOPGQL_SDL")
	}
	if *dsn == "" {
		return errors.New("no database: pass --dsn or set GOPGQL_DSN")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Telemetry is established here, at the top of the composition root, so
	// that everything constructed below it — the schema, the pool, the server
	// and every tool call they serve — runs in a process whose providers are
	// already installed. A span opened before them is lost.
	//
	// The deferred cleanup is registered before the pool's Close, so it runs
	// after it: the pool shuts down, then telemetry flushes, and the shutdown
	// itself is inside the trace.
	//
	// Nothing is exported unless the environment asks for it, which is what
	// keeps an exporter's failures off the stderr of a server whose stdout is
	// the MCP protocol. See internal/telemetry.
	cleanup, err := telemetry.Setup(ctx, "gopgql-mcp", version)
	if err != nil {
		return err
	}
	defer cleanup()

	// Parse and validate the schema before connecting: a half-loaded schema
	// would serve tools that cannot answer.
	src, err := sdlsource.Load(*sdlPaths)
	if err != nil {
		return err
	}
	doc, err := sdl.ParseSources(src.Sources...)
	if err != nil {
		return err
	}

	// Every session starts read-only, so a statement that would write is
	// refused by the database rather than by convention (design D4). The pool
	// is pinged here, so an unreachable database is reported at startup instead
	// of failing every tool call.
	pool, err := exec.OpenReadOnly(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The schema the server serves over MCP is the documents together, which is
	// the same text a single --sdl would have held.
	srv := mcp.New(doc, src.Text, exec.PgxQuerier(pool), mcp.WithVersion(version))

	if *transport == transportHTTP {
		return serveHTTP(ctx, srv, pool, *addr, *path)
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

// The bounds the HTTP transport runs with. Both are what the hand-rolled
// *http.Server this command used to build applied, carried over unchanged so
// that adopting goga/serve is not also a behaviour change.
const (
	// readHeaderTimeout bounds how long a connection may take to send its
	// request headers. It is the one timeout that must stay on a streaming
	// transport: an idle connection held for the life of the process is the
	// cheapest denial of service there is, and no MCP session needs longer than
	// this to send a header.
	readHeaderTimeout = 10 * time.Second

	// shutdownGrace bounds the drain. In-flight tool calls get this long to
	// finish once the context is cancelled.
	shutdownGrace = 10 * time.Second
)

// newMux builds this command's routing: the streamable HTTP transport, mounted
// at path.
//
// It is the whole of gopgql's HTTP handler, and goga/serve takes it as an
// http.Handler without knowing what is on it. It is a function so that the
// tests drive the same routing the binary serves rather than a copy of it.
func newMux(srv *mcp.Server, path string) *http.ServeMux {
	handler := mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv.MCPServer() }, nil)

	mux := http.NewServeMux()
	mux.Handle(path, handler)
	return mux
}

// serveOptions is the goga/serve configuration this command ships.
//
// It is a function rather than a literal inside serveHTTP so that the tests
// assert against the very options the binary runs with. An assertion about a
// timeout or a drain means nothing if the test configured them itself.
//
// ready is the readiness probe's only check. It is taken as a function rather
// than as the pool so that a test can supply one without a database.
func serveOptions(addr string, ready func(ctx context.Context) error) []serve.Option {
	return []serve.Option{
		serve.WithAddr(addr),
		serve.WithReadHeaderTimeout(readHeaderTimeout),
		serve.WithShutdownGrace(shutdownGrace),
		// Whether the database answers is a readiness question, not a liveness
		// one: an unreachable database is a reason to stop sending this
		// instance traffic, not a reason to restart it. Nothing here is a
		// liveness input — the process is either up or it is not — so no health
		// check is registered and /livez answers for the process being alive,
		// which is all it can honestly claim.
		serve.WithReadinessCheck("postgres", ready),
	}
}

// unboundStreamTimeouts clears the whole-request read and write deadlines on
// the listener underneath s.
//
// The streamable HTTP transport holds a response open for the life of an MCP
// session. goga bounds ReadTimeout and WriteTimeout at thirty seconds each by
// default, and net/http sets the write deadline when the request headers are
// read — so a session would be cut off mid-stream at thirty seconds, which is
// not a failure any client could tell from a network fault.
//
// The options cannot express this: WithWriteTimeout rejects zero deliberately,
// on the grounds that no goga timeout should be unbounded. That rule is right
// for a request/response service and wrong for a stream, so the field is set
// through Server.As, which exists for exactly the fields goga does not expose.
// It must happen before Run: net/http reads these as it serves.
//
// A false from As is treated as an error here, unlike the skip-and-carry-on
// goga documents. This command supplies no listener of its own, so the in-tree
// *net/http.Server is guaranteed; and if that ever stops being true, carrying
// on would silently reinstate a thirty-second cap on every MCP session rather
// than merely skip a tweak.
func unboundStreamTimeouts(s *serve.Server) error {
	var std *http.Server
	if !s.As(&std) {
		return errors.New("serve: listener is not a *net/http.Server, so the streaming read and write deadlines cannot be cleared")
	}
	std.ReadTimeout = 0
	std.WriteTimeout = 0
	return nil
}

// serveHTTP serves the streamable HTTP transport until the context is
// cancelled, then drains in-flight requests.
//
// Every session gets the same *mcp.Server: the tools are stateless over one
// schema and one pool, so there is nothing per-client to keep apart.
//
// The mux and the MCP handler mounted on it are this command's own and are
// handed to goga/serve unchanged — the port is http.Handler precisely so that a
// project keeps its own routing. What goga replaces is the listener, its
// timeouts and its drain, and nothing above them.
func serveHTTP(ctx context.Context, srv *mcp.Server, pool *pgxpool.Pool, addr, path string) error {
	// /healthz is no longer registered here. goga serves it, along with /livez,
	// /readyz and /metrics, on a mux it dispatches before this one — so a probe
	// never reaches the MCP handler and never appears in a trace. With no health
	// check registered it answers 200 and "ok\n", byte for byte what the
	// hand-rolled handler returned, which is what the examples' container
	// healthchecks read.
	httpSrv, err := serve.New(ctx, newMux(srv, path), serveOptions(addr, pool.Ping)...)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	if err := unboundStreamTimeouts(httpSrv); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "gopgql-mcp: listening on %s%s\n", addr, path)

	// Run installs no signal handling of its own and returns when ctx is
	// cancelled: this command keeps the handler it established in run, which is
	// the one handler the process has.
	if err := httpSrv.Run(ctx); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
