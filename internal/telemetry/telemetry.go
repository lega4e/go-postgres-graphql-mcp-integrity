// Package telemetry establishes gopgql's OpenTelemetry lifecycle.
//
// It is a thin policy layer over github.com/lega4e/goga/telemetry: goga
// builds the tracer, meter and logger providers, installs them as the
// OpenTelemetry globals and returns the cleanup that flushes them; this package
// decides, for gopgql specifically, whether anything is exported at all.
//
// # Silent unless configured
//
// goga follows the OpenTelemetry specification's default, which is OTLP — a
// process with no configuration at all exports to a collector on localhost.
// That default suits a service; it does not suit gopgql. Both binaries here are
// short-lived processes an operator runs by hand or an agent spawns per session
// (`gopgql migrate` in an init container, `gopgql-mcp` on a client's stdio), and
// on a machine with no collector the OTLP exporter's failures would print on
// every run, on stderr, next to the command's real output.
//
// So: if the process environment carries no OTEL_ configuration, every exporter
// is set to "none" and the Prometheus reader is left off. The providers are
// still installed and every span is still opened — the instrumentation is always
// live, it simply has nowhere to go — so turning telemetry on is a matter of
// setting the standard variables and needs no gopgql flag of its own:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 gopgql migrate …
//	OTEL_TRACES_EXPORTER=console gopgql conform …
//
// The presence of any OTEL_ variable is what switches goga's own resolution
// back on, and from there the standard variables mean exactly what the
// specification says they mean.
package telemetry

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	gogatel "github.com/lega4e/goga/telemetry"
)

// envPrefix is the prefix every OpenTelemetry SDK environment variable shares.
// Its presence in the environment is gopgql's signal that somebody configured
// telemetry on purpose.
const envPrefix = "OTEL_"

// exporterNone is the OpenTelemetry-standard exporter name for "export
// nothing". It is what gopgql selects when the environment says nothing.
const exporterNone = "none"

// Setup installs the OpenTelemetry providers for one gopgql process and returns
// the cleanup that flushes them.
//
// service is the binary's name and version its build version, which become the
// service.name and service.version resource attributes. The returned cleanup is
// safe to defer and blocks for at most goga's shutdown timeout.
//
// It is called from a composition root, before anything that could record
// telemetry is constructed: a handle taken by a package before Setup runs
// resolves through the globals on every use and starts emitting the moment they
// are installed, but a span opened before them is lost.
func Setup(ctx context.Context, service, version string) (cleanup func(), err error) {
	opts := []gogatel.Option{
		gogatel.WithServiceName(service),
		gogatel.WithServiceVersion(version),
	}
	if !configured(os.Environ()) {
		opts = append(opts,
			gogatel.WithTraceExporter(exporterNone),
			gogatel.WithMetricExporter(exporterNone),
			gogatel.WithLogExporter(exporterNone),
			gogatel.WithPrometheus(false),
		)
	}

	// goga's Setup makes an OpenTelemetry-bridged logger the slog default, and
	// slog.SetDefault redirects the standard library's log package into that
	// handler too. gopgql cannot have that: goose reports every migration it
	// applies through log.Printf, and those lines are `gopgql migrate`'s answer
	// rather than telemetry — routed into a log bridge with no exporter they
	// would simply disappear. So the log package is put back as it was found.
	//
	// slog itself is left installed: nothing in gopgql writes through it, so a
	// gopgql-bridged slog record can only have come from a library, which is
	// telemetry and belongs in the bridge.
	writer, flags, prefix := log.Writer(), log.Flags(), log.Prefix()

	_, cleanup, err = gogatel.Setup(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gopgql/telemetry: setup: %w", err)
	}

	log.SetOutput(writer)
	log.SetFlags(flags)
	log.SetPrefix(prefix)
	return cleanup, nil
}

// configured reports whether an environment carries any OpenTelemetry SDK
// configuration.
//
// It takes the environment rather than reading it so the policy is testable
// without mutating the process's own.
func configured(environ []string) bool {
	for _, kv := range environ {
		if strings.HasPrefix(kv, envPrefix) {
			return true
		}
	}
	return false
}
