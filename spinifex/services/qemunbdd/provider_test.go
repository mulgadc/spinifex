package qemunbdd

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// call records one invocation the fake runner captured, so tests can assert
// the exact argv a provider method built.
type call struct {
	name string
	args []string
}

// fakeRunner stands in for qemu-img/qemu-io/qemu-nbd. It mimics the side
// effect a real "create" or "convert" has (leaving a file behind) so
// existence checks with os.Stat behave the same as against a real qemu-img, without needing qemu installed.
type fakeRunner struct {
	mu    sync.Mutex
	calls []call

	// infoResponses maps a path to the canned `qemu-img info --output=json`
	// body returned when that path is queried.
	infoResponses map[string]string

	// failOn, when non-empty, makes Run return err for every call whose
	// command name matches.
	failOn string
	err    error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{infoResponses: make(map[string]string)}
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...)})
	f.mu.Unlock()

	if f.err != nil && name == f.failOn {
		return nil, f.err
	}

	if name == "qemu-img" && len(args) > 0 {
		switch args[0] {
		case "create":
			// The size argument is always last; the target path is the
			// element before it.
			_ = os.WriteFile(args[len(args)-2], nil, 0o640)
		case "convert":
			_ = os.WriteFile(args[len(args)-1], nil, 0o640)
		case "info":
			path := args[len(args)-1]
			f.mu.Lock()
			resp, ok := f.infoResponses[path]
			f.mu.Unlock()
			if ok {
				return []byte(resp), nil
			}
			return []byte(`{"virtual-size":0}`), nil
		}
	}
	return []byte("ok"), nil
}

func (f *fakeRunner) callsSnapshot() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func newTestProvider(t *testing.T) (*Provider, *fakeRunner) {
	t.Helper()
	fr := newFakeRunner()
	p, err := newProvider(t.TempDir(), fr)
	require.NoError(t, err)
	return p, fr
}

func TestNewProvider_CreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	_, err := newProvider(dir, newFakeRunner())
	require.NoError(t, err)
	for _, sub := range []string{"volumes", "snapshots", "sockets", "tmp"} {
		info, err := os.Stat(filepath.Join(dir, sub))
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	}
}

func TestNewProvider_RequiresBaseDir(t *testing.T) {
	_, err := newProvider("", newFakeRunner())
	require.Error(t, err)
}

func versioned() ebsprovider.Versioned { return ebsprovider.NewVersioned() }

func TestCreateVolume_DefaultArgv(t *testing.T) {
	p, fr := newTestProvider(t)
	ctx := context.Background()

	vol, err := p.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned:     versioned(),
		VolumeID:      "vol-1",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
	require.NotNil(t, vol)
	assert.Equal(t, "vol-1", vol.ID)
	assert.Equal(t, int64(1<<30), vol.CapacityBytes)
	assert.Equal(t, ebsprovider.VolumeStateAvailable, vol.State)
	assert.Equal(t, p.volumePath("vol-1"), vol.Handle)

	calls := fr.callsSnapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "qemu-img", calls[0].name)
	assert.Equal(t, []string{"create", "-f", "qcow2", p.volumePath("vol-1"), "1073741824"}, calls[0].args)
}

func TestCreateVolume_FromSnapshotArgv(t *testing.T) {
	p, fr := newTestProvider(t)
	ctx := context.Background()

	snapPath := p.snapshotPath("snap-1")
	require.NoError(t, os.WriteFile(snapPath, nil, 0o640))
	fr.infoResponses[snapPath] = `{"virtual-size":536870912}`

	vol, err := p.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned:              versioned(),
		VolumeID:               "vol-clone",
		CapacityRange:          ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
		SourceSnapshotID:       "snap-1",
		SourceSnapshotVolumeID: "vol-origin",
	})
	require.NoError(t, err)
	require.NotNil(t, vol)

	volPath := p.volumePath("vol-clone")
	calls := fr.callsSnapshot()
	require.Len(t, calls, 2)
	assert.Equal(t, []string{"info", "--output=json", "--force-share", snapPath}, calls[0].args)
	assert.Equal(t, []string{"create", "-f", "qcow2", "-b", snapPath, "-F", "qcow2", volPath, "1073741824"}, calls[1].args)
}

