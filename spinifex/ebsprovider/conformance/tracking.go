package conformance

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
)

// trackingProvider delegates every call to the provider under test and records
// what it created, so the suite can remove it afterwards. An in-process
// provider is discarded with the test, but a live one keeps its volumes in S3
// and would fail the next run on already_exists.
type trackingProvider struct {
	ebsprovider.EBSProvider

	t *testing.T

	volumes   map[string]string
	snapshots map[string]string
	published map[string]string
}

var _ ebsprovider.EBSProvider = (*trackingProvider)(nil)

func newTrackingProvider(t *testing.T, inner ebsprovider.EBSProvider) *trackingProvider {
	t.Helper()
	p := &trackingProvider{
		EBSProvider: inner,
		t:           t,
		volumes:     make(map[string]string),
		snapshots:   make(map[string]string),
		published:   make(map[string]string),
	}
	t.Cleanup(p.cleanup)
	return p
}

func (p *trackingProvider) CreateVolume(ctx context.Context, req ebsprovider.CreateVolumeRequest) (*ebsprovider.Volume, error) {
	vol, err := p.EBSProvider.CreateVolume(ctx, req)
	if err == nil && vol != nil {
		p.volumes[vol.ID] = vol.Handle
	}
	return vol, err
}

func (p *trackingProvider) DeleteVolume(ctx context.Context, req ebsprovider.DeleteVolumeRequest) error {
	err := p.EBSProvider.DeleteVolume(ctx, req)
	if err == nil {
		delete(p.volumes, req.VolumeID)
	}
	return err
}

func (p *trackingProvider) CreateSnapshot(ctx context.Context, req ebsprovider.CreateSnapshotRequest) (*ebsprovider.Snapshot, error) {
	snap, err := p.EBSProvider.CreateSnapshot(ctx, req)
	if err == nil && snap != nil {
		p.snapshots[snap.ID] = snap.Handle
	}
	return snap, err
}

func (p *trackingProvider) CopySnapshot(ctx context.Context, req ebsprovider.CopySnapshotRequest) (*ebsprovider.Snapshot, error) {
	snap, err := p.EBSProvider.CopySnapshot(ctx, req)
	if err == nil && snap != nil {
		p.snapshots[snap.ID] = snap.Handle
	}
	return snap, err
}

func (p *trackingProvider) DeleteSnapshot(ctx context.Context, req ebsprovider.DeleteSnapshotRequest) error {
	err := p.EBSProvider.DeleteSnapshot(ctx, req)
	if err == nil {
		delete(p.snapshots, req.SnapshotID)
	}
	return err
}

func (p *trackingProvider) PublishVolume(ctx context.Context, req ebsprovider.PublishVolumeRequest) (*ebsprovider.PublishedVolume, error) {
	pub, err := p.EBSProvider.PublishVolume(ctx, req)
	if err == nil {
		p.published[req.VolumeID] = req.NodeID
	}
	return pub, err
}

func (p *trackingProvider) UnpublishVolume(ctx context.Context, req ebsprovider.UnpublishVolumeRequest) error {
	err := p.EBSProvider.UnpublishVolume(ctx, req)
	if err == nil {
		delete(p.published, req.VolumeID)
	}
	return err
}

// cleanup removes what the subtest left behind, in dependency order: a
// published volume cannot be deleted, and a provider may hold a volume for its
// snapshots. Failures are logged rather than failed on, so a teardown problem
// never masks the assertion the subtest was actually making.
func (p *trackingProvider) cleanup() {
	ctx := context.Background()

	for volumeID, nodeID := range p.published {
		if err := p.EBSProvider.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{
			Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, Handle: p.volumes[volumeID], NodeID: nodeID,
		}); err != nil {
			p.t.Logf("conformance cleanup: unpublish %s: %v", volumeID, err)
		}
	}

	for snapshotID, handle := range p.snapshots {
		if err := p.EBSProvider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{
			Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, Handle: handle,
		}); err != nil {
			p.t.Logf("conformance cleanup: delete snapshot %s: %v", snapshotID, err)
		}
	}

	for volumeID, handle := range p.volumes {
		if err := p.EBSProvider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{
			Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, Handle: handle,
		}); err != nil {
			p.t.Logf("conformance cleanup: delete volume %s: %v", volumeID, err)
		}
	}
}
