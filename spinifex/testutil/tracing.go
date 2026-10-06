package testutil

import (
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// RecordSpans installs a recording tracer provider and the W3C trace-context
// propagator for the duration of the test, restoring both afterwards. The
// defaults are no-ops, so without this a span assertion sees nothing.
//
// Both are process globals, so a test using this cannot run in parallel.
func RecordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	previousTracer := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousTracer)
		otel.SetTextMapPropagator(previousPropagator)
	})
	return recorder
}

// SpanAttribute returns span's value for key, failing the test when absent.
func SpanAttribute(t *testing.T, span sdktrace.ReadOnlySpan, key string) attribute.Value {
	t.Helper()
	for _, attr := range span.Attributes() {
		if string(attr.Key) == key {
			return attr.Value
		}
	}
	t.Fatalf("span %q has no attribute %q", span.Name(), key)
	return attribute.Value{}
}

// SpansByKind returns the last recorded span of each kind (owner-first routing
// can record several client legs; the last answered). Servers reply before
// ending their span, so this waits for one and fails the test if none ends.
func SpansByKind(t *testing.T, recorder *tracetest.SpanRecorder) (client, server sdktrace.ReadOnlySpan) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		client, server = lastSpansByKind(recorder.Ended())
		if server != nil {
			return client, server
		}
		if time.Now().After(deadline) {
			t.Fatal("no server span recorded within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}

func lastSpansByKind(spans []sdktrace.ReadOnlySpan) (client, server sdktrace.ReadOnlySpan) {
	for _, span := range spans {
		switch span.SpanKind() {
		case trace.SpanKindClient:
			client = span
		case trace.SpanKindServer:
			server = span
		}
	}
	return client, server
}