func TestCreateVolume_FromMissingSnapshotNotFound(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned:              versioned(),
		VolumeID:               "vol-orphan",
		CapacityRange:          ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
		SourceSnapshotID:       "snap-missing",
		SourceSnapshotVolumeID: "vol-origin",
	})
	require.ErrorIs(t, err, ebsprovider.ErrNotFound)
}

// TestCreateVolume_SnapshotSourceRequiresOriginVolume covers the contract rule
// that a snapshot source must name the volume it was taken from, so a provider
// needing it to resolve blocks gets it rather than guessing.
func TestCreateVolume_SnapshotSourceRequiresOriginVolume(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned:        versioned(),
		VolumeID:         "vol-noorigin",
		CapacityRange:    ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
		SourceSnapshotID: "snap-any",
	})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
}

func TestCreateVolume_IdempotentRepeatSameParameters(t *testing.T) {
	p, fr := newTestProvider(t)
	ctx := context.Background()
	req := ebsprovider.CreateVolumeRequest{
		Versioned:     versioned(),
		VolumeID:      "vol-idem",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	}

	first, err := p.CreateVolume(ctx, req)
	require.NoError(t, err)
	fr.infoResponses[p.volumePath("vol-idem")] = `{"virtual-size":1073741824}`

	second, err := p.CreateVolume(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, first, second)

	calls := fr.callsSnapshot()
	require.Len(t, calls, 2, "the repeat must only inspect the file, not recreate it")
	assert.Equal(t, "create", calls[0].args[0])
	assert.Equal(t, "info", calls[1].args[0])
}

func TestCreateVolume_ConflictingCapacityIsAlreadyExists(t *testing.T) {
	p, fr := newTestProvider(t)
	ctx := context.Background()
	req := ebsprovider.CreateVolumeRequest{
		Versioned:     versioned(),
		VolumeID:      "vol-conflict",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	}
	_, err := p.CreateVolume(ctx, req)
	require.NoError(t, err)
	fr.infoResponses[p.volumePath("vol-conflict")] = `{"virtual-size":1073741824}`

	req2 := req
	req2.CapacityRange.RequiredBytes = 2 << 30
	_, err = p.CreateVolume(ctx, req2)
	require.ErrorIs(t, err, ebsprovider.ErrAlreadyExists)
}

func TestCreateVolume_InvalidArgumentOnEmptyID(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned: versioned(), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
}

func TestCreateVolume_UnsupportedVersion(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		VolumeID: "vol-noversion", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.ErrorIs(t, err, ebsprovider.ErrUnsupportedVersion)
}

func TestCreateVolume_SeedDataWritesViaQemuIO(t *testing.T) {
	p, fr := newTestProvider(t)
	ctx := context.Background()
	seed := []byte("firmware-vars-template")

	_, err := p.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned:     versioned(),
		VolumeID:      "vol-seeded",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 4096},
		SeedData:      seed,
	})
	require.NoError(t, err)

	calls := fr.callsSnapshot()
	require.Len(t, calls, 2)
	assert.Equal(t, "qemu-img", calls[0].name)
	seedCall := calls[1]
	assert.Equal(t, "qemu-io", seedCall.name)
	require.Len(t, seedCall.args, 5)
	assert.Equal(t, []string{"-f", "qcow2"}, seedCall.args[0:2])
	assert.Equal(t, "-c", seedCall.args[2])
	assert.Contains(t, seedCall.args[3], "write -s ")
	assert.Contains(t, seedCall.args[3], " 0 22")
	assert.Equal(t, p.volumePath("vol-seeded"), seedCall.args[4])
}

func TestCreateVolume_SeedDataExceedsMax(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned:     versioned(),
		VolumeID:      "vol-seed-toobig",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
		SeedData:      make([]byte, ebsprovider.MaxSeedBytes+1),
	})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
}

func TestCreateVolume_SeedDataExceedsCapacity(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned:     versioned(),
		VolumeID:      "vol-seed-overcap",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 512},
		SeedData:      make([]byte, 4096),
	})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
}

