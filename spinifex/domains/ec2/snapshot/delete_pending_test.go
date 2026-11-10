package snapshot

import (
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fastClaims makes svc's writers retry a held claim without the real wait.
func fastClaims(svc *SnapshotServiceImpl) *SnapshotServiceImpl {
	svc.claimRetryDelay = time.Millisecond
	return svc
}

func (f *pendingFixture) deleteSnapshot(svc *SnapshotServiceImpl, id string) error {
	_, err := svc.DeleteSnapshot(f.t.Context(), &ec2.DeleteSnapshotInput{SnapshotId: aws.String(id)}, testAccountID)
	return err
}

// seedDoc writes a snapshot document of pendingVolumeID and its reference.
func (f *pendingFixture) seedDoc(svc *SnapshotServiceImpl, doc ebsmetadata.Snapshot) {
	f.t.Helper()
	doc.VolumeID, doc.OwnerID, doc.VolumeSize = pendingVolumeID, testAccountID, 4
	require.NoError(f.t, f.metadata.PutSnapshot(f.t.Context(), doc))
	require.NoError(f.t, svc.addSnapshotRef(f.t.Context(), pendingVolumeID, doc.SnapshotID))
}

// assertHidden requires that every request answers id as a deleted snapshot.
func (f *pendingFixture) assertHidden(svc *SnapshotServiceImpl, id string) {
	f.t.Helper()
	listed, err := svc.DescribeSnapshots(f.t.Context(), &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(f.t, err)
	for _, snap := range listed.Snapshots {
		assert.NotEqual(f.t, id, aws.StringValue(snap.SnapshotId), "DescribeSnapshots lists a deleted snapshot")
	}
	_, err = svc.DescribeSnapshots(f.t.Context(), &ec2.DescribeSnapshotsInput{SnapshotIds: aws.StringSlice([]string{id})}, testAccountID)
	assert.EqualError(f.t, err, awserrors.ErrorInvalidSnapshotNotFound)
	_, err = svc.DescribeSnapshotsStrict(f.t.Context(), &ec2.DescribeSnapshotsInput{SnapshotIds: aws.StringSlice([]string{id})}, testAccountID)
	assert.EqualError(f.t, err, awserrors.ErrorInvalidSnapshotNotFound)
	_, err = svc.CopySnapshot(f.t.Context(), &ec2.CopySnapshotInput{SourceSnapshotId: aws.String(id)}, testAccountID)
	assert.EqualError(f.t, err, awserrors.ErrorInvalidSnapshotNotFound)
	assert.EqualError(f.t, f.deleteSnapshot(svc, id), awserrors.ErrorInvalidSnapshotNotFound, "a repeated delete")
}

// DeleteSnapshot marks a pending or AmbiguousCreate snapshot and returns,
// leaving the provider alone; any other is deleted as it always was.
func TestDeleteSnapshot_MarksWhatTheProviderMayStillHold(t *testing.T) {
	const id = "snap-markdelete0001"
	cases := []struct {
		name       string
		doc        ebsmetadata.Snapshot
		wantMarked bool
	}{
		{name: "pending", doc: ebsmetadata.Snapshot{State: snapshotStatePending}, wantMarked: true},
		{name: "pending with an unanswered create", doc: ebsmetadata.Snapshot{State: snapshotStatePending, AmbiguousCreate: true}, wantMarked: true},
		{name: "completed with an unanswered create", doc: ebsmetadata.Snapshot{State: snapshotStateCompleted, AmbiguousCreate: true}, wantMarked: true},
		{name: "errored with an unanswered create", doc: ebsmetadata.Snapshot{State: snapshotStateError, AmbiguousCreate: true}, wantMarked: true},
		{name: "completed", doc: ebsmetadata.Snapshot{State: snapshotStateCompleted}},
		{name: "errored", doc: ebsmetadata.Snapshot{State: snapshotStateError}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			svc := f.node(nodeA)
			tc.doc.SnapshotID = id
			f.seedDoc(svc, tc.doc)
			providerSnapshot(f, id)

			require.NoError(t, f.deleteSnapshot(svc, id))

			doc, ok := f.doc(id)
			if !tc.wantMarked {
				assert.False(t, ok, "a settled snapshot is deleted as it always was")
				assert.Equal(t, []string{"snapshot " + id}, f.provider.deleteCalls())
				assert.Empty(t, f.refsOf(pendingVolumeID))
				return
			}
			require.True(t, ok)
			assert.True(t, doc.DeleteRequested)
			assert.Equal(t, tc.doc.State, doc.State, "the mark changes nothing else")
			assert.Empty(t, f.provider.deleteCalls(), "the provider is not asked while the snapshot may be in progress")
			_, held := f.providerHolds(id)
			assert.True(t, held)
			assert.Equal(t, []string{id}, f.refsOf(pendingVolumeID), "the reference stays for the reaper")
			assert.Empty(t, f.claimHolder(id), "the claim is released once the mark is written")
			f.assertHidden(svc, id)
		})
	}
}

// The InUse checks run first, unchanged, on a pending snapshot too.
func TestDeleteSnapshot_PendingInUseIsStillRefused(t *testing.T) {
	const id = "snap-markinuse00001"
	cases := []struct {
		name string
		seed func(f *pendingFixture)
	}{
		{name: "a volume was created from it", seed: func(f *pendingFixture) {
			require.NoError(f.t, f.metadata.PutVolume(f.t.Context(), ebsmetadata.Volume{
				VolumeID: "vol-fromsnap000001", TenantID: testAccountID, SnapshotID: id, State: "available",
			}))
		}},
		{name: "a registered AMI names it", seed: func(f *pendingFixture) {
			require.NoError(f.t, f.metadata.PutAMI(f.t.Context(), ebsmetadata.AMI{ImageID: "ami-0aaaaaaaaaaaaaaa1", SnapshotID: id, State: "pending"}))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			svc := f.node(nodeA)
			f.seedDoc(svc, ebsmetadata.Snapshot{SnapshotID: id, State: snapshotStatePending})
			tc.seed(f)

			err := f.deleteSnapshot(svc, id)
			require.Error(t, err)
			assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInvalidSnapshotInUse), "got %v", err)
			doc, ok := f.doc(id)
			require.True(t, ok)
			assert.False(t, doc.DeleteRequested)
		})
	}
}

