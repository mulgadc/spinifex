package ocibsd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
)

func targetOf(a oci.VolumeAttachment) Target {
	return Target{IQN: a.IQN, IPv4: a.IPv4, Port: a.Port}
}

// checkNode refuses a request for another node: this provider can attach a
// volume only to its own instance.
func (p *Provider) checkNode(nodeID string) error {
	if nodeID != p.cfg.NodeID {
		return fmt.Errorf("%w: node %s is served by its own provider, not %s's", ebsprovider.ErrInvalidArgument, nodeID, p.cfg.NodeID)
	}
	return nil
}

// errPublishTimeout is the cause of a publish that ran out of its own budget.
var errPublishTimeout = errors.New("ocibsd: publish budget spent")

// PublishVolume attaches the volume to this node's instance over iSCSI and
// returns its block device. An attachment here in the same access mode is
// reused; any other attachment refuses the publish.
func (p *Provider) PublishVolume(ctx context.Context, req ebsprovider.PublishVolumeRequest) (*ebsprovider.PublishedVolume, error) {
	publishCtx, cancel := context.WithTimeoutCause(ctx, p.cfg.PublishTimeout, errPublishTimeout)
	defer cancel()
	published, err := p.publishVolume(publishCtx, req)
	if err != nil && ctx.Err() == nil && errors.Is(context.Cause(publishCtx), errPublishTimeout) {
		slog.WarnContext(ctx, "ocibsd: publish ran out of its budget", "volume", req.VolumeID, "budget_ms", otelsetup.Millis(p.cfg.PublishTimeout))
		// Retry-later, since a retry reuses any attachment left behind. The cause is
		// text so a sentinel inside it cannot override that classification.
		return nil, fmt.Errorf("%w: publish of volume %s did not finish in time: %s", ebsprovider.ErrUnavailable, req.VolumeID, err.Error())
	}
	return published, err
}

func (p *Provider) publishVolume(ctx context.Context, req ebsprovider.PublishVolumeRequest) (*ebsprovider.PublishedVolume, error) {
	if err := ebsprovider.CheckVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	// A block export has no form below the descriptor version, so refuse
	// before attaching rather than attach a volume the caller cannot read.
	if req.SchemaVersion < ebsprovider.DescriptorSchemaVersion {
		return nil, fmt.Errorf("%w: a block export requires schema version %d, got %d", ebsprovider.ErrUnsupportedVersion, ebsprovider.DescriptorSchemaVersion, req.SchemaVersion)
	}
	if req.VolumeID == "" || req.NodeID == "" {
		return nil, fmt.Errorf("%w: volume and node IDs are required", ebsprovider.ErrInvalidArgument)
	}
	if err := p.checkNode(req.NodeID); err != nil {
		return nil, err
	}
	if err := p.checkStarted(); err != nil {
		return nil, err
	}
	unlock := p.lockVolume(req.VolumeID)
	defer unlock()

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
	if capacity < 0 {
		return nil, fmt.Errorf("ocibsd: volume %s has a negative capacity %d", req.VolumeID, capacity)
	}

	attached, err := p.attachments(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	var mine *oci.VolumeAttachment
	for i, a := range attached {
		if a.InstanceID != p.cfg.InstanceID {
			return nil, fmt.Errorf("%w: volume %s is attached to instance %s", ebsprovider.HeldElsewhereError(req.SchemaVersion), req.VolumeID, a.InstanceID)
		}
		mine = &attached[i]
	}
	if mine != nil {
		// OCI changes an attachment's access mode only by detaching it.
		if mine.ReadOnly != req.ReadOnly {
			return nil, fmt.Errorf("%w: volume %s is already attached here read_only=%t", ebsprovider.ErrVolumeInUse, req.VolumeID, mine.ReadOnly)
		}
		if mine.State == oci.AttachmentDetaching {
			return nil, fmt.Errorf("%w: volume %s is still detaching from this node", ebsprovider.ErrUnavailable, req.VolumeID)
		}
	} else {
		a, err := p.attach(ctx, req, v.ID)
		if err != nil {
			return nil, err
		}
		mine = &a
	}

	ready, err := p.awaitAttached(ctx, mine.ID)
	if err != nil {
		return nil, err
	}
	device, err := p.cfg.Initiator.Login(ctx, targetOf(ready))
	if err != nil {
		return nil, fmt.Errorf("%w: log in to volume %s: %w", ebsprovider.ErrUnavailable, req.VolumeID, err)
	}
	return &ebsprovider.PublishedVolume{
		VolumeID: req.VolumeID,
		NodeID:   req.NodeID,
		Export: ebsprovider.VolumeExport{
			Kind:  ebsprovider.ExportKindBlock,
			Block: &ebsprovider.BlockExport{DevicePath: device, SizeBytes: uint64(capacity)},
		},
	}, nil
}

// attach asks OCI for a new attachment to this node's instance. A conflict is
// another attachment that appeared since the list, so it is read again.
func (p *Provider) attach(ctx context.Context, req ebsprovider.PublishVolumeRequest, volumeOCID string) (oci.VolumeAttachment, error) {
	a, err := p.cfg.Client.AttachVolume(ctx, oci.AttachVolumeInput{
		InstanceID: p.cfg.InstanceID, VolumeID: volumeOCID, DisplayName: req.VolumeID, ReadOnly: req.ReadOnly,
	})
	switch {
	case err == nil:
		return a, nil
	case errors.Is(err, oci.ErrNotFound):
		return oci.VolumeAttachment{}, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.VolumeID)
	case !errors.Is(err, oci.ErrConflict):
		return oci.VolumeAttachment{}, ociError("attach volume "+req.VolumeID, err)
	}
	attached, listErr := p.attachments(ctx, volumeOCID)
	if listErr != nil {
		return oci.VolumeAttachment{}, listErr
	}
	for _, other := range attached {
		if other.InstanceID != p.cfg.InstanceID {
			return oci.VolumeAttachment{}, fmt.Errorf("%w: volume %s is attached to instance %s", ebsprovider.HeldElsewhereError(req.SchemaVersion), req.VolumeID, other.InstanceID)
		}
	}
	return oci.VolumeAttachment{}, ociError("attach volume "+req.VolumeID, err, ebsprovider.ErrUnavailable)
}