func TestGetVolume_Success(t *testing.T) {
	p, fr := newTestProvider(t)
	volPath := p.volumePath("vol-get")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))
	fr.infoResponses[volPath] = `{"virtual-size":268435456}`

	vol, err := p.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{Versioned: versioned(), VolumeID: "vol-get"})
	require.NoError(t, err)
	assert.Equal(t, int64(268435456), vol.CapacityBytes)
	assert.Equal(t, ebsprovider.VolumeStateAvailable, vol.State)
}

func TestGetVolume_NotFound(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{Versioned: versioned(), VolumeID: "vol-never-existed"})
	require.ErrorIs(t, err, ebsprovider.ErrNotFound)
}

func TestGetVolume_InvalidArgumentOnEmptyID(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{Versioned: versioned()})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
}

func TestExpandVolume_Argv(t *testing.T) {
	p, fr := newTestProvider(t)
	volPath := p.volumePath("vol-expand")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))
	fr.infoResponses[volPath] = `{"virtual-size":1073741824}`

	vol, err := p.ExpandVolume(context.Background(), ebsprovider.ExpandVolumeRequest{
		Versioned: versioned(), VolumeID: "vol-expand", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 2 << 30},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2<<30), vol.CapacityBytes)

	calls := fr.callsSnapshot()
	require.Len(t, calls, 2)
	assert.Equal(t, []string{"resize", volPath, "2147483648"}, calls[1].args)
}

func TestExpandVolume_ShrinkIsInvalidArgument(t *testing.T) {
	p, fr := newTestProvider(t)
	volPath := p.volumePath("vol-shrink")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))
	fr.infoResponses[volPath] = `{"virtual-size":2147483648}`

	_, err := p.ExpandVolume(context.Background(), ebsprovider.ExpandVolumeRequest{
		Versioned: versioned(), VolumeID: "vol-shrink", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
}

func TestExpandVolume_PublishedRefusesOnlineExpansion(t *testing.T) {
	p, fr := newTestProvider(t)
	volPath := p.volumePath("vol-expand-inuse")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))
	fr.infoResponses[volPath] = `{"virtual-size":1073741824}`

	_, err := p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-expand-inuse", NodeID: "node-1"})
	require.NoError(t, err)

	_, err = p.ExpandVolume(context.Background(), ebsprovider.ExpandVolumeRequest{
		Versioned: versioned(), VolumeID: "vol-expand-inuse", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 2 << 30},
	})
	require.ErrorIs(t, err, ebsprovider.ErrVolumeInUse)
}

func TestExpandVolume_NotFound(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.ExpandVolume(context.Background(), ebsprovider.ExpandVolumeRequest{
		Versioned: versioned(), VolumeID: "vol-never-existed", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.ErrorIs(t, err, ebsprovider.ErrNotFound)
}

func TestDeleteVolume_Success(t *testing.T) {
	p, _ := newTestProvider(t)
	volPath := p.volumePath("vol-del")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))

	require.NoError(t, p.DeleteVolume(context.Background(), ebsprovider.DeleteVolumeRequest{Versioned: versioned(), VolumeID: "vol-del"}))
	_, err := os.Stat(volPath)
	require.True(t, os.IsNotExist(err))
}

func TestDeleteVolume_AbsentIsIdempotent(t *testing.T) {
	p, _ := newTestProvider(t)
	require.NoError(t, p.DeleteVolume(context.Background(), ebsprovider.DeleteVolumeRequest{Versioned: versioned(), VolumeID: "vol-never-existed"}))
	// A second delete of the same, already-absent target must also succeed.
	require.NoError(t, p.DeleteVolume(context.Background(), ebsprovider.DeleteVolumeRequest{Versioned: versioned(), VolumeID: "vol-never-existed"}))
}

func TestDeleteVolume_PublishedRefuses(t *testing.T) {
	p, fr := newTestProvider(t)
	volPath := p.volumePath("vol-del-inuse")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))
	fr.infoResponses[volPath] = `{"virtual-size":1073741824}`

	_, err := p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-del-inuse", NodeID: "node-1"})
	require.NoError(t, err)

	err = p.DeleteVolume(context.Background(), ebsprovider.DeleteVolumeRequest{Versioned: versioned(), VolumeID: "vol-del-inuse"})
	require.ErrorIs(t, err, ebsprovider.ErrVolumeInUse)
}

