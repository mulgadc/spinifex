package otelsetup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

// The global meter binds its instruments to the first real provider installed
// in the process and ignores later ones, so the reader is installed once and
// each test filters by its own action name.
var metricReader = func() *sdkmetric.ManualReader {
	r := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	return r
}()

// requestPointsFor returns the outcome of every mulga.requests point recorded
// under action's rpc.method attribute. Points are cumulative for the life of
// the test binary, so actions must be unique per test.
func requestPointsFor(t *testing.T, action string) []string {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := metricReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	var outcomes []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if m.Name != "mulga.requests" || !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if v, found := dp.Attributes.Value(attribute.Key(actionAttrKey)); !found || v.AsString() != action {
					continue
				}
				v, _ := dp.Attributes.Value(attribute.Key("outcome"))
				outcomes = append(outcomes, v.AsString())
			}
		}
	}
	return outcomes
}

// TestHTTPMiddlewareRecordsOnSpinifexInstruments pins the WithRecorder binding:
// requests must land on spinifex's rpc.method counter, the one the NATS paths
// also write to, not on bluebottle's s3.action instruments.
func TestHTTPMiddlewareRecordsOnSpinifexInstruments(t *testing.T) {
	withRecorder(t)

	h := HTTPMiddleware("test-server")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetRequestAction(r.Context(), "test.http.recorded")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/latest/api/token", nil))

	if got := requestPointsFor(t, "test.http.recorded"); len(got) != 1 || got[0] != "client_error" {
		t.Errorf("recorded outcomes = %v, want [client_error]", got)
	}
}

// TestHTTPMiddlewareHealthProbeSkipsTrace guards the behaviour the fork existed
// for: health probes fire every few seconds per service and must be measured
// without rooting a trace each.
func TestHTTPMiddlewareHealthProbeSkipsTrace(t *testing.T) {
	sr := withRecorder(t)

	h := HTTPMiddleware("test-server")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if trace.SpanContextFromContext(r.Context()).IsValid() {
			t.Error("health probe rooted a span")
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))

	if spans := sr.Ended(); len(spans) != 0 {
		t.Fatalf("ended spans = %d, want 0", len(spans))
	}
	if got := requestPointsFor(t, "GET /health"); len(got) != 1 || got[0] != "success" {
		t.Errorf("recorded outcomes = %v, want [success]", got)
	}
}

// TestHTTPMiddlewareStillTracesRealRequests keeps the probe skip path-scoped.
func TestHTTPMiddlewareStillTracesRealRequests(t *testing.T) {
	sr := withRecorder(t)

	h := HTTPMiddleware("test-server")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/foo", nil))

	if spans := sr.Ended(); len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
}
