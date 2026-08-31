package handlers_rds

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// onePass runs one reconcile and keeps only its error, for the tests that assert
// on what converged rather than on when the loop should next run. The tests that
// do care about the deadline call reconcileOnce directly.
func onePass(t *testing.T, r *Reconciler) error {
	t.Helper()
	_, err := r.reconcileOnce(t.Context())
	return err
}

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
	deps := Deps{LoadCA: newTestCA(t), InstanceState: state, ServingCertKeyBits: testServingCertKeyBits}
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
	t.Parallel()
	h := newReconcileHarness(t)
	rec := healthyRecord()
	rec.FormatAuthorized = true
	seedInstance(t, h.svc, rec)

	require.NoError(t, onePass(t, h.rec))

	status, reason := h.statusOf(t, testDBID)
	assert.Equal(t, StatusAvailable, status)
	assert.Empty(t, reason)
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	stored, _, err := h.svc.getDBInstance(t.Context(), kv, testDBID)
	require.NoError(t, err)
	assert.False(t, stored.FormatAuthorized, "a completed create must not retain a reusable grant")
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
	t.Parallel()
	h := newReconcileHarness(t)
	h.state.state = "pending"
	seedInstance(t, h.svc, healthyRecord())

	require.NoError(t, onePass(t, h.rec))

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
		// Silent for longer than even the slack a persisted beat earns, so no
		// source could have seen this agent recently.
		{"StaleHeartbeat", func(rec *DBInstanceRecord) {
			stale := time.Now().UTC().Add(-2 * (HeartbeatStaleAfter + HeartbeatPersistFloor))
			rec.Agent.LastSeen = &stale
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReconcileHarness(t)
			rec := healthyRecord()
			tc.mutate(&rec)
			seedInstance(t, h.svc, rec)

			require.NoError(t, onePass(t, h.rec))

			status, _ := h.statusOf(t, testDBID)
			assert.Equal(t, StatusCreating, status)
		})
	}
}

// The transitional states judge freshness by the same rule as the health
// classifier. A leader handling none of an instance's beats sees them only
// through KV, which a healthy agent refreshes no more often than the persist
// floor; judging that beat by the raw stale window failed a live database at the
// transition timeout.
func TestReconciler_CompletesACreateOnAPersistedHeartbeatInsideTheFloor(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)
	rec := healthyRecord()
	// Older than the stale window, younger than the window plus the floor: what a
	// perfectly healthy instance looks like to a leader that has seen no beat.
	persisted := time.Now().UTC().Add(-HeartbeatStaleAfter - time.Minute)
	rec.Agent.LastSeen = &persisted
	seedInstance(t, h.svc, rec)

	require.NoError(t, onePass(t, h.rec))

	status, _ := h.statusOf(t, testDBID)
	assert.Equal(t, StatusAvailable, status)
}

// A create that never comes up has to end somewhere: the customer sees a broken
// instance either way, and failed is the state they can act on.
func TestReconciler_MarksFailedWhenTheBootstrapTimesOut(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t, func(d *Deps) { d.BootstrapTimeout = time.Minute })
	rec := healthyRecord()
	rec.Agent = AgentState{
		EngineHealth: EngineHealthStarting,
		Message:      "bootstrap fetch failed: 403 AccessDenied",
	}
	rec.CreatedAt = time.Now().UTC().Add(-10 * time.Minute)
	seedInstance(t, h.svc, rec)

	require.NoError(t, onePass(t, h.rec))

	status, reason := h.statusOf(t, testDBID)
	assert.Equal(t, StatusFailed, status)
	assert.Contains(t, reason, "did not report healthy")
	assert.Contains(t, reason, "bootstrap fetch failed: 403 AccessDenied")
}

// Inside the window a slow bootstrap is still a bootstrap: a false failed is
// worse than a slow one.
func TestReconciler_LeavesASlowBootstrapAloneInsideTheWindow(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t, func(d *Deps) { d.BootstrapTimeout = time.Hour })
	rec := healthyRecord()
	rec.Agent = AgentState{}
	rec.CreatedAt = time.Now().UTC().Add(-10 * time.Minute)
	seedInstance(t, h.svc, rec)

	require.NoError(t, onePass(t, h.rec))

	status, _ := h.statusOf(t, testDBID)
	assert.Equal(t, StatusCreating, status)
}

// A deliberately stopped instance is left alone: touching it would race its
// owner. available and failed are the classifier's, and covered in
// recovery_test.go.
func TestReconciler_LeavesSettledInstancesAlone(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)
	rec := healthyRecord()
	rec.Status = StatusStopped
	rec.CreatedAt = time.Now().UTC().Add(-24 * time.Hour)
	seedInstance(t, h.svc, rec)

	require.NoError(t, onePass(t, h.rec))

	got, _ := h.statusOf(t, testDBID)
	assert.Equal(t, StatusStopped, got)
	assert.Empty(t, h.state.calls, "a settled instance is not even inspected")
}

// A VM-state lookup that fails is not evidence the instance is unhealthy, so the
// pass reports the error rather than silently holding — or worse, failing — it.
func TestReconciler_SurfacesAVMStateLookupFailure(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)
	h.state.err = errors.New("no node answered the describe")
	seedInstance(t, h.svc, healthyRecord())

	err := onePass(t, h.rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no node answered the describe")

	status, _ := h.statusOf(t, testDBID)
	assert.Equal(t, StatusCreating, status)
}

