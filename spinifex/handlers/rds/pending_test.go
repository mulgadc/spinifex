package handlers_rds

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A record mid-modify: in modifying with the change recorded, which is the
// state both the immediate path and a resumed one drain from.
func modifyingRecord(pending *PendingModifiedValues) DBInstanceRecord {
	rec := modifiableRecord()
	rec.Status = StatusModifying
	started := time.Now().UTC().Add(-time.Minute)
	rec.TransitionStartedAt = &started
	rec.PendingModifiedValues = pending
	return rec
}

// A grow with no class change alongside it keeps the VM: it goes down, the
// volume grows, and the same VM comes back up on the same datadir.
func TestApplyPendingModifications_GrowsStorageOnTheSameVM(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifyingRecord(&PendingModifiedValues{AllocatedStorage: aws.Int64(50), RequestedAt: time.Now().UTC()})
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.svc.applyPendingModifications(t.Context(), h.kv(t), testAccountID, &rec))

	assert.Equal(t, []string{"stop:" + testInstance, "start:" + testInstance}, h.cmdr.calls)
	assert.Empty(t, h.launch.launcher.terminated, "a grow does not replace the VM")
	assert.Equal(t, int64(50), h.storage.sizes[testDataVolume])

	stored := h.record(t)
	assert.Equal(t, int64(50), stored.AllocatedStorage)
	assert.Equal(t, testInstance, stored.InstanceID)
	// The volume is at its new size but the guest's filesystem is not yet on it,
	// so exactly one step is left outstanding.
	require.NotNil(t, stored.PendingModifiedValues)
	assert.True(t, stored.PendingModifiedValues.FilesystemGrowPending)
	assert.Nil(t, stored.PendingModifiedValues.AllocatedStorage)
}

// A class change is a VM replace, and a grow asked for alongside it rides
// the same outage rather than opening a second one.
func TestApplyPendingModifications_ClassChangeReplacesTheVMAndCarriesTheGrow(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifyingRecord(&PendingModifiedValues{
		DBInstanceClass:  "db.m5.large",
		AllocatedStorage: aws.Int64(80),
		RequestedAt:      time.Now().UTC(),
	})
	seedReplaceable(t, h, rec)

	require.NoError(t, h.svc.applyPendingModifications(t.Context(), h.kv(t), testAccountID, &rec))

	require.NotNil(t, h.launch.launcher.input)
	assert.Equal(t, "m5.large", h.launch.launcher.input.InstanceType)
	assert.Equal(t, []string{testInstance}, h.launch.launcher.terminated)
	assert.Empty(t, h.cmdr.calls, "a replace terminates the VM rather than power-cycling it")
	assert.Equal(t, int64(80), h.storage.sizes[testDataVolume])

	stored := h.record(t)
	assert.Equal(t, "db.m5.large", stored.DBInstanceClass)
	assert.Equal(t, int64(80), stored.AllocatedStorage)
	assert.Equal(t, testReplacementInstance, stored.InstanceID)
	require.NotNil(t, stored.PendingModifiedValues)
	assert.True(t, stored.PendingModifiedValues.FilesystemGrowPending)
}

// The parameters go in while the engine this modify started against is
// still the one running, so the restart that follows is what adopts the
// statically-scoped ones.
func TestApplyPendingModifications_AppliesTheParametersBeforeTheOutage(t *testing.T) {
	h := newModifyHarness(t)
	h.agent.replyWith("shared_buffers")
	rec := modifyingRecord(&PendingModifiedValues{
		DBParameterGroupName: testDefaultGroup,
		AllocatedStorage:     aws.Int64(50),
		RequestedAt:          time.Now().UTC(),
	})
	rec.DBParameterGroupName = ""
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.svc.applyPendingModifications(t.Context(), h.kv(t), testAccountID, &rec))

	issued := h.agent.received()
	require.Len(t, issued, 2)
	assert.Equal(t, CommandApplyParams, issued[0].Type, "the parameters go in before the engine is stopped")
	assert.Equal(t, CommandStopEngine, issued[1].Type)

	stored := h.record(t)
	assert.Equal(t, testDefaultGroup, stored.DBParameterGroupName)
	// The grow's stop/start is that restart, so the record does not go on
	// advertising settings the engine came back on.
	assert.Empty(t, stored.PendingRebootParameters)
	assert.Contains(t, h.eventMessages(t), "Applied the parameters that were pending a reboot.")
}

