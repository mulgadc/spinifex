package otelsetup

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName is the instrumentation scope every spinifex request metric shares,
// so the HTTP and NATS paths land on one set of instruments.
const meterName = "github.com/mulgadc/spinifex/spinifex/otelsetup"

// actionAttrKey names the logical operation on request metrics. Values must
// stay low-cardinality: resolved action names only, never resource IDs.
const actionAttrKey = "rpc.method"

// leakKindAttrKey names the class of resource that could not be reclaimed.
// Values must stay low-cardinality: resource classes only, never addresses or
// IDs — those belong in the accompanying log line.
const leakKindAttrKey = "resource.kind"

var (
	instrumentsOnce sync.Once
	requestCounter  metric.Int64Counter
	requestDuration metric.Float64Histogram

	leakOnce    sync.Once
	leakCounter metric.Int64Counter
)

// requestInstruments lazily creates the shared request instruments. The
// global meter delegates to the real provider once Init installs it.
func requestInstruments() (metric.Int64Counter, metric.Float64Histogram) {
	instrumentsOnce.Do(func() {
		m := otel.Meter(meterName)
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

// RecordResourceLeak counts one resource that teardown could not reclaim and
// will not retry. kind is a resource class such as "public_ip"; the identity of
// the specific resource goes in the caller's log line, not here.
func RecordResourceLeak(ctx context.Context, kind string) {
	leakOnce.Do(func() {
		var err error
		leakCounter, err = otel.Meter(meterName).Int64Counter("mulga.resource.leaked",
			metric.WithDescription("Count of resources teardown abandoned without reclaiming."),
			metric.WithUnit("{resource}"))
		if err != nil {
			otel.Handle(err)
		}
	})
	if leakCounter != nil {
		leakCounter.Add(ctx, 1, metric.WithAttributeSet(
			attribute.NewSet(attribute.String(leakKindAttrKey, kind))))
	}
}
