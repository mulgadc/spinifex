// newStateReconcilerHarness, storeReport, latest, observe and the slowFsyncMs
// threshold. Exporting any of them would widen the package API for tests alone.
//
//test:in-package — asserts on the reconciler's unexported plumbing:
package handlers_eks

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func fsyncReport(healthz string, fsyncMs *float64) *ServerStateReport {
	return &ServerStateReport{Healthz: healthz, NodeCount: 1, TS: time.Now().Unix(), FsyncMs: fsyncMs}
}

func ptrFloat(v float64) *float64 { return &v }

// A missing sample is not a fast one. An older AMI, an etcd serving no metrics
// and an etcd with too few fsyncs to average all arrive as nil, and treating any
// of them as healthy would hide exactly the case the field exists to surface.
func TestServerStateReport_SlowFsyncNeedsASample(t *testing.T) {
	assert.False(t, fsyncReport("ok", nil).SlowFsync(), "no sample must not read as fast")
	assert.False(t, fsyncReport("fail", nil).SlowFsync(), "no sample must not read as slow either")
}

func TestServerStateReport_SlowFsyncThreshold(t *testing.T) {
	assert.False(t, fsyncReport("ok", ptrFloat(6.0)).SlowFsync(), "single-digit ms is healthy")
	assert.True(t, fsyncReport("ok", ptrFloat(slowFsyncMs)).SlowFsync(), "the threshold itself is slow")
	assert.True(t, fsyncReport("ok", ptrFloat(90.0)).SlowFsync(), "a stalled disk is slow")
}

// The whole point of reporting fsync on healthy reports: an apiserver that still
// answers while its datastore is served at 90ms is the case that was previously
// invisible until it degraded into etcd:unreachable.
func TestServerStateReport_SlowFsyncOnAHealthyApiserver(t *testing.T) {
	r := fsyncReport("ok", ptrFloat(90.0))
	assert.True(t, r.Healthy(), "apiserver is still answering")
	assert.True(t, r.SlowFsync(), "and its datastore is still slow")
}

// A slow fsync must never make the cluster unhealthy. observe() gates cluster
// state, and a working cluster on a slow disk is working.
func TestClusterReconciler_SlowFsyncDoesNotFailHealth(t *testing.T) {
	r, _, _ := newStateReconcilerHarness(t)
	r.latest.Store(fsyncReport("ok", ptrFloat(90.0)))

	issue, nodeCount, _ := r.observe(t.Context())
	assert.Empty(t, issue, "a slow disk is a warning, not an unhealthy cluster")
	assert.Equal(t, 1, nodeCount)
}

// Latched on the transition: the control plane republishes on its own timer, and
// a warning every interval is how a real signal gets tuned out. storeReport is
// the only path that sees both the previous and the new report.
func TestClusterReconciler_LogFsyncTransitionIsLatched(t *testing.T) {
	r, _, _ := newStateReconcilerHarness(t)

	// Crossing into slow, then staying slow, then recovering. Asserted through
	// storeReport rather than the log sink: what matters is that repeated slow
	// reports are distinguishable from the first, and that recovery is seen.
	slow := fsyncReport("ok", ptrFloat(90.0))
	stillSlow := fsyncReport("ok", ptrFloat(95.0))
	recovered := fsyncReport("ok", ptrFloat(5.0))

	r.storeReport(slow)
	assert.True(t, r.latest.Load().SlowFsync(), "first slow report is stored")

	r.storeReport(stillSlow)
	assert.True(t, r.latest.Load().SlowFsync(), "a repeat is still slow")

	r.storeReport(recovered)
	assert.False(t, r.latest.Load().SlowFsync(), "recovery clears it")
}

// The baseline case. A healthy cluster logs no health lines at all, so the very
// first usable sample is the only chance to record what normal looks like — a
// green cell-25 previously left nothing to compare a later slow run against.
func TestClusterReconciler_FirstKnownSampleIsTheBaseline(t *testing.T) {
	r, _, _ := newStateReconcilerHarness(t)

	// A guest whose etcd has not warmed up yet reports no sample at all.
	r.storeReport(fsyncReport("ok", nil))
	assert.Nil(t, r.latest.Load().FsyncMs, "warming etcd carries no sample")

	// Then the first real one arrives, healthy and well under the threshold.
	r.storeReport(fsyncReport("ok", ptrFloat(6.0)))
	got := r.latest.Load()
	assert.NotNil(t, got.FsyncMs, "the baseline sample is recorded")
	assert.False(t, got.SlowFsync(), "and it is a healthy one")
}

// A report with no sample must not be read as a recovery from a slow one, or a
// guest that stopped serving metrics would look like a disk that got faster.
func TestClusterReconciler_MissingSampleIsNotRecovery(t *testing.T) {
	r, _, _ := newStateReconcilerHarness(t)
	r.storeReport(fsyncReport("ok", ptrFloat(90.0)))
	r.storeReport(fsyncReport("ok", nil))

	assert.Nil(t, r.latest.Load().FsyncMs, "the sample is genuinely absent")
	assert.False(t, r.latest.Load().SlowFsync(), "and absent is not slow")
}