// A group change on its own restarts nothing, so its statically-scoped settings
// stay pending until the customer reboots — the one path that still reports
// pending-reboot.
func TestApplyPendingModifications_KeepsThePendingRebootParametersWithoutAnOutage(t *testing.T) {
	h := newModifyHarness(t)
	h.agent.replyWith("shared_buffers")
	rec := modifyingRecord(&PendingModifiedValues{
		DBParameterGroupName: testDefaultGroup,
		RequestedAt:          time.Now().UTC(),
	})
	rec.DBParameterGroupName = ""
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.svc.applyPendingModifications(t.Context(), h.kv(t), testAccountID, &rec))

	assert.Empty(t, h.cmdr.calls, "a group change alone takes the engine down for nothing")
	assert.Equal(t, []string{"shared_buffers"}, h.record(t).PendingRebootParameters)
}

// A class change re-resolves the size-derived defaults and the replacement
// VM boots on them, so the record must not report a change that has landed —
// the customer would reboot a healthy database to clear it, and Terraform would
// see it in every configuration read.
func TestApplyPendingModifications_ClassChangeClearsThePendingRebootParameters(t *testing.T) {
	h := newModifyHarness(t)
	h.agent.replyWith("shared_buffers")
	rec := modifyingRecord(&PendingModifiedValues{
		DBInstanceClass: "db.m5.large",
		RequestedAt:     time.Now().UTC(),
	})
	seedReplaceable(t, h, rec)

	require.NoError(t, h.svc.applyPendingModifications(t.Context(), h.kv(t), testAccountID, &rec))

	stored := h.record(t)
	assert.Equal(t, "db.m5.large", stored.DBInstanceClass)
	assert.Empty(t, stored.PendingRebootParameters)
	assert.Empty(t, rec.PendingRebootParameters, "the caller's copy reports what the store holds")
	group := projectParameterGroup(&stored)
	require.Len(t, group, 1)
	assert.Equal(t, "in-sync", *group[0].ParameterApplyStatus)
}

// A parameter apply the agent rejects stops the modify before the outage: the
// customer asked for one change, and delivering half of it silently is the
// failure mode this closes.
func TestApplyPendingModifications_KeepsThePendingValuesWhenTheChangeFails(t *testing.T) {
	h := newModifyHarness(t)
	h.storage.modifyErr = errors.New("the volume store is unavailable")
	pending := &PendingModifiedValues{
		DBParameterGroupName: testDefaultGroup,
		AllocatedStorage:     aws.Int64(50),
		RequestedAt:          time.Now().UTC(),
	}
	rec := modifyingRecord(pending)
	seedInstance(t, h.svc, rec)

	require.Error(t, h.svc.applyPendingModifications(t.Context(), h.kv(t), testAccountID, &rec))

	// The whole request is still recorded, including the half that landed: a
	// resumed drain re-applies the parameters, which is idempotent, rather than
	// losing the grow it never reached.
	stored := h.record(t)
	require.NotNil(t, stored.PendingModifiedValues)
	assert.Equal(t, int64(50), aws.Int64Value(stored.PendingModifiedValues.AllocatedStorage))
	assert.Equal(t, testDefaultGroup, stored.PendingModifiedValues.DBParameterGroupName)
	assert.Equal(t, int64(20), stored.AllocatedStorage, "the record reports the size the volume actually has")
}

// Nothing outstanding is not an error: a record whose values have already
// landed is drained again by any resumed pass.
func TestApplyPendingModifications_IsANoOpWithNothingOutstanding(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifyingRecord(nil)
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.svc.applyPendingModifications(t.Context(), h.kv(t), testAccountID, &rec))
	assert.Empty(t, h.cmdr.calls)
	assert.Empty(t, h.storage.modified)
}

