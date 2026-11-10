package ocibsd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
)

// tagged is an OCI resource ocibsd finds by its tags.
type tagged interface {
	Tags() map[string]string
	IsGone() bool
}

// findTagged resolves an EBS ID to the live OCI resource whose idTag carries
// it, by handle when the caller has one and by tag otherwise. A resource
// another cluster tagged, or one OCI is deleting, is not found.
func findTagged[T tagged](ctx context.Context, p *Provider, kind, idTag, idName, id, handle string,
	get func(context.Context, string) (T, error), list func(context.Context, map[string]string) ([]T, error),
) (T, bool, error) {
	var none T
	if handle != "" {
		r, err := get(ctx, handle)
		if errors.Is(err, oci.ErrNotFound) {
			return none, false, nil
		}
		if err != nil {
			return none, false, ociError("get "+kind, err)
		}
		if r.IsGone() || r.Tags()[TagCluster] != p.cfg.ClusterID || r.Tags()[idTag] != id {
			return none, false, nil
		}
		return r, true, nil
	}
	found, err := list(ctx, map[string]string{TagCluster: p.cfg.ClusterID, idTag: id})
	if err != nil {
		return none, false, ociError("list "+kind+"s", err)
	}
	live := slices.DeleteFunc(found, T.IsGone)
	switch len(live) {
	case 0:
		return none, false, nil
	case 1:
		return live[0], true, nil
	default:
		return none, false, fmt.Errorf("ocibsd: %d OCI %ss carry %s %s", len(live), kind, idName, id)
	}
}

// findVolume resolves an EBS volume to its live OCI volume.
func (p *Provider) findVolume(ctx context.Context, volumeID, handle string) (oci.BlockVolume, bool, error) {
	return findTagged(ctx, p, "volume", TagVolumeID, "volume ID", volumeID, handle, p.cfg.Client.GetVolume,
		func(ctx context.Context, tags map[string]string) ([]oci.BlockVolume, error) {
			return p.cfg.Client.ListVolumes(ctx, oci.VolumeFilter{CompartmentID: p.cfg.CompartmentID, Tags: tags})
		})
}

// attachments lists the volume's attachments in any state but DETACHED.
func (p *Provider) attachments(ctx context.Context, volumeOCID string) ([]oci.VolumeAttachment, error) {
	all, err := p.cfg.Client.ListVolumeAttachments(ctx, oci.AttachmentFilter{CompartmentID: p.cfg.CompartmentID, VolumeID: volumeOCID})
	if err != nil {
		return nil, ociError("list volume attachments", err)
	}
	return slices.DeleteFunc(all, func(a oci.VolumeAttachment) bool { return a.State == oci.AttachmentDetached }), nil
}

func (p *Provider) volumeOf(ctx context.Context, v oci.BlockVolume) (*ebsprovider.Volume, error) {
	capacity, err := capacityOf(v.FreeformTags)
	if err != nil {
		return nil, err
	}
	state := ebsprovider.VolumeStateAvailable
	switch v.State {
	case oci.VolumeProvisioning, oci.VolumeRestoring:
		state = ebsprovider.VolumeStateCreating
	default:
		attached, err := p.attachments(ctx, v.ID)
		if err != nil {
			return nil, err
		}
		if len(attached) > 0 {
			state = ebsprovider.VolumeStateInUse
		}
	}
	return &ebsprovider.Volume{
		ID:               v.FreeformTags[TagVolumeID],
		CapacityBytes:    capacity,
		State:            state,
		Handle:           v.ID,
		AvailabilityZone: p.cfg.AvailabilityZone,
	}, nil
}

