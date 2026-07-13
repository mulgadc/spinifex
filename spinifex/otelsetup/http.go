package otelsetup

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const httpTracerName = "github.com/mulgadc/spinifex/spinifex/otelsetup"

// statusRecorder captures the response status for span attributes.
type statusRecorder struct {
	http.ResponseWriter

	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap returns the wrapped ResponseWriter, letting http.ResponseController
// (and any other Unwrap-aware caller) walk past statusRecorder to reach the
// real flusher underneath.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush implements http.Flusher by delegating to the wrapped ResponseWriter.
// Without this, statusRecorder blocks every downstream Flush call (including
// chi's WrapResponseWriter, which wraps statusRecorder) from ever reaching
// the real writer: the call succeeds as a no-op but no bytes reach the
// socket, silently breaking streaming handlers.
func (w *statusRecorder) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// HTTPMiddleware opens a server span per request, honoring an inbound W3C
// traceparent header, and records request count/duration metrics. Handlers
// rename the span (and SetRequestAction) once they resolve a logical
// operation (e.g. the SigV4 action). No-op unless Init configured export.
func HTTPMiddleware(serverName string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health probes fire every few seconds per service and would root a
			// trace each — record metrics only.
			if r.URL.Path == "/health" {
				start := time.Now()
				rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
				next.ServeHTTP(rec, r)
				outcome := "success"
				if rec.status >= http.StatusInternalServerError {
					outcome = "error"
				}
				RecordRequest(r.Context(), r.Method+" /health", outcome, time.Since(start))
				return
			}
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			action := &requestAction{name: r.Method}
			ctx = context.WithValue(ctx, requestActionKey{}, action)
			ctx, span := otel.Tracer(httpTracerName).Start(ctx, r.Method+" "+r.URL.Path,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					semconv.HTTPRequestMethodKey.String(r.Method),
					semconv.URLPath(r.URL.Path),
					attribute.String("server.name", serverName),
				))
			defer span.End()

			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			span.SetAttributes(semconv.HTTPResponseStatusCode(rec.status))
			outcome := "success"
			if rec.status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", rec.status))
				outcome = "error"
			}
			RecordRequest(ctx, action.name, outcome, time.Since(start))
		})
	}
}
