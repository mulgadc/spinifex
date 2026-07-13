package handlers_eks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStateReconcilerHarness builds a reconciler wired to a real NATS conn as its
// state source (so observe() reads r.latest) and returns the conn + account KV.
// It does NOT start a subscription — tests drive r.latest directly for
// determinism; the integration test below exercises the real Subscribe path.
func newStateReconcilerHarness(t *testing.T, opts ...ReconcilerOption) (*ClusterReconciler, *nats.Conn, nats.KeyValue) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	leaderKV, err := InitLeaderBucket(js, 1)
	require.NoError(t, err)
	acctKV, err := GetOrCreateAccountBucket(js, testAccountID, 1)
	require.NoError(t, err)
	require.NoError(t, PutClusterMeta(acctKV, sampleClusterMeta("alpha")))

	base := []ReconcilerOption{WithStateSource(nc, StateSubject(testAccountID, "alpha"))}
	r, err := NewClusterReconciler(leaderKV, acctKV, testAccountID, "alpha", "holder-1", "", append(base, opts...)...)
	require.NoError(t, err)
	return r, nc, acctKV
}

func freshReport(healthz string, nodes int) *ServerStateReport {
	return &ServerStateReport{Healthz: healthz, NodeCount: nodes, TS: time.Now().Unix()}
}

func TestClusterReconciler_CreatingTransitionsToActiveOnStateReport(t *testing.T) {
	r, _, acctKV := newStateReconcilerHarness(t)
	freshenClusterCreatedAt(t, acctKV)
	seedBootstrapState(t, acctKV)
	r.latest.Store(freshReport("ok", 3))

	require.NoError(t, r.reconcileOnce(context.Background()))

	meta, err := GetClusterMeta(acctKV, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusActive, meta.Status)
	assert.Equal(t, 3, meta.NodeCount)
	assert.Empty(t, meta.HealthIssue)
}

func TestClusterReconciler_CreatingStaysWithoutStateReport(t *testing.T) {
	r, _, acctKV := newStateReconcilerHarness(t)
	freshenClusterCreatedAt(t, acctKV)
	seedBootstrapState(t, acctKV)
	// No report stored.

	require.NoError(t, r.reconcileOnce(context.Background()))

	meta, err := GetClusterMeta(acctKV, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusCreating, meta.Status, "no state report yet → stays CREATING")
}

func TestClusterReconciler_CreatingStaysOnUnhealthyReport(t *testing.T) {
	r, _, acctKV := newStateReconcilerHarness(t)
	freshenClusterCreatedAt(t, acctKV)
	seedBootstrapState(t, acctKV)
	r.latest.Store(freshReport("fail", 0))

	require.NoError(t, r.reconcileOnce(context.Background()))

	meta, err := GetClusterMeta(acctKV, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusCreating, meta.Status, "unhealthy apiserver → stays CREATING")
}

func TestClusterReconciler_ActiveRecordsNodeCountAndClearsIssue(t *testing.T) {
	r, _, acctKV := newStateReconcilerHarness(t)
	require.NoError(t, SetClusterStatus(acctKV, "alpha", ClusterStatusActive))
	require.NoError(t, SetClusterHealthState(acctKV, "alpha", "stale", 0, nil))
	r.latest.Store(freshReport("ok", 4))

	require.NoError(t, r.reconcileOnce(context.Background()))

	meta, err := GetClusterMeta(acctKV, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusActive, meta.Status)
	assert.Equal(t, 4, meta.NodeCount)
	assert.Empty(t, meta.HealthIssue, "healthy report clears the prior issue")
}

func TestClusterReconciler_ActiveFlagsStaleReport(t *testing.T) {
	r, _, acctKV := newStateReconcilerHarness(t, WithStateStaleAfter(90*time.Second))
	require.NoError(t, SetClusterStatus(acctKV, "alpha", ClusterStatusActive))
	r.latest.Store(&ServerStateReport{Healthz: "ok", NodeCount: 2, TS: time.Now().Add(-5 * time.Minute).Unix()})

	require.NoError(t, r.reconcileOnce(context.Background()))

	meta, err := GetClusterMeta(acctKV, "alpha")
	require.NoError(t, err)
	assert.Contains(t, meta.HealthIssue, "stale", "report older than stale window flags an issue")
	assert.Equal(t, 2, meta.NodeCount, "last known node count still recorded")
}

func TestClusterReconciler_ActiveUnhealthyReportSurfacesReason(t *testing.T) {
	r, _, acctKV := newStateReconcilerHarness(t)
	require.NoError(t, SetClusterStatus(acctKV, "alpha", ClusterStatusActive))
	r.latest.Store(&ServerStateReport{
		Healthz: "fail", NodeCount: 0, TS: time.Now().Unix(),
		Reason: "readyz:[etcd]; etcd:unreachable; disk:ok",
	})

	require.NoError(t, r.reconcileOnce(context.Background()))

	meta, err := GetClusterMeta(acctKV, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusActive, meta.Status)
	assert.Contains(t, meta.HealthIssue, `apiserver healthz="fail"`)
	assert.Contains(t, meta.HealthIssue, "etcd:unreachable", "in-guest diagnosis surfaced in the health issue")
}

func TestClusterReconciler_ActiveUnhealthyReportWithoutReasonStaysTerse(t *testing.T) {
	r, _, acctKV := newStateReconcilerHarness(t)
	require.NoError(t, SetClusterStatus(acctKV, "alpha", ClusterStatusActive))
	r.latest.Store(freshReport("fail", 0))

	require.NoError(t, r.reconcileOnce(context.Background()))

	meta, err := GetClusterMeta(acctKV, "alpha")
	require.NoError(t, err)
	assert.Equal(t, `apiserver healthz="fail"`, meta.HealthIssue, "no reason → terse issue, back-compatible with older AMI")
}

func TestClusterReconciler_StateReportSubscriptionDrivesActive(t *testing.T) {
	r, nc, acctKV := newStateReconcilerHarness(t,
		WithReconcileInterval(10*time.Millisecond),
		WithLeaseRefresh(10*time.Second),
	)
	freshenClusterCreatedAt(t, acctKV)
	seedBootstrapState(t, acctKV)

	release, ok := r.AcquireLease()
	require.True(t, ok)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- r.Run(ctx) }()

	// Publish healthy reports until the cluster goes ACTIVE (core NATS only
	// delivers post-subscribe, so keep emitting while Run's subscription opens).
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		payload, _ := json.Marshal(ServerStateReport{Healthz: "ok", NodeCount: 3, TS: time.Now().Unix()})
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				_ = nc.Publish(StateSubject(testAccountID, "alpha"), payload)
			}
		}
	}()

	require.Eventually(t, func() bool {
		meta, err := GetClusterMeta(acctKV, "alpha")
		return err == nil && meta.Status == ClusterStatusActive && meta.NodeCount == 3
	}, 1500*time.Millisecond, 10*time.Millisecond, "state reports should drive CREATING → ACTIVE")

	cancel()
	select {
	case err := <-runErr:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
