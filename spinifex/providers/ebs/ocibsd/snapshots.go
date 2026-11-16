package ocibsd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
)

// snapshotState is an OCI backup state as an EBS snapshot state. A state this
// build does not know is reported pending, so a caller keeps reading it.
func snapshotState(state oci.BackupState) ebsprovider.SnapshotState {
	switch state {
	case oci.BackupAvailable:
		return ebsprovider.SnapshotStateCompleted
	case oci.BackupFaulty:
		return ebsprovider.SnapshotStateError
	default:
		return ebsprovider.SnapshotStatePending
	}
}

// findBackup resolves an EBS snapshot to its live OCI backup.
func (p *Provider) findBackup(ctx context.Context, snapshotID, handle string) (oci.VolumeBackup, bool, error) {
	return findTagged(ctx, p, "volume backup", TagSnapshotID, "snapshot ID", snapshotID, handle, p.cfg.Client.GetVolumeBackup,
		func(ctx context.Context, tags map[string]string) ([]oci.VolumeBackup, error) {
			return p.cfg.Client.ListVolumeBackups(ctx, oci.BackupFilter{CompartmentID: p.cfg.CompartmentID, Tags: tags})
		})
}

func snapshotOf(b oci.VolumeBackup) (*ebsprovider.Snapshot, error) {
	capacity, err := capacityOf(b.FreeformTags)
	if err != nil {
		return nil, err
	}
	return &ebsprovider.Snapshot{
		ID:             b.FreeformTags[TagSnapshotID],
		SourceVolumeID: b.FreeformTags[TagVolumeID],
		SizeBytes:      capacity,
		CreatedAt:      b.TimeCreated,
		State:          snapshotState(b.State),
		Handle:         b.ID,
	}, nil
}

// CreateSnapshot starts an incremental backup and answers pending as soon as
// OCI accepts it. A repeat is answered from the backup the first call started,
// which its tags find even once the source volume is gone.
func (p *Provider) CreateSnapshot(ctx context.Context, req ebsprovider.CreateSnapshotRequest) (*ebsprovider.Snapshot, error) {
	if err := ebsprovider.CheckVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.SnapshotID == "" || req.VolumeID == "" {
		return nil, fmt.Errorf("%w: snapshot and volume IDs are required", ebsprovider.ErrInvalidArgument)
	}
	existing, ok, err := p.findBackup(ctx, req.SnapshotID, "")
	if err != nil {
		return nil, err
	}
	if ok {
		if existing.FreeformTags[TagVolumeID] != req.VolumeID {
			return nil, fmt.Errorf("%w: snapshot %s", ebsprovider.ErrAlreadyExists, req.SnapshotID)
		}
		return snapshotOf(existing)
	}

	v, ok, err := p.findVolume(ctx, req.VolumeID, req.VolumeHandle)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.VolumeID)
	}
	capacity, ok := v.FreeformTags[TagCapacityBytes]
	if !ok {
		return nil, fmt.Errorf("ocibsd: volume %s has no %s tag", req.VolumeID, TagCapacityBytes)
	}
	created, err := p.cfg.Client.CreateVolumeBackup(ctx, oci.CreateVolumeBackupInput{
		VolumeID:    v.ID,
		DisplayName: req.SnapshotID,
		FreeformTags: map[string]string{
			TagCluster: p.cfg.ClusterID, TagSnapshotID: req.SnapshotID, TagVolumeID: req.VolumeID, TagCapacityBytes: capacity,
		},
		RetryToken: RetryToken("snapshot", p.cfg.ClusterID, req.SnapshotID),
	})
	switch {
	case errors.Is(err, oci.ErrNotFound):
		return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.VolumeID)
	case err != nil:
		return nil, ociError("create snapshot "+req.SnapshotID, err, ebsprovider.ErrUnavailable)
	}
	if created.State.Gone() {
		return nil, fmt.Errorf("ocibsd: create snapshot %s answered with %s backup %s", req.SnapshotID, created.State, created.ID)
	}
	return snapshotOf(created)
}

// DeleteSnapshot deletes the backup. OCI refuses one still being taken, which
// is answered retry-later; a backup that does not exist is deleted.
func (p *Provider) DeleteSnapshot(ctx context.Context, req ebsprovider.DeleteSnapshotRequest) error {
	if err := ebsprovider.CheckVersion(req.SchemaVersion); err != nil {
		return err
	}
	if req.SnapshotID == "" {
		return fmt.Errorf("%w: snapshot ID is required", ebsprovider.ErrInvalidArgument)
	}
	b, ok, err := p.findBackup(ctx, req.SnapshotID, req.Handle)
	if err != nil || !ok {
		return err
	}
	if err := p.cfg.Client.DeleteVolumeBackup(ctx, b.ID); err != nil && !errors.Is(err, oci.ErrNotFound) {
		return ociError("delete snapshot "+req.SnapshotID, err, ebsprovider.ErrUnavailable)
	}
	return nil
}

// CopySnapshot is refused: OCI copies a backup only to another region.
func (p *Provider) CopySnapshot(_ context.Context, req ebsprovider.CopySnapshotRequest) (*ebsprovider.Snapshot, error) {
	if err := ebsprovider.CheckVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: snapshot copy", ebsprovider.ErrUnsupportedCapability)
}

// ListSnapshots lists this cluster's backups by EBS snapshot ID, or reads one.
func (p *Provider) ListSnapshots(ctx context.Context, req ebsprovider.ListSnapshotsRequest) (*ebsprovider.ListSnapshotsResponse, error) {
	if err := ebsprovider.CheckListSnapshotsVersion(req); err != nil {
		return nil, err
	}
	refOf := func(b oci.VolumeBackup) ebsprovider.SnapshotRef {
		return ebsprovider.SnapshotRef{
			ID:             b.FreeformTags[TagSnapshotID],
			SourceVolumeID: b.FreeformTags[TagVolumeID],
			Handle:         b.ID,
			State:          ebsprovider.RefState(req.SchemaVersion, snapshotState(b.State)),
		}
	}
	if req.SnapshotID != "" {
		b, ok, err := p.findBackup(ctx, req.SnapshotID, "")
		if err != nil {
			return nil, err
		}
		if !ok {
			return ebsprovider.FilteredSnapshots(req, nil), nil
		}
		ref := refOf(b)
		return ebsprovider.FilteredSnapshots(req, &ref), nil
	}

	found, err := p.cfg.Client.ListVolumeBackups(ctx, oci.BackupFilter{CompartmentID: p.cfg.CompartmentID, Tags: map[string]string{TagCluster: p.cfg.ClusterID}})
	if err != nil {
		return nil, ociError("list volume backups", err)
	}
	backups := make(map[string]oci.VolumeBackup, len(found))
	for _, b := range found {
		if id := b.FreeformTags[TagSnapshotID]; id != "" && !b.State.Gone() {
			backups[id] = b
		}
	}
	ids := slices.Sorted(maps.Keys(backups))
	response := &ebsprovider.ListSnapshotsResponse{Versioned: ebsprovider.ReplyVersioned(req.SchemaVersion)}
	response.Snapshots, response.NextToken = ebsprovider.Page(ids, req.StartingToken, int(req.PageSize()),
		func(id string) ebsprovider.SnapshotRef { return refOf(backups[id]) })
	return response, nil
}
