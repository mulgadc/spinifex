package handlers_eks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCPControl is a stub CPInstanceControl recording calls and returning
// configurable state/errors, so the restart orchestration can be tested without
// a real VM manager.
type fakeCPControl struct {
	state       string
	stateErr    error
	startErr    error
	stopErr     error
	stateCalls  int
	startCalls  int
	stopCalls   int
	lastStarted string
	lastStopped string
}

func (f *fakeCPControl) InstanceState(_ context.Context, _ string) (string, error) {
	f.stateCalls++
	return f.state, f.stateErr
}

func (f *fakeCPControl) StartInstance(_ context.Context, id string) error {
	f.startCalls++
	f.lastStarted = id
	return f.startErr
}

func (f *fakeCPControl) StopInstance(_ context.Context, id string) error {
	f.stopCalls++
	f.lastStopped = id
	return f.stopErr
}

func metaWithCP(id string) *ClusterMeta {
	return &ClusterMeta{Name: "alpha", ControlPlaneInstanceID: id}
}

// metaWithCPNodes builds an HA cluster meta whose ControlPlaneNodes lists every
// member; the scalar ControlPlaneInstanceID mirrors the primary ([0]).
func metaWithCPNodes(ids ...string) *ClusterMeta {
	nodes := make([]ControlPlaneNode, 0, len(ids))
	for _, id := range ids {
		nodes = append(nodes, ControlPlaneNode{InstanceID: id})
	}
	return &ClusterMeta{Name: "alpha", ControlPlaneInstanceID: ids[0], ControlPlaneNodes: nodes}
}

// newRecoveryReconciler returns a reconciler wired with the given CP control and
// default restart policy (2m grace/backoff, 3 attempts).
func newRecoveryReconciler(t *testing.T, cp CPInstanceControl) *ClusterReconciler {
	t.Helper()
	r, _, _ := newStateReconcilerHarness(t, WithCPInstanceControl(cp))
	return r
}

func TestMaybeRecoverCP_HealthyClearsDegradedClock(t *testing.T) {
	cp := &fakeCPControl{state: "stopped"}
	r := newRecoveryReconciler(t, cp)
	r.degradedSince = time.Now().Add(-time.Hour)
	r.restartAttempts = 2

	r.maybeRecoverControlPlane(context.Background(), metaWithCP("i-cp"), "")

	assert.True(t, r.degradedSince.IsZero(), "healthy CP clears the degraded window")
	assert.Zero(t, r.restartAttempts, "healthy CP resets the attempt count")
	assert.Zero(t, cp.startCalls, "healthy CP is never restarted")
}

func TestMaybeRecoverCP_WithinGraceDoesNotRestart(t *testing.T) {
	cp := &fakeCPControl{state: "stopped"}
	r := newRecoveryReconciler(t, cp)

	r.maybeRecoverControlPlane(context.Background(), metaWithCP("i-cp"), "stale")

	assert.False(t, r.degradedSince.IsZero(), "first unhealthy tick starts the degraded clock")
	assert.Zero(t, cp.startCalls, "no restart before the grace window elapses")
}

func TestMaybeRecoverCP_RestartsStoppedCPAfterGrace(t *testing.T) {
	cp := &fakeCPControl{state: "stopped"}
	r := newRecoveryReconciler(t, cp)
	r.degradedSince = time.Now().Add(-10 * time.Minute)

	r.maybeRecoverControlPlane(context.Background(), metaWithCP("i-cp"), "stale")

	assert.Equal(t, 1, cp.startCalls, "stopped CP past grace is restarted")
	assert.Equal(t, "i-cp", cp.lastStarted)
	assert.Equal(t, 1, r.restartAttempts)
	assert.False(t, r.lastRestartAt.IsZero())
}

func TestMaybeRecoverCP_RestartsAllHAMembersAfterGrace(t *testing.T) {
	cp := &fakeCPControl{state: "stopped"}
	r := newRecoveryReconciler(t, cp)
	r.degradedSince = time.Now().Add(-10 * time.Minute)

	r.maybeRecoverControlPlane(context.Background(), metaWithCPNodes("i-cp0", "i-cp1", "i-cp2"), "stale")

	assert.Equal(t, 3, cp.startCalls, "every stopped HA member is restarted, not just the primary")
	assert.Equal(t, 1, r.restartAttempts, "one recovery pass counts as a single attempt")
	assert.False(t, r.lastRestartAt.IsZero())
}

