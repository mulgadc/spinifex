package instancecache

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName is the instrumentation scope for every instancecache metric.
const meterName = "github.com/mulgadc/spinifex/spinifex/instancecache"

// cacheMetrics holds one Cache's instruments. Registered per instance, not as
// a package-level singleton, because the gauges read that instance's own
// state through a callback closure.
type cacheMetrics struct {
	resyncFailures  metric.Int64Counter
	watchReconnects metric.Int64Counter
	decodeFailures  metric.Int64Counter
}

// newCacheMetrics registers c's instruments against the global meter
// provider. A registration failure is handled by otel.Handle and leaves the
// affected instrument nil; every recorder below tolerates a nil instrument.
func newCacheMetrics(c *Cache) *cacheMetrics {
	meter := otel.Meter(meterName)
	m := &cacheMetrics{}

	var err error
	m.resyncFailures, err = meter.Int64Counter("mulga.instancecache.resync_failures",
		metric.WithDescription("Count of failed initial, periodic or watcher-replacement syncs."),
		metric.WithUnit("{failure}"))
	if err != nil {
		otel.Handle(err)
	}
	m.watchReconnects, err = meter.Int64Counter("mulga.instancecache.watch_reconnects",
		metric.WithDescription("Count of times the KV watch was replaced after dying."),
		metric.WithUnit("{reconnect}"))
	if err != nil {
		otel.Handle(err)
	}
	m.decodeFailures, err = meter.Int64Counter("mulga.instancecache.decode_failures",
		metric.WithDescription("Count of instance records that failed to decode. The offending key is logged, not attributed, to bound cardinality."),
		metric.WithUnit("{failure}"))
	if err != nil {
		otel.Handle(err)
	}

	if _, err := meter.Int64ObservableGauge("mulga.instancecache.entries",
		metric.WithDescription("Cached instance count by raw instance state."),
		metric.WithUnit("{instance}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for state, n := range c.entriesByState() {
				o.Observe(n, metric.WithAttributeSet(attribute.NewSet(attribute.String("state", state))))
			}
			return nil
		})); err != nil {
		otel.Handle(err)
	}

	if _, err := meter.Float64ObservableGauge("mulga.instancecache.resync_age_seconds",
		metric.WithDescription("Age of the last successful resync. Absent until the first one completes."),
		metric.WithUnit("s"),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			if age, ok := c.resyncAge(); ok {
				o.Observe(age.Seconds())
			}
			return nil
		})); err != nil {
		otel.Handle(err)
	}

	return m
}

func (m *cacheMetrics) resyncFailed() {
	if m.resyncFailures == nil {
		return
	}
	m.resyncFailures.Add(context.Background(), 1)
}

func (m *cacheMetrics) watchReconnected() {
	if m.watchReconnects == nil {
		return
	}
	m.watchReconnects.Add(context.Background(), 1)
}

func (m *cacheMetrics) decodeFailure() {
	if m.decodeFailures == nil {
		slog.Warn("instancecache: decode failure counter unavailable")
		return
	}
	m.decodeFailures.Add(context.Background(), 1)
}
