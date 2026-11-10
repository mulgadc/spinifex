package instance

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/snapshot"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pendingFinalSnapshotID = "orders-db-final"

// ec2Snapshots is the real EC2 snapshot service over the memory provider, so
// a snapshot can answer pending and settle later as an OCI backup does. It
// records every snapshot delete it is asked for, and can fail the volume
// describe a teardown's release reads.
type ec2Snapshots struct {
	*snapshot.SnapshotServiceImpl

	mem *ebsprovider.MemoryProvider

	mu          sync.Mutex
	deleted     []string
	describeErr error
	// strictErr fails the strict describe a reconciler reads a snapshot's state by.
	strictErr error
}

var _ snapshotProvider = (*ec2Snapshots)(nil)

func (e *ec2Snapshots) DescribeSnapshots(ctx context.Context, input *ec2.DescribeSnapshotsInput, accountID string) (*ec2.DescribeSnapshotsOutput, error) {
	e.mu.Lock()
	err := e.describeErr
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return e.SnapshotServiceImpl.DescribeSnapshots(ctx, input, accountID)
}

func (e *ec2Snapshots) DescribeSnapshotsStrict(ctx context.Context, input *ec2.DescribeSnapshotsInput, accountID string) (*ec2.DescribeSnapshotsOutput, error) {
	e.mu.Lock()
	err := e.strictErr
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return e.SnapshotServiceImpl.DescribeSnapshotsStrict(ctx, input, accountID)
}

func (e *ec2Snapshots) DeleteSnapshot(ctx context.Context, input *ec2.DeleteSnapshotInput, accountID string) (*ec2.DeleteSnapshotOutput, error) {
	e.mu.Lock()
	e.deleted = append(e.deleted, aws.StringValue(input.SnapshotId))
	e.mu.Unlock()
	return e.SnapshotServiceImpl.DeleteSnapshot(ctx, input, accountID)
}

// withEC2Snapshots replaces the harness's snapshot fake with the real service,
// holding the instance's data volume as the system account's.
func withEC2Snapshots(t *testing.T, h *lifecycleHarness) *ec2Snapshots {
	t.Helper()
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	require.NoError(t, ebsmetadata.NewStore(store, cfg.Predastore.Bucket).PutVolume(t.Context(), ebsmetadata.Volume{
		VolumeID: "vol-rdsdata01", TenantID: awsidentifiers.GlobalAccountID, CapacityGiB: 20, State: "in-use",
		AvailabilityZone: "us-east-1a", ProviderHandle: "memory://volume/vol-rdsdata01",
	}))
	mem := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	_, err := mem.CreateVolume(t.Context(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-rdsdata01",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 20 << 30},
	})
	require.NoError(t, err)
	svc := snapshot.NewSnapshotServiceImplWithStore(cfg, store, nil)
	svc.SetEBSProvider(mem)
	e := &ec2Snapshots{SnapshotServiceImpl: svc, mem: mem}
	h.svc.deps.Snapshots = e
	return e
}

// settle finishes the provider's backup and lets the EC2 reaper record it.
func (e *ec2Snapshots) settle(t *testing.T, snapshotID string, state ebsprovider.SnapshotState) {
	t.Helper()
	require.NoError(t, e.mem.SettleSnapshot(snapshotID, state))
	_, err := e.NewPendingSnapshotReaper().Sweep(t.Context())
	require.NoError(t, err)
}

func (e *ec2Snapshots) deletes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.deleted...)
}

func (h *lifecycleHarness) rewriteSnapshotRecord(t *testing.T, id string, edit func(*DBSnapshotRecord)) {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	rec, found := h.snapshotRecord(t, id)
	require.True(t, found)
	edit(&rec)
	require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(id), &rec))
}

func eventsOf(t *testing.T, h *lifecycleHarness, sourceType, id string) []string {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var ring eventRing
	found, err := getJSON(t.Context(), kv, EventRingKey(sourceType, id), &ring)
	require.NoError(t, err)
	if !found {
		return nil
	}
	messages := make([]string, 0, len(ring.Events))
	for _, event := range ring.Events {
		messages = append(messages, event.Message)
	}
	return messages
}

func countContaining(messages []string, part string) int {
	n := 0
	for _, m := range messages {
		if strings.Contains(m, part) {
			n++
		}
	}
	return n
}

