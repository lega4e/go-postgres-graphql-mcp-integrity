package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/lega4e/goga/serve"
	"github.com/lega4e/goga/serve/servetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/mcp"
	"github.com/lega4e/gopgql/sdl"
)

// testSDL is the smallest schema the MCP server will accept. The transport
// tests below never run a query, so the schema only has to parse.
const testSDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
}
`

// testPath is where these tests mount the transport, matching the --path
// default the binary ships.
const testPath = "/mcp"

// testMux builds the command's real routing over a real MCP server, so that
// what these tests drive is the handler the binary serves and not a stand-in.
// The querier is nil because no test here executes a query.
func testMux(t *testing.T) *http.ServeMux {
	t.Helper()
	doc, err := sdl.Parse(testSDL)
	require.NoError(t, err)
	srv, err := mcp.New(doc, testSDL, nil)
	require.NoError(t, err)
	return newMux(srv, testPath)
}

// okReady is a readiness check that passes, standing in for the pool's Ping.
func okReady(context.Context) error { return nil }

// TestHealthzAnswersTheContainerHealthcheck is the behaviour this adoption had
// to preserve exactly. Every examples/*/docker-compose.yml probes
// `wget -qO- http://127.0.0.1:8080/healthz` on the same port the MCP transport
// listens on, so goga's ops mux has to answer there — with no separate ops
// address configured — and answer without opening an MCP session.
//
// The body is asserted too, not just the status: the hand-rolled handler this
// replaces wrote "ok\n", and goga's probe with no health checks registered
// writes the same, so the replacement is byte-for-byte invisible to the
// healthcheck.
func TestHealthzAnswersTheContainerHealthcheck(t *testing.T) {
	h := servetest.Start(t.Context(), t, testMux(t), serveOptions(":0", okReady)...)

	status, body := h.Get(serve.HealthzPath)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "ok\n", body, "the container healthcheck reads this body")
}

// TestReadinessReportsTheDatabase pins what the readiness probe is wired to.
// A database that does not answer takes this instance out of rotation; it is
// deliberately not a liveness input, because an unreachable database is not a
// reason to restart the process.
func TestReadinessReportsTheDatabase(t *testing.T) {
	down := func(context.Context) error { return errors.New("connection refused") }
	h := servetest.Start(t.Context(), t, testMux(t), serveOptions(":0", down)...)

	status, body := h.Get(serve.ReadyzPath)
	assert.Equal(t, http.StatusServiceUnavailable, status,
		"an unreachable database has to fail readiness")
	assert.Contains(t, body, "postgres", "the probe names the check that failed")

	// Liveness is unaffected: nothing here asks for a restart.
	status, _ = h.Get(serve.LivezPath)
	assert.Equal(t, http.StatusOK, status,
		"a database outage must not make the process look dead")
}

// TestTransportIsTracedOnceAndProbesAreNot is the pair of properties goga/serve
// exists to guarantee, asserted against this command's own options and its own
// handler rather than trusted.
//
// Once is the whole assertion for the transport: two spans would mean the
// handler was instrumented twice, which in a backend looks like a slow service
// rather than like a bug.
func TestTransportIsTracedOnceAndProbesAreNot(t *testing.T) {
	h := servetest.Start(t.Context(), t, testMux(t), serveOptions(":0", okReady)...)

	// A bare GET to the streamable transport is refused by the MCP handler
	// without a session, which is fine: the assertion is that the request was
	// traced exactly once, not that it succeeded.
	h.AssertTracedOnce(testPath)
	h.AssertOpsPathsNotTraced()
}

// TestDrainsInFlightRequest asserts the drain this command used to hand-roll:
// a tool call already running when the process is signalled runs to completion
// and gets its response, rather than losing its connection.
func TestDrainsInFlightRequest(t *testing.T) {
	servetest.AssertDrainsInFlightRequest(t.Context(), t, serveOptions(":0", okReady)...)
}

// TestReadHeaderTimeoutIsEnforced proves the one timeout that survives
// unboundStreamTimeouts is really applied on the wire. An idle connection held
// for the life of the process is the cheapest denial of service there is, and
// it is invisible in every functional test.
func TestReadHeaderTimeoutIsEnforced(t *testing.T) {
	h := servetest.Start(t.Context(), t, testMux(t), serveOptions(":0", okReady)...)
	h.AssertHeaderTimeoutEnforced(readHeaderTimeout + 5*time.Second)
}

// TestUnboundStreamTimeoutsClearsTheStreamDeadlines is a regression test for a
// silent, delayed failure. goga defaults ReadTimeout and WriteTimeout to thirty
// seconds each, and net/http sets the write deadline when the request headers
// are read — so an MCP session over the streamable transport would be cut off
// mid-stream at thirty seconds, indistinguishable from a network fault.
//
// goga's options cannot express this: WithWriteTimeout rejects zero on purpose.
// The deadlines are therefore cleared through Server.As, and this test is what
// keeps that from being dropped as an unexplained line.
func TestUnboundStreamTimeoutsClearsTheStreamDeadlines(t *testing.T) {
	srv, err := serve.New(t.Context(), testMux(t), serveOptions(":0", okReady)...)
	require.NoError(t, err)

	var std *http.Server
	require.True(t, srv.As(&std), "the in-tree listener is a *net/http.Server")
	require.NotZero(t, std.WriteTimeout,
		"goga is expected to default a write timeout; without one this test asserts nothing")

	require.NoError(t, unboundStreamTimeouts(srv))

	assert.Zero(t, std.WriteTimeout, "a bounded write deadline cuts every MCP session short")
	assert.Zero(t, std.ReadTimeout, "a bounded read deadline cuts every MCP session short")
	assert.Equal(t, readHeaderTimeout, std.ReadHeaderTimeout,
		"the idle-connection bound must survive: it is the one timeout a stream still needs")
}

// TestServeOptionsAreAccepted guards the option values themselves. goga rejects
// a non-positive duration at the call site that supplied it, so a zero constant
// here would fail at startup rather than in a test.
func TestServeOptionsAreAccepted(t *testing.T) {
	_, err := serve.New(t.Context(), http.NewServeMux(), serveOptions(":0", okReady)...)
	require.NoError(t, err)
}
