package handlers_rds

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInstanceState answers the VM half of the bootstrap check.
type fakeInstanceState struct {
	state string
	// A stopped VM is still reported stopping for this many more readings, which
	// is the window a real stop spends draining and detaching its volumes. The
	// node stamps stopping as it accepts the command, long before the detach.
	detachReads int
	err         error
	calls       []string
}

var _ InstanceStateResolver = (*fakeInstanceState)(nil)

func (f *fakeInstanceState) InstanceState(_ context.Context, instanceID, _ string) (string, error) {
	f.calls = append(f.calls, instanceID)
	if f.err != nil {
		return "", f.err
	}
	if f.detachReads > 0 {
		f.detachReads--
		return "stopping", nil
	}
	return f.state, nil
}

// The VM the node has taken down. The fleet only reports it once the detach
// window has been read through.
func (f *fakeInstanceState) stop() { f.state = instanceStateStopped }

type reconcileHarness struct {
	svc   *Service
	rec   *Reconciler
	state *fakeInstanceState
	nc    *nats.Conn
}

func newReconcileHarness(t *testing.T, opts ...func(*Deps)) *reconcileHarness {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	state := &fakeInstanceState{state: instanceStateRunning}
	deps := Deps{LoadCA: newTestCA(t), InstanceState: state}
	for _, opt := range opts {
		opt(&deps)
	}
	svc := NewService(nc, testRegion).WithDeps(deps)
	return &reconcileHarness{svc: svc, rec: NewReconciler(svc, "node-a"), state: state, nc: nc}
}

// healthyRecord is a creating instance whose agent has just reported a healthy
// engine from the record's current VM — the shape the reconciler flips.
func healthyRecord() DBInstanceRecord {
	now := time.Now().UTC()
	rec := defaultRecord()
	rec.CreatedAt = now.Add(-2 * time.Minute)
	rec.Agent = AgentState{
		InstanceID:   testInstance,
		EngineHealth: EngineHealthHealthy,
		LastSeen:     &now,
	}
	return rec
}

func (h *reconcileHarness) statusOf(t *testing.T, id string) (Status, string) {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec DBInstanceRecord
	found, err := getJSON(t.Context(), kv, DBInstanceKey(id), &rec)
	require.NoError(t, err)
	require.True(t, found)
	return rec.Status, rec.FailureReason
}

func TestReconciler_MarksAvailableOnHealthyHeartbeatFromARunningVM(t *testing.T) {
	h := newReconcileHarness(t)
	seedInstance(t, h.svc, healthyRecord())

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	status, reason := h.statusOf(t, testDBID)
	assert.Equal(t, StatusAvailable, status)
	assert.Empty(t, reason)
	assert.Equal(t, []string{testInstance}, h.state.calls)

	events := describeEvents(t, h.svc, &rds.DescribeEventsInput{
		SourceType:       aws.String(EventSourceTypeDBInstance),
		SourceIdentifier: aws.String(testDBID),
	})
	require.Len(t, events, 1)
	assert.Equal(t, "DB instance is available.", aws.StringValue(events[0].Message))
	assert.Equal(t, []string{EventCategoryAvailability}, aws.StringValueSlice(events[0].EventCategories))
}

// Both halves must hold. A healthy beat from a VM that is not running means the
// engine reported before the platform finished bringing the VM up, or the beat
// is stale — either way the instance is not ready.
func TestReconciler_HoldsCreatingWhenTheVMIsNotRunning(t *testing.T) {
	h := newReconcileHarness(t)
	h.state.state = "pending"
	seedInstance(t, h.svc, healthyRecord())

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	status, _ := h.statusOf(t, testDBID)
	assert.Equal(t, StatusCreating, status)
}

func TestReconciler_HoldsCreatingUntilTheEngineIsHealthy(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*DBInstanceRecord)
	}{
		{"NoHeartbeatYet", func(rec *DBInstanceRecord) { rec.Agent = AgentState{} }},
		{"EngineStillStarting", func(rec *DBInstanceRecord) { rec.Agent.EngineHealth = EngineHealthStarting }},
		{"EngineUnhealthy", func(rec *DBInstanceRecord) { rec.Agent.EngineHealth = EngineHealthUnhealthy }},
		// A beat from a VM other than the record's current one is a superseded VM
		// still running after a replace; it must not report the instance ready.
		{"BeatFromASupersededVM", func(rec *DBInstanceRecord) { rec.Agent.InstanceID = "i-oldvm" }},
		{"StaleHeartbeat", func(rec *DBInstanceRecord) {
			stale := time.Now().UTC().Add(-2 * HeartbeatStaleAfter)
			rec.Agent.LastSeen = &stale
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReconcileHarness(t)
			rec := healthyRecord()
			tc.mutate(&rec)
			seedInstance(t, h.svc, rec)

			require.NoError(t, h.rec.reconcileOnce(t.Context()))

			status, _ := h.statusOf(t, testDBID)
			assert.Equal(t, StatusCreating, status)
		})
	}
}

