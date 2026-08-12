package hostile

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
)

// Verb names match the span names the contract emits, so an injection log and
// a trace can be read side by side.
const (
	verbCapabilities    = "capabilities"
	verbVolumeCreate    = "volume.create"
	verbVolumeDescribe  = "volume.describe"
	verbVolumeList      = "volume.list"
	verbVolumeExpand    = "volume.expand"
	verbVolumeDelete    = "volume.delete"
	verbVolumePublish   = "volume.publish"
	verbVolumeUnpublish = "volume.unpublish"
	verbSnapshotCreate  = "snapshot.create"
	verbSnapshotDelete  = "snapshot.delete"
	verbSnapshotCopy    = "snapshot.copy"
	verbSnapshotList    = "snapshot.list"
)

// lieFlavours are the ways a successful answer can be wrong. Each one is a
// field the control plane reads and acts on.
var lieFlavours = []string{"capacity", "handle", "state"}

// corruptVolume returns a copy of volume with one field wrong. It copies
// rather than mutating: the inner provider's own object must stay correct, or
// the fault becomes a real bug in the fake and nothing downstream means
// anything.
func corruptVolume(volume *ebsprovider.Volume, flavour string) *ebsprovider.Volume {
	if volume == nil {
		return nil
	}
	corrupted := *volume
	switch flavour {
	case "capacity":
		corrupted.CapacityBytes *= 2
	case "handle":
		corrupted.Handle = "hostile://wrong-handle"
	case "state":
		if corrupted.State == ebsprovider.VolumeStateAvailable {
			corrupted.State = ebsprovider.VolumeStateInUse
		} else {
			corrupted.State = ebsprovider.VolumeStateAvailable
		}
	}
	return &corrupted
}

func corruptSnapshot(snapshot *ebsprovider.Snapshot, flavour string) *ebsprovider.Snapshot {
	if snapshot == nil {
		return nil
	}
	corrupted := *snapshot
	switch flavour {
	case "capacity":
		corrupted.SizeBytes *= 2
	case "handle":
		corrupted.SourceVolumeID = "vol-hostilewrongsrc"
	case "state":
		corrupted.State = ebsprovider.SnapshotStatePending
	}
	return &corrupted
}

func corruptPublished(published *ebsprovider.PublishedVolume, flavour string) *ebsprovider.PublishedVolume {
	if published == nil {
		return nil
	}
	corrupted := *published
	switch flavour {
	case "handle", "capacity":
		corrupted.NBDURI = "nbd+unix:///?socket=/hostile/wrong.sock"
	case "state":
		corrupted.NodeID = "hostile-wrong-node"
	}
	return &corrupted
}

func (p *Provider) GetCapabilities(ctx context.Context, req ebsprovider.GetCapabilitiesRequest) (*ebsprovider.GetCapabilitiesResponse, error) {
	injection := p.draw(verbCapabilities, "")
	if stop, err := p.apply(ctx, injection); stop || err != nil {
		return nil, err
	}
	resp, err := p.inner.GetCapabilities(ctx, req)
	if err := p.after(injection, err); err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *Provider) CreateVolume(ctx context.Context, req ebsprovider.CreateVolumeRequest) (*ebsprovider.Volume, error) {
	injection := p.draw(verbVolumeCreate, req.VolumeID)
	if stop, err := p.apply(ctx, injection); stop || err != nil {
		return nil, err
	}
	volume, err := p.inner.CreateVolume(ctx, req)
	if err := p.after(injection, err); err != nil {
		return nil, err
	}
	if injection.Fault == FaultLie {
		return corruptVolume(volume, injection.Detail), nil
	}
	return volume, nil
}

func (p *Provider) GetVolume(ctx context.Context, req ebsprovider.GetVolumeRequest) (*ebsprovider.Volume, error) {
	injection := p.draw(verbVolumeDescribe, req.VolumeID)
	if stop, err := p.apply(ctx, injection); stop || err != nil {
		return nil, err
	}
	volume, err := p.inner.GetVolume(ctx, req)
	if err := p.after(injection, err); err != nil {
		return nil, err
	}
	if injection.Fault == FaultLie {
		return corruptVolume(volume, injection.Detail), nil
	}
	return volume, nil
}

