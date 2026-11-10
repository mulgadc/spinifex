package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

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
	// SnapshotTimeout bounds the wait for a snapshot the provider answers
	// pending to complete. Zero leaves only ctx to bound it.
	SnapshotTimeout time.Duration
}

// snapshotPollInterval is how often a pending snapshot is read again; a test
// shortens it so the wait does not sleep.
var snapshotPollInterval = 5 * time.Second

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
	if _, err := provider.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{
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
		snapshotID := SnapPrefix(opts.VolumeID)
		created, err := provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{
			Versioned:  ebsprovider.NewVersioned(),
			SnapshotID: snapshotID,
			VolumeID:   opts.VolumeID,
		})
		if err != nil {
			return fmt.Errorf("import: snapshot volume %s: %w", opts.VolumeID, err)
		}
		if created != nil && created.State == ebsprovider.SnapshotStatePending {
			if err := waitForSnapshot(ctx, provider, snapshotID, opts.SnapshotTimeout); err != nil {
				return fmt.Errorf("import: snapshot volume %s: %w", opts.VolumeID, err)
			}
		}
	}

	return nil
}

// waitForSnapshot reads a pending snapshot every snapshotPollInterval until it
// is completed, failing when it errors, is gone, or timeout passes. A read that
// fails is retried, and reported if the timeout passes first.
func waitForSnapshot(ctx context.Context, provider ebsprovider.EBSProvider, snapshotID string, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ticker := time.NewTicker(snapshotPollInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("snapshot %s did not complete: %w (last read failed: %w)", snapshotID, ctx.Err(), lastErr)
			}
			return fmt.Errorf("snapshot %s did not complete: %w", snapshotID, ctx.Err())
		case <-ticker.C:
		}
		resp, err := provider.ListSnapshots(ctx, ebsprovider.ListSnapshotsRequest{
			Versioned: ebsprovider.NewLatestVersioned(), SnapshotID: snapshotID,
		})
		if err != nil {
			lastErr = err
			continue
		}
		if resp == nil || len(resp.Snapshots) == 0 {
			return fmt.Errorf("snapshot %s is no longer held by the provider", snapshotID)
		}
		switch resp.Snapshots[0].State {
		case ebsprovider.SnapshotStateCompleted:
			return nil
		case ebsprovider.SnapshotStateError:
			return fmt.Errorf("snapshot %s ended in error", snapshotID)
		}
	}
}

// writeImage is the bulk copy step, indirected so a test can drive the verb
// sequence around it without a live NBD export to write to.
var writeImage = nbd.WriteRawImage