// A DB snapshot whose EC2 snapshot is pending stays creating, past the resolve
// timeout and across a reconciler restart, and settles with the EC2 snapshot:
// completed makes it available, error makes it failed with an event.
func TestReconciler_SettlesACreatingDBSnapshotFromItsEC2Snapshot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// adopt seeds the record a dead worker left, naming no EC2 snapshot,
		// and the EC2 snapshot it cut, instead of a CreateDBSnapshot.
		adopt      bool
		ambiguous  bool // the provider's first reply is lost after it accepts
		settleTo   ebsprovider.SnapshotState
		wantStatus string
	}{
		{name: "completed", settleTo: ebsprovider.SnapshotStateCompleted, wantStatus: SnapshotStatusAvailable},
		{name: "error", settleTo: ebsprovider.SnapshotStateError, wantStatus: SnapshotStatusFailed},
		{name: "completed after a lost reply", ambiguous: true, settleTo: ebsprovider.SnapshotStateCompleted,
			wantStatus: SnapshotStatusAvailable},
		{name: "adopted, completed", adopt: true, settleTo: ebsprovider.SnapshotStateCompleted, wantStatus: SnapshotStatusAvailable},
		{name: "adopted, error", adopt: true, settleTo: ebsprovider.SnapshotStateError, wantStatus: SnapshotStatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newLifecycleHarness(t, false)
			e := withEC2Snapshots(t, h)
			e.mem.AnswerSnapshotsPending(true)
			if tc.ambiguous {
				e.mem.FailCreateSnapshotsAfterAccept(1, nil)
			}
			rec := availableRecord()
			seedInstance(t, h.svc, rec)

			if tc.adopt {
				kv, err := h.svc.bucket(t.Context(), testAccountID)
				require.NoError(t, err)
				stale := newDBSnapshotRecord(testAccountID, &rec, &validatedSnapshot{DBSnapshotIdentifier: testSnapshotID})
				stale.CreatedAt = time.Now().UTC().Add(-2 * snapshotResolveTimeout)
				require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(testSnapshotID), &stale))
				_, _, _, err = h.svc.snapshotDataVolume(t.Context(), testAccountID, &rec, StatusStopped, testSnapshotID)
				require.NoError(t, err)
			} else {
				out, err := h.svc.CreateDBSnapshot(t.Context(), &rds.CreateDBSnapshotInput{
					DBSnapshotIdentifier: aws.String(testSnapshotID), DBInstanceIdentifier: aws.String(testDBID),
				}, testAccountID)
				require.NoError(t, err)
				assert.Equal(t, SnapshotStatusCreating, aws.StringValue(out.DBSnapshot.Status))
				assert.Equal(t, StatusAvailable, h.record(t).Status, "the instance leaves backing-up once CreateSnapshot returns")
			}

			require.NoError(t, onePass(t, testReconciler(t, h.svc, "node-a")))
			stored, found := h.snapshotRecord(t, testSnapshotID)
			require.True(t, found)
			assert.Equal(t, SnapshotStatusCreating, stored.Status)
			require.NotEmpty(t, stored.SnapshotID, "the record names its EC2 snapshot")
			assert.Equal(t, tc.adopt, stored.CrashConsistent)
			ec2ID := stored.SnapshotID

			// Past the resolve timeout, and on a reconciler that never saw the record.
			h.rewriteSnapshotRecord(t, testSnapshotID, func(r *DBSnapshotRecord) {
				r.CreatedAt = time.Now().UTC().Add(-2 * snapshotResolveTimeout)
			})
			restarted := testReconciler(t, h.svc, "node-b")
			revisit, err := restarted.reconcileOnce(t.Context())
			require.NoError(t, err)
			assert.LessOrEqual(t, revisit, reconcileInterval, "a pending EC2 snapshot is read again every pass")
			stored, _ = h.snapshotRecord(t, testSnapshotID)
			assert.Equal(t, SnapshotStatusCreating, stored.Status, "a pending EC2 snapshot keeps the DB snapshot creating")
			assert.Equal(t, ec2ID, stored.SnapshotID)

			e.settle(t, ec2ID, tc.settleTo)
			require.NoError(t, onePass(t, restarted))
			require.NoError(t, onePass(t, restarted))

			stored, found = h.snapshotRecord(t, testSnapshotID)
			require.True(t, found, "a settled DB snapshot is never withdrawn")
			assert.Equal(t, tc.wantStatus, stored.Status)
			assert.Equal(t, ec2ID, stored.SnapshotID)
			assert.NotNil(t, stored.SettledAt)
			events := eventsOf(t, h, EventSourceTypeDBSnapshot, testSnapshotID)
			if tc.wantStatus == SnapshotStatusFailed {
				assert.Equal(t, 1, countContaining(events, "ended in error"), "one failure event: %v", events)
			} else {
				assert.Equal(t, 1, countContaining(events, "DB snapshot created."), "one creation event: %v", events)
			}
			assert.Empty(t, e.deletes())
		})
	}
}

