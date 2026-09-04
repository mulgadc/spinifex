package otelsetup_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collect installs an in-memory meter provider and returns a function that
// reads what has been recorded on it so far.
func collect(t *testing.T) func() metricdata.ResourceMetrics {
	t.Helper()

	reader := metric.NewManualReader()
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(metric.NewMeterProvider(metric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	return func() metricdata.ResourceMetrics {
		var out metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(t.Context(), &out))
		return out
	}
}

// sumFor returns the total recorded on the named instrument, and whether the
// instrument exists at all.
func sumFor(rm metricdata.ResourceMetrics, name string) (int64, bool) {
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				return 0, false
			}
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			return total, true
		}
	}
	return 0, false
}

// outcomesFor returns the set of outcome attribute values seen on a counter.
// The fence's whole diagnostic value is in that attribute: it separates a
// volume that changed hands from a node that lost contact with NATS.
func outcomesFor(rm metricdata.ResourceMetrics, name string) []string {
	var seen []string
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					if v, found := dp.Attributes.Value("outcome"); found {
						seen = append(seen, v.AsString())
					}
				}
			}
		}
	}
	return seen
}

// TestRecordVolumeFence_SeparatesTheOutcomes pins that the four fence outcomes
// stay distinguishable. They call for different responses — a takeover is
// working as designed, a kill_failed means the protection did not happen — so
// collapsing them into one number would make the metric unactionable.
func TestRecordVolumeFence_SeparatesTheOutcomes(t *testing.T) {
	read := collect(t)

	otelsetup.RecordVolumeFence(t.Context(), otelsetup.FenceOutcomeTaken)
	otelsetup.RecordVolumeFence(t.Context(), otelsetup.FenceOutcomeExpired)
	otelsetup.RecordVolumeFence(t.Context(), otelsetup.FenceOutcomeStalled)
	otelsetup.RecordVolumeFence(t.Context(), otelsetup.FenceOutcomeKillFailed)

	rm := read()
	total, ok := sumFor(rm, "mulga.volume.fence")
	require.True(t, ok, "the fence counter must be registered once a fence is recorded")
	assert.EqualValues(t, 4, total)
	assert.ElementsMatch(t,
		[]string{"taken", "expired", "stalled", "kill_failed"},
		outcomesFor(rm, "mulga.volume.fence"))
}

// TestRecordVolumeTakeover_Counts covers the one quantity that says how often
// the platform is choosing availability over the newer copy of a volume.
func TestRecordVolumeTakeover_Counts(t *testing.T) {
	read := collect(t)

	otelsetup.RecordVolumeTakeover(t.Context())
	otelsetup.RecordVolumeTakeover(t.Context())

	total, ok := sumFor(read(), "mulga.volume.takeover")
	require.True(t, ok, "the takeover counter must be registered once a takeover is recorded")
	assert.EqualValues(t, 2, total)
}

// TestRecordResourceLeak_CountsByKind covers the leak counter's attribute,
// which is what makes a rising number point at a resource class rather than
// just going up.
func TestRecordResourceLeak_CountsByKind(t *testing.T) {
	read := collect(t)

	otelsetup.RecordResourceLeak(t.Context(), "public_ip")

	total, ok := sumFor(read(), "mulga.resource.leaked")
	require.True(t, ok)
	assert.EqualValues(t, 1, total)
}
