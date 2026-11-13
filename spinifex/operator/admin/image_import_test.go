package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/imagewrite"
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

	// publishExport, when its Kind is set, replaces the nbd export built
	// from publishURI.
	publishExport ebsprovider.VolumeExport

	// onList runs before read n (from 1) of the snapshot; a non-nil error
	// fails that read.
	onList func(n int) error
	lists  int
}

func (p *recordingProvider) ListSnapshots(ctx context.Context, req ebsprovider.ListSnapshotsRequest) (*ebsprovider.ListSnapshotsResponse, error) {
	p.lists++
	if p.onList != nil {
		if err := p.onList(p.lists); err != nil {
			return nil, err
		}
	}
	return p.MemoryProvider.ListSnapshots(ctx, req)
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
	if p.publishExport.Kind == ebsprovider.ExportKindBlock && req.SchemaVersion < ebsprovider.DescriptorSchemaVersion {
		return nil, fmt.Errorf("%w: a block export has no version %d form", ebsprovider.ErrUnsupportedVersion, req.SchemaVersion)
	}
	if _, err := p.MemoryProvider.PublishVolume(ctx, req); err != nil {
		return nil, err
	}
	export := ebsprovider.NewNBDExport(p.publishURI)
	if p.publishExport.Kind != "" {
		export = p.publishExport
	}
	return &ebsprovider.PublishedVolume{VolumeID: req.VolumeID, NodeID: req.NodeID, Export: export}, nil
}

func (p *recordingProvider) UnpublishVolume(ctx context.Context, req ebsprovider.UnpublishVolumeRequest) (*ebsprovider.UnpublishedVolume, error) {
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
	writeImage = func(_ context.Context, _ string, export ebsprovider.VolumeExport, _ io.Writer) error {
		if seen != nil && export.NBD != nil {
			*seen = export.NBD.NBDURI
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

// blockImport sets up an import against a provider publishing a host block
// device, with the shared writer running a fake qemu-img.
func blockImport(t *testing.T, imageBytes int64, deviceBytes uint64) (*recordingProvider, ImportOpts, *[][]string) {
	t.Helper()
	provider := newRecordingProvider()
	provider.publishExport = ebsprovider.VolumeExport{
		Kind:  ebsprovider.ExportKindBlock,
		Block: &ebsprovider.BlockExport{DevicePath: "/dev/sdx", SizeBytes: deviceBytes},
	}
	image := filepath.Join(t.TempDir(), "image.raw")
	require.NoError(t, os.WriteFile(image, nil, 0o600))
	require.NoError(t, os.Truncate(image, imageBytes))

	var ran [][]string
	original := writeImage
	writeImage = imagewrite.Writer{QemuImg: func(_ context.Context, _ io.Writer, args ...string) error {
		ran = append(ran, args)
		return nil
	}}.Write
	t.Cleanup(func() { writeImage = original })
	return provider, ImportOpts{VolumeID: "ami-import005", NodeID: "node-a", SizeBytes: 1 << 30, SourcePath: image, Snapshot: true}, &ran
}

// A block export has no version 1 form, so only a publish at the latest
// version reaches the device at all.
func TestImportImage_BlockExportIsWrittenAtItsDevicePath(t *testing.T) {
	provider, opts, ran := blockImport(t, 1<<20, 1<<30)

	require.NoError(t, ImportImage(context.Background(), provider, opts))

	require.Len(t, provider.publishes, 1)
	assert.Equal(t, ebsprovider.SchemaVersion, provider.publishes[0].SchemaVersion)
	require.Len(t, *ran, 1)
	assert.Equal(t, "/dev/sdx", (*ran)[0][len((*ran)[0])-1], "the copy must target the device the provider published")
	assert.Equal(t, []string{"create", "publish", "unpublish", "snapshot"}, provider.calls)
	vol, err := provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewLatestVersioned(), VolumeID: opts.VolumeID})
	require.NoError(t, err)
	assert.Equal(t, ebsprovider.VolumeStateAvailable, vol.State)
}

func TestImportImage_ImageLargerThanTheBlockExportIsRefusedAndUnpublished(t *testing.T) {
	provider, opts, ran := blockImport(t, 1<<20+1, 1<<20)

	err := ImportImage(context.Background(), provider, opts)
	require.ErrorContains(t, err, "larger than")
	assert.Empty(t, *ran, "nothing may be written to a device too small for the image")
	assert.Equal(t, []string{"create", "publish", "unpublish"}, provider.calls,
		"a refused write must not be snapshotted, and must still be unpublished")
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

// A snapshot the provider answers pending is read until it completes, and the
// import fails if it errors, disappears, or outlasts SnapshotTimeout.
func TestImportImage_WaitsForAPendingSnapshot(t *testing.T) {
	original := snapshotPollInterval
	snapshotPollInterval = time.Millisecond
	t.Cleanup(func() { snapshotPollInterval = original })
	snapshotID := SnapPrefix("ami-import001")

	cases := []struct {
		name      string
		pending   bool
		onList    func(p *recordingProvider) func(n int) error
		wantErr   string // empty: the import succeeds
		wantLists int    // -1: at least one
	}{
		{name: "answered completed is not read", wantLists: 0},
		{name: "completes on the second read", pending: true, wantLists: 2,
			onList: func(p *recordingProvider) func(int) error {
				return func(n int) error {
					if n == 2 {
						return p.SettleSnapshot(snapshotID, ebsprovider.SnapshotStateCompleted)
					}
					return nil
				}
			}},
		{name: "a failed read is retried", pending: true, wantLists: 2,
			onList: func(p *recordingProvider) func(int) error {
				return func(n int) error {
					if n == 1 {
						return ebsprovider.ErrUnavailable
					}
					return p.SettleSnapshot(snapshotID, ebsprovider.SnapshotStateCompleted)
				}
			}},
		{name: "ends in error", pending: true, wantErr: "ended in error", wantLists: 1,
			onList: func(p *recordingProvider) func(int) error {
				return func(int) error { return p.SettleSnapshot(snapshotID, ebsprovider.SnapshotStateError) }
			}},
		{name: "disappears", pending: true, wantErr: "no longer held", wantLists: 1,
			onList: func(p *recordingProvider) func(int) error {
				return func(int) error {
					return p.DeleteSnapshot(context.Background(), ebsprovider.DeleteSnapshotRequest{
						Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID,
					})
				}
			}},
		{name: "outlasts the timeout", pending: true, wantErr: context.DeadlineExceeded.Error(), wantLists: -1},
		{name: "outlasts the timeout while reads fail", pending: true, wantErr: "last read failed", wantLists: -1,
			onList: func(*recordingProvider) func(int) error {
				return func(int) error { return ebsprovider.ErrUnavailable }
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubWriteImage(t, nil, nil)
			provider := newRecordingProvider()
			provider.AnswerSnapshotsPending(tc.pending)
			if tc.onList != nil {
				provider.onList = tc.onList(provider)
			}

			err := ImportImage(context.Background(), provider, ImportOpts{
				VolumeID: "ami-import001", NodeID: "node-a", SizeBytes: 1 << 30,
				SourcePath: "/tmp/image.raw", Snapshot: true, SnapshotTimeout: 50 * time.Millisecond,
			})

			if tc.wantErr == "" {
				require.NoError(t, err)
				snap, ok := provider.Snapshot(snapshotID)
				require.True(t, ok)
				assert.Equal(t, ebsprovider.SnapshotStateCompleted, snap.State)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
			if tc.wantLists < 0 {
				assert.Positive(t, provider.lists)
			} else {
				assert.Equal(t, tc.wantLists, provider.lists)
			}
		})
	}
}