func TestCreateSnapshot_Argv(t *testing.T) {
	p, fr := newTestProvider(t)
	volPath := p.volumePath("vol-snap-src")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))

	snap, err := p.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-1", VolumeID: "vol-snap-src"})
	require.NoError(t, err)
	assert.Equal(t, ebsprovider.SnapshotStateCompleted, snap.State)
	assert.Equal(t, "vol-snap-src", snap.SourceVolumeID)

	calls := fr.callsSnapshot()
	snapPath := p.snapshotPath("snap-1")
	require.Len(t, calls, 2)
	assert.Equal(t, []string{"convert", "-O", "qcow2", volPath, snapPath}, calls[0].args)
	assert.Equal(t, []string{"info", "--output=json", "--force-share", snapPath}, calls[1].args)
}

func TestCreateSnapshot_NotFoundSourceVolume(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-orphan", VolumeID: "vol-never-existed"})
	require.ErrorIs(t, err, ebsprovider.ErrNotFound)
}

func TestCreateSnapshot_IdempotentSameVolume(t *testing.T) {
	p, _ := newTestProvider(t)
	volPath := p.volumePath("vol-snap-idem")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))

	first, err := p.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-idem", VolumeID: "vol-snap-idem"})
	require.NoError(t, err)
	second, err := p.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-idem", VolumeID: "vol-snap-idem"})
	require.NoError(t, err)
	assert.Equal(t, first.SourceVolumeID, second.SourceVolumeID)
}

func TestCreateSnapshot_ConflictingSourceVolumeIsAlreadyExists(t *testing.T) {
	p, _ := newTestProvider(t)
	volA := p.volumePath("vol-snap-a")
	volB := p.volumePath("vol-snap-b")
	require.NoError(t, os.WriteFile(volA, nil, 0o640))
	require.NoError(t, os.WriteFile(volB, nil, 0o640))

	_, err := p.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-conflict", VolumeID: "vol-snap-a"})
	require.NoError(t, err)
	_, err = p.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-conflict", VolumeID: "vol-snap-b"})
	require.ErrorIs(t, err, ebsprovider.ErrAlreadyExists)
}

