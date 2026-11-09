package instance

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (h *lifecycleHarness) recordExists(t *testing.T, id string) bool {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec DBInstanceRecord
	found, err := getJSON(t.Context(), kv, DBInstanceKey(id), &rec)
	require.NoError(t, err)
	return found
}

func (h *lifecycleHarness) snapshotRecord(t *testing.T, id string) (DBSnapshotRecord, bool) {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec DBSnapshotRecord
	found, err := getJSON(t.Context(), kv, DBSnapshotKey(id), &rec)
	require.NoError(t, err)
	return rec, found
}

func (h *lifecycleHarness) retainedVolume(t *testing.T, volumeID string) (RetainedVolumeRecord, bool) {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec RetainedVolumeRecord
	found, err := getJSON(t.Context(), kv, RetainedVolumeKey(volumeID), &rec)
	require.NoError(t, err)
	return rec, found
}

func skipFinalSnapshot() *rds.DeleteDBInstanceInput {
	return &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: aws.String(testDBID),
		SkipFinalSnapshot:    aws.Bool(true),
	}
}

// The whole teardown: the VM, the data volume, both NICs, the reverse index and
// the record itself.
func TestDeleteDBInstance_TearsDownEverythingItOwns(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	seedInstance(t, h.svc, availableRecord())
	require.NoError(t, h.svc.PutInstanceIndex(t.Context(), testInstance, InstanceIndexEntry{
		AccountID:            testAccountID,
		DBInstanceIdentifier: testDBID,
	}))

	out, err := h.svc.DeleteDBInstance(t.Context(), skipFinalSnapshot(), testAccountID)
	require.NoError(t, err)

	// Answered with the last state it held, as AWS does.
	assert.Equal(t, string(StatusDeleting), aws.StringValue(out.DBInstance.DBInstanceStatus))

	assert.Equal(t, []string{testInstance}, h.launcher.terminated)
	assert.Equal(t, []string{"vol-rdsdata01"}, h.volumes.deleted)
	assert.ElementsMatch(t, []string{"eni-cust01", "eni-sys01"}, h.enis.deleted)
	assert.False(t, h.recordExists(t, testDBID))

	entry, err := h.svc.LookupInstanceIndex(t.Context(), testInstance)
	require.NoError(t, err)
	assert.Nil(t, entry, "the reverse index must not outlive the instance")
}

// AWS requires the caller to choose explicitly, because the request that omits
// both would silently destroy the only copy of the data.
func TestDeleteDBInstance_RequiresAnExplicitSnapshotChoice(t *testing.T) {
	cases := []struct {
		name  string
		input *rds.DeleteDBInstanceInput
	}{
		{"NeitherSupplied", &rds.DeleteDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}},
		{"BothSupplied", &rds.DeleteDBInstanceInput{
			DBInstanceIdentifier:      aws.String(testDBID),
			SkipFinalSnapshot:         aws.Bool(true),
			FinalDBSnapshotIdentifier: aws.String("orders-db-final"),
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLifecycleHarness(t, false)
			seedInstance(t, h.svc, availableRecord())

			_, err := h.svc.DeleteDBInstance(t.Context(), tc.input, testAccountID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterCombination)
			assert.True(t, h.recordExists(t, testDBID), "a rejected delete must leave the instance alone")
			assert.Empty(t, h.launcher.terminated)
		})
	}
}

