package handlers_rds

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The whole (VM state, heartbeat age, RDS status) matrix against the pure rule,
// so the interlocks are pinned without a cluster or a clock.
func TestClassifyHealth_TheMatrix(t *testing.T) {
	cases := []struct {
		name string
		obs  healthObservation
		want healthVerdict
	}{
		{"ServingEngine", healthObservation{StatusAvailable, true, true, true, time.Now()}, verdictHealthy},
		{"DarkVMAndSilentAgent", healthObservation{StatusAvailable, false, false, false, time.Time{}}, verdictFailed},
		// A stale beat under a live VM is an agent or network fault, not a dead
		// database; calling it failed would report an outage that is not happening.
		{"SilentAgentUnderALiveVM", healthObservation{StatusAvailable, false, false, true, time.Now()}, verdictDegraded},
		{"UnhealthyEngineUnderALiveVM", healthObservation{StatusAvailable, false, true, true, time.Now()}, verdictDegraded},
		// Impossible in practice, so the VM lookup is what is treated as stale.
		{"FreshBeatFromAStoppedVM", healthObservation{StatusAvailable, true, true, false, time.Now()}, verdictDegraded},
		// The heartbeat's own way back out of failed, which is the rung the AMI's
		// Restart=on-failure and EC2's VM auto-restart provide underneath.
		{"RecoveredWhileFailed", healthObservation{StatusFailed, true, true, true, time.Now()}, verdictHealthy},
		{"StillDarkWhileFailed", healthObservation{StatusFailed, false, false, false, time.Time{}}, verdictFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyHealth(tc.obs))
		})
	}
}

// Every state some other owner is driving is skipped outright, whatever the VM
// and the agent are doing. This is the interlock that stops the classifier
// racing a lifecycle op over the same VM, and stopped is a legitimate state
// rather than a failure.
func TestClassifyHealth_SkipsEveryStateItDoesNotOwn(t *testing.T) {
	for _, status := range []Status{
		StatusCreating, StatusModifying, StatusBackingUp, StatusRebooting,
		StatusStopping, StatusStarting, StatusStopped, StatusRecovering,
		StatusDeleting, StatusDeleted,
	} {
		t.Run(string(status), func(t *testing.T) {
			dark := healthObservation{status: status}
			assert.Equal(t, verdictSkip, classifyHealth(dark))

			serving := healthObservation{status: status, engineHealthy: true, heartbeatFresh: true, vmRunning: true}
			assert.Equal(t, verdictSkip, classifyHealth(serving))
		})
	}
}

// servingRecord is a settled instance whose agent has just reported a healthy
// engine — the shape the classifier leaves alone.
func servingRecord() DBInstanceRecord {
	rec := healthyRecord()
	rec.Status = StatusAvailable
	return rec
}

// darkRecord is an instance whose agent stopped reporting long enough ago that
// even the persisted beat's slacker bound is exhausted.
func darkRecord() DBInstanceRecord {
	rec := servingRecord()
	silent := time.Now().UTC().Add(-2 * (HeartbeatStaleAfter + HeartbeatPersistFloor))
	rec.Agent.LastSeen = &silent
	return rec
}

func (h *reconcileHarness) recordOf(t *testing.T, id string) DBInstanceRecord {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec DBInstanceRecord
	found, err := getJSON(t.Context(), kv, DBInstanceKey(id), &rec)
	require.NoError(t, err)
	require.True(t, found)
	return rec
}

// An instance that has been dark for a week must not keep reporting available:
// the failure mode is silent for exactly as long as nobody tries to connect.
func TestReconciler_MarksADarkInstanceFailed(t *testing.T) {
	h := newReconcileHarness(t, func(d *Deps) { d.FailureGrace = time.Nanosecond })
	h.state.state = "stopped"
	seedInstance(t, h.svc, darkRecord())

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	rec := h.recordOf(t, testDBID)
	assert.Equal(t, StatusFailed, rec.Status)
	assert.Contains(t, rec.FailureReason, "the DB instance is not running")
	assert.Contains(t, rec.FailureReason, "its agent has not reported for")
	require.NotNil(t, rec.UnhealthySince, "the first-failure timestamp is what a later leader measures against")
}