// A claim another live node holds is retried three times and then answered
// IncorrectState; one a stopped node holds is taken over.
func TestDeleteSnapshot_ClaimContention(t *testing.T) {
	const id = "snap-markheld000001"
	cases := []struct {
		name        string
		holder      string
		holderSeen  time.Time
		wantErr     string
		wantRefused int
	}{
		{name: "held by a live node", holder: nodeC, holderSeen: time.Now(), wantErr: awserrors.ErrorIncorrectState, wantRefused: claimRetries + 1},
		{name: "held by a node whose heartbeat stopped", holder: nodeC, holderSeen: time.Now().Add(-2 * clusterv1.LivenessWindow)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			svc := fastClaims(f.node(nodeA))
			refused := 0
			svc.claimRefused = func(string) { refused++ }
			f.seedDoc(svc, ebsmetadata.Snapshot{SnapshotID: id, State: snapshotStatePending})
			f.holdClaim(tc.holder, id)
			f.liveness.beat(tc.holder, tc.holderSeen)

			err := f.deleteSnapshot(svc, id)

			assert.Equal(t, tc.wantRefused, refused)
			doc, ok := f.doc(id)
			require.True(t, ok)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.True(t, awserrors.IsErrorCode(err, tc.wantErr), "got %v", err)
				assert.False(t, doc.DeleteRequested)
				assert.Equal(t, tc.holder, f.claimHolder(id))
				return
			}
			require.NoError(t, err)
			assert.True(t, doc.DeleteRequested)
			assert.Empty(t, f.claimHolder(id))
		})
	}
}