// The flag exists to stop exactly this call, so honouring it has to happen
// before anything is torn down.
func TestDeleteDBInstance_HonoursDeletionProtection(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	rec := availableRecord()
	rec.DeletionProtection = true
	seedInstance(t, h.svc, rec)

	_, err := h.svc.DeleteDBInstance(t.Context(), skipFinalSnapshot(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deletion protection")

	assert.Equal(t, StatusAvailable, h.record(t).Status)
	assert.Empty(t, h.launcher.terminated)
	assert.Empty(t, h.volumes.deleted)
}

// The snapshot is taken once the VM is gone, so it reads a sealed data
// volume rather than one a live engine is still writing to.
func TestDeleteDBInstance_TakesTheFinalSnapshotAfterTheEngineAndVMAreDown(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.DeleteDBInstance(t.Context(), &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier:      aws.String(testDBID),
		FinalDBSnapshotIdentifier: aws.String("orders-db-final"),
	}, testAccountID)
	require.NoError(t, err)

	issued := h.agent.received()
	require.Len(t, issued, 1)
	assert.Equal(t, CommandStopEngine, issued[0].Type)
	require.Len(t, h.snaps.created, 1)
	assert.Equal(t, "vol-rdsdata01", aws.StringValue(h.snaps.created[0].VolumeId))
	assert.Equal(t, testAccountID,
		tagOf(h.snaps.created[0].TagSpecifications, rdsSnapshotAccountTagKey))

	// Everything a restore needs is copied onto the snapshot record: the DB
	// instance record is deleted moments later.
	snapshot, found := h.snapshotRecord(t, "orders-db-final")
	require.True(t, found)
	assert.Equal(t, testDBID, snapshot.DBInstanceIdentifier)
	assert.Equal(t, SnapshotTypeManual, snapshot.SnapshotType)
	assert.Equal(t, "snap-0001", snapshot.SnapshotID)
	assert.Equal(t, "postgres", snapshot.Engine)
	assert.Equal(t, "vol-rdsdata01", snapshot.SourceVolumeID)
}

func TestDeleteDBInstance_ReservesTheFinalSnapshotBeforeCuttingIt(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	seedInstance(t, h.svc, availableRecord())

	h.snaps.beforeCreate = func() {
		record, found := h.snapshotRecord(t, "orders-db-final")
		require.True(t, found)
		assert.Equal(t, SnapshotStatusCreating, record.Status)
		assert.Empty(t, record.SnapshotID)
	}

	_, err := h.svc.DeleteDBInstance(t.Context(), &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier:      aws.String(testDBID),
		FinalDBSnapshotIdentifier: aws.String("orders-db-final"),
	}, testAccountID)
	require.NoError(t, err)
}

// A dead worker can leave the EC2 snapshot cut while its RDS record still says
// creating. The resumed teardown adopts that exact account-scoped snapshot
// instead of cutting another one and permanently pinning the source volume twice.
func TestReconciler_AdoptsAnInterruptedFinalSnapshot(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	rec := abandonedDelete(availableRecord())
	rec.FinalSnapshotIdentifier = "orders-db-final"
	seedInstance(t, h.svc, rec)

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	creating := newDBSnapshotRecord(testAccountID, &rec, &validatedSnapshot{
		DBSnapshotIdentifier: rec.FinalSnapshotIdentifier,
		Tags:                 rec.Tags,
	})
	require.NoError(t, createJSON(t.Context(), kv, DBSnapshotKey(rec.FinalSnapshotIdentifier), &creating))
	h.snaps.holding = []string{"snap-cut-before-crash"}
	h.snaps.tagged = map[string]map[string]string{
		"snap-cut-before-crash": {
			rdsSnapshotTagKey:        rec.FinalSnapshotIdentifier,
			rdsSnapshotAccountTagKey: testAccountID,
		},
	}

	require.NoError(t, onePass(t, testReconciler(t, h.svc, "node-a")))

	assert.Empty(t, h.snaps.created, "the resumed teardown must adopt the cut snapshot, not create another")
	stored, found := h.snapshotRecord(t, rec.FinalSnapshotIdentifier)
	require.True(t, found)
	assert.Equal(t, SnapshotStatusAvailable, stored.Status)
	assert.Equal(t, "snap-cut-before-crash", stored.SnapshotID)
}