// The last step of a grow: the control plane has already grown the volume, and
// this is what turns it into capacity the database can use.
func TestFinishFilesystemGrow_ExtendsTheGuestAndClearsThePending(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifyingRecord(&PendingModifiedValues{FilesystemGrowPending: true, RequestedAt: time.Now().UTC()})
	rec.AllocatedStorage = 50
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.svc.finishFilesystemGrow(t.Context(), h.kv(t), testAccountID, &rec))

	issued := h.agent.received()
	require.Len(t, issued, 1)
	assert.Equal(t, CommandGrowFilesystem, issued[0].Type)
	assert.Nil(t, h.record(t).PendingModifiedValues)
	assert.Nil(t, rec.PendingModifiedValues)

	messages := h.eventMessages(t)
	require.NotEmpty(t, messages)
	assert.Contains(t, messages[0], "storage grown")
}

// A guest that cannot extend its filesystem keeps the step outstanding: the
// volume is bigger and the database cannot use it, which is exactly the state
// the reconciler has to keep retrying.
func TestFinishFilesystemGrow_KeepsTheStepWhenTheGuestFails(t *testing.T) {
	h := newModifyHarnessWithAgent(t, true)
	rec := modifyingRecord(&PendingModifiedValues{FilesystemGrowPending: true, RequestedAt: time.Now().UTC()})
	seedInstance(t, h.svc, rec)

	require.Error(t, h.svc.finishFilesystemGrow(t.Context(), h.kv(t), testAccountID, &rec))

	stored := h.record(t)
	require.NotNil(t, stored.PendingModifiedValues)
	assert.True(t, stored.PendingModifiedValues.FilesystemGrowPending)
}

// A leader that died mid-modify leaves the change recorded and nothing else, so
// the next pass re-runs it rather than waiting on a VM that was never touched.
func TestReconciler_ResumesAnInterruptedModify(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifyingRecord(&PendingModifiedValues{AllocatedStorage: aws.Int64(50), RequestedAt: time.Now().UTC()})
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	assert.Equal(t, int64(50), h.storage.sizes[testDataVolume])
	stored := h.record(t)
	assert.Equal(t, StatusModifying, stored.Status, "the engine still has to come back before it is available")
	assert.Equal(t, int64(50), stored.AllocatedStorage)
	require.NotNil(t, stored.PendingModifiedValues)
	assert.True(t, stored.PendingModifiedValues.FilesystemGrowPending)
}

// The in-guest grow can only run once the agent is back, so the reconciler is
// what issues it — a customer's modify finishes without them watching.
func TestReconciler_FinishesTheFilesystemGrowOnceTheAgentIsBack(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifyingRecord(&PendingModifiedValues{FilesystemGrowPending: true, RequestedAt: time.Now().UTC()})
	beat := time.Now().UTC()
	rec.Agent = AgentState{InstanceID: testInstance, EngineHealth: EngineHealthHealthy, LastSeen: &beat}
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	issued := h.agent.received()
	require.Len(t, issued, 1)
	assert.Equal(t, CommandGrowFilesystem, issued[0].Type)
	assert.Nil(t, h.record(t).PendingModifiedValues)

	// The record moved under the revision that pass read, so the transition to
	// available is the next pass's rather than a raced write.
	require.NoError(t, h.rec.reconcileOnce(t.Context()))
	assert.Equal(t, StatusAvailable, h.record(t).Status)
}

// A modify whose VM never comes back cannot sit in modifying forever: the
// instance is failed with the reason, which is a state a retry is legal from.
func TestReconciler_FailsAModifyThatOverrunsItsBudget(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifyingRecord(nil)
	started := time.Now().UTC().Add(-2 * transitionTimeout)
	rec.TransitionStartedAt = &started
	rec.Agent = AgentState{}
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	stored := h.record(t)
	assert.Equal(t, StatusFailed, stored.Status)
	assert.Contains(t, stored.FailureReason, "did not report healthy")
}

