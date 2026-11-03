//test:in-package — the reaper and the terminator adapter are unexported daemon types
package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const uefiTestBucket = "spinifex"

type uefiReaperFixture struct {
	m       *JetStreamManager
	objects *objectstore.MemoryObjectStore
	r       *uefiStoreReaper
}

func newUEFIReaperFixture(t *testing.T) *uefiReaperFixture {
	t.Helper()
	m := newRecordManagerInPackage(t)
	require.NoError(t, m.InitUEFIStoreBucket())
	objects := objectstore.NewMemoryObjectStore()
	return &uefiReaperFixture{
		m:       m,
		objects: objects,
		r:       &uefiStoreReaper{stores: m.UEFIStores(), objects: objects, bucket: uefiTestBucket, instances: m},
	}
}

// seedStore writes a live store record and n objects under its prefix, the
// state a launch leaves, then marks it deleting if asked, as terminate does.
func (f *uefiReaperFixture) seedStore(t *testing.T, id string, n int, deleting bool) {
	t.Helper()
	_, err := f.m.UEFIStores().Create(t.Context(), &uefistore.UEFIStoreRecord{InstanceID: id, AccountID: "111122223333", State: uefistore.StateLive})
	require.NoError(t, err)
	for i := range n {
		_, err := f.objects.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String(uefiTestBucket),
			Key:    aws.String(uefistore.KeyPrefix(id) + string(rune('a'+i))),
			Body:   bytes.NewReader([]byte("vars")),
		})
		require.NoError(t, err)
	}
	if deleting {
		_, err := f.m.UEFIStores().MarkDeleting(t.Context(), id)
		require.NoError(t, err)
	}
}

func (f *uefiReaperFixture) writeInstance(t *testing.T, id string, status vm.InstanceState) {
	t.Helper()
	v := &vm.VM{ID: id, Status: status, AccountID: "111122223333", Instance: &ec2.Instance{InstanceId: aws.String(id)}}
	require.NoError(t, f.m.WriteInstanceRecord(id, v.Record()))
}

func (f *uefiReaperFixture) writeTerminated(t *testing.T, id string, uefi vm.TeardownState) {
	t.Helper()
	v := &vm.VM{ID: id, Status: vm.StateTerminated, AccountID: "111122223333", Instance: &ec2.Instance{InstanceId: aws.String(id)},
		Teardown: map[string]string{vm.TeardownUEFI: string(uefi)}}
	require.NoError(t, f.m.WriteTerminatedInstance(id, v))
}

func (f *uefiReaperFixture) objectCount(t *testing.T, id string) int {
	t.Helper()
	keys, err := uefistore.ListObjects(t.Context(), f.objects, uefiTestBucket, id)
	require.NoError(t, err)
	return len(keys)
}

func (f *uefiReaperFixture) recordState(t *testing.T, id string) string {
	t.Helper()
	rec, _, err := f.m.UEFIStores().Get(t.Context(), id)
	if errors.Is(err, kvstore.ErrNotFound) {
		return "absent"
	}
	require.NoError(t, err)
	return rec.State
}

func (f *uefiReaperFixture) terminatedUEFIMark(t *testing.T, id string) string {
	t.Helper()
	v, err := f.m.LoadTerminatedInstance(id)
	require.NoError(t, err)
	require.NotNil(t, v)
	return v.Teardown[vm.TeardownUEFI]
}

// TestS7_UEFIStoreReaperDeletesOnlyAStoreATerminateMarkedDeleting enforces
// ADR-0003:S7 for the store reaper: it deletes the objects and record of a
// store a terminate set deleting, and never a live store, whether its
// instance runs, is stopped, or has no record the reaper could judge it by.
func TestS7_UEFIStoreReaperDeletesOnlyAStoreATerminateMarkedDeleting(t *testing.T) {
	f := newUEFIReaperFixture(t)
	const (
		running     = "i-0000000000000running"
		stopped     = "i-0000000000000stopped"
		unowned     = "i-0000000000000unowned"
		terminated  = "i-000000000terminated"
		terminating = "i-00000000terminating"
	)
	f.writeInstance(t, running, vm.StateRunning)
	f.seedStore(t, running, 3, false)
	f.writeInstance(t, stopped, vm.StateStopped)
	f.seedStore(t, stopped, 3, false)
	f.seedStore(t, unowned, 3, false)
	f.writeTerminated(t, terminated, vm.TeardownPending)
	f.seedStore(t, terminated, 3, true)
	f.writeInstance(t, terminating, vm.StateShuttingDown)
	f.seedStore(t, terminating, 2, true)

	reaped, err := f.r.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, reaped)

	for _, id := range []string{running, stopped, unowned} {
		assert.Equalf(t, 3, f.objectCount(t, id), "ADR-0003:S7: the reaper deleted objects of the live store %s", id)
		assert.Equalf(t, uefistore.StateLive, f.recordState(t, id), "ADR-0003:S7: the reaper changed the live store record %s", id)
	}
	for _, id := range []string{terminated, terminating} {
		assert.Zerof(t, f.objectCount(t, id), "a store a terminate marked deleting must be deleted: %s", id)
		assert.Equalf(t, "absent", f.recordState(t, id), "the record goes once its prefix is gone: %s", id)
	}
	assert.Equal(t, string(vm.TeardownDone), f.terminatedUEFIMark(t, terminated))
}