func TestDeleteSnapshot_Success(t *testing.T) {
	p, _ := newTestProvider(t)
	volPath := p.volumePath("vol-delsnap-src")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))
	_, err := p.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-delete-ok", VolumeID: "vol-delsnap-src"})
	require.NoError(t, err)

	require.NoError(t, p.DeleteSnapshot(context.Background(), ebsprovider.DeleteSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-delete-ok"}))
	_, err = os.Stat(p.snapshotPath("snap-delete-ok"))
	require.True(t, os.IsNotExist(err))
}

func TestDeleteSnapshot_AbsentIsIdempotent(t *testing.T) {
	p, _ := newTestProvider(t)
	require.NoError(t, p.DeleteSnapshot(context.Background(), ebsprovider.DeleteSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-never-existed"}))
	require.NoError(t, p.DeleteSnapshot(context.Background(), ebsprovider.DeleteSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-never-existed"}))
}

func TestDeleteSnapshot_RefusedWhenBackingFileInUse(t *testing.T) {
	p, fr := newTestProvider(t)
	snapPath := p.snapshotPath("snap-backing")
	require.NoError(t, os.WriteFile(snapPath, nil, 0o640))
	// The snapshotInUse scan lists every file under volumes/ and checks its
	// reported backing-filename, so a bare marker file with a canned info
	// response is enough to simulate a CoW clone without a real qemu-img.
	clonePath := p.volumePath("vol-clone")
	require.NoError(t, os.WriteFile(clonePath, nil, 0o640))
	fr.infoResponses[clonePath] = `{"virtual-size":1073741824,"backing-filename":"` + snapPath + `"}`

	err := p.DeleteSnapshot(context.Background(), ebsprovider.DeleteSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-backing"})
	require.ErrorIs(t, err, ebsprovider.ErrVolumeInUse)
	_, statErr := os.Stat(snapPath)
	require.NoError(t, statErr, "a refused delete must leave the snapshot file in place")
}

func TestCopySnapshot_Success(t *testing.T) {
	p, _ := newTestProvider(t)
	volPath := p.volumePath("vol-copysnap-src")
	require.NoError(t, os.WriteFile(volPath, []byte("volume-bytes"), 0o640))
	_, err := p.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-copysnap-src", VolumeID: "vol-copysnap-src"})
	require.NoError(t, err)

	copied, err := p.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{
		Versioned: versioned(), SourceSnapshotID: "snap-copysnap-src", DestinationSnapshotID: "snap-copysnap-dst", VolumeID: "vol-copysnap-src",
	})
	require.NoError(t, err)
	assert.Equal(t, "snap-copysnap-dst", copied.ID)
	assert.Equal(t, "vol-copysnap-src", copied.SourceVolumeID)

	srcBytes, err := os.ReadFile(p.snapshotPath("snap-copysnap-src"))
	require.NoError(t, err)
	dstBytes, err := os.ReadFile(p.snapshotPath("snap-copysnap-dst"))
	require.NoError(t, err)
	assert.Equal(t, srcBytes, dstBytes)
}

func TestCopySnapshot_NotFoundSource(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{
		Versioned: versioned(), SourceSnapshotID: "snap-missing", DestinationSnapshotID: "snap-dst", VolumeID: "vol-a",
	})
	require.ErrorIs(t, err, ebsprovider.ErrNotFound)
}

func TestCopySnapshot_AlreadyExistsDestination(t *testing.T) {
	p, _ := newTestProvider(t)
	volPath := p.volumePath("vol-copysnap-conflict")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))
	_, err := p.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-a", VolumeID: "vol-copysnap-conflict"})
	require.NoError(t, err)
	_, err = p.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-b", VolumeID: "vol-copysnap-conflict"})
	require.NoError(t, err)

	_, err = p.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{
		Versioned: versioned(), SourceSnapshotID: "snap-a", DestinationSnapshotID: "snap-b", VolumeID: "vol-copysnap-conflict",
	})
	require.ErrorIs(t, err, ebsprovider.ErrAlreadyExists)
}

func TestCopySnapshot_InvalidArgumentWhenIDsMatch(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{
		Versioned: versioned(), SourceSnapshotID: "snap-same", DestinationSnapshotID: "snap-same", VolumeID: "vol-a",
	})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
}

func TestCopySnapshot_InvalidArgumentWrongOwner(t *testing.T) {
	p, _ := newTestProvider(t)
	volPath := p.volumePath("vol-owner")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))
	_, err := p.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{Versioned: versioned(), SnapshotID: "snap-owned", VolumeID: "vol-owner"})
	require.NoError(t, err)

	_, err = p.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{
		Versioned: versioned(), SourceSnapshotID: "snap-owned", DestinationSnapshotID: "snap-owned-dst", VolumeID: "vol-foreign",
	})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
}

func TestPublishVolume_ArgvReadWrite(t *testing.T) {
	p, fr := newTestProvider(t)
	volPath := p.volumePath("vol-pub")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))

	pub, err := p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-pub", NodeID: "node-1"})
	require.NoError(t, err)
	assert.Equal(t, "vol-pub", pub.VolumeID)
	assert.Equal(t, "node-1", pub.NodeID)
	assert.Equal(t, "nbd+unix:///?socket="+p.socketPath("vol-pub"), pub.NBDURI)

	calls := fr.callsSnapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "qemu-nbd", calls[0].name)
	assert.Equal(t, []string{
		"--socket", p.socketPath("vol-pub"),
		"--format", "qcow2",
		"--persistent",
		"--shared=1",
		"--fork",
		"--pid-file", p.pidFilePath("vol-pub"),
		volPath,
	}, calls[0].args)
	assert.NotContains(t, calls[0].args, "-r")
}