// Two lost replies are an EC2 InternalError, and the DB snapshot record that
// only described that call is discarded rather than left creating.
func TestCreateDBSnapshot_DiscardsTheRecordWhenEC2AnswersInternalError(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	e := withEC2Snapshots(t, h)
	e.mem.AnswerSnapshotsPending(true)
	e.mem.FailCreateSnapshotsAfterAccept(2, nil)
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.CreateDBSnapshot(t.Context(), &rds.CreateDBSnapshotInput{
		DBSnapshotIdentifier: aws.String(testSnapshotID), DBInstanceIdentifier: aws.String(testDBID),
	}, testAccountID)
	require.Error(t, err)

	_, found := h.snapshotRecord(t, testSnapshotID)
	assert.False(t, found)
	assert.Equal(t, StatusAvailable, h.record(t).Status)
}

// startPendingFinalSnapshot deletes the instance with a final snapshot the
// provider answers pending, then backdates the delete past its handler's claim
// and transitionTimeout, as a backup that outlasts both does.
func startPendingFinalSnapshot(t *testing.T, h *lifecycleHarness, e *ec2Snapshots, ago time.Duration) string {
	t.Helper()
	e.mem.AnswerSnapshotsPending(true)
	seedInstance(t, h.svc, availableRecord())

	out, err := h.svc.DeleteDBInstance(t.Context(), &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: aws.String(testDBID), FinalDBSnapshotIdentifier: aws.String(pendingFinalSnapshotID),
	}, testAccountID)
	require.NoError(t, err, "a pending final snapshot is not a failed delete")
	assert.Equal(t, string(StatusDeleting), aws.StringValue(out.DBInstance.DBInstanceStatus))

	rec := h.record(t)
	started := time.Now().UTC().Add(-ago)
	rec.TransitionStartedAt = &started
	seedInstance(t, h.svc, rec)
	h.rewriteSnapshotRecord(t, pendingFinalSnapshotID, func(r *DBSnapshotRecord) { r.CreatedAt = started.Add(time.Minute) })

	final, found := h.snapshotRecord(t, pendingFinalSnapshotID)
	require.True(t, found)
	assert.Equal(t, SnapshotStatusCreating, final.Status)
	require.NotEmpty(t, final.SnapshotID)
	return final.SnapshotID
}

