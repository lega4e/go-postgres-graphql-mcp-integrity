package telemetry

import (
	"bytes"
	"log"
	"testing"

	gogatel "github.com/lega4e/goga/telemetry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

func TestConfiguredReadsTheOTelEnvironment(t *testing.T) {
	assert.False(t, configured(nil))
	assert.False(t, configured([]string{"PATH=/usr/bin", "GOPGQL_DSN=postgres://x"}))

	// Any OTEL_ variable counts: an operator who set one configured telemetry
	// on purpose, and from there the standard variables mean what the
	// specification says they mean.
	assert.True(t, configured([]string{"PATH=/usr/bin", "OTEL_TRACES_EXPORTER=console"}))
	assert.True(t, configured([]string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318"}))
}

// TestSetupInstallsProviders is the property the two commands depend on: after
// Setup, a span opened through a goga handle is a real recorded span and not
// the no-op the OpenTelemetry globals start out as.
//
// It runs with whatever environment the test process has, which on a developer
// machine and in CI alike has no collector — the exporters are irrelevant here,
// the installed providers are the point.
func TestSetupInstallsProviders(t *testing.T) {
	before := gogatel.For("probe")
	_, endBefore := before.Start(t.Context(), "beforeSetup")
	endBefore(nil)

	cleanup, err := Setup(t.Context(), "gopgql-test", "v0.0.0-test")
	require.NoError(t, err)
	require.NotNil(t, cleanup)
	t.Cleanup(cleanup)

	// The same handle, taken before Setup — a handle resolves through the
	// globals on every use, so a package that takes one at construction time
	// starts emitting as soon as the composition root has run.
	ctx, end := before.Start(t.Context(), "afterSetup")
	defer end(nil)

	assert.True(t, trace.SpanFromContext(ctx).IsRecording(),
		"Setup did not install a recording tracer provider")
}

// TestSetupLeavesTheStandardLoggerAlone is a regression test for output that
// would otherwise vanish. goga installs an OpenTelemetry-bridged slog default,
// and slog.SetDefault redirects the standard library's log package into the
// same handler — so `gopgql migrate`, whose per-migration lines are goose's
// log.Printf calls, would print nothing at all with no log exporter configured.
func TestSetupLeavesTheStandardLoggerAlone(t *testing.T) {
	var buf bytes.Buffer
	saved, savedFlags, savedPrefix := log.Writer(), log.Flags(), log.Prefix()
	t.Cleanup(func() {
		log.SetOutput(saved)
		log.SetFlags(savedFlags)
		log.SetPrefix(savedPrefix)
	})
	log.SetOutput(&buf)

	cleanup, err := Setup(t.Context(), "gopgql-test", "v0.0.0-test")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	log.Printf("OK 00001_init.sql")
	assert.Contains(t, buf.String(), "OK 00001_init.sql",
		"Setup swallowed the standard library logger; goose's migration output goes through it")
}