func (p *Provider) CreateVolume(ctx context.Context, req ebsprovider.CreateVolumeRequest) (*ebsprovider.Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	required := req.CapacityRange.RequiredBytes
	if req.VolumeID == "" || required <= 0 || (req.CapacityRange.LimitBytes > 0 && required > req.CapacityRange.LimitBytes) {
		return nil, fmt.Errorf("%w: volume ID and a positive capacity range are required", ebsprovider.ErrInvalidArgument)
	}
	if len(req.SeedData) > 0 {
		return nil, fmt.Errorf("%w: volume seeding", ebsprovider.ErrUnsupportedCapability)
	}
	if req.SourceSnapshotID != "" && req.SourceSnapshotVolumeID == "" {
		return nil, fmt.Errorf("%w: a source snapshot needs its source volume ID", ebsprovider.ErrInvalidArgument)
	}
	if req.AvailabilityZone != "" && req.AvailabilityZone != p.cfg.AvailabilityZone {
		return nil, fmt.Errorf("%w: availability zone %q is not this cluster's %q", ebsprovider.ErrInvalidArgument, req.AvailabilityZone, p.cfg.AvailabilityZone)
	}
	params, err := ebsprovider.ParseVolumeParameters(req.Parameters)
	if err != nil {
		return nil, err
	}
	vpus, err := VPUsPerGB(params.VolumeType)
	if err != nil {
		return nil, err
	}

	// A repeat, including one after a lost reply, is answered from the volume
	// the first call created, which its tags find.
	existing, ok, err := p.findVolume(ctx, req.VolumeID, "")
	if err != nil {
		return nil, err
	}
	if ok {
		if err := p.sameVolume(ctx, existing, req, vpus); err != nil {
			return nil, err
		}
		return p.awaitVolume(ctx, existing.ID)
	}

	in := oci.CreateVolumeInput{
		CompartmentID:      p.cfg.CompartmentID,
		AvailabilityDomain: p.cfg.AvailabilityDomain,
		DisplayName:        req.VolumeID,
		SizeGB:             SizeGB(required),
		VPUsPerGB:          vpus,
		FreeformTags: map[string]string{
			TagCluster: p.cfg.ClusterID, TagVolumeID: req.VolumeID, TagCapacityBytes: strconv.FormatInt(required, 10),
		},
		RetryToken: RetryToken("volume", p.cfg.ClusterID, req.VolumeID),
	}
	if req.SourceSnapshotID != "" {
		backup, err := p.restorableBackup(ctx, req)
		if err != nil {
			return nil, err
		}
		in.SourceBackupID = backup.ID
		in.SizeGB = max(in.SizeGB, backup.SizeGB)
	}
	created, err := p.cfg.Client.CreateVolume(ctx, in)
	if err != nil {
		return nil, ociError("create volume "+req.VolumeID, err, ebsprovider.ErrUnavailable)
	}
	// OCI replays a retry token for a day, so a volume deleted and created
	// again under the same ID inside that window comes back terminated.
	if created.State.Gone() {
		return nil, fmt.Errorf("ocibsd: create volume %s answered with %s volume %s", req.VolumeID, created.State, created.ID)
	}
	return p.awaitVolume(ctx, created.ID)
}

// sameVolume refuses a repeat create whose request differs from the one that
// made existing, as CSI's idempotency rule requires.
func (p *Provider) sameVolume(ctx context.Context, existing oci.BlockVolume, req ebsprovider.CreateVolumeRequest, vpus int64) error {
	conflict := fmt.Errorf("%w: volume %s", ebsprovider.ErrAlreadyExists, req.VolumeID)
	capacity, err := capacityOf(existing.FreeformTags)
	if err != nil {
		return err
	}
	if capacity != req.CapacityRange.RequiredBytes || existing.VPUsPerGB != vpus {
		return conflict
	}
	if (existing.SourceBackupID == "") != (req.SourceSnapshotID == "") {
		return conflict
	}
	if existing.SourceBackupID == "" {
		return nil
	}
	source, err := p.cfg.Client.GetVolumeBackup(ctx, existing.SourceBackupID)
	if err != nil && !errors.Is(err, oci.ErrNotFound) {
		return ociError("get volume backup", err)
	}
	if err != nil || source.FreeformTags[TagSnapshotID] != req.SourceSnapshotID {
		return conflict
	}
	return nil
}

// restorableBackup is the backup a create from req's snapshot restores.
func (p *Provider) restorableBackup(ctx context.Context, req ebsprovider.CreateVolumeRequest) (oci.VolumeBackup, error) {
	backup, ok, err := p.findBackup(ctx, req.SourceSnapshotID, "")
	if err != nil {
		return oci.VolumeBackup{}, err
	}
	if !ok {
		return oci.VolumeBackup{}, fmt.Errorf("%w: snapshot %s", ebsprovider.ErrNotFound, req.SourceSnapshotID)
	}
	if origin := backup.FreeformTags[TagVolumeID]; origin != req.SourceSnapshotVolumeID {
		return oci.VolumeBackup{}, fmt.Errorf("%w: snapshot %s was taken from volume %s, not %s",
			ebsprovider.ErrInvalidArgument, req.SourceSnapshotID, origin, req.SourceSnapshotVolumeID)
	}
	if capacity, err := capacityOf(backup.FreeformTags); err == nil && req.CapacityRange.RequiredBytes < capacity {
		return oci.VolumeBackup{}, fmt.Errorf("%w: requested capacity is smaller than snapshot %s", ebsprovider.ErrInvalidArgument, req.SourceSnapshotID)
	}
	switch snapshotState(backup.State) {
	case ebsprovider.SnapshotStatePending:
		return oci.VolumeBackup{}, fmt.Errorf("%w: snapshot %s is still pending", ebsprovider.ErrUnavailable, req.SourceSnapshotID)
	case ebsprovider.SnapshotStateError:
		return oci.VolumeBackup{}, fmt.Errorf("%w: snapshot %s failed", ebsprovider.ErrInvalidArgument, req.SourceSnapshotID)
	}
	return backup, nil
}

// awaitVolume waits for a volume to leave PROVISIONING or RESTORING.
func (p *Provider) awaitVolume(ctx context.Context, volumeOCID string) (*ebsprovider.Volume, error) {
	var v oci.BlockVolume
	err := p.waitFor(ctx, p.cfg.VolumeTimeout, "volume "+volumeOCID+" becoming available", func(ctx context.Context) (bool, error) {
		var err error
		if v, err = p.cfg.Client.GetVolume(ctx, volumeOCID); err != nil {
			return false, ociError("get volume", err)
		}
		switch v.State {
		case oci.VolumeAvailable:
			return true, nil
		case oci.VolumeProvisioning, oci.VolumeRestoring:
			return false, nil
		default:
			return false, fmt.Errorf("ocibsd: volume %s is %s", volumeOCID, v.State)
		}
	})
	if err != nil {
		return nil, err
	}
	return p.volumeOf(ctx, v)
}