// A snapshot references its source volume's chunks, so the volume cannot
// be deleted while the final snapshot survives. It is retained and recorded
// with the snapshots holding it.
func TestDeleteDBInstance_RetainsTheVolumeAFinalSnapshotStillHolds(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.DeleteDBInstance(t.Context(), &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier:      aws.String(testDBID),
		FinalDBSnapshotIdentifier: aws.String("orders-db-final"),
	}, testAccountID)
	require.NoError(t, err)

	assert.Empty(t, h.volumes.deleted, "a volume a snapshot references must not be deleted")
	retained, found := h.retainedVolume(t, "vol-rdsdata01")
	require.True(t, found)
	assert.Equal(t, testDBID, retained.DBInstanceIdentifier)
	assert.Equal(t, []string{"snap-0001"}, retained.Snapshots)
}

// A snapshot record outlives the instance it came from, so an instance
// re-created under the same name must not have a stale snapshot of a previous
// data volume accepted as its own — the volume would then be deleted unbacked.
func TestDeleteDBInstance_RejectsAFinalSnapshotIdentifierAlreadyTaken(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	seedInstance(t, h.svc, availableRecord())

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	// The snapshot left behind by the earlier instance of the same name.
	require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey("orders-db-final"), &DBSnapshotRecord{
		DBSnapshotIdentifier: "orders-db-final",
		DBInstanceIdentifier: testDBID,
		SourceVolumeID:       "vol-rdsdata00",
	}))

	_, err = h.svc.DeleteDBInstance(t.Context(), &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier:      aws.String(testDBID),
		FinalDBSnapshotIdentifier: aws.String("orders-db-final"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBSnapshotAlreadyExists)

	// Rejected before anything was torn down, so the customer loses nothing.
	assert.True(t, h.recordExists(t, testDBID))
	assert.Empty(t, h.launcher.terminated)
	assert.Empty(t, h.volumes.deleted)
	assert.Empty(t, h.snaps.created)
}

// Keeping the in-flight choice silently would answer a caller who asked for a
// final snapshot with no snapshot at all.
func TestDeleteDBInstance_RejectsARetryThatChangesTheSnapshotChoice(t *testing.T) {
	cases := []struct {
		name     string
		inFlight string
		retry    *rds.DeleteDBInstanceInput
	}{
		{"SnapshotRetryOfASkippedDelete", "", &rds.DeleteDBInstanceInput{
			DBInstanceIdentifier:      aws.String(testDBID),
			FinalDBSnapshotIdentifier: aws.String("orders-db-final"),
		}},
		{"SkipRetryOfASnapshottedDelete", "orders-db-final", skipFinalSnapshot()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLifecycleHarness(t, false)
			rec := availableRecord()
			rec.Status = StatusDeleting
			rec.FinalSnapshotIdentifier = tc.inFlight
			seedInstance(t, h.svc, rec)

			_, err := h.svc.DeleteDBInstance(t.Context(), tc.retry, testAccountID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceInvalidState)
			assert.Contains(t, err.Error(), snapshotChoice(tc.inFlight))

			assert.True(t, h.recordExists(t, testDBID))
			assert.Empty(t, h.launcher.terminated)
			assert.Empty(t, h.volumes.deleted)
		})
	}
}

// The volume store enforces against its own snapshot index, which can see a
// reference the EC2 enumeration missed. Retaining converges; erroring would
// wedge the delete on every retry until the bound marked it failed.
func TestDeleteDBInstance_RetainsTheVolumeWhenTheVolumeStoreReportsItInUse(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.volumes.deleteErr = errors.New(awserrors.ErrorVolumeInUse)
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.DeleteDBInstance(t.Context(), skipFinalSnapshot(), testAccountID)
	require.NoError(t, err)

	retained, found := h.retainedVolume(t, "vol-rdsdata01")
	require.True(t, found, "a volume the store refused to delete must be recorded, not forgotten")
	assert.True(t, retained.HoldersUnresolved,
		"nothing named a holder, so a release must re-check rather than trust the empty list")
	assert.Equal(t, testDBID, retained.DBInstanceIdentifier)

	// The teardown still completed: the record is what makes the volume findable.
	assert.False(t, h.recordExists(t, testDBID))
}

