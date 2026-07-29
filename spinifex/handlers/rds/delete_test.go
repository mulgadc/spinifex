package handlers_rds

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
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

// D18: the snapshot is taken once the VM is gone, so it reads a sealed data
// volume rather than one a live engine is still writing to.
func TestDeleteDBInstance_TakesTheFinalSnapshotAfterTheEngineAndVMAreDown(t *testing.T) {
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

// D10: a snapshot references its source volume's chunks, so the volume cannot
// be deleted while the final snapshot survives. It is retained and recorded
// with the snapshots holding it.
func TestDeleteDBInstance_RetainsTheVolumeAFinalSnapshotStillHolds(t *testing.T) {
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

// A teardown that died partway through is replayed, so every step has to treat
// a resource that is already gone as work it has done.
func TestDeleteDBInstance_IsIdempotentAcrossARetriedTeardown(t *testing.T) {
	h := newLifecycleHarness(t, false)
	rec := availableRecord()
	// The shape a delete leaves behind when its caller died mid-teardown.
	rec.Status = StatusDeleting
	rec.FinalSnapshotIdentifier = "orders-db-final"
	seedInstance(t, h.svc, rec)

	input := &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier:      aws.String(testDBID),
		FinalDBSnapshotIdentifier: aws.String("orders-db-final"),
	}
	_, err := h.svc.DeleteDBInstance(t.Context(), input, testAccountID)
	require.NoError(t, err)

	// The record left behind by the interrupted attempt, replayed.
	seedInstance(t, h.svc, rec)
	_, err = h.svc.DeleteDBInstance(t.Context(), input, testAccountID)
	require.NoError(t, err)

	assert.Len(t, h.snaps.created, 1, "a replayed teardown must not take a second final snapshot")
	assert.False(t, h.recordExists(t, testDBID))
}

// The reconciler owns a teardown whose caller never came back, so an
// interrupted delete finishes without an operator.
func TestReconciler_ResumesAnInterruptedDelete(t *testing.T) {
	h := newLifecycleHarness(t, false)
	rec := availableRecord()
	rec.Status = StatusDeleting
	seedInstance(t, h.svc, rec)

	reconciler := NewReconciler(h.svc, "node-a")
	require.NoError(t, reconciler.reconcileOnce(t.Context()))

	assert.Equal(t, []string{testInstance}, h.launcher.terminated)
	assert.False(t, h.recordExists(t, testDBID))
}

// A stop whose caller died leaves the VM possibly still running, so the stop is
// re-issued rather than assumed.
func TestReconciler_ResumesAnInterruptedStop(t *testing.T) {
	h := newLifecycleHarness(t, false)
	rec := availableRecord()
	rec.Status = StatusStopping
	seedInstance(t, h.svc, rec)

	reconciler := NewReconciler(h.svc, "node-a")
	require.NoError(t, reconciler.reconcileOnce(t.Context()))

	assert.Equal(t, []string{"stop:" + testInstance}, h.cmdr.calls)
	assert.Equal(t, StatusStopped, h.record(t).Status)
}