// ListVolumes is never made to lie. Enumeration is the oracle a soak run
// checks its invariants with, and an instrument that reports what it was told
// to report cannot falsify anything.
func (p *Provider) ListVolumes(ctx context.Context, req ebsprovider.ListVolumesRequest) (*ebsprovider.ListVolumesResponse, error) {
	injection := p.drawTruthful(verbVolumeList, "")
	if stop, err := p.apply(ctx, injection); stop || err != nil {
		return nil, err
	}
	resp, err := p.inner.ListVolumes(ctx, req)
	if err := p.after(injection, err); err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *Provider) ExpandVolume(ctx context.Context, req ebsprovider.ExpandVolumeRequest) (*ebsprovider.Volume, error) {
	injection := p.draw(verbVolumeExpand, req.VolumeID)
	if stop, err := p.apply(ctx, injection); stop || err != nil {
		return nil, err
	}
	volume, err := p.inner.ExpandVolume(ctx, req)
	if err := p.after(injection, err); err != nil {
		return nil, err
	}
	if injection.Fault == FaultLie {
		return corruptVolume(volume, injection.Detail), nil
	}
	return volume, nil
}

func (p *Provider) DeleteVolume(ctx context.Context, req ebsprovider.DeleteVolumeRequest) error {
	injection := p.draw(verbVolumeDelete, req.VolumeID)
	if stop, err := p.apply(ctx, injection); stop || err != nil {
		return err
	}
	return p.after(injection, p.inner.DeleteVolume(ctx, req))
}

func (p *Provider) CreateSnapshot(ctx context.Context, req ebsprovider.CreateSnapshotRequest) (*ebsprovider.Snapshot, error) {
	injection := p.draw(verbSnapshotCreate, req.SnapshotID)
	if stop, err := p.apply(ctx, injection); stop || err != nil {
		return nil, err
	}
	snapshot, err := p.inner.CreateSnapshot(ctx, req)
	if err := p.after(injection, err); err != nil {
		return nil, err
	}
	if injection.Fault == FaultLie {
		return corruptSnapshot(snapshot, injection.Detail), nil
	}
	return snapshot, nil
}

func (p *Provider) DeleteSnapshot(ctx context.Context, req ebsprovider.DeleteSnapshotRequest) error {
	injection := p.draw(verbSnapshotDelete, req.SnapshotID)
	if stop, err := p.apply(ctx, injection); stop || err != nil {
		return err
	}
	return p.after(injection, p.inner.DeleteSnapshot(ctx, req))
}

func (p *Provider) CopySnapshot(ctx context.Context, req ebsprovider.CopySnapshotRequest) (*ebsprovider.Snapshot, error) {
	injection := p.draw(verbSnapshotCopy, req.DestinationSnapshotID)
	if stop, err := p.apply(ctx, injection); stop || err != nil {
		return nil, err
	}
	snapshot, err := p.inner.CopySnapshot(ctx, req)
	if err := p.after(injection, err); err != nil {
		return nil, err
	}
	if injection.Fault == FaultLie {
		return corruptSnapshot(snapshot, injection.Detail), nil
	}
	return snapshot, nil
}

// ListSnapshots is never made to lie, for the same reason ListVolumes is not:
// it is an oracle, and an instrument that reports what it was told to report
// cannot falsify anything.
func (p *Provider) ListSnapshots(ctx context.Context, req ebsprovider.ListSnapshotsRequest) (*ebsprovider.ListSnapshotsResponse, error) {
	injection := p.drawTruthful(verbSnapshotList, "")
	if stop, err := p.apply(ctx, injection); stop || err != nil {
		return nil, err
	}
	resp, err := p.inner.ListSnapshots(ctx, req)
	if err := p.after(injection, err); err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *Provider) PublishVolume(ctx context.Context, req ebsprovider.PublishVolumeRequest) (*ebsprovider.PublishedVolume, error) {
	injection := p.draw(verbVolumePublish, req.VolumeID)
	if stop, err := p.apply(ctx, injection); stop || err != nil {
		return nil, err
	}
	published, err := p.inner.PublishVolume(ctx, req)
	if err := p.after(injection, err); err != nil {
		return nil, err
	}
	if injection.Fault == FaultLie {
		return corruptPublished(published, injection.Detail), nil
	}
	return published, nil
}

func (p *Provider) UnpublishVolume(ctx context.Context, req ebsprovider.UnpublishVolumeRequest) error {
	injection := p.draw(verbVolumeUnpublish, req.VolumeID)
	if stop, err := p.apply(ctx, injection); stop || err != nil {
		return err
	}
	return p.after(injection, p.inner.UnpublishVolume(ctx, req))
}