func (p *Provider) GetVolume(ctx context.Context, req ebsprovider.GetVolumeRequest) (*ebsprovider.Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.VolumeID == "" {
		return nil, fmt.Errorf("%w: volume ID is required", ebsprovider.ErrInvalidArgument)
	}
	v, ok, err := p.findVolume(ctx, req.VolumeID, req.Handle)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.VolumeID)
	}
	return p.volumeOf(ctx, v)
}

// ListVolumes lists this cluster's volumes, by EBS volume ID.
func (p *Provider) ListVolumes(ctx context.Context, req ebsprovider.ListVolumesRequest) (*ebsprovider.ListVolumesResponse, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	found, err := p.cfg.Client.ListVolumes(ctx, oci.VolumeFilter{CompartmentID: p.cfg.CompartmentID, Tags: map[string]string{TagCluster: p.cfg.ClusterID}})
	if err != nil {
		return nil, ociError("list volumes", err)
	}
	handles := make(map[string]string, len(found))
	for _, v := range found {
		if id := v.FreeformTags[TagVolumeID]; id != "" && !v.State.Gone() {
			handles[id] = v.ID
		}
	}
	ids := make([]string, 0, len(handles))
	for id := range handles {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	response := &ebsprovider.ListVolumesResponse{Versioned: ebsprovider.ReplyVersioned(req.SchemaVersion)}
	response.Volumes, response.NextToken = ebsprovider.Page(ids, req.StartingToken, int(req.PageSize()),
		func(id string) ebsprovider.VolumeRef { return ebsprovider.VolumeRef{ID: id, Handle: handles[id]} })
	return response, nil
}

// ExpandVolume within the OCI floor changes only the recorded capacity; past
// it, OCI resizes the volume too. A volume attached anywhere is refused.
func (p *Provider) ExpandVolume(ctx context.Context, req ebsprovider.ExpandVolumeRequest) (*ebsprovider.Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	required := req.CapacityRange.RequiredBytes
	if req.VolumeID == "" || required <= 0 || (req.CapacityRange.LimitBytes > 0 && required > req.CapacityRange.LimitBytes) {
		return nil, fmt.Errorf("%w: volume ID and a positive capacity range are required", ebsprovider.ErrInvalidArgument)
	}
	v, ok, err := p.findVolume(ctx, req.VolumeID, req.Handle)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.VolumeID)
	}
	capacity, err := capacityOf(v.FreeformTags)
	if err != nil {
		return nil, err
	}
	if required < capacity {
		return nil, fmt.Errorf("%w: volume %s cannot shrink from %d to %d bytes", ebsprovider.ErrInvalidArgument, req.VolumeID, capacity, required)
	}
	if required == capacity {
		return p.volumeOf(ctx, v)
	}
	attached, err := p.attachments(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	if len(attached) > 0 {
		return nil, fmt.Errorf("%w: volume %s is attached, and online expansion is not supported", ebsprovider.ErrVolumeInUse, req.VolumeID)
	}
	tags := maps.Clone(v.FreeformTags)
	tags[TagCapacityBytes] = strconv.FormatInt(required, 10)
	in := oci.UpdateVolumeInput{FreeformTags: tags}
	if size := SizeGB(required); size > v.SizeGB {
		in.SizeGB = size
	}
	updated, err := p.cfg.Client.UpdateVolume(ctx, v.ID, in)
	if errors.Is(err, oci.ErrNotFound) {
		return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.VolumeID)
	}
	if err != nil {
		return nil, ociError("expand volume "+req.VolumeID, err, ebsprovider.ErrUnavailable)
	}
	return p.volumeOf(ctx, updated)
}

// DeleteVolume refuses a volume attached anywhere, and OCI's refusal while a
// backup of it is being taken. A volume that does not exist is deleted.
func (p *Provider) DeleteVolume(ctx context.Context, req ebsprovider.DeleteVolumeRequest) error {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return err
	}
	if req.VolumeID == "" {
		return fmt.Errorf("%w: volume ID is required", ebsprovider.ErrInvalidArgument)
	}
	v, ok, err := p.findVolume(ctx, req.VolumeID, req.Handle)
	if err != nil || !ok {
		return err
	}
	attached, err := p.attachments(ctx, v.ID)
	if err != nil {
		return err
	}
	if len(attached) > 0 {
		return fmt.Errorf("%w: volume %s is attached to %s", ebsprovider.ErrVolumeInUse, req.VolumeID, attached[0].InstanceID)
	}
	if err := p.cfg.Client.DeleteVolume(ctx, v.ID); err != nil && !errors.Is(err, oci.ErrNotFound) {
		return ociError("delete volume "+req.VolumeID, err, ebsprovider.ErrVolumeInUse)
	}
	return nil
}