// A deletion stays in deleting while its final snapshot is pending, however
// long that takes, and the wait does not count toward transitionTimeout once
// the snapshot completes; the time after it still does.
func TestReconciler_HoldsADeletionWhileItsFinalSnapshotIsPending(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	e := withEC2Snapshots(t, h)
	ec2ID := startPendingFinalSnapshot(t, h, e, 3*time.Hour)
	stopsBefore := len(h.agent.received())

	r := testReconciler(t, h.svc, "node-a")
	require.NoError(t, onePass(t, r))
	require.NoError(t, onePass(t, testReconciler(t, h.svc, "node-b")))

	assert.Equal(t, StatusDeleting, h.record(t).Status, "transitionTimeout does not run while the final snapshot is pending")
	assert.Len(t, h.agent.received(), stopsBefore, "a teardown waiting on its cut snapshot does not stop the engine again")
	assert.Empty(t, h.volumes.deleted)

	// Completes, but the volume release then fails: the three hours waited do
	// not count, so the deletion is retried rather than failed.
	e.mu.Lock()
	e.describeErr = errors.New("snapshot metadata temporarily unavailable")
	e.mu.Unlock()
	e.settle(t, ec2ID, ebsprovider.SnapshotStateCompleted)
	require.NoError(t, onePass(t, r))

	final, _ := h.snapshotRecord(t, pendingFinalSnapshotID)
	assert.Equal(t, SnapshotStatusAvailable, final.Status)
	assert.Equal(t, StatusDeleting, h.record(t).Status, "the wait on the final snapshot is excluded from transitionTimeout")

	// Still failing transitionTimeout after it settled, the deletion fails.
	h.rewriteSnapshotRecord(t, pendingFinalSnapshotID, func(r *DBSnapshotRecord) {
		settled := r.SettledAt.Add(-transitionTimeout - time.Minute)
		r.SettledAt = &settled
	})
	require.NoError(t, onePass(t, r))
	assert.Equal(t, StatusFailed, h.record(t).Status)
	assert.Empty(t, h.volumes.deleted)

	// Once the release can be read, the retained volume is recorded and the
	// instance goes, as for a final snapshot that completed at once.
	e.mu.Lock()
	e.describeErr = nil
	e.mu.Unlock()
	rec := h.record(t)
	rec.Status = StatusDeleting
	now := time.Now().UTC().Add(-deleteTimeout - time.Minute)
	rec.TransitionStartedAt = &now
	seedInstance(t, h.svc, rec)
	require.NoError(t, onePass(t, r))
	assert.False(t, h.recordExists(t, testDBID))
	retained, found := h.retainedVolume(t, "vol-rdsdata01")
	require.True(t, found)
	assert.Equal(t, []string{ec2ID}, retained.Snapshots)
	assert.Empty(t, h.volumes.deleted)
	assert.Equal(t, 1, countContaining(eventsOf(t, h, EventSourceTypeDBSnapshot, pendingFinalSnapshotID), "Final DB snapshot created."))
}

// A pending final snapshot whose state cannot be read for a while does not
// fail the deletion past transitionTimeout: the time it has been pending is
// excluded before it settles, and the deletion finishes once it completes.
func TestReconciler_PendingFinalSnapshotReadFailureDoesNotFailTheDeletion(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	e := withEC2Snapshots(t, h)
	ec2ID := startPendingFinalSnapshot(t, h, e, 3*time.Hour)
	r := testReconciler(t, h.svc, "node-a")

	e.mu.Lock()
	e.strictErr = errors.New("snapshot metadata temporarily unavailable")
	e.mu.Unlock()
	require.ErrorContains(t, onePass(t, r), "temporarily unavailable", "the creating DB snapshot's settle reports the read")
	assert.Equal(t, StatusDeleting, h.record(t).Status, "a pending backup's wait is excluded even when its state cannot be read")
	assert.Empty(t, h.volumes.deleted)

	e.mu.Lock()
	e.strictErr = nil
	e.mu.Unlock()
	require.NoError(t, onePass(t, r))
	assert.Equal(t, StatusDeleting, h.record(t).Status, "still pending, so still waiting")

	// One pass records the snapshot available, and the next finishes the deletion.
	e.settle(t, ec2ID, ebsprovider.SnapshotStateCompleted)
	require.NoError(t, onePass(t, r))
	require.NoError(t, onePass(t, r))
	assert.False(t, h.recordExists(t, testDBID), "the deletion recovers once the backup completes")
	final, _ := h.snapshotRecord(t, pendingFinalSnapshotID)
	assert.Equal(t, SnapshotStatusAvailable, final.Status)
}

// A final snapshot that errors keeps the instance in deleting, with one event
// naming the failed snapshot, however many passes follow.
func TestReconciler_KeepsADeletionWhoseFinalSnapshotFailed(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	e := withEC2Snapshots(t, h)
	ec2ID := startPendingFinalSnapshot(t, h, e, 3*time.Hour)

	e.settle(t, ec2ID, ebsprovider.SnapshotStateError)
	r := testReconciler(t, h.svc, "node-a")
	for range 3 {
		require.NoError(t, onePass(t, r))
	}

	assert.Equal(t, StatusDeleting, h.record(t).Status)
	final, found := h.snapshotRecord(t, pendingFinalSnapshotID)
	require.True(t, found)
	assert.Equal(t, SnapshotStatusFailed, final.Status)
	instanceEvents := eventsOf(t, h, EventSourceTypeDBInstance, testDBID)
	assert.Equal(t, 1, countContaining(instanceEvents, "The final DB snapshot "+pendingFinalSnapshotID+" failed"), "%v", instanceEvents)
	assert.Equal(t, 1, countContaining(eventsOf(t, h, EventSourceTypeDBSnapshot, pendingFinalSnapshotID), ec2ID))
	assert.Empty(t, h.volumes.deleted)
	_, retained := h.retainedVolume(t, "vol-rdsdata01")
	assert.False(t, retained, "the teardown stops before it decides the volume's fate")

	// A retried DeleteDBInstance reports the deletion and changes nothing.
	_, err := h.svc.DeleteDBInstance(t.Context(), &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: aws.String(testDBID), FinalDBSnapshotIdentifier: aws.String(pendingFinalSnapshotID),
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, h.volumes.deleted)
}