// Without a VM-state resolver the heartbeat alone decides, so a node with EC2
// unwired still makes progress rather than stalling every create.
func TestReconciler_FallsBackToTheHeartbeatWhenVMStateIsUnwired(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t, func(d *Deps) { d.InstanceState = nil })
	seedInstance(t, h.svc, healthyRecord())

	require.NoError(t, onePass(t, h.rec))

	status, _ := h.statusOf(t, testDBID)
	assert.Equal(t, StatusAvailable, status)
}

// The launch surface the retrofit sweep needs: a system VPC to ensure the group
// in, and the ENI surface to move the NIC with.
func withSystemNICPath(enis *fakeENIs) func(*Deps) {
	return func(d *Deps) {
		d.Launch = LaunchDeps{
			Config:    &config.Config{Region: testRegion},
			SystemVPC: (&fakeSystemVPC{}).deps(),
			VPC:       enis,
		}
	}
}

func (h *reconcileHarness) systemSGOf(t *testing.T, id string) string {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec DBInstanceRecord
	found, err := getJSON(t.Context(), kv, DBInstanceKey(id), &rec)
	require.NoError(t, err)
	require.True(t, found)
	return rec.SystemSGID
}

// A DB VM launched before the RDS system group existed sits in the system VPC's
// default group, whose sole ingress rule admits every other member of itself.
func TestReconciler_MovesAnExistingSystemNICOntoTheRDSSystemGroup(t *testing.T) {
	t.Parallel()
	enis := &fakeENIs{}
	h := newReconcileHarness(t, withSystemNICPath(enis))
	rec := healthyRecord()
	rec.SystemENIID = "eni-sys01"
	seedInstance(t, h.svc, rec)

	require.NoError(t, onePass(t, h.rec))

	require.Len(t, enis.modified, 1)
	assert.Equal(t, "eni-sys01", aws.StringValue(enis.modified[0].NetworkInterfaceId))
	assert.Equal(t, []string{testSystemSG}, aws.StringValueSlice(enis.modified[0].Groups))
	assert.Equal(t, testSystemSG, h.systemSGOf(t, testDBID))

	// The remediation yields its pass, so the record it wrote is not transitioned
	// on a revision the status handler read before the move.
	status, _ := h.statusOf(t, testDBID)
	assert.Equal(t, StatusCreating, status)

	require.NoError(t, onePass(t, h.rec))
	assert.Len(t, enis.modified, 1, "once the group is recorded the sweep is a string compare")
	status, _ = h.statusOf(t, testDBID)
	assert.Equal(t, StatusAvailable, status)
}

// The group is regional, so the sweep must not describe and create one per DB
// instance per pass.
func TestReconciler_EnsuresTheSystemGroupOncePerProcess(t *testing.T) {
	t.Parallel()
	enis := &fakeENIs{}
	h := newReconcileHarness(t, withSystemNICPath(enis))
	rec := healthyRecord()
	rec.SystemENIID = "eni-sys01"
	seedInstance(t, h.svc, rec)

	require.NoError(t, onePass(t, h.rec))
	require.NoError(t, onePass(t, h.rec))

	assert.Len(t, enis.sgDescribed, 1)
	assert.Len(t, enis.sgCreated, 1)
}

// Recording the move before vpcd accepted it would leave the ENI in the default
// group with the record claiming otherwise, which no later pass would revisit.
func TestReconciler_RetriesTheSystemNICMoveAfterAFailure(t *testing.T) {
	t.Parallel()
	enis := &fakeENIs{modifyErr: errors.New("vpcd is not answering")}
	h := newReconcileHarness(t, withSystemNICPath(enis))
	rec := healthyRecord()
	rec.SystemENIID = "eni-sys01"
	seedInstance(t, h.svc, rec)

	err := onePass(t, h.rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vpcd is not answering")
	assert.Empty(t, h.systemSGOf(t, testDBID))

	enis.modifyErr = nil
	require.NoError(t, onePass(t, h.rec))
	assert.Len(t, enis.modified, 1)
	assert.Equal(t, testSystemSG, h.systemSGOf(t, testDBID))
}

// One node does the control work; the rest keep serving the API. A second
// claimant must not also believe it is leader, or two nodes would transition
// the same records.
func TestReconciler_ElectsASingleLeader(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)
	other := NewReconciler(h.svc, "node-b")

	assert.True(t, h.rec.lease.TryAcquire(t.Context()))
	assert.False(t, other.lease.TryAcquire(t.Context()))

	// The holder reports the leasership it already holds, and does not hand it away.
	assert.True(t, h.rec.lease.TryAcquire(t.Context()))

	// Releasing on shutdown hands over immediately instead of after the TTL.
	h.rec.lease.Release(t.Context())
	assert.True(t, other.lease.TryAcquire(t.Context()))
	assert.False(t, h.rec.lease.TryAcquire(t.Context()))
}

func TestReconciler_RunHoldsLeadershipIndependentlyOfTheReconcileLoop(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); h.rec.Run(ctx) }()

	require.Eventually(t, h.rec.isLeader, 2*time.Second, 10*time.Millisecond,
		"Run must elect without waiting for a reconcile tick")

	cancel()
	<-done

	other := NewReconciler(h.svc, "node-b")
	assert.True(t, other.lease.TryAcquire(t.Context()),
		"Run returned with the lease key still present")
}

// A node that never won the lease must not delete the holder's key on shutdown.
func TestReconciler_RelinquishOnlyReleasesItsOwnLease(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)
	other := NewReconciler(h.svc, "node-b")

	require.True(t, h.rec.lease.TryAcquire(t.Context()))
	other.lease.Release(t.Context())

	assert.False(t, other.lease.TryAcquire(t.Context()), "the original holder still owns the lease")
}
