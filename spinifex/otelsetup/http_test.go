package otelsetup

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// withRecorder installs a recording tracer provider for the test and returns
// the recorder; the previous global provider is restored on cleanup.
func withRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return sr
}

// flusherSpy records whether Flush was called, satisfying http.Flusher so a
// statusRecorder wrapping it can prove the flush chain reaches it.
type flusherSpy struct {
	http.ResponseWriter

	flushed bool
}

func (f *flusherSpy) Flush() { f.flushed = true }

// TestStatusRecorderFlushReachesUnderlyingFlusher guards the §0 fix: before
// Unwrap()/Flush() were added, http.NewResponseController(rec).Flush() would
// fail with ErrNotSupported (or, via a Flusher-only outer wrapper such as
// chi's WrapResponseWriter, silently no-op) because statusRecorder itself was
// not a Flusher and exposed no way to reach the one it wraps.
func TestStatusRecorderFlushReachesUnderlyingFlusher(t *testing.T) {
	spy := &flusherSpy{ResponseWriter: httptest.NewRecorder()}
	rec := &statusRecorder{ResponseWriter: spy, status: http.StatusOK}

	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !spy.flushed {
		t.Error("flush did not reach the underlying Flusher spy")
	}
}

func TestHTTPMiddlewareSpanPerRequest(t *testing.T) {
	sr := withRecorder(t)

	h := HTTPMiddleware("test-server")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !trace.SpanContextFromContext(r.Context()).IsValid() {
			t.Error("handler context has no span")
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/foo", nil))

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "POST /foo" {
		t.Errorf("span name = %q, want %q", span.Name(), "POST /foo")
	}
	got := map[string]string{}
	for _, kv := range span.Attributes() {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	if got["http.response.status_code"] != "418" {
		t.Errorf("status attr = %q, want 418", got["http.response.status_code"])
	}
	if got["server.name"] != "test-server" {
		t.Errorf("server.name = %q", got["server.name"])
	}
	if span.Status().Code == codes.Error {
		t.Error("4xx must not mark span as error")
	}
}

func TestHTTPMiddleware5xxSetsErrorStatus(t *testing.T) {
	sr := withRecorder(t)

	h := HTTPMiddleware("test-server")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if spans[0].Status().Code != codes.Error {
		t.Errorf("span status = %v, want Error on 5xx", spans[0].Status().Code)
	}
}

func TestHTTPMiddlewareExtractsTraceparent(t *testing.T) {
	sr := withRecorder(t)
	// Extraction needs the W3C propagator installed (Init does this even
	// without an endpoint).
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if _, err := Init(t.Context(), "test-svc"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	h := HTTPMiddleware("test-server")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/y", nil)
	req.Header.Set("Traceparent", "00-0102030405060708090a0b0c0d0e0f10-0a0b0c0d0e0f0102-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if got := spans[0].SpanContext().TraceID().String(); got != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("trace id = %s, want inbound traceparent trace id", got)
	}
	if got := spans[0].Parent().SpanID().String(); got != "0a0b0c0d0e0f0102" {
		t.Errorf("parent span id = %s, want inbound span id", got)
	}
}
