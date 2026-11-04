package vm

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withVolumes(v *VM, names ...string) *VM {
	for _, name := range names {
		v.EBSRequests.Requests = append(v.EBSRequests.Requests, viperblocklegacyv1.EBSRequest{Name: name})
	}
	return v
}

func TestVolumesLeftPublished(t *testing.T) {
	volumes := []string{"vol-a", "vol-b", "vol-c"}

	assert.Nil(t, volumesLeftPublished(nil, volumes), "a completed unmount leaves nothing exported")

	named := fmt.Errorf("unmount i-1: %w", errors.Join(
		&VolumeUnpublishError{VolumeID: "vol-b", Err: errors.New("seal stalled")},
		fmt.Errorf("wrapped: %w", &VolumeUnpublishError{VolumeID: "vol-c", Err: errors.New("no responders")}),
	))
	assert.Equal(t, []string{"vol-b", "vol-c"}, volumesLeftPublished(named, volumes),
		"only the volumes the mounter names are left exported")

	assert.Equal(t, volumes, volumesLeftPublished(errors.New("mounter unreachable"), volumes),
		"an error naming no volume leaves every volume's outcome unknown")
}

func TestVolumeUnpublishError_UnwrapsItsCause(t *testing.T) {
	cause := errors.New("seal stalled")
	err := &VolumeUnpublishError{VolumeID: "vol-a", Err: cause}
	require.ErrorIs(t, err, cause)
	assert.Equal(t, "seal vol-a: seal stalled", err.Error())
}

// Every unmount replaces the record, so a later unmount that completes clears
// what an earlier failed one left.
func TestShutdownAndUnmount_RecordsTheVolumesTheUnmountLeftExported(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	mounter := &fakeVolumeMounter{unmountErr: errors.Join(
		&VolumeUnpublishError{VolumeID: "vol-data", Err: errSealStalled})}
	m := NewManagerWithDeps(Deps{VolumeMounter: mounter})
	instance := withVolumes(&VM{ID: "i-owed"}, "vol-root", "vol-data")

	require.ErrorIs(t, m.shutdownAndUnmount(instance), ErrVolumeSealFailed)
	assert.Equal(t, []string{"vol-data"}, instance.UnpublishOwed)

	mounter.unmountErr = nil
	require.NoError(t, m.shutdownAndUnmount(instance))
	assert.Empty(t, instance.UnpublishOwed)
}

// The force-complete path kills QEMU without an unmount, so the cleaner that
// clears the attachments must see every volume as still exported.
func TestForceFinalizeStuckTerminate_OwesAnUnpublishForEveryVolume(t *testing.T) {
	hostprocess.ShortenProcessExitPollForTest(t, 5*time.Millisecond)
	cleaner := &recordingInstanceCleaner{}
	reaper, store := newStuckTerminateReaper(t, cleaner)
	const id = "i-wedged-owed"
	reaper.m.InsertIfAbsent(withVolumes(&VM{
		ID:             id,
		Status:         StateShuttingDown,
		ShuttingDownAt: time.Now().Add(-(stuckTerminateTimeout + time.Minute)),
	}, "vol-root", "vol-data"))

	_, err := reaper.Sweep(context.Background())
	require.NoError(t, err)

	require.Len(t, cleaner.owedAtDelete, 1)
	assert.Equal(t, []string{"vol-root", "vol-data"}, cleaner.owedAtDelete[0])
	assert.Equal(t, []string{"vol-root", "vol-data"}, store.terminated[id].UnpublishOwed,
		"the terminated record carries it, so a retried teardown still records the obligation")
}

func TestReconcileVanishedQEMU_OwesAnUnpublishForEveryVolume(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	store := newFakeStateStore()
	m := NewManager()
	m.SetDeps(Deps{NodeID: "test-node", StateStore: store})
	instance := withVolumes(&VM{ID: "i-vanished-owed", Status: StateShuttingDown}, "vol-root")
	m.InsertIfAbsent(instance)

	require.NoError(t, m.reconcileVanishedQEMU(instance))

	assert.Equal(t, []string{"vol-root"}, store.terminated["i-vanished-owed"].UnpublishOwed)
}

// stalledMounter's Unmount holds the instance's requests until released, as a
// seal stalled in the provider does.
type stalledMounter struct {
	fakeVolumeMounter

	unmounting chan struct{}
	release    chan struct{}
}

func (s *stalledMounter) Unmount(_ context.Context, v *VM) error {
	v.EBSRequests.Mu.Lock()
	defer v.EBSRequests.Mu.Unlock()
	close(s.unmounting)
	<-s.release
	return nil
}

// Recovery from a wedged terminate never waits on the stalled unmount, and the
// record it leaves still owes an unpublish for every volume.
func TestReconcileVanishedQEMU_DoesNotWaitOnAStalledUnmount(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	store := newFakeStateStore()
	mounter := &stalledMounter{unmounting: make(chan struct{}), release: make(chan struct{})}
	m := NewManager()
	m.SetDeps(Deps{NodeID: "test-node", StateStore: store, VolumeMounter: mounter})
	instance := withVolumes(&VM{ID: "i-stalled-owed", Status: StateShuttingDown}, "vol-root", "vol-data")
	m.InsertIfAbsent(instance)

	unmounted := make(chan error, 1)
	go func() { unmounted <- m.unmountVolumes(context.Background(), instance) }()
	<-mounter.unmounting

	reconciled := make(chan error, 1)
	go func() { reconciled <- m.reconcileVanishedQEMU(instance) }()
	select {
	case err := <-reconciled:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("recovery waited on the stalled unmount")
	}
	assert.Equal(t, []string{"vol-root", "vol-data"}, store.terminated["i-stalled-owed"].UnpublishOwed)

	close(mounter.release)
	require.NoError(t, <-unmounted)
}
