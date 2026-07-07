package qmpcollector

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const bridgeMeterName = "github.com/mulgadc/spinifex/spinifex/services/qmpcollector"

// bridgeQueueGroup makes exactly one node's bridge forward each series: NATS
// is clustered, so a plain metrics.> subscription on every node would ship
// duplicates to the sink.
const bridgeQueueGroup = "obs-bridge"

// bridgeEntry is the last observed value of one series identity.
type bridgeEntry struct {
	value   float64
	attrs   attribute.Set
	expires time.Time
}

// bridge taps NATS metrics.> into the process's OTel meter so guest series
// reach the operator sink today. Deletable once Goanna consumes the subjects.
type bridge struct {
	meter metric.Meter

	mu sync.Mutex
	// series holds last values per metric name, keyed by canonical labels.
	series map[string]map[string]bridgeEntry
	gauges map[string]metric.Float64ObservableGauge
}

// startBridge subscribes metrics.> and returns an unsubscribe func.
func startBridge(nc *nats.Conn) (func(), error) {
	b := &bridge{
		meter:  otel.Meter(bridgeMeterName),
		series: make(map[string]map[string]bridgeEntry),
		gauges: make(map[string]metric.Float64ObservableGauge),
	}
	sub, err := nc.QueueSubscribe("metrics.>", bridgeQueueGroup, b.handle)
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}

func (b *bridge) handle(msg *nats.Msg) {
	var batch types.TelemetryBatch
	if err := json.Unmarshal(msg.Data, &batch); err != nil {
		slog.Warn("qmp-collector bridge: bad batch", "subject", msg.Subject, "err", err)
		return
	}
	// Values live 3 periods: a terminated instance ages out instead of
	// reporting its last datapoint forever.
	ttl := 3 * time.Duration(batch.PeriodSeconds) * time.Second
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	expires := time.Now().Add(ttl)

	for _, s := range batch.Series {
		attrs := make([]attribute.KeyValue, 0, len(s.Labels)+1)
		for k, v := range s.Labels {
			attrs = append(attrs, attribute.String(k, v))
		}
		if batch.Node != "" {
			attrs = append(attrs, attribute.String("node", batch.Node))
		}
		b.observe(s.Name, labelKey(s.Name, s.Labels), bridgeEntry{
			value:   s.Value,
			attrs:   attribute.NewSet(attrs...),
			expires: expires,
		})
	}
}

// observe stores the latest value and lazily registers one observable gauge
// per metric name; its callback reports every live series identity.
func (b *bridge) observe(name, key string, entry bridgeEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.series[name] == nil {
		b.series[name] = make(map[string]bridgeEntry)
	}
	b.series[name][key] = entry

	if _, ok := b.gauges[name]; ok {
		return
	}
	gauge, err := b.meter.Float64ObservableGauge(name,
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			now := time.Now()
			for k, e := range b.series[name] {
				if now.After(e.expires) {
					delete(b.series[name], k)
					continue
				}
				o.Observe(e.value, metric.WithAttributeSet(e.attrs))
			}
			return nil
		}))
	if err != nil {
		slog.Warn("qmp-collector bridge: gauge registration failed", "metric", name, "err", err)
		return
	}
	b.gauges[name] = gauge
}
