package handlers_imds

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// withRecordedSpans installs a fresh SpanRecorder as the global tracer
// provider for the duration of the test and restores the previous provider
// on cleanup, mirroring gateway/trace_test.go's pattern.
func withRecordedSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return sr
}

// TestHTTPHandler_ProducesSpan pins the httpHandler wrap added for
// otelsetup.HTTPMiddleware: a normal guest request through the IMDS mux opens
// one root server span, so the DescribeInstances/Attribute NATS fan-out it
// triggers has something to parent to instead of rooting its own trace.
func TestHTTPHandler_ProducesSpan(t *testing.T) {
	sr := withRecordedSpans(t)

	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())

	rec := get(t, h, prefixMetaData+"instance-id", "")
	// Unauthorized (no token) is expected here; the span must exist regardless
	// of the eventual handler outcome.
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	spans := sr.Ended()
	require.Len(t, spans, 1, "expected exactly one server span for the request")
	assert.Equal(t, "GET "+prefixMetaData+"instance-id", spans[0].Name())
}

// TestHTTPHandler_RejectedForwardedStillTraced is the load-bearing assertion
// for the middleware-ordering call: otelsetup.HTTPMiddleware wraps OUTSIDE
// rejectForwarded, so an X-Forwarded-For request rejected by the SSRF guard
// still produces a span (the 431+431 rooted fan-out gap the bead reports
// starts on requests exactly like this) — but the rejection itself is
// unaffected by tracing being present. A span cannot let a rejected request
// through; it only observes it.
func TestHTTPHandler_RejectedForwardedStillTraced(t *testing.T) {
	sr := withRecordedSpans(t)

	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())

	req := httptest.NewRequest(http.MethodGet, "http://"+MetaDataServerIP+prefixMetaData+"instance-id", nil)
	req.RemoteAddr = testIP + ":50000"
	req.Header.Set(hdrForwardedFor, "203.0.113.99")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// The security control's outcome must be unchanged by the middleware wrap.
	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.Empty(t, rr.Body.String())

	spans := sr.Ended()
	require.Len(t, spans, 1, "rejected request must still be traced")
	assert.Equal(t, "GET "+prefixMetaData+"instance-id", spans[0].Name())
}