// Each row of the marked column: the reaper deletes a marked snapshot once
// the provider settles it, never repeats one the provider does not hold, and
// keeps a tombstone in place of a document that had an unanswered create.
func TestPendingSnapshotReaper_MarkedColumn(t *testing.T) {
	const id = "snap-markedrow00001"
	cases := []struct {
		name      string
		ambiguous bool
		setup     func(f *pendingFixture)
		// wantKept: the document stays marked, its reference and provider snapshot untouched.
		wantKept      bool
		wantTombstone bool
		wantDeletes   int // provider DeleteSnapshot calls
	}{
		{name: "provider pending", setup: func(f *pendingFixture) {
			f.mem.AnswerSnapshotsPending(true)
			providerSnapshot(f, id)
		}, wantKept: true},
		{name: "provider completed", setup: func(f *pendingFixture) { providerSnapshot(f, id) }, wantDeletes: 1},
		{name: "provider error", setup: func(f *pendingFixture) {
			f.mem.AnswerSnapshotsPending(true)
			providerSnapshot(f, id)
			require.NoError(f.t, f.mem.SettleSnapshot(id, ebsprovider.SnapshotStateError))
		}, wantDeletes: 1},
		{name: "provider completed, create unanswered", ambiguous: true, setup: func(f *pendingFixture) { providerSnapshot(f, id) },
			wantTombstone: true, wantDeletes: 1},
		{name: "provider delete answers not_found", setup: func(f *pendingFixture) {
			providerSnapshot(f, id)
			f.provider.onDelete = func(ebsprovider.DeleteSnapshotRequest) error { return ebsprovider.ErrNotFound }
		}, wantDeletes: 1},
		{name: "absent", wantDeletes: 0},
		{name: "absent, create unanswered", ambiguous: true, wantTombstone: true},
		{name: "absent, claim held by a live node", setup: func(f *pendingFixture) { f.holdClaim(nodeC, id) }, wantKept: true},
		{name: "provider read fails", setup: func(f *pendingFixture) {
			f.provider.onList = func(ebsprovider.ListSnapshotsRequest) (bool, *ebsprovider.ListSnapshotsResponse, error) {
				return true, nil, ebsprovider.ErrUnavailable
			}
		}, wantKept: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			reaper := f.node(nodeB)
			f.seedDoc(reaper, ebsmetadata.Snapshot{SnapshotID: id, State: snapshotStatePending, DeleteRequested: true, AmbiguousCreate: tc.ambiguous})
			if tc.setup != nil {
				tc.setup(f)
			}
			_, heldBefore := f.providerHolds(id)

			f.sweep(reaper)

			assert.Zero(t, f.provider.createCalls(), "a marked snapshot is never repeated")
			assert.Len(t, f.provider.deleteCalls(), tc.wantDeletes)
			doc, ok := f.doc(id)
			switch {
			case tc.wantKept:
				require.True(t, ok)
				assert.True(t, doc.DeleteRequested)
				assert.False(t, doc.Tombstoned())
				assert.Equal(t, []string{id}, f.refsOf(pendingVolumeID))
				_, held := f.providerHolds(id)
				assert.Equal(t, heldBefore, held)
			case tc.wantTombstone:
				require.True(t, ok, "a document that had an unanswered create is kept as a tombstone")
				assert.True(t, doc.Tombstoned())
				assert.Empty(t, f.refsOf(pendingVolumeID))
				f.assertHidden(f.node(nodeA), id)
			default:
				assert.False(t, ok)
				assert.Empty(t, f.refsOf(pendingVolumeID))
			}
			if !tc.wantKept && tc.wantDeletes > 0 && f.provider.onDelete == nil {
				_, held := f.providerHolds(id)
				assert.False(t, held)
			}
		})
	}
}

