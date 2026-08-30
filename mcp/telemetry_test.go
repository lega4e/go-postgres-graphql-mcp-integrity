package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recordSpans installs an in-memory tracer provider as the OpenTelemetry global
// for the duration of one test and returns the recorder.
//
// Installing the global is what makes this a test of the instrumentation rather
// than of a tracer handed in: telemetry.For resolves through the globals on
// every use, which is the property that lets a package take its handle at
// construction time and still emit once the composition root has configured
// telemetry.
func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	rec := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(rec))
	saved := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(saved)
		// Not t.Context(): it is already cancelled by the time a cleanup runs,
		// and a shutdown through a cancelled context reports that rather than
		// flushing.
		require.NoError(t, tp.Shutdown(context.Background()))
	})
	return rec
}

// spanNamed returns the one recorded span with this name.
func spanNamed(t *testing.T, rec *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	t.Helper()
	var found []tracetest.SpanStub
	for _, s := range rec.GetSpans() {
		if s.Name == name {
			found = append(found, s)
		}
	}
	require.Len(t, found, 1, "expected exactly one %q span, got %d", name, len(found))
	return found[0]
}

// TestIntrospectIsTraced is the adoption's basic claim: a tool call that was
// invisible before now produces a span.
func TestIntrospectIsTraced(t *testing.T) {
	rec := recordSpans(t)
	s := newTestServer(t)

	_, err := s.Introspect(t.Context(), "Person", false, "")
	require.NoError(t, err)

	span := spanNamed(t, rec, "goga.mcp.Introspect")
	assert.Equal(t, codes.Ok, span.Status.Code)
	assert.Contains(t, span.Attributes, attrIntrospectType.String("Person"))
	assert.Contains(t, span.Attributes, attrFormat.String(FormatIntrospection))

	// The introspection is answered by running a query over the schema, so the
	// query span is a child of the introspect span rather than a trace of its
	// own. That nesting is the whole reason Introspect takes a context.
	inner := spanNamed(t, rec, "goga.mcp.Query")
	assert.Equal(t, span.SpanContext.SpanID(), inner.Parent.SpanID())
}

// TestFailedCallIsRecordedAsAnError guards the defect the goga conventions
// exist to prevent: with an unnamed result, the deferred closer observes a nil
// error variable and every failure is recorded as a success — which inverts the
// one signal telemetry is for. It fails loudly if the spans below are ever
// reintroduced without their named results.
func TestFailedCallIsRecordedAsAnError(t *testing.T) {
	rec := recordSpans(t)
	s := newTestServer(t)

	t.Run("introspect", func(t *testing.T) {
		_, err := s.Introspect(t.Context(), "", false, "yaml")
		require.Error(t, err)
		assert.Equal(t, codes.Error, spanNamed(t, rec, "goga.mcp.Introspect").Status.Code)
	})

	t.Run("query", func(t *testing.T) {
		rec.Reset()
		_, err := s.Query(t.Context(), `{ notAField { id } }`, nil, FormatJSON)
		require.Error(t, err)
		assert.Equal(t, codes.Error, spanNamed(t, rec, "goga.mcp.Query").Status.Code)
	})
}
