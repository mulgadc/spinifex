package handlers_eks

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newResetReconciler wires a reconciler with the given CP control plus the etcd
// quorum-reformation escalation enabled at its defaults (5m grace/backoff, 2
// attempts). The acctKV is real so recovery directives round-trip through KV.
func newResetReconciler(t *testing.T, cp CPInstanceControl) (*ClusterReconciler, jetstream.KeyValue) {
	t.Helper()
	r, _, acctKV := newStateReconcilerHarness(t, WithCPInstanceControl(cp), WithEtcdResetRecovery(0, 0, 0))
	return r, acctKV
}

// etcdIssue is a representative health string carrying the guest's etcd-down
// diagnosis token, the signal maybeReformEtcdQuorum gates on.
const etcdIssue = `apiserver healthz="fail": etcd:unreachable`

func TestReformEtcd_DisabledIsNoop(t *testing.T) {
	cp := &fakeCPControl{state: "running"}
	// CP control wired but reset NOT enabled.
	r := newRecoveryReconciler(t, cp)
	r.resetSince = time.Now().Add(-time.Hour)

	r.maybeReformEtcdQuorum(context.Background(), metaWithCPNodes("i-0", "i-1", "i-2"), etcdIssue)

	assert.Zero(t, cp.stopCalls, "escalation is inert unless explicitly enabled")
}

func TestReformEtcd_NonEtcdIssueIgnored(t *testing.T) {
	cp := &fakeCPControl{state: "running"}
	r, _ := newResetReconciler(t, cp)
	r.resetSince = time.Now().Add(-time.Hour)

	r.maybeReformEtcdQuorum(context.Background(), metaWithCPNodes("i-0", "i-1", "i-2"), "stale state report")

	assert.Zero(t, cp.stopCalls, "a non-etcd health issue is owned by the restart/replace paths")
}

func TestReformEtcd_SingleCPIgnored(t *testing.T) {
	cp := &fakeCPControl{state: "running"}
	r, _ := newResetReconciler(t, cp)
	r.resetSince = time.Now().Add(-time.Hour)

	r.maybeReformEtcdQuorum(context.Background(), metaWithCP("i-solo"), etcdIssue)

	assert.Zero(t, cp.stopCalls, "a single-CP cluster has no quorum to reform")
}

func TestReformEtcd_WithinGraceDoesNotReset(t *testing.T) {
	cp := &fakeCPControl{state: "running"}
	r, _ := newResetReconciler(t, cp)

	r.maybeReformEtcdQuorum(context.Background(), metaWithCPNodes("i-0", "i-1", "i-2"), etcdIssue)

	assert.False(t, r.resetSince.IsZero(), "first wedged tick starts the reset clock")
	assert.Zero(t, cp.stopCalls, "no reset before the grace window elapses")
}

func TestReformEtcd_MemberNotRunningDefers(t *testing.T) {
	// A stopped member is the in-place restart path's job; reset only fires when
	// every member is VM-running but etcd is still wedged.
	cp := &fakeCPControl{state: "stopped"}
	r, _ := newResetReconciler(t, cp)
	r.resetSince = time.Now().Add(-time.Hour)

	r.maybeReformEtcdQuorum(context.Background(), metaWithCPNodes("i-0", "i-1", "i-2"), etcdIssue)

	assert.Zero(t, cp.stopCalls, "a stopped member defers reset to the restart path")
}