// A create that never comes up has to end somewhere: the customer sees a broken
// instance either way, and failed is the state they can act on.
func TestReconciler_MarksFailedWhenTheBootstrapTimesOut(t *testing.T) {
	h := newReconcileHarness(t, func(d *Deps) { d.BootstrapTimeout = time.Minute })
	rec := healthyRecord()
	rec.Agent = AgentState{
		EngineHealth: EngineHealthStarting,
		Message:      "bootstrap fetch failed: 403 AccessDenied",
	}
	rec.CreatedAt = time.Now().UTC().Add(-10 * time.Minute)
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	status, reason := h.statusOf(t, testDBID)
	assert.Equal(t, StatusFailed, status)
	assert.Contains(t, reason, "did not report healthy")
	assert.Contains(t, reason, "bootstrap fetch failed: 403 AccessDenied")
}

// Inside the window a slow bootstrap is still a bootstrap: a false failed is
// worse than a slow one.
func TestReconciler_LeavesASlowBootstrapAloneInsideTheWindow(t *testing.T) {
	h := newReconcileHarness(t, func(d *Deps) { d.BootstrapTimeout = time.Hour })
	rec := healthyRecord()
	rec.Agent = AgentState{}
	rec.CreatedAt = time.Now().UTC().Add(-10 * time.Minute)
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	status, _ := h.statusOf(t, testDBID)
	assert.Equal(t, StatusCreating, status)
}

// A deliberately stopped instance is left alone: touching it would race its
// owner. available and failed are the classifier's, and covered in
// recovery_test.go.
func TestReconciler_LeavesSettledInstancesAlone(t *testing.T) {
	h := newReconcileHarness(t)
	rec := healthyRecord()
	rec.Status = StatusStopped
	rec.CreatedAt = time.Now().UTC().Add(-24 * time.Hour)
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	got, _ := h.statusOf(t, testDBID)
	assert.Equal(t, StatusStopped, got)
	assert.Empty(t, h.state.calls, "a settled instance is not even inspected")
}

// A VM-state lookup that fails is not evidence the instance is unhealthy, so the
// pass reports the error rather than silently holding — or worse, failing — it.
func TestReconciler_SurfacesAVMStateLookupFailure(t *testing.T) {
	h := newReconcileHarness(t)
	h.state.err = errors.New("no node answered the describe")
	seedInstance(t, h.svc, healthyRecord())

	err := h.rec.reconcileOnce(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no node answered the describe")

	status, _ := h.statusOf(t, testDBID)
	assert.Equal(t, StatusCreating, status)
}

// Without a VM-state resolver the heartbeat alone decides, so a node with EC2
// unwired still makes progress rather than stalling every create.
func TestReconciler_FallsBackToTheHeartbeatWhenVMStateIsUnwired(t *testing.T) {
	h := newReconcileHarness(t, func(d *Deps) { d.InstanceState = nil })
	seedInstance(t, h.svc, healthyRecord())

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	status, _ := h.statusOf(t, testDBID)
	assert.Equal(t, StatusAvailable, status)
}

// One node does the control work; the rest keep serving the API. A second
// claimant must not also believe it is leader, or two nodes would transition
// the same records.
func TestReconciler_ElectsASingleLeader(t *testing.T) {
	h := newReconcileHarness(t)
	other := NewReconciler(h.svc, "node-b")

	assert.True(t, h.rec.acquireOrRefresh(t.Context()))
	assert.False(t, other.acquireOrRefresh(t.Context()))

	// The holder refreshes its own lease rather than losing it to itself.
	assert.True(t, h.rec.acquireOrRefresh(t.Context()))

	// Releasing on shutdown hands over immediately instead of after the TTL.
	h.rec.relinquish()
	assert.True(t, other.acquireOrRefresh(t.Context()))
	assert.False(t, h.rec.acquireOrRefresh(t.Context()))
}

// A node that never won the lease must not delete the holder's key on shutdown.
func TestReconciler_RelinquishOnlyReleasesItsOwnLease(t *testing.T) {
	h := newReconcileHarness(t)
	other := NewReconciler(h.svc, "node-b")

	require.True(t, h.rec.acquireOrRefresh(t.Context()))
	other.relinquish()

	assert.False(t, other.acquireOrRefresh(t.Context()), "the original holder still owns the lease")
}