// A failure or interruption between the provider delete, the reference's
// removal and the document's removal leaves the marked document for the next
// sweep, which finishes; no reference is ever left without its document.
func TestPendingSnapshotReaper_CleanupInterrupted(t *testing.T) {
	const id = "snap-cleanupstep001"
	cases := []struct {
		name      string
		ambiguous bool
		break_    func(f *pendingFixture, reaper *SnapshotServiceImpl)
		heal      func(f *pendingFixture, reaper *SnapshotServiceImpl)
		wantRef   bool // the reference is still there after the broken sweep
	}{
		{name: "the provider delete fails", break_: func(f *pendingFixture, _ *SnapshotServiceImpl) {
			f.provider.onDelete = func(ebsprovider.DeleteSnapshotRequest) error { return ebsprovider.ErrUnavailable }
		}, heal: func(f *pendingFixture, _ *SnapshotServiceImpl) { f.provider.onDelete = nil }, wantRef: true},
		{name: "interrupted after the provider delete", break_: func(_ *pendingFixture, r *SnapshotServiceImpl) {
			r.reapAt = func(step reapStep, _ string) bool { return step == reapProviderDeleted }
		}, heal: func(_ *pendingFixture, r *SnapshotServiceImpl) { r.reapAt = nil }, wantRef: true},
		{name: "the reference removal fails", break_: func(f *pendingFixture, r *SnapshotServiceImpl) {
			r.snapRefs = f.unreachableRefs()
		}, heal: func(f *pendingFixture, r *SnapshotServiceImpl) { r.snapRefs = f.node(nodeB).snapRefs }, wantRef: true},
		{name: "interrupted after the reference removal", break_: func(_ *pendingFixture, r *SnapshotServiceImpl) {
			r.reapAt = func(step reapStep, _ string) bool { return step == reapRefRemoved }
		}, heal: func(_ *pendingFixture, r *SnapshotServiceImpl) { r.reapAt = nil }},
		{name: "the document removal fails", break_: func(f *pendingFixture, _ *SnapshotServiceImpl) { f.store.failDelete = true },
			heal: func(f *pendingFixture, _ *SnapshotServiceImpl) { f.store.failDelete = false }},
		{name: "the tombstone write fails", ambiguous: true, break_: func(f *pendingFixture, _ *SnapshotServiceImpl) { f.store.putsBefore = 0 },
			heal: func(f *pendingFixture, _ *SnapshotServiceImpl) { f.store.putsBefore = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			reaper := f.node(nodeB)
			f.seedDoc(reaper, ebsmetadata.Snapshot{SnapshotID: id, State: snapshotStatePending, DeleteRequested: true, AmbiguousCreate: tc.ambiguous})
			providerSnapshot(f, id)

			tc.break_(f, reaper)
			f.sweep(reaper)

			doc, ok := f.doc(id)
			require.True(t, ok, "the document is the record the next sweep finds the work from")
			assert.True(t, doc.DeleteRequested)
			assert.False(t, doc.Tombstoned())
			if tc.wantRef {
				assert.Equal(t, []string{id}, f.refsOf(pendingVolumeID))
			} else {
				assert.Empty(t, f.refsOf(pendingVolumeID))
			}

			tc.heal(f, reaper)
			f.sweep(reaper)

			_, held := f.providerHolds(id)
			assert.False(t, held)
			assert.Empty(t, f.refsOf(pendingVolumeID))
			doc, ok = f.doc(id)
			if tc.ambiguous {
				require.True(t, ok)
				assert.True(t, doc.Tombstoned())
			} else {
				assert.False(t, ok)
			}
		})
	}
}

// A DeleteSnapshot that lands while the reaper holds the claim, between its
// reread and its write, waits on the claim and ends with the snapshot deleted,
// whether it runs on another node or on the reaper's own.
func TestDeleteSnapshot_LandsBetweenTheReapersRereadAndWrite(t *testing.T) {
	const id = "snap-deleteraced001"
	for _, sameNode := range []bool{false, true} {
		name := "another node"
		if sameNode {
			name = "the reaper's node"
		}
		t.Run(name, func(t *testing.T) {
			f := newPendingFixture(t)
			reaper := fastClaims(f.node(nodeB))
			deleter := fastClaims(f.node(nodeA))
			if sameNode {
				deleter = reaper
			}
			f.seedDoc(reaper, ebsmetadata.Snapshot{SnapshotID: id, State: snapshotStatePending})
			providerSnapshot(f, id)

			refused := make(chan struct{}, claimRetries+1)
			deleter.claimRefused = func(string) { refused <- struct{}{} }
			var wg sync.WaitGroup
			var deleteErr error
			reaper.reapAt = func(step reapStep, _ string) bool {
				if step == reapReread {
					wg.Go(func() { deleteErr = f.deleteSnapshot(deleter, id) })
					<-refused
				}
				return false
			}
			deleter.claimRetryDelay = 50 * time.Millisecond

			f.sweep(reaper)
			wg.Wait()

			require.NoError(t, deleteErr, "the delete waits out the reaper's claim")
			_, ok := f.doc(id)
			assert.False(t, ok, "the delete is not undone by the reaper's settle")
			_, held := f.providerHolds(id)
			assert.False(t, held)
			assert.Empty(t, f.refsOf(pendingVolumeID))
		})
	}
}

// A request whose provider call returns after its claim was taken over and the
// snapshot marked leaves the document marked and pending, for the reaper.
func TestCreateSnapshot_AnswerAfterTheMarkIsNotWritten(t *testing.T) {
	f := newPendingFixture(t)
	svc := f.node(nodeA)
	f.liveness.beat(nodeA, time.Now().Add(-2*clusterv1.LivenessWindow))
	deleter := fastClaims(f.node(nodeB))
	f.provider.onCreate = func(n int, req ebsprovider.CreateSnapshotRequest) (bool, *ebsprovider.Snapshot, error) {
		if n == 1 {
			require.NoError(t, f.deleteSnapshot(deleter, req.SnapshotID))
		}
		return false, nil, nil
	}

	_, err := f.create(svc)
	require.NoError(t, err, "the provider answered, so the request succeeds")

	doc := f.onlyDoc()
	assert.True(t, doc.DeleteRequested)
	assert.Equal(t, snapshotStatePending, doc.State, "a marked document is never settled by the request")
	assert.True(t, doc.AmbiguousCreate)

	f.sweep(f.node(nodeC))
	assert.True(t, f.onlyDoc().Tombstoned())
	_, held := f.providerHolds(doc.SnapshotID)
	assert.False(t, held)
}