// A modify that cannot be applied at all is failed rather than retried
// forever, and the request stays recorded so the retry has something to run.
func TestReconciler_FailsAModifyItCannotApplyWithinTheBudget(t *testing.T) {
	h := newModifyHarness(t)
	h.storage.modifyErr = errors.New("the volume store is unavailable")
	rec := modifyingRecord(&PendingModifiedValues{AllocatedStorage: aws.Int64(50), RequestedAt: time.Now().UTC()})
	started := time.Now().UTC().Add(-2 * transitionTimeout)
	rec.TransitionStartedAt = &started
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	stored := h.record(t)
	assert.Equal(t, StatusFailed, stored.Status)
	assert.Contains(t, stored.FailureReason, "could not be modified")
	require.NotNil(t, stored.PendingModifiedValues)
	assert.Equal(t, int64(50), aws.Int64Value(stored.PendingModifiedValues.AllocatedStorage))
}

// Inside the budget a failed attempt is retried on the next pass rather than
// failing the instance on the first transient error.
func TestReconciler_RetriesAFailedModifyInsideTheBudget(t *testing.T) {
	h := newModifyHarness(t)
	h.storage.modifyErr = errors.New("the volume store is briefly unavailable")
	rec := modifyingRecord(&PendingModifiedValues{AllocatedStorage: aws.Int64(50), RequestedAt: time.Now().UTC()})
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.rec.reconcileOnce(t.Context()))
	assert.Equal(t, StatusModifying, h.record(t).Status)

	h.storage.modifyErr = nil
	require.NoError(t, h.rec.reconcileOnce(t.Context()))
	assert.Equal(t, int64(50), h.record(t).AllocatedStorage)
}

// The size-derived defaults are the reason a class change has to re-resolve
// rather than carry the old set forward — a shared_buffers computed for the old
// class is wrong at the new one in whichever direction the class moved.
func TestApplyPendingModifications_ReResolvesTheParametersForTheNewClass(t *testing.T) {
	h := newModifyHarness(t)
	h.agent.replyWith("shared_buffers")
	rec := modifyingRecord(&PendingModifiedValues{
		DBInstanceClass: "db.m5.xlarge",
		RequestedAt:     time.Now().UTC(),
	})
	seedReplaceable(t, h, rec)

	require.NoError(t, h.svc.applyPendingModifications(t.Context(), h.kv(t), testAccountID, &rec))

	issued := h.agent.received()
	require.NotEmpty(t, issued)
	assert.Equal(t, CommandApplyParams, issued[0].Type)

	applied := map[string]string{}
	for _, param := range issued[0].Parameters {
		applied[param.Name] = param.Value
	}
	memoryMiB, err := classMemoryMiB("db.m5.xlarge")
	require.NoError(t, err)
	assert.Equal(t, sharedBuffersFor(memoryMiB), applied["shared_buffers"],
		"the set was resolved against the class the instance is becoming")

	stored := h.record(t)
	assert.Equal(t, applied["shared_buffers"], parameterValue(stored.Bootstrap.ResolvedParameters, "shared_buffers"),
		"the stored set has to match what the engine was given, or the next replace boots on something else")
}

// A re-resolve rather than a merge: a parameter the old group set and the new
// one does not reverts to its catalog default instead of lingering.
func TestApplyPendingModifications_ParameterGroupChangeRevertsTheOldGroupsValues(t *testing.T) {
	h := newModifyHarness(t)
	h.agent.replyWith("")

	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput("lean"), testAccountID)
	require.NoError(t, err)

	rec := modifyingRecord(&PendingModifiedValues{
		DBParameterGroupName: "lean",
		RequestedAt:          time.Now().UTC(),
	})
	// What the old group had set, carried on the record as the engine's current
	// values; the new group sets nothing.
	rec.Bootstrap.ResolvedParameters = []Parameter{{Name: "work_mem", Value: "262144"}}
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.svc.applyPendingModifications(t.Context(), h.kv(t), testAccountID, &rec))

	issued := h.agent.received()
	require.NotEmpty(t, issued)
	applied := map[string]string{}
	for _, param := range issued[0].Parameters {
		applied[param.Name] = param.Value
	}
	workMem, _ := LookupParameter("work_mem")
	assert.Equal(t, workMem.Default, applied["work_mem"],
		"the old group's value should have reverted to the catalog default")
}

func parameterValue(params []Parameter, name string) string {
	for _, param := range params {
		if param.Name == name {
			return param.Value
		}
	}
	return ""
}