func (p *Provider) awaitAttached(ctx context.Context, attachmentID string) (oci.VolumeAttachment, error) {
	var a oci.VolumeAttachment
	err := p.waitFor(ctx, p.cfg.AttachTimeout, "attachment "+attachmentID+" attaching", func(ctx context.Context) (bool, error) {
		var err error
		if a, err = p.cfg.Client.GetVolumeAttachment(ctx, attachmentID); err != nil {
			return false, ociError("get volume attachment", err)
		}
		switch a.State {
		case oci.AttachmentAttached:
			return true, nil
		case oci.AttachmentAttaching:
			return false, nil
		default:
			return false, fmt.Errorf("%w: attachment %s is %s", ebsprovider.ErrUnavailable, attachmentID, a.State)
		}
	})
	return a, err
}

// UnpublishVolume logs out of the volume's session on this node, detaches it
// from this node's instance, and waits for OCI to report it DETACHED, the
// point after which the old session can write nothing. NoSeal changes nothing.
func (p *Provider) UnpublishVolume(ctx context.Context, req ebsprovider.UnpublishVolumeRequest) (*ebsprovider.UnpublishedVolume, error) {
	if err := ebsprovider.CheckUnpublishVersion(req); err != nil {
		return nil, err
	}
	if req.VolumeID == "" || req.NodeID == "" {
		return nil, fmt.Errorf("%w: volume and node IDs are required", ebsprovider.ErrInvalidArgument)
	}
	if err := p.checkNode(req.NodeID); err != nil {
		return nil, err
	}
	unlock := p.lockVolume(req.VolumeID)
	defer unlock()

	result := &ebsprovider.UnpublishedVolume{}
	v, ok, err := p.findVolume(ctx, req.VolumeID, req.Handle)
	if err != nil || !ok {
		return result, err
	}
	attached, err := p.attachments(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	tornDown := false
	for _, a := range attached {
		if a.InstanceID != p.cfg.InstanceID {
			continue
		}
		if err := p.cfg.Initiator.Logout(ctx, targetOf(a)); err != nil {
			return nil, fmt.Errorf("%w: log out of volume %s: %w", ebsprovider.ErrUnavailable, req.VolumeID, err)
		}
		if a.State != oci.AttachmentDetaching {
			err := p.cfg.Client.DetachVolume(ctx, a.ID)
			if err != nil && !errors.Is(err, oci.ErrNotFound) && !errors.Is(err, oci.ErrConflict) {
				return nil, ociError("detach volume "+req.VolumeID, err)
			}
		}
		if err := p.awaitDetached(ctx, a.ID); err != nil {
			return nil, err
		}
		tornDown = true
	}
	result.TornDown = tornDown && req.SchemaVersion >= ebsprovider.NoSealSchemaVersion
	return result, nil
}

func (p *Provider) awaitDetached(ctx context.Context, attachmentID string) error {
	return p.waitFor(ctx, p.cfg.DetachTimeout, "attachment "+attachmentID+" detaching", func(ctx context.Context) (bool, error) {
		a, err := p.cfg.Client.GetVolumeAttachment(ctx, attachmentID)
		if errors.Is(err, oci.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, ociError("get volume attachment", err)
		}
		return a.State == oci.AttachmentDetached, nil
	})
}