// Verifies: ADR-0003:S7.
//
// TestS7_RDSDeletionNeverDeletesTheDataVolumeWithoutACompletedFinalSnapshot
// drives a deletion whose final snapshot is pending or errored through every
// pass past every timeout, and requires no volume or snapshot was deleted.
func TestS7_RDSDeletionNeverDeletesTheDataVolumeWithoutACompletedFinalSnapshot(t *testing.T) {
	t.Parallel()
	for _, settleTo := range []ebsprovider.SnapshotState{"", ebsprovider.SnapshotStateError} {
		name := "pending"
		if settleTo != "" {
			name = string(settleTo)
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newLifecycleHarness(t, false)
			e := withEC2Snapshots(t, h)
			ec2ID := startPendingFinalSnapshot(t, h, e, 24*time.Hour)
			if settleTo != "" {
				e.settle(t, ec2ID, settleTo)
			}
			for _, holder := range []string{"node-a", "node-b", "node-a"} {
				require.NoError(t, onePass(t, testReconciler(t, h.svc, holder)))
			}

			assert.Empty(t, h.volumes.deleted, "ADR-0003:S7: the data volume was deleted with no completed final snapshot")
			assert.Empty(t, e.deletes(), "ADR-0003:S7: an EC2 snapshot was deleted")
			_, err := e.mem.GetVolume(t.Context(), ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-rdsdata01"})
			assert.NoError(t, err, "ADR-0003:S7: the data volume is gone at the provider")
			_, held := e.mem.Snapshot(ec2ID)
			assert.True(t, held, "ADR-0003:S7: the final snapshot is gone at the provider")
			assert.True(t, h.recordExists(t, testDBID), "ADR-0003:S7: the DB instance record is gone")
		})
	}
}

// A creating record whose EC2 snapshot is gone fails with an event, once: no
// pass can make that snapshot again, so creating would wait on it forever. A
// final snapshot that fails so keeps its deletion in deleting and the volume.
func TestReconciler_FailsACreatingDBSnapshotWhoseEC2SnapshotIsGone(t *testing.T) {
	t.Parallel()
	t.Run("DB snapshot", func(t *testing.T) {
		t.Parallel()
		h := newLifecycleHarness(t, false)
		kv, err := h.svc.bucket(t.Context(), testAccountID)
		require.NoError(t, err)
		rec := availableRecord()
		named := newDBSnapshotRecord(testAccountID, &rec, &validatedSnapshot{DBSnapshotIdentifier: testSnapshotID})
		named.SnapshotID = "snap-named01"
		require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(testSnapshotID), &named))

		r := testReconciler(t, h.svc, "node-a")
		for range 3 {
			require.NoError(t, onePass(t, r))
		}

		stored, found := h.snapshotRecord(t, testSnapshotID)
		require.True(t, found, "a failed DB snapshot is kept for the customer to see and delete")
		assert.Equal(t, SnapshotStatusFailed, stored.Status)
		assert.NotNil(t, stored.SettledAt)
		events := eventsOf(t, h, EventSourceTypeDBSnapshot, testSnapshotID)
		assert.Equal(t, 1, countContaining(events, "snap-named01 no longer exists"), "one failure event: %v", events)
	})
	t.Run("final snapshot", func(t *testing.T) {
		t.Parallel()
		h := newLifecycleHarness(t, false)
		e := withEC2Snapshots(t, h)
		ec2ID := startPendingFinalSnapshot(t, h, e, 3*time.Hour)
		require.NoError(t, e.MetadataStore().DeleteSnapshot(t.Context(), awsidentifiers.GlobalAccountID, ec2ID))

		r := testReconciler(t, h.svc, "node-a")
		for range 3 {
			require.NoError(t, onePass(t, r))
		}

		assert.Equal(t, StatusDeleting, h.record(t).Status)
		final, found := h.snapshotRecord(t, pendingFinalSnapshotID)
		require.True(t, found)
		assert.Equal(t, SnapshotStatusFailed, final.Status)
		instanceEvents := eventsOf(t, h, EventSourceTypeDBInstance, testDBID)
		assert.Equal(t, 1, countContaining(instanceEvents, "The final DB snapshot "+pendingFinalSnapshotID+" failed"), "%v", instanceEvents)
		assert.Empty(t, h.volumes.deleted, "the data volume is the only copy left")
	})
}