func TestPublishVolume_ReadOnlyAddsFlag(t *testing.T) {
	p, fr := newTestProvider(t)
	volPath := p.volumePath("vol-pub-ro")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))

	_, err := p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-pub-ro", NodeID: "node-1", ReadOnly: true})
	require.NoError(t, err)

	calls := fr.callsSnapshot()
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].args, "-r")
	// -r must precede the trailing volume path argument.
	assert.Equal(t, "-r", calls[0].args[len(calls[0].args)-2])
}

func TestPublishVolume_IdempotentRepublishSameNode(t *testing.T) {
	p, fr := newTestProvider(t)
	volPath := p.volumePath("vol-pub-idem")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))

	first, err := p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-pub-idem", NodeID: "node-1"})
	require.NoError(t, err)
	second, err := p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-pub-idem", NodeID: "node-1"})
	require.NoError(t, err)
	assert.Equal(t, first, second)

	calls := fr.callsSnapshot()
	require.Len(t, calls, 1, "a republish to the same node must not start a second qemu-nbd")
}

func TestPublishVolume_ConflictDifferentNode(t *testing.T) {
	p, _ := newTestProvider(t)
	volPath := p.volumePath("vol-pub-conflict")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))

	_, err := p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-pub-conflict", NodeID: "node-1"})
	require.NoError(t, err)
	_, err = p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-pub-conflict", NodeID: "node-2"})
	require.ErrorIs(t, err, ebsprovider.ErrVolumeInUse)
}

func TestPublishVolume_NotFound(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-never-existed", NodeID: "node-1"})
	require.ErrorIs(t, err, ebsprovider.ErrNotFound)
}

func TestUnpublishVolume_Success(t *testing.T) {
	p, _ := newTestProvider(t)
	volPath := p.volumePath("vol-unpub")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))
	_, err := p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-unpub", NodeID: "node-1"})
	require.NoError(t, err)

	require.NoError(t, p.UnpublishVolume(context.Background(), ebsprovider.UnpublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-unpub", NodeID: "node-1"}))
	_, err = os.Stat(p.socketPath("vol-unpub"))
	require.True(t, os.IsNotExist(err))
}

func TestUnpublishVolume_AbsentIsIdempotent(t *testing.T) {
	p, _ := newTestProvider(t)
	require.NoError(t, p.UnpublishVolume(context.Background(), ebsprovider.UnpublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-never-existed", NodeID: "node-1"}))
}

func TestUnpublishVolume_RepeatIsIdempotent(t *testing.T) {
	p, fr := newTestProvider(t)
	volPath := p.volumePath("vol-unpub-repeat")
	require.NoError(t, os.WriteFile(volPath, nil, 0o640))
	_, err := p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-unpub-repeat", NodeID: "node-1"})
	require.NoError(t, err)

	require.NoError(t, p.UnpublishVolume(context.Background(), ebsprovider.UnpublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-unpub-repeat", NodeID: "node-1"}))
	require.NoError(t, p.UnpublishVolume(context.Background(), ebsprovider.UnpublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-unpub-repeat", NodeID: "node-1"}))

	// Republishing after unpublish must start a fresh qemu-nbd.
	_, err = p.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: versioned(), VolumeID: "vol-unpub-repeat", NodeID: "node-1"})
	require.NoError(t, err)
	calls := fr.callsSnapshot()
	nbdCalls := 0
	for _, c := range calls {
		if c.name == "qemu-nbd" {
			nbdCalls++
		}
	}
	assert.Equal(t, 2, nbdCalls)
}

func TestGetCapabilities_Fixed(t *testing.T) {
	p, _ := newTestProvider(t)
	resp, err := p.GetCapabilities(context.Background(), ebsprovider.GetCapabilitiesRequest{Versioned: versioned()})
	require.NoError(t, err)
	assert.Equal(t, capabilities, resp.Capabilities)
	assert.True(t, resp.Capabilities.CrashConsistentSnapshot)
	assert.True(t, resp.Capabilities.VolumeSeeding)
	assert.True(t, resp.Capabilities.SparseExtentReporting, "qemu-nbd advertises base:allocation")
	assert.True(t, resp.Capabilities.ReadOnlyPublish, "PublishVolume passes -r to qemu-nbd")
	assert.False(t, resp.Capabilities.OnlineExpansion)
	assert.False(t, resp.Capabilities.OwnerRouting)
}
