package daemon

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// stateWriteMeterName is the instrumentation scope for persistState timings.
const stateWriteMeterName = "github.com/mulgadc/spinifex/spinifex/daemon/statewrite"

// Duration is reported as a sum plus an op count rather than a histogram:
// ES|QL cannot aggregate a native histogram field, so a panel computes
// avg = SUM(duration.sum) / SUM(ops).
type stateWriteInstruments struct {
	ops      metric.Int64Counter
	duration metric.Float64Counter
	vms      metric.Int64Counter
}

var (
	stateWriteOnce sync.Once
	stateWriteInst stateWriteInstruments
)

func stateWriteMetrics() stateWriteInstruments {
	stateWriteOnce.Do(func() {
		meter := otel.Meter(stateWriteMeterName)

		var err error
		stateWriteInst.ops, err = meter.Int64Counter("mulga.daemon.state_write.ops",
			metric.WithDescription("Count of local-file and KV state writes, by store."),
			metric.WithUnit("{write}"))
		if err != nil {
			otel.Handle(err)
		}
		stateWriteInst.duration, err = meter.Float64Counter("mulga.daemon.state_write.duration.sum",
			metric.WithDescription("Summed wall time of state writes, by store. Divide by ops for the mean."),
			metric.WithUnit("s"))
		if err != nil {
			otel.Handle(err)
		}
		stateWriteInst.vms, err = meter.Int64Counter("mulga.daemon.state_write.records.sum",
			metric.WithDescription("Summed records each write actually handled: the whole set for store=local, only the changed ones for store=kv. Divide by ops for the mean."),
			metric.WithUnit("{record}"))
		if err != nil {
			otel.Handle(err)
		}
	})
	return stateWriteInst
}

// recordStateWrite reports one write of store ("local" or "kv") over records,
// which is the whole set locally but only the changed ones in KV. Every
// recorder tolerates a nil instrument.
func recordStateWrite(store string, records int, d time.Duration) {
	inst := stateWriteMetrics()
	set := metric.WithAttributeSet(attribute.NewSet(attribute.String("store", store)))
	ctx := context.Background()

	if inst.ops != nil {
		inst.ops.Add(ctx, 1, set)
	}
	if inst.duration != nil {
		inst.duration.Add(ctx, d.Seconds(), set)
	}
	if inst.vms != nil {
		inst.vms.Add(ctx, int64(records), set)
	}
}