// A creating record naming an EC2 snapshot that reports no state, or cannot
// be read, is left creating: neither says the data is restorable or lost.
func TestReconciler_LeavesACreatingDBSnapshotItCannotSettle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setup   func(f *fakeSnapshots)
		wantErr bool
	}{
		{name: "the EC2 snapshot reports no state", setup: func(f *fakeSnapshots) {
			f.holding = []string{"snap-named01"}
			f.states = map[string]string{"snap-named01": ""}
		}},
		{name: "the EC2 snapshot cannot be read", setup: func(f *fakeSnapshots) {
			f.describeErr = errors.New("snapshot metadata temporarily unavailable")
		}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newLifecycleHarness(t, false)
			if tc.setup != nil {
				tc.setup(h.snaps)
			}
			kv, err := h.svc.bucket(t.Context(), testAccountID)
			require.NoError(t, err)
			rec := availableRecord()
			named := newDBSnapshotRecord(testAccountID, &rec, &validatedSnapshot{DBSnapshotIdentifier: testSnapshotID})
			named.SnapshotID = "snap-named01"
			named.CreatedAt = time.Now().UTC().Add(-2 * snapshotResolveTimeout)
			require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(testSnapshotID), &named))

			err = onePass(t, testReconciler(t, h.svc, "node-a"))
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			stored, found := h.snapshotRecord(t, testSnapshotID)
			require.True(t, found)
			assert.Equal(t, SnapshotStatusCreating, stored.Status)
			assert.Equal(t, "snap-named01", stored.SnapshotID)
		})
	}
}

// The state the creating handler's own CreateSnapshot answers decides the
// final snapshot: error holds the deletion with the volume kept, and a state
// it cannot read fails the request with the volume kept.
func TestDeleteDBInstance_FinalSnapshotStateAtCreate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		state      string
		wantErr    bool
		wantStatus string
	}{
		{name: "error", state: ec2.SnapshotStateError, wantStatus: SnapshotStatusFailed},
		{name: "no state", state: "", wantErr: true, wantStatus: SnapshotStatusCreating},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newLifecycleHarness(t, false)
			h.snaps.states = map[string]string{"snap-0001": tc.state}
			seedInstance(t, h.svc, availableRecord())

			_, err := h.svc.DeleteDBInstance(t.Context(), &rds.DeleteDBInstanceInput{
				DBInstanceIdentifier: aws.String(testDBID), FinalDBSnapshotIdentifier: aws.String(pendingFinalSnapshotID),
			}, testAccountID)

			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			final, found := h.snapshotRecord(t, pendingFinalSnapshotID)
			require.True(t, found)
			assert.Equal(t, tc.wantStatus, final.Status)
			assert.Equal(t, StatusDeleting, h.record(t).Status)
			assert.Empty(t, h.volumes.deleted)

			if tc.wantStatus == SnapshotStatusCreating {
				rec := h.record(t)
				h.rewriteSnapshotRecord(t, pendingFinalSnapshotID, func(r *DBSnapshotRecord) {
					r.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
				})
				wait, err := h.svc.finalSnapshotWait(t.Context(), mustBucket(t, h), &rec)
				require.NoError(t, err)
				assert.GreaterOrEqual(t, wait, 2*time.Hour, "an unsettled final snapshot excludes the time since it was reserved")
			}
		})
	}
}

func mustBucket(t *testing.T, h *lifecycleHarness) *kvstore.Bucket {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	return kv
}