// A teardown that died partway through is replayed, so every step has to treat
// a resource that is already gone as work it has done.
func TestReconciler_IsIdempotentAcrossARetriedTeardown(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	rec := abandonedDelete(availableRecord())
	rec.FinalSnapshotIdentifier = "orders-db-final"
	seedInstance(t, h.svc, rec)
	reconciler := testReconciler(t, h.svc, "node-a")
	require.NoError(t, onePass(t, reconciler))

	// The record left behind by the interrupted attempt, replayed.
	seedInstance(t, h.svc, rec)
	require.NoError(t, onePass(t, reconciler))

	assert.Len(t, h.snaps.created, 1, "a replayed teardown must not take a second final snapshot")
	assert.False(t, h.recordExists(t, testDBID))
}

// The reconciler owns a teardown whose caller never came back, so an
// interrupted delete finishes without an operator.
func TestReconciler_ResumesAnInterruptedDelete(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	seedInstance(t, h.svc, abandonedDelete(availableRecord()))

	reconciler := testReconciler(t, h.svc, "node-a")
	require.NoError(t, onePass(t, reconciler))

	assert.Equal(t, []string{testInstance}, h.launcher.terminated)
	assert.False(t, h.recordExists(t, testDBID))
}

// A second teardown racing the handler's sees the VM mid-shutdown as gone and
// fails on the still-attached volume, so the handler's delete is left to it.
func TestReconciler_LeavesADeleteItsHandlerMayStillBeRunning(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	rec := availableRecord()
	started := time.Now().UTC()
	rec.Status = StatusDeleting
	rec.TransitionStartedAt = &started
	seedInstance(t, h.svc, rec)

	require.NoError(t, onePass(t, testReconciler(t, h.svc, "node-a")))

	assert.Empty(t, h.launcher.terminated)
	assert.True(t, h.recordExists(t, testDBID))
}

// A repeat must not start a teardown of its own alongside the one under way;
// the reconciler is what resumes an abandoned one.
func TestDeleteDBInstance_ARepeatStartsNoTeardown(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	seedInstance(t, h.svc, abandonedDelete(availableRecord()))

	out, err := h.svc.DeleteDBInstance(t.Context(), skipFinalSnapshot(), testAccountID)
	require.NoError(t, err)

	assert.Equal(t, string(StatusDeleting), aws.StringValue(out.DBInstance.DBInstanceStatus))
	assert.Empty(t, h.launcher.terminated)
	assert.True(t, h.recordExists(t, testDBID))
}

// The reconciler resumes a delete from the persisted stamp, so the handler's
// teardown must stop at that same instant, however long the acceptance took.
func TestDeleteDBInstance_TeardownEndsWhenTheClaimDoes(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	seedInstance(t, h.svc, availableRecord())
	var claimEnds time.Time
	h.snaps.beforeCreate = func() { stored := h.record(t); claimEnds = deleteClaimEnds(&stored) }

	_, err := h.svc.DeleteDBInstance(t.Context(), &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier:      aws.String(testDBID),
		FinalDBSnapshotIdentifier: aws.String("orders-db-final"),
	}, testAccountID)
	require.NoError(t, err)

	require.False(t, claimEnds.IsZero())
	assert.True(t, h.snaps.deadline.Equal(claimEnds), "deadline %s, claim ends %s", h.snaps.deadline, claimEnds)
}

// A graceful stop that runs into the claim's end is swallowed, so the teardown
// itself has to notice and leave the terminate to the reconciler.
func TestTeardownDBInstance_StartsNoTerminatePastItsDeadline(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.agent.silenceType(CommandStopEngine)
	rec := availableRecord()
	rec.Status = StatusDeleting
	rec.FinalSnapshotIdentifier = "orders-db-final"
	seedInstance(t, h.svc, rec)
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	err = h.svc.teardownDBInstance(ctx, kv, testAccountID, &rec, true)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, h.launcher.terminated)
	assert.True(t, h.recordExists(t, testDBID))
}