// AmbiguousCreate is on the document whenever the provider is called, and is
// cleared only by a definite answer to a document with no earlier call left
// unanswered.
func TestAmbiguousCreate_SetBeforeEveryCall(t *testing.T) {
	const reaped = "snap-ambiguousreap1"
	cases := []struct {
		name string
		// reaper seeds a pending document with the flag clear and sweeps it,
		// instead of a CreateSnapshot.
		reaper  bool
		pending bool              // the memory provider answers pending
		onCall  func(n int) error // nil error: the memory provider answers
		crash   createStep
		want    bool
		wantErr bool
	}{
		{name: "request answered completed", want: false},
		{name: "request answered pending", pending: true, want: false},
		{name: "request first call unanswered, repeat completed", onCall: firstFails(errAmbiguous), want: true},
		{name: "request crashed after sending", crash: stepAnswered, want: true, wantErr: true},
		{name: "request repeat refused for good", onCall: func(n int) error {
			if n == 1 {
				return errAmbiguous
			}
			return ebsprovider.ErrAlreadyExists
		}, want: true, wantErr: true},
		{name: "reaper repeat answered", reaper: true, want: false},
		{name: "reaper repeat unanswered", reaper: true, onCall: firstFails(errAmbiguous), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			svc := f.node(nodeA)
			f.mem.AnswerSnapshotsPending(tc.pending)
			var flagged []bool
			f.provider.onCreate = func(n int, req ebsprovider.CreateSnapshotRequest) (bool, *ebsprovider.Snapshot, error) {
				doc, _ := f.doc(req.SnapshotID)
				flagged = append(flagged, doc.AmbiguousCreate)
				if tc.onCall != nil {
					if err := tc.onCall(n); err != nil {
						return true, nil, err
					}
				}
				return false, nil, nil
			}
			if tc.crash != 0 {
				svc.crashAt = func(step createStep) bool { return step == tc.crash }
			}

			var id string
			if tc.reaper {
				id = reaped
				f.seedDoc(svc, ebsmetadata.Snapshot{SnapshotID: id, State: snapshotStatePending})
				f.sweep(f.node(nodeB))
			} else {
				_, err := f.create(svc)
				assert.Equal(t, tc.wantErr, err != nil, "err %v", err)
				id = f.onlyDoc().SnapshotID
			}

			require.NotEmpty(t, flagged)
			for i, set := range flagged {
				assert.True(t, set, "call %d was sent with AmbiguousCreate clear", i+1)
			}
			doc, ok := f.doc(id)
			require.True(t, ok)
			assert.Equal(t, tc.want, doc.AmbiguousCreate)
		})
	}
}

// firstFails answers the first call with err and lets the rest through.
func firstFails(err error) func(int) error {
	return func(n int) error {
		if n == 1 {
			return err
		}
		return nil
	}
}

