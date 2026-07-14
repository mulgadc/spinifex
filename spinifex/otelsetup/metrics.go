package otelsetup

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// actionAttrKey names the logical operation on request metrics. Values must
// stay low-cardinality: resolved action names only, never resource IDs.
const actionAttrKey = "rpc.method"

var (
	instrumentsOnce sync.Once
	requestCounter  metric.Int64Counter
	requestDuration metric.Float64Histogram
)

// requestInstruments lazily creates the shared request instruments. The
// global meter delegates to the real provider once Init installs it.
func requestInstruments() (metric.Int64Counter, metric.Float64Histogram) {
	instrumentsOnce.Do(func() {
		m := otel.Meter(httpTracerName)
		var err error
		requestCounter, err = m.Int64Counter("mulga.requests",
			metric.WithDescription("Count of service requests handled."),
			metric.WithUnit("{request}"))
		if err != nil {
			otel.Handle(err)
		}
		requestDuration, err = m.Float64Histogram("mulga.request.duration",
			metric.WithDescription("Duration of handled service requests."),
			metric.WithUnit("s"))
		if err != nil {
			otel.Handle(err)
		}
	})
	return requestCounter, requestDuration
}

// RecordRequest records one handled request on the shared counter and
// duration histogram. outcome is "success"/"error", or empty when the
// result is not observable at the instrumentation point.
func RecordRequest(ctx context.Context, action, outcome string, elapsed time.Duration) {
	counter, duration := requestInstruments()
	attrs := []attribute.KeyValue{attribute.String(actionAttrKey, action)}
	if outcome != "" {
		attrs = append(attrs, attribute.String("outcome", outcome))
	}
	opt := metric.WithAttributeSet(attribute.NewSet(attrs...))
	if counter != nil {
		counter.Add(ctx, 1, opt)
	}
	if duration != nil {
		duration.Record(ctx, elapsed.Seconds(), opt)
	}
}

// requestActionKey carries a mutable per-request holder so downstream
// middleware can name the request after routing/auth resolves the action.
type requestActionKey struct{}

type requestAction struct{ name string }

// SetRequestAction sets the logical action recorded on request metrics for
// the in-flight request. No-op when the request did not pass through
// HTTPMiddleware.
func SetRequestAction(ctx context.Context, action string) {
	if h, ok := ctx.Value(requestActionKey{}).(*requestAction); ok && action != "" {
		h.name = action
	}
}