// The shape a delete leaves once its handler's request budget has run out.
func abandonedDelete(rec DBInstanceRecord) DBInstanceRecord {
	started := time.Now().UTC().Add(-deleteTimeout - time.Minute)
	rec.Status = StatusDeleting
	rec.TransitionStartedAt = &started
	return rec
}

// A stop whose caller died leaves the VM possibly still running, so the stop is
// re-issued rather than assumed.
func TestReconciler_ResumesAnInterruptedStop(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	rec := availableRecord()
	rec.Status = StatusStopping
	seedInstance(t, h.svc, rec)

	reconciler := testReconciler(t, h.svc, "node-a")
	require.NoError(t, onePass(t, reconciler))

	assert.Equal(t, []string{"stop:" + testInstance}, h.cmdr.calls)
	assert.Equal(t, StatusStopped, h.record(t).Status)
}

// A stopped DB VM is held by no node, so the system terminate finds nothing. Its
// data volume leaves the shared record and is released before the record is
// terminated, so no later terminate of that record can detach the volume again.
func TestDeleteDBInstance_TerminatesAStoppedVMFromItsRecord(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.launcher.terminateErr = systeminstance.ErrSystemInstanceNotFound
	h.storage.attachedTo = map[string]string{"vol-rdsdata01": testInstance}
	h.cmdr.onTerminateStopped = func(string) {
		assert.Empty(t, h.storage.attachedTo, "the data volume is released before the record is terminated")
	}
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.DeleteDBInstance(t.Context(), skipFinalSnapshot(), testAccountID)
	require.NoError(t, err)

	assert.Equal(t, []string{
		"detach-stopped:" + testInstance + ":vol-rdsdata01",
		"terminate-stopped:" + testInstance,
	}, h.cmdr.calls)
	assert.Equal(t, []string{"vol-rdsdata01"}, h.storage.detached)
	assert.Equal(t, []string{"vol-rdsdata01"}, h.volumes.deleted)
	assert.False(t, h.recordExists(t, testDBID))
}

// A DB record whose InstanceID is a customer instance must leave that instance
// alone: stopped, its volumes still attached, nothing released or terminated.
func TestS7_DeleteDBInstanceNeverTouchesACustomerInstance(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.launcher.terminateErr = fmt.Errorf("%w: %s", systeminstance.ErrSystemInstanceNotSystem, testInstance)
	h.storage.attachedTo = map[string]string{"vol-rdsdata01": testInstance}
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.DeleteDBInstance(t.Context(), skipFinalSnapshot(), testAccountID)

	require.ErrorIs(t, err, systeminstance.ErrSystemInstanceNotSystem)
	assert.Empty(t, h.cmdr.calls, "the customer instance's record is never edited or terminated")
	assert.Empty(t, h.storage.detached, "its volumes stay attached")
	assert.Equal(t, map[string]string{"vol-rdsdata01": testInstance}, h.storage.attachedTo)
	assert.Empty(t, h.volumes.deleted)
	assert.True(t, h.recordExists(t, testDBID), "the record stays so the delete can be inspected")
}