// The original call of a snapshot that a repeat completed and the customer
// then deleted is accepted late: the snapshot it recreates is deleted under
// the tombstone once it settles, and the tombstone is dropped after 7 days.
func TestTombstone_DeletesALateAcceptedSnapshotAndExpires(t *testing.T) {
	f := newPendingFixture(t)
	svc := f.node(nodeA)
	reaper := f.node(nodeB)
	now := time.Now()
	reaper.now = func() time.Time { return now }
	var late ebsprovider.CreateSnapshotRequest
	f.provider.onCreate = func(n int, req ebsprovider.CreateSnapshotRequest) (bool, *ebsprovider.Snapshot, error) {
		if n <= 2 {
			late = req
			return true, nil, errAmbiguous
		}
		return false, nil, nil
	}
	_, err := f.create(svc)
	require.EqualError(t, err, awserrors.ErrorServerInternal)
	id := f.onlyDoc().SnapshotID

	// The reaper's repeat completes it, and the customer deletes it.
	f.sweep(reaper)
	doc := f.onlyDoc()
	require.Equal(t, snapshotStateCompleted, doc.State)
	require.True(t, doc.AmbiguousCreate, "the request's calls were never answered")
	require.NoError(t, f.deleteSnapshot(svc, id))
	f.sweep(reaper)
	require.True(t, f.onlyDoc().Tombstoned())
	_, held := f.providerHolds(id)
	require.False(t, held)

	// The request's original call is accepted now, and is still in progress.
	f.mem.AnswerSnapshotsPending(true)
	_, err = f.mem.CreateSnapshot(t.Context(), late)
	require.NoError(t, err)
	f.sweep(reaper)
	_, held = f.providerHolds(id)
	assert.True(t, held, "a snapshot still pending is not deleted")
	require.NoError(t, f.mem.SettleSnapshot(id, ebsprovider.SnapshotStateCompleted))
	f.sweep(reaper)
	_, held = f.providerHolds(id)
	assert.False(t, held, "the late snapshot is deleted under the tombstone")
	assert.True(t, f.onlyDoc().Tombstoned())
	f.assertHidden(svc, id)
	assert.Equal(t, 3, f.provider.createCalls(), "the request's two calls and one repeat; nothing repeats a tombstone's create")

	now = now.Add(tombstoneRetention - time.Minute)
	f.sweep(reaper)
	assert.Len(t, f.docs(), 1, "the tombstone is kept for its retention")
	now = now.Add(2 * time.Minute)
	f.sweep(reaper)
	assert.Empty(t, f.docs(), "the tombstone is dropped after its retention")
	assert.Empty(t, f.refsOf(pendingVolumeID))
}

// The AMI pass settles each pending AMI from its snapshot's document.
func TestPendingSnapshotReaper_SettlesPendingAMIs(t *testing.T) {
	const snapID = "snap-amisettle00001"
	cases := []struct {
		name     string
		amiState string
		snap     *ebsmetadata.Snapshot // nil: no snapshot document
		want     string
	}{
		{name: "snapshot completed", amiState: amiStatePending, snap: &ebsmetadata.Snapshot{State: snapshotStateCompleted}, want: amiStateAvailable},
		{name: "snapshot errored", amiState: amiStatePending, snap: &ebsmetadata.Snapshot{State: snapshotStateError}, want: amiStateFailed},
		{name: "snapshot document missing", amiState: amiStatePending, want: amiStateFailed},
		{name: "snapshot pending", amiState: amiStatePending, snap: &ebsmetadata.Snapshot{State: snapshotStatePending}, want: amiStatePending},
		{name: "AMI already available", amiState: amiStateAvailable, want: amiStateAvailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			if tc.snap != nil {
				tc.snap.SnapshotID = snapID
				f.seedDoc(f.node(nodeA), *tc.snap)
				if tc.snap.State == snapshotStatePending {
					f.mem.AnswerSnapshotsPending(true)
					providerSnapshot(f, snapID)
				}
			}
			const amiID = "ami-0bbbbbbbbbbbbbbb1"
			require.NoError(t, f.metadata.PutAMI(t.Context(), ebsmetadata.AMI{
				ImageID: amiID, SnapshotID: snapID, ImageOwnerAlias: testAccountID, State: tc.amiState,
			}))

			f.sweep(f.node(nodeB))

			ami, err := f.metadata.GetAMI(t.Context(), amiID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, ami.State)
		})
	}
}

func TestCopySnapshot_RefusesASourceThatIsNotCompleted(t *testing.T) {
	for _, state := range []string{snapshotStatePending, "error"} {
		t.Run(state, func(t *testing.T) {
			svc, _ := providerSnapshotService(t, "vol-copygate")
			require.NoError(t, svc.MetadataStore().PutSnapshot(t.Context(), ebsmetadata.Snapshot{
				SnapshotID: "snap-copygate00001", VolumeID: "vol-copygate", VolumeSize: 4, State: state, OwnerID: testAccountID,
			}))

			_, err := svc.CopySnapshot(t.Context(), &ec2.CopySnapshotInput{SourceSnapshotId: aws.String("snap-copygate00001")}, testAccountID)
			code, _, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok, "unregistered error: %v", err)
			assert.Equal(t, awserrors.ErrorIncorrectState, code)
			snaps, err := svc.MetadataStore().ListAllSnapshots(t.Context())
			require.NoError(t, err)
			assert.Len(t, snaps, 1, "a refused copy writes no snapshot document")
		})
	}
}