func TestReformEtcd_EscalatesAfterGrace(t *testing.T) {
	cp := &fakeCPControl{state: "running"}
	r, acctKV := newResetReconciler(t, cp)
	r.resetSince = time.Now().Add(-10 * time.Minute)

	r.maybeReformEtcdQuorum(context.Background(), metaWithCPNodes("i-seed", "i-1", "i-2"), etcdIssue)

	assert.Equal(t, 3, cp.stopCalls, "every member is stopped so the restart path boots it clean and applies the directive")
	assert.Zero(t, cp.startCalls, "reset stops running members; the restart path owns bringing them back up")
	assert.Equal(t, 1, r.resetAttempts)
	assert.True(t, r.resetIssued)
	assert.False(t, r.lastResetAt.IsZero())

	seed, err := LoadRecoveryDirective(t.Context(), acctKV, "alpha", "i-seed")
	require.NoError(t, err)
	assert.Equal(t, RecoveryActionClusterReset, seed.Action, "primary reseeds the etcd quorum")
	assert.Equal(t, int64(1), seed.Epoch)

	follower, err := LoadRecoveryDirective(t.Context(), acctKV, "alpha", "i-1")
	require.NoError(t, err)
	assert.Equal(t, RecoveryActionWipeRejoin, follower.Action, "followers wipe and rejoin")
	assert.Equal(t, int64(1), follower.Epoch)
}

func TestReformEtcd_BackoffPreventsRapidReset(t *testing.T) {
	cp := &fakeCPControl{state: "running"}
	r, _ := newResetReconciler(t, cp)
	r.resetSince = time.Now().Add(-10 * time.Minute)

	r.maybeReformEtcdQuorum(context.Background(), metaWithCPNodes("i-0", "i-1", "i-2"), etcdIssue)
	r.maybeReformEtcdQuorum(context.Background(), metaWithCPNodes("i-0", "i-1", "i-2"), etcdIssue)

	assert.Equal(t, 3, cp.stopCalls, "second escalation within backoff is suppressed")
}

func TestReformEtcd_StopsAtMaxAttempts(t *testing.T) {
	cp := &fakeCPControl{state: "running"}
	r, _ := newResetReconciler(t, cp)
	r.resetSince = time.Now().Add(-10 * time.Minute)
	r.resetAttempts = r.maxResetAttempts

	r.maybeReformEtcdQuorum(context.Background(), metaWithCPNodes("i-0", "i-1", "i-2"), etcdIssue)

	assert.Zero(t, cp.stopCalls, "no reset once the attempt cap is reached")
}

func TestReformEtcd_RecoveryClearsDirectivesAndClock(t *testing.T) {
	cp := &fakeCPControl{state: "running"}
	r, acctKV := newResetReconciler(t, cp)
	r.resetSince = time.Now().Add(-10 * time.Minute)
	meta := metaWithCPNodes("i-seed", "i-1", "i-2")

	// Escalate, then observe recovery (issue == "").
	r.maybeReformEtcdQuorum(context.Background(), meta, etcdIssue)
	require.True(t, r.resetIssued)
	r.maybeReformEtcdQuorum(context.Background(), meta, "")

	assert.True(t, r.resetSince.IsZero(), "recovery clears the reset clock")
	assert.Zero(t, r.resetAttempts, "recovery resets the attempt count")
	assert.False(t, r.resetIssued, "one-shot clear consumed")

	seed, err := LoadRecoveryDirective(t.Context(), acctKV, "alpha", "i-seed")
	require.NoError(t, err)
	assert.Equal(t, RecoveryActionNone, seed.Action, "stale cluster-reset is cleared on recovery")
	assert.Equal(t, int64(2), seed.Epoch, "clear advances the epoch past the applied directive")
}

func TestReformEtcd_SteadyHealthyDoesNotBumpEpoch(t *testing.T) {
	// A healthy cluster that never escalated must not write directives each tick.
	cp := &fakeCPControl{state: "running"}
	r, acctKV := newResetReconciler(t, cp)
	meta := metaWithCPNodes("i-0", "i-1", "i-2")

	r.maybeReformEtcdQuorum(context.Background(), meta, "")
	r.maybeReformEtcdQuorum(context.Background(), meta, "")

	d, err := LoadRecoveryDirective(t.Context(), acctKV, "alpha", "i-0")
	require.NoError(t, err)
	assert.Equal(t, int64(0), d.Epoch, "no directive is written for a never-wedged cluster")
	assert.Equal(t, RecoveryActionNone, d.Action)
}