// A record edit that fails leaves the record naming the volume, so neither the
// release nor the terminate may run; the teardown stops and is retried.
func TestDeleteDBInstance_StopsWhileTheStoppedRecordStillNamesTheVolume(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.launcher.terminateErr = systeminstance.ErrSystemInstanceNotFound
	h.storage.attachedTo = map[string]string{"vol-rdsdata01": testInstance}
	h.cmdr.detachStoppedErr = errors.New(awserrors.ErrorIncorrectInstanceState)
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.DeleteDBInstance(t.Context(), skipFinalSnapshot(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remove the data volume")

	assert.NotContains(t, h.cmdr.calls, "terminate-stopped:"+testInstance)
	assert.Empty(t, h.storage.detached)
	assert.Empty(t, h.volumes.deleted)
	assert.True(t, h.recordExists(t, testDBID), "the record stays so the teardown can be resumed")
}

// A release that keeps failing must not let the volume be read as snapshot-held:
// the teardown stops with the record kept, and is retried.
func TestDeleteDBInstance_StopsWhileTheDataVolumeCannotBeReleased(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.launcher.terminateErr = systeminstance.ErrSystemInstanceNotFound
	h.storage.attachedTo = map[string]string{"vol-rdsdata01": testInstance}
	h.storage.detachErr = errors.New("the volume store is unavailable")
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.DeleteDBInstance(t.Context(), skipFinalSnapshot(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "release the data volume")

	assert.NotContains(t, h.cmdr.calls, "terminate-stopped:"+testInstance)
	assert.Empty(t, h.volumes.deleted)
	_, retained := h.retainedVolume(t, "vol-rdsdata01")
	assert.False(t, retained, "an attached volume must not be recorded as retained")
	assert.True(t, h.recordExists(t, testDBID), "the record stays so the teardown can be resumed")
}

// A terminate that left the data volume attached stops the teardown before the
// release; the retry finds no VM and releases the volume itself.
func TestDeleteDBInstance_RetryReleasesWhatATerminateLeftAttached(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.storage.attachedTo = map[string]string{"vol-rdsdata01": testInstance}
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.DeleteDBInstance(t.Context(), skipFinalSnapshot(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still attached to the terminated VM")
	assert.Empty(t, h.volumes.deleted)
	assert.True(t, h.recordExists(t, testDBID))

	h.launcher.terminateErr = systeminstance.ErrSystemInstanceNotFound
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	rec := h.record(t)
	require.NoError(t, h.svc.teardownDBInstance(t.Context(), kv, testAccountID, &rec, false))
	assert.Equal(t, []string{"vol-rdsdata01"}, h.storage.detached)
	assert.Equal(t, []string{"vol-rdsdata01"}, h.volumes.deleted)
	assert.False(t, h.recordExists(t, testDBID))
}

// A stopped VM that refuses the terminate is not gone, so the teardown must not
// carry on as though it were.
func TestDeleteDBInstance_ReturnsAStoppedTerminateRefusal(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.launcher.terminateErr = systeminstance.ErrSystemInstanceNotFound
	h.cmdr.terminateStoppedErr = errors.New(awserrors.ErrorIncorrectInstanceState)
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.DeleteDBInstance(t.Context(), skipFinalSnapshot(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIncorrectInstanceState)
	assert.Empty(t, h.volumes.deleted)
	assert.True(t, h.recordExists(t, testDBID))
}

// A class change that finished after the delete was accepted replaced the VM;
// the teardown must take down the replacement, not the VM the delete first saw.
func TestTeardownDBInstance_UsesTheVMAModifyLeftBehind(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	stale := availableRecord()
	stale.Status = StatusDeleting
	replaced := stale
	replaced.InstanceID = "i-replacement"
	seedInstance(t, h.svc, replaced)

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	require.NoError(t, h.svc.teardownDBInstance(t.Context(), kv, testAccountID, &stale, false))

	assert.Equal(t, []string{"i-replacement"}, h.launcher.terminated)
	assert.False(t, h.recordExists(t, testDBID))
}

// A modify that fails after a delete was accepted must not turn the record to
// failed, or the reconciler would never finish the delete.
func TestFailTransition_KeepsAnAcceptedDelete(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	rec := availableRecord()
	rec.Status = StatusDeleting
	seedInstance(t, h.svc, rec)

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	err = h.svc.failTransition(t.Context(), kv, testAccountID, &rec, "the DB instance could not be modified: boom")
	require.Error(t, err)

	assert.Equal(t, StatusDeleting, h.record(t).Status)
}
