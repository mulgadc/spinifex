package admin

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingProvider wraps MemoryProvider to record the verb sequence and the
// arguments the import passes, so a test can assert the order and the IDs
// rather than only the end state.
type recordingProvider struct {
	*ebsprovider.MemoryProvider

	calls      []string
	publishes  []ebsprovider.PublishVolumeRequest
	snapshots  []ebsprovider.CreateSnapshotRequest
	publishURI string
}

func newRecordingProvider() *recordingProvider {
	return &recordingProvider{MemoryProvider: ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{CrashConsistentSnapshot: true}), publishURI: "nbd:unix:/tmp/does-not-matter.sock"}
}

func (p *recordingProvider) CreateVolume(ctx context.Context, req ebsprovider.CreateVolumeRequest) (*ebsprovider.Volume, error) {
	p.calls = append(p.calls, "create")
	return p.MemoryProvider.CreateVolume(ctx, req)
}

func (p *recordingProvider) PublishVolume(ctx context.Context, req ebsprovider.PublishVolumeRequest) (*ebsprovider.PublishedVolume, error) {
	p.calls = append(p.calls, "publish")
	p.publishes = append(p.publishes, req)
	if _, err := p.MemoryProvider.PublishVolume(ctx, req); err != nil {
		return nil, err
	}
	return &ebsprovider.PublishedVolume{VolumeID: req.VolumeID, NodeID: req.NodeID, NBDURI: p.publishURI}, nil
}

func (p *recordingProvider) UnpublishVolume(ctx context.Context, req ebsprovider.UnpublishVolumeRequest) error {
	p.calls = append(p.calls, "unpublish")
	return p.MemoryProvider.UnpublishVolume(ctx, req)
}

func (p *recordingProvider) CreateSnapshot(ctx context.Context, req ebsprovider.CreateSnapshotRequest) (*ebsprovider.Snapshot, error) {
	p.calls = append(p.calls, "snapshot")
	p.snapshots = append(p.snapshots, req)
	return p.MemoryProvider.CreateSnapshot(ctx, req)
}

// stubWriteImage swaps the bulk copy for one that records what it was handed
// and returns err, restoring the real writer when the test ends.
func stubWriteImage(t *testing.T, err error, seen *string) {
	t.Helper()
	original := writeImage
	writeImage = func(_ context.Context, _, nbdURI string, _ io.Writer) error {
		if seen != nil {
			*seen = nbdURI
		}
		return err
	}
	t.Cleanup(func() { writeImage = original })
}

// TestImportImage_VerbSequenceAndSnapshotID is the assertion the old
// config.json read-back stood for: the snapshot the import creates must be the
// one the AMI document will name, and both derive from SnapPrefix. A snapshot
// under any other ID leaves the registered AMI pointing at nothing.
func TestImportImage_VerbSequenceAndSnapshotID(t *testing.T) {
	var wroteTo string
	stubWriteImage(t, nil, &wroteTo)
	provider := newRecordingProvider()

	err := ImportImage(context.Background(), provider, ImportOpts{
		VolumeID:         "ami-import001",
		NodeID:           "node-a",
		SizeBytes:        8 << 30,
		AvailabilityZone: "ap-southeast-2a",
		SourcePath:       "/dev/null",
		Snapshot:         true,
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"create", "publish", "unpublish", "snapshot"}, provider.calls,
		"the volume must be unpublished before it is snapshotted")
	require.Len(t, provider.snapshots, 1)
	assert.Equal(t, SnapPrefix("ami-import001"), provider.snapshots[0].SnapshotID)
	assert.Equal(t, "ami-import001", provider.snapshots[0].VolumeID)
	require.Len(t, provider.publishes, 1)
	assert.Equal(t, "node-a", provider.publishes[0].NodeID)
	assert.Equal(t, provider.publishURI, wroteTo, "the copy must target the URI the provider published")
}

// TestImportImage_WriteFailureStillUnpublishes covers the failure path: a
// volume left published can be neither attached nor snapshotted, so the
// export must be released even when the copy that needed it failed.
func TestImportImage_WriteFailureStillUnpublishes(t *testing.T) {
	stubWriteImage(t, errors.New("copy exploded"), nil)
	provider := newRecordingProvider()

	err := ImportImage(context.Background(), provider, ImportOpts{
		VolumeID:   "ami-import002",
		NodeID:     "node-a",
		SizeBytes:  1 << 30,
		SourcePath: "/dev/null",
		Snapshot:   true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "copy exploded")

	assert.Equal(t, []string{"create", "publish", "unpublish"}, provider.calls,
		"a failed copy must not be snapshotted, and must still be unpublished")
}

// TestImportImage_NoSnapshotWhenNotAsked covers the weights-style import that
// wants the volume but not an AMI snapshot.
func TestImportImage_NoSnapshotWhenNotAsked(t *testing.T) {
	stubWriteImage(t, nil, nil)
	provider := newRecordingProvider()

	require.NoError(t, ImportImage(context.Background(), provider, ImportOpts{
		VolumeID:   "vol-import003",
		NodeID:     "node-a",
		SizeBytes:  1 << 30,
		SourcePath: "/dev/null",
	}))
	assert.Equal(t, []string{"create", "publish", "unpublish"}, provider.calls)
	assert.Empty(t, provider.snapshots)
}

// TestImportImage_RejectsIncompleteOpts keeps a caller from creating a volume
// it then cannot address, which would strand blocks under a name nobody holds.
func TestImportImage_RejectsIncompleteOpts(t *testing.T) {
	stubWriteImage(t, nil, nil)

	for name, opts := range map[string]ImportOpts{
		"no volume ID": {NodeID: "node-a", SizeBytes: 1, SourcePath: "/dev/null"},
		"no node ID":   {VolumeID: "vol-a", SizeBytes: 1, SourcePath: "/dev/null"},
		"no source":    {VolumeID: "vol-a", NodeID: "node-a", SizeBytes: 1},
		"no size":      {VolumeID: "vol-a", NodeID: "node-a", SourcePath: "/dev/null"},
	} {
		t.Run(name, func(t *testing.T) {
			provider := newRecordingProvider()
			require.Error(t, ImportImage(context.Background(), provider, opts))
			assert.Empty(t, provider.calls, "nothing may be created before the options are checked")
		})
	}
}

// TestQemuNBDTarget_RewritesUnixForm locks the translation: the contract's
// unix URI is not a form qemu accepts, so passing it through unchanged would
// fail at the copy rather than here.
func TestQemuNBDTarget_RewritesUnixForm(t *testing.T) {
	socket := t.TempDir() + "/export.sock"
	require.NoError(t, os.WriteFile(socket, nil, 0o600))

	got, err := qemuNBDTarget("nbd:unix:" + socket)
	require.NoError(t, err)
	assert.Equal(t, "nbd+unix:///?socket="+socket, got)

	got, err = qemuNBDTarget("nbd://10.0.0.5:10809")
	require.NoError(t, err)
	assert.Equal(t, "nbd://10.0.0.5:10809", got)

	_, err = qemuNBDTarget("nbd:unix:" + t.TempDir() + "/absent.sock")
	require.Error(t, err, "a socket that is not there must fail before qemu-img is run")
}
