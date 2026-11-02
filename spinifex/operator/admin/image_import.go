package admin

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/nbd"
)

// ImportOpts describes one raw disk image to import into a new volume.
type ImportOpts struct {
	// VolumeID names the volume to create. For an AMI import this is the
	// image ID, so the snapshot lands at SnapPrefix(imageID).
	VolumeID string
	// NodeID is the node whose provider serves the NBD export written to. It
	// must be the node this process runs on: the export is local to it.
	NodeID string
	// SizeBytes is the volume's capacity, which the caller rounds to a whole
	// GiB. It must be at least the source image's size.
	SizeBytes int64
	// AvailabilityZone is recorded on the volume.
	AvailabilityZone string
	// SourcePath is the raw disk image on local disk.
	SourcePath string
	// Snapshot creates SnapPrefix(VolumeID) once the write completes, so
	// launches can clone the image instead of copying it block by block.
	Snapshot bool
	// Progress receives the importer's own output (a percentage counter).
	// Nil discards it.
	Progress io.Writer
}

// ImportImage writes a local raw disk image into a new provider volume over
// NBD, then optionally snapshots it. Everything here goes through the provider
// contract, so an import works against any conforming provider rather than
// only the one whose storage library this process happens to link.
//
// A failed import deliberately leaves the volume behind: its blocks are
// already in the object store, and deleting it would take the ID an operator
// needs to reclaim them with it.
func ImportImage(ctx context.Context, provider ebsprovider.EBSProvider, opts ImportOpts) error {
	if opts.VolumeID == "" || opts.NodeID == "" || opts.SourcePath == "" {
		return errors.New("import: volume ID, node ID and source path are all required")
	}
	if opts.SizeBytes <= 0 {
		return fmt.Errorf("import: invalid volume size %d", opts.SizeBytes)
	}

	if _, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned:        ebsprovider.NewVersioned(),
		VolumeID:         opts.VolumeID,
		CapacityRange:    ebsprovider.CapacityRange{RequiredBytes: opts.SizeBytes},
		AvailabilityZone: opts.AvailabilityZone,
	}); err != nil {
		return fmt.Errorf("import: create volume %s: %w", opts.VolumeID, err)
	}

	published, err := provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{
		Versioned: ebsprovider.NewVersioned(),
		VolumeID:  opts.VolumeID,
		NodeID:    opts.NodeID,
	})
	if err != nil {
		return fmt.Errorf("import: publish volume %s: %w", opts.VolumeID, err)
	}

	var writeErr error
	if published.Export.Kind == ebsprovider.ExportKindNBD && published.Export.NBD != nil {
		writeErr = writeImage(ctx, opts.SourcePath, published.Export.NBD.NBDURI, opts.Progress)
	} else {
		writeErr = fmt.Errorf("volume is published as a %q export; image import writes only through nbd", published.Export.Kind)
	}

	// Unpublish regardless: a volume left published cannot be snapshotted or
	// attached, and on the failure path it would strand the export too.
	if err := provider.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{
		Versioned: ebsprovider.NewVersioned(),
		VolumeID:  opts.VolumeID,
		NodeID:    opts.NodeID,
	}); err != nil {
		if writeErr != nil {
			return fmt.Errorf("import: write image: %w (unpublish also failed: %w)", writeErr, err)
		}
		return fmt.Errorf("import: unpublish volume %s: %w", opts.VolumeID, err)
	}
	if writeErr != nil {
		return fmt.Errorf("import: write image to %s: %w", opts.VolumeID, writeErr)
	}

	if opts.Snapshot {
		if _, err := provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{
			Versioned:  ebsprovider.NewVersioned(),
			SnapshotID: SnapPrefix(opts.VolumeID),
			VolumeID:   opts.VolumeID,
		}); err != nil {
			return fmt.Errorf("import: snapshot volume %s: %w", opts.VolumeID, err)
		}
	}

	return nil
}

// writeImage is the bulk copy step, indirected so a test can drive the verb
// sequence around it without a live NBD export to write to.
var writeImage = nbd.WriteRawImage