// The reason has to reach the customer, not just the record: an instance a
// customer's monitoring can alert on is the whole point of detecting failure
// when nothing repairs it.
func TestReconciler_RecordsAnEventForADarkInstance(t *testing.T) {
	h := newReconcileHarness(t, func(d *Deps) { d.FailureGrace = time.Nanosecond })
	h.state.state = "stopped"
	seedInstance(t, h.svc, darkRecord())

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	out, err := h.svc.DescribeEvents(t.Context(), &rds.DescribeEventsInput{
		SourceType:       aws.String(EventSourceTypeDBInstance),
		SourceIdentifier: aws.String(testDBID),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Events, 1)
	assert.Contains(t, aws.StringValue(out.Events[0].Message), "DB instance failed")
	assert.Contains(t, aws.StringValueSlice(out.Events[0].EventCategories), EventCategoryFailure)
}

// A steady instance is answered from the record alone. The VM describe is a
// fleet-wide fan-out, so issuing one per instance per pass would put the whole
// fleet's liveness on the bus every 15 seconds.
func TestReconciler_LeavesAHealthyInstanceUntouchedWithoutAVMLookup(t *testing.T) {
	h := newReconcileHarness(t)
	seedInstance(t, h.svc, servingRecord())

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	rec := h.recordOf(t, testDBID)
	assert.Equal(t, StatusAvailable, rec.Status)
	assert.Nil(t, rec.UnhealthySince)
	assert.Empty(t, h.state.calls, "a healthy instance costs no fleet describe")
}

// Both halves must hold, never either alone.
func TestReconciler_HoldsAvailableOnHalfTheEvidence(t *testing.T) {
	cases := []struct {
		name   string
		vm     string
		mutate func(*DBInstanceRecord)
	}{
		// The agent or the network is at fault; the database is still serving.
		{"SilentAgentUnderARunningVM", instanceStateRunning, func(*DBInstanceRecord) {}},
		// The VM lookup is what is stale: an agent cannot beat from a dead VM.
		{"FreshBeatFromAVMReportedStopped", "stopped", func(rec *DBInstanceRecord) {
			now := time.Now().UTC()
			rec.Agent.LastSeen = &now
		}},
		// A superseded VM's beat proves nothing about the current one, but on its
		// own it is still not evidence the instance is down.
		{"BeatFromASupersededVMUnderARunningVM", instanceStateRunning, func(rec *DBInstanceRecord) {
			rec.Agent.InstanceID = "i-oldvm"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReconcileHarness(t, func(d *Deps) { d.FailureGrace = time.Nanosecond })
			h.state.state = tc.vm
			rec := darkRecord()
			tc.mutate(&rec)
			seedInstance(t, h.svc, rec)

			require.NoError(t, h.rec.reconcileOnce(t.Context()))

			stored := h.recordOf(t, testDBID)
			assert.Equal(t, StatusAvailable, stored.Status)
			assert.Nil(t, stored.UnhealthySince, "half the evidence neither starts nor advances the clock")
		})
	}
}

// The first dark pass only starts the clock. Two passes have to agree before a
// live database can be reported down, which is what a transient fleet describe
// during a node restart cannot get past.
func TestReconciler_StartsTheClockBeforeFailingWithinTheGrace(t *testing.T) {
	h := newReconcileHarness(t, func(d *Deps) { d.FailureGrace = time.Hour })
	h.state.state = "stopped"
	seedInstance(t, h.svc, darkRecord())

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	rec := h.recordOf(t, testDBID)
	assert.Equal(t, StatusAvailable, rec.Status)
	require.NotNil(t, rec.UnhealthySince)
	assert.Empty(t, rec.FailureReason)
}

// The clock a previous leader stamped is what the grace is measured against, so
// a leader change does not restart the window and defer detection indefinitely.
func TestReconciler_FailsOnAClockStampedByAnEarlierLeader(t *testing.T) {
	h := newReconcileHarness(t, func(d *Deps) { d.FailureGrace = time.Minute })
	h.state.state = "stopped"
	rec := darkRecord()
	stamped := time.Now().UTC().Add(-10 * time.Minute)
	rec.UnhealthySince = &stamped
	seedInstance(t, h.svc, rec)

	// A different node holding the lease, with no beats of its own in memory.
	other := NewReconciler(h.svc, "node-b")
	require.NoError(t, other.reconcileOnce(t.Context()))

	stored := h.recordOf(t, testDBID)
	assert.Equal(t, StatusFailed, stored.Status)
	assert.NotEmpty(t, stored.FailureReason)
	assert.Equal(t, stamped, stored.UnhealthySince.UTC(), "the first-failure timestamp survives the leader change")
}

// Only a healthy heartbeat resets the clock. A VM that boots and immediately
// wedges must not reset it every pass and mask a persistent fault — which
// matters more without a recovery ladder than with one, since nothing else is
// watching.
func TestReconciler_TheClockResetsOnlyOnAHealthyHeartbeat(t *testing.T) {
	h := newReconcileHarness(t, func(d *Deps) { d.FailureGrace = time.Hour })
	rec := darkRecord()
	stamped := time.Now().UTC().Add(-30 * time.Minute)
	rec.UnhealthySince = &stamped
	seedInstance(t, h.svc, rec)

	// The VM is back but the agent still is not: not a failure, and not a reason
	// to forget that the instance has been dark for half an hour.
	h.state.state = instanceStateRunning
	require.NoError(t, h.rec.reconcileOnce(t.Context()))
	stored := h.recordOf(t, testDBID)
	require.NotNil(t, stored.UnhealthySince)
	assert.Equal(t, stamped, stored.UnhealthySince.UTC())

	// The engine reports healthy from the record's current VM, and only then.
	seedInstance(t, h.svc, func() DBInstanceRecord {
		recovered := stored
		now := time.Now().UTC()
		recovered.Agent.LastSeen = &now
		return recovered
	}())
	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	stored = h.recordOf(t, testDBID)
	assert.Equal(t, StatusAvailable, stored.Status)
	assert.Nil(t, stored.UnhealthySince)
}

// failed is terminal for the control plane, but not for the instance: the FSM
// allows failed → available, so an instance the AMI or EC2 brought back reports
// itself recovered without an operator touching it.
func TestReconciler_ClearsFailedOnARecoveredHeartbeat(t *testing.T) {
	h := newReconcileHarness(t)
	rec := servingRecord()
	rec.Status = StatusFailed
	rec.FailureReason = "the DB instance is not running and its agent has not reported for 5m0s"
	stamped := time.Now().UTC().Add(-time.Hour)
	rec.UnhealthySince = &stamped
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	stored := h.recordOf(t, testDBID)
	assert.Equal(t, StatusAvailable, stored.Status)
	assert.Empty(t, stored.FailureReason, "a stale reason must not outlive the failure it describes")
	assert.Nil(t, stored.UnhealthySince)

	out, err := h.svc.DescribeEvents(t.Context(), &rds.DescribeEventsInput{
		SourceType:       aws.String(EventSourceTypeDBInstance),
		SourceIdentifier: aws.String(testDBID),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Events, 1)
	assert.Contains(t, aws.StringValue(out.Events[0].Message), "DB instance recovered")
	assert.Contains(t, aws.StringValueSlice(out.Events[0].EventCategories), EventCategoryRecovery)
}

// Nothing retries a failed instance in v1.0, so a still-dark one is left where
// it is rather than having its reason and clock rewritten every pass.
func TestReconciler_LeavesAStillDarkFailedInstanceAlone(t *testing.T) {
	h := newReconcileHarness(t, func(d *Deps) { d.FailureGrace = time.Nanosecond })
	h.state.state = "stopped"
	rec := darkRecord()
	rec.Status = StatusFailed
	rec.FailureReason = "the original reason"
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	stored := h.recordOf(t, testDBID)
	assert.Equal(t, StatusFailed, stored.Status)
	assert.Equal(t, "the original reason", stored.FailureReason)
	assert.Nil(t, stored.UnhealthySince)
}

// Beats are queue-group delivered, so the leader mostly sees them only through
// KV — and a healthy agent refreshes the record no more often than the persist
// floor. Judging a persisted beat by the raw stale window would report a steady
// fleet as dark for most of every persist cycle.
func TestReconciler_AllowsThePersistFloorOnAPersistedHeartbeat(t *testing.T) {
	h := newReconcileHarness(t, func(d *Deps) { d.FailureGrace = time.Nanosecond })
	h.state.state = "stopped"
	rec := servingRecord()
	// Older than the stale window, younger than the window plus the floor: what a
	// perfectly healthy instance looks like to a leader handling none of its beats.
	persisted := time.Now().UTC().Add(-HeartbeatStaleAfter - time.Minute)
	rec.Agent.LastSeen = &persisted
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	stored := h.recordOf(t, testDBID)
	assert.Equal(t, StatusAvailable, stored.Status)
	assert.Nil(t, stored.UnhealthySince)
}

// The persist floor is slack for beats this node cannot see. A beat it handled
// itself earns no slack, and a leader that took the last beat before the VM went
// dark must call it failed on the stale window rather than on window plus floor.
func TestReconciler_UsesTheTightBoundOnABeatThisNodeHandled(t *testing.T) {
	h := newReconcileHarness(t, func(d *Deps) { d.FailureGrace = time.Nanosecond })
	h.state.state = instanceStateStopped
	rec := servingRecord()
	// The shape a persisting beat leaves behind: one instant, written to the
	// record and held in memory, and nothing since.
	beat := time.Now().UTC().Add(-HeartbeatStaleAfter - time.Second)
	rec.Agent.LastSeen = &beat
	seedInstance(t, h.svc, rec)
	h.svc.liveness[testAccountID+"/"+testDBID] = &agentLiveness{lastSeen: beat, health: EngineHealthHealthy}

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	stored := h.recordOf(t, testDBID)
	assert.Equal(t, StatusFailed, stored.Status)
	assert.Contains(t, stored.FailureReason, "its agent has not reported for")
}

// Without a VM-state resolver the failure half of the evidence cannot be
// gathered at all, so nothing is ever reported failed. Detection goes missing
// rather than being declared on the heartbeat alone.
func TestReconciler_NeverFailsAnInstanceWhenVMStateIsUnwired(t *testing.T) {
	h := newReconcileHarness(t, func(d *Deps) {
		d.InstanceState = nil
		d.FailureGrace = time.Nanosecond
	})
	seedInstance(t, h.svc, darkRecord())

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	stored := h.recordOf(t, testDBID)
	assert.Equal(t, StatusAvailable, stored.Status)
	assert.Nil(t, stored.UnhealthySince)
}

// A VM lookup that fails is not evidence the instance is down, so the pass
// reports the error rather than starting a failure clock on a guess.
func TestReconciler_SurfacesAVMLookupFailureWithoutFailingTheInstance(t *testing.T) {
	h := newReconcileHarness(t, func(d *Deps) { d.FailureGrace = time.Nanosecond })
	h.state.err = errors.New("no node answered the describe")
	seedInstance(t, h.svc, darkRecord())

	err := h.rec.reconcileOnce(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no node answered the describe")

	stored := h.recordOf(t, testDBID)
	assert.Equal(t, StatusAvailable, stored.Status)
	assert.Nil(t, stored.UnhealthySince)
}

// A failed instance has to be able to explain itself to the customer, not just
// to the log.
func TestProjectDBInstance_ReportsTheFailureReason(t *testing.T) {
	svc := NewService(nil, testRegion)
	rec := defaultRecord()
	rec.Status = StatusFailed
	rec.FailureReason = "the DB instance is not running and its agent has not reported for 5m0s"

	out := svc.projectDBInstance(&rec)

	require.Len(t, out.StatusInfos, 1)
	assert.Equal(t, "instance", aws.StringValue(out.StatusInfos[0].StatusType))
	assert.Equal(t, string(StatusFailed), aws.StringValue(out.StatusInfos[0].Status))
	assert.False(t, aws.BoolValue(out.StatusInfos[0].Normal))
	assert.Equal(t, rec.FailureReason, aws.StringValue(out.StatusInfos[0].Message))

	rec.Status = StatusAvailable
	rec.FailureReason = ""
	assert.Empty(t, svc.projectDBInstance(&rec).StatusInfos, "a healthy instance carries no status info, as AWS leaves it")
}