func TestMaybeRecoverCP_RunningCPNotRestarted(t *testing.T) {
	cp := &fakeCPControl{state: "running"}
	r := newRecoveryReconciler(t, cp)
	r.degradedSince = time.Now().Add(-10 * time.Minute)

	r.maybeRecoverControlPlane(context.Background(), metaWithCP("i-cp"), "stale")

	assert.Equal(t, 1, cp.stateCalls)
	assert.Zero(t, cp.startCalls, "a running-but-unhealthy CP is left for Phase-2 restore")
}

func TestMaybeRecoverCP_BackoffPreventsRapidRetry(t *testing.T) {
	cp := &fakeCPControl{state: "stopped"}
	r := newRecoveryReconciler(t, cp)
	r.degradedSince = time.Now().Add(-10 * time.Minute)

	r.maybeRecoverControlPlane(context.Background(), metaWithCP("i-cp"), "stale")
	r.maybeRecoverControlPlane(context.Background(), metaWithCP("i-cp"), "stale")

	assert.Equal(t, 1, cp.startCalls, "second attempt within backoff is suppressed")
}

func TestMaybeRecoverCP_StopsAtMaxAttempts(t *testing.T) {
	cp := &fakeCPControl{state: "stopped"}
	r := newRecoveryReconciler(t, cp)
	r.degradedSince = time.Now().Add(-10 * time.Minute)
	r.restartAttempts = r.maxRestartAttempts

	r.maybeRecoverControlPlane(context.Background(), metaWithCP("i-cp"), "stale")

	assert.Zero(t, cp.startCalls, "no restart once the attempt cap is reached")
}

func TestMaybeRecoverCP_NilControlIsNoop(t *testing.T) {
	r, _, _ := newStateReconcilerHarness(t) // no CP control wired
	r.degradedSince = time.Now().Add(-10 * time.Minute)

	require.NotPanics(t, func() {
		r.maybeRecoverControlPlane(context.Background(), metaWithCP("i-cp"), "stale")
	})
}

func TestMaybeRecoverCP_EmptyInstanceIDIsNoop(t *testing.T) {
	cp := &fakeCPControl{state: "stopped"}
	r := newRecoveryReconciler(t, cp)
	r.degradedSince = time.Now().Add(-10 * time.Minute)

	r.maybeRecoverControlPlane(context.Background(), &ClusterMeta{Name: "alpha"}, "stale")

	assert.Zero(t, cp.stateCalls, "no CP recorded on meta → nothing to restart")
	assert.Zero(t, cp.startCalls)
}

func TestMaybeRecoverCP_StateQueryErrorSkipsRestart(t *testing.T) {
	cp := &fakeCPControl{stateErr: errors.New("query-pci failed")}
	r := newRecoveryReconciler(t, cp)
	r.degradedSince = time.Now().Add(-10 * time.Minute)

	r.maybeRecoverControlPlane(context.Background(), metaWithCP("i-cp"), "stale")

	assert.Zero(t, cp.startCalls, "unreadable instance state defers the restart to next tick")
}

// TestActiveBranchRestartsWedgedCP exercises the full ACTIVE reconcile path:
// a stale state report records the health issue AND drives an in-place restart
// once the CP has been degraded past the grace window.
func TestActiveBranchRestartsWedgedCP(t *testing.T) {
	cp := &fakeCPControl{state: "stopped"}
	r, _, acctKV := newStateReconcilerHarness(t, WithCPInstanceControl(cp))
	require.NoError(t, SetClusterStatus(t.Context(), acctKV, "alpha", ClusterStatusActive))
	require.NoError(t, casUpdateMeta(t.Context(), acctKV, "alpha", func(m *ClusterMeta) bool {
		m.ControlPlaneInstanceID = "i-cp"
		return true
	}))
	r.degradedSince = time.Now().Add(-10 * time.Minute)
	r.latest.Store(&ServerStateReport{Healthz: "ok", NodeCount: 2, TS: time.Now().Add(-5 * time.Minute).Unix()})

	require.NoError(t, r.reconcileOnce(context.Background()))

	meta, err := GetClusterMeta(t.Context(), acctKV, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusActive, meta.Status, "status stays AWS-faithful ACTIVE, not a new DEGRADED enum")
	assert.Contains(t, meta.HealthIssue, "stale", "degradation is reflected in the health field")
	assert.Equal(t, 1, cp.startCalls, "wedged CP is restarted in place")
	assert.Equal(t, "i-cp", cp.lastStarted)
}