// TestS7_UEFIStoreReaperRefusesADeletingStoreOfALiveInstance enforces
// ADR-0003:S7 when the record and the instance disagree: a deleting record
// whose instance still runs or is stopped is kept, objects and all.
func TestS7_UEFIStoreReaperRefusesADeletingStoreOfALiveInstance(t *testing.T) {
	for _, status := range []vm.InstanceState{vm.StateRunning, vm.StateStopped, vm.StatePending} {
		t.Run(string(status), func(t *testing.T) {
			f := newUEFIReaperFixture(t)
			const id = "i-0000000000000000a"
			f.writeInstance(t, id, status)
			f.seedStore(t, id, 3, true)

			reaped, err := f.r.Sweep(t.Context())
			require.NoError(t, err)
			assert.Zero(t, reaped)
			assert.Equal(t, 3, f.objectCount(t, id), "ADR-0003:S7: a live instance's store must survive a deleting record")
			assert.Equal(t, uefistore.StateDeleting, f.recordState(t, id))
		})
	}
}

// failingObjects stands in for predastore while it is down: every list and
// delete fails.
type failingObjects struct {
	objectstore.ObjectStore
}

func (failingObjects) ListObjectsV2(context.Context, *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	return nil, errors.New("predastore: connection refused")
}

func (failingObjects) DeleteObject(context.Context, *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	return nil, errors.New("predastore: connection refused")
}

// The obligation lives in the store record, which has no TTL, so a terminate
// still completes when predastore is down for longer than the terminated
// record lives.
func TestUEFIStoreReaper_CompletesAfterTheTerminatedRecordExpires(t *testing.T) {
	f := newUEFIReaperFixture(t)
	const id = "i-0000000000000000a"
	f.writeTerminated(t, id, vm.TeardownPending)
	f.seedStore(t, id, 3, true)

	f.r.objects = failingObjects{f.objects}
	reaped, err := f.r.Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, reaped)
	assert.Equal(t, uefistore.StateDeleting, f.recordState(t, id), "a failed delete keeps the obligation")
	assert.Equal(t, string(vm.TeardownPending), f.terminatedUEFIMark(t, id))

	// The bucket's TTL expires the terminated record while predastore is away.
	require.NoError(t, f.m.DeleteTerminatedInstance(id))
	f.r.objects = f.objects

	reaped, err = f.r.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)
	assert.Zero(t, f.objectCount(t, id))
	assert.Equal(t, "absent", f.recordState(t, id))
}

// A terminate whose mark failed left the store live and the terminated
// record's entry failed; the reaper completes that terminate.
func TestUEFIStoreReaper_CompletesATerminateWhoseMarkFailed(t *testing.T) {
	f := newUEFIReaperFixture(t)
	const id = "i-0000000000000000a"
	f.seedStore(t, id, 3, false)
	f.writeTerminated(t, id, vm.TeardownFailed)

	reaped, err := f.r.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)
	assert.Zero(t, f.objectCount(t, id))
	assert.Equal(t, "absent", f.recordState(t, id))
	assert.Equal(t, string(vm.TeardownDone), f.terminatedUEFIMark(t, id))
}

// A pending entry whose store record is already gone has nothing left to
// delete, so the entry is closed and the terminated record can be purged.
func TestUEFIStoreReaper_ClosesAPendingEntryWithNoStore(t *testing.T) {
	f := newUEFIReaperFixture(t)
	const id = "i-0000000000000000a"
	f.writeTerminated(t, id, vm.TeardownPending)

	_, err := f.r.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, string(vm.TeardownDone), f.terminatedUEFIMark(t, id))
}

func TestUEFIStoreTerminator_MarksTheRecordAndRemovesOnlyTheInstanceDirectory(t *testing.T) {
	f := newUEFIReaperFixture(t)
	const id = "i-0000000000000000a"
	f.seedStore(t, id, 1, false)
	dataDir := t.TempDir()
	mine := uefiLocalDir(dataDir, id)
	other := uefiLocalDir(dataDir, "i-0000000000000000b")
	for _, dir := range []string{mine, other} {
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "vars.fd"), []byte("vars"), 0o600))
	}
	term := &uefiStoreTerminator{stores: f.m.UEFIStores(), dataDir: dataDir}

	has, err := term.MarkUEFIStoreDeleting(id)
	require.NoError(t, err)
	assert.True(t, has)
	assert.Equal(t, uefistore.StateDeleting, f.recordState(t, id))

	require.NoError(t, term.RemoveLocalUEFIStore(id))
	assert.NoDirExists(t, mine)
	assert.DirExists(t, other)
	require.NoError(t, term.RemoveLocalUEFIStore(id), "removal is idempotent")

	for _, bad := range []string{"", "..", "i-0/../..", "i-0000000000000000b/vars.fd", "vol-0123"} {
		require.Errorf(t, term.RemoveLocalUEFIStore(bad), "%q", bad)
	}
	assert.FileExists(t, filepath.Join(other, "vars.fd"))
}
