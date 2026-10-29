// Package storevolume prepares the data volume an SQS store guest mounts. The
// guest never formats a disk, so a new volume gets its ext4 filesystem here,
// on the host, before anything else can attach it.
package storevolume

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
)

// VolumeService is the slice of the EC2 volume service preparation needs. A
// volume it creates stays "creating", which public callers cannot attach,
// delete, modify or snapshot, until it is completed or discarded.
type VolumeService interface {
	CreateVolumeForPreparation(ctx context.Context, input *ec2.CreateVolumeInput, accountID string) (*ec2.Volume, error)
	CompleteVolumePreparation(ctx context.Context, accountID, volumeID string) error
	DiscardVolumePreparation(ctx context.Context, accountID, volumeID string) error
}

// Publisher is the provider pair used to reach the new volume's blocks.
type Publisher interface {
	PublishVolume(context.Context, ebsprovider.PublishVolumeRequest) (*ebsprovider.PublishedVolume, error)
	UnpublishVolume(context.Context, ebsprovider.UnpublishVolumeRequest) error
}

// Request names the volume to create.
type Request struct {
	AccountID        string
	AvailabilityZone string
	SizeGiB          int64
}

// Preparer creates fresh SQS store volumes. NodeID must be the node this
// process runs on, because the copy reads the export it publishes locally.
type Preparer struct {
	Volumes    VolumeService
	Provider   Publisher
	NodeID     string
	ScratchDir string
	// WriteImage copies a raw image onto an NBD export. The composition root
	// supplies it, since a domain does not import the runtime copy helper.
	WriteImage func(ctx context.Context, sourcePath, nbdURI string, progress io.Writer) error

	lookPath  func(string) (string, error)
	runMkfs   func(ctx context.Context, mkfs, path string) error
	toolPaths []string
	// cleanupTimeout bounds unpublish and discard, which ignore caller
	// cancellation so a cancelled caller cannot skip closing the export.
	cleanupTimeout time.Duration
}

const defaultCleanupTimeout = 30 * time.Second

// RetainedVolumeError reports a volume left in "creating" because an export
// of it might still be live, so deleting it was not safe. VolumeID is what an
// operator needs to inspect and reclaim it.
type RetainedVolumeError struct {
	VolumeID string
	Err      error
}

func (e *RetainedVolumeError) Error() string {
	return fmt.Sprintf("volume %s retained in creating for recovery: %v", e.VolumeID, e.Err)
}

func (e *RetainedVolumeError) Unwrap() error { return e.Err }

// ErrToolMissing is returned before anything is allocated when the host lacks
// a tool preparation needs.
var ErrToolMissing = errors.New("required host tool is missing")

// sbinDirs covers tools a root daemon finds on its PATH but a login shell
// may not, since mkfs lives in sbin.
var sbinDirs = []string{"/usr/sbin", "/sbin"}

// Prepare creates a new volume, writes an empty ext4 filesystem to it and
// releases it as available. It never writes any volume it did not just create.
func (p *Preparer) Prepare(ctx context.Context, req Request) (string, error) {
	if p.Volumes == nil || p.Provider == nil || p.WriteImage == nil || p.NodeID == "" || p.ScratchDir == "" {
		return "", errors.New("storevolume: volume service, provider, image writer, node ID and scratch dir are required")
	}
	if req.AccountID == "" || req.AvailabilityZone == "" || req.SizeGiB < 1 {
		return "", fmt.Errorf("storevolume: account, availability zone and a size of at least 1 GiB are required")
	}
	mkfs, err := p.findTool("mkfs.ext4")
	if err != nil {
		return "", err
	}
	if _, err := p.findTool("qemu-img"); err != nil {
		return "", err
	}

	scratch, err := os.MkdirTemp(p.ScratchDir, "sqs-store-volume-")
	if err != nil {
		return "", fmt.Errorf("storevolume: scratch dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			slog.WarnContext(ctx, "storevolume: scratch cleanup failed", "dir", scratch, "err", err)
		}
	}()
	image := filepath.Join(scratch, "ext4.raw")
	if err := buildImage(ctx, p.mkfsRunner(), mkfs, image, req.SizeGiB); err != nil {
		return "", err
	}

	vol, err := p.Volumes.CreateVolumeForPreparation(ctx, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(req.AvailabilityZone),
		Size:             aws.Int64(req.SizeGiB),
	}, req.AccountID)
	if err != nil {
		return "", fmt.Errorf("storevolume: create volume: %w", err)
	}
	volumeID := aws.StringValue(vol.VolumeId)
	if volumeID == "" {
		return "", errors.New("storevolume: create volume returned no volume ID")
	}
	// Only a volume held in creating is known to be this call's alone.
	if state := aws.StringValue(vol.State); state != string(ebsprovider.VolumeStateCreating) {
		return "", &RetainedVolumeError{VolumeID: volumeID, Err: fmt.Errorf("new volume is %q, not creating; not writing it", state)}
	}

	if err := p.fill(ctx, volumeID, image); err != nil {
		return "", p.fail(ctx, req.AccountID, err)
	}
	if err := p.Volumes.CompleteVolumePreparation(ctx, req.AccountID, volumeID); err != nil {
		// The release write may still land, so the volume may already be
		// available to others and is never discarded here.
		return "", &RetainedVolumeError{VolumeID: volumeID, Err: fmt.Errorf("complete preparation failed; state unknown, creating or available: %w", err)}
	}
	slog.InfoContext(ctx, "storevolume: prepared SQS store volume", "volumeId", volumeID, "sizeGiB", req.SizeGiB)
	return volumeID, nil
}

// prepError records whether the failed volume can still have a live export.
type prepError struct {
	volumeID     string
	err          error
	exportClosed bool
}

func (e *prepError) Error() string { return e.err.Error() }

// fill publishes the volume, copies the image and unpublishes it. Its error
// says whether the export is known to be closed.
func (p *Preparer) fill(ctx context.Context, volumeID, image string) error {
	published, err := p.Provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: p.NodeID,
	})
	if err != nil {
		// These refusals may mean another export exists, which an unpublish
		// by volume and node would tear down, so it is left alone.
		if refusedForAnotherExport(err) {
			return &prepError{volumeID: volumeID, err: fmt.Errorf("publish refused, export not touched: %w", err)}
		}
		// Otherwise the publish may still land. Unpublish names only volume
		// and node, so it is best effort, and the volume is kept regardless.
		if uerr := p.unpublish(ctx, volumeID); uerr != nil {
			err = fmt.Errorf("%w (unpublish also failed: %w)", err, uerr)
		}
		return &prepError{volumeID: volumeID, err: fmt.Errorf("publish outcome unknown: %w", err)}
	}

	var writeErr error
	if published == nil || published.Export.Kind != ebsprovider.ExportKindNBD || published.Export.NBD == nil {
		writeErr = errors.New("volume was not published as an nbd export")
	} else {
		writeErr = p.WriteImage(ctx, image, published.Export.NBD.NBDURI, nil)
	}

	if err := p.unpublish(ctx, volumeID); err != nil {
		if writeErr != nil {
			err = fmt.Errorf("write: %w (unpublish also failed: %w)", writeErr, err)
		} else {
			err = fmt.Errorf("unpublish: %w", err)
		}
		return &prepError{volumeID: volumeID, err: err}
	}
	if writeErr != nil {
		return &prepError{volumeID: volumeID, err: fmt.Errorf("write: %w", writeErr), exportClosed: true}
	}
	return nil
}

func (p *Preparer) unpublish(ctx context.Context, volumeID string) error {
	cctx, cancel := p.cleanupContext(ctx)
	defer cancel()
	return p.Provider.UnpublishVolume(cctx, ebsprovider.UnpublishVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: p.NodeID,
	})
}

// fail discards the volume when its export is known closed and otherwise
// keeps it in creating, reporting its ID.
func (p *Preparer) fail(ctx context.Context, accountID string, err error) error {
	var pe *prepError
	if !errors.As(err, &pe) {
		return err
	}
	if !pe.exportClosed {
		return &RetainedVolumeError{VolumeID: pe.volumeID, Err: pe.err}
	}
	cctx, cancel := p.cleanupContext(ctx)
	defer cancel()
	if derr := p.Volumes.DiscardVolumePreparation(cctx, accountID, pe.volumeID); derr != nil {
		return &RetainedVolumeError{VolumeID: pe.volumeID, Err: fmt.Errorf("%w (discard also failed: %w)", pe.err, derr)}
	}
	return fmt.Errorf("storevolume: prepare %s failed and the volume was discarded: %w", pe.volumeID, pe.err)
}

func refusedForAnotherExport(err error) bool {
	return errors.Is(err, ebsprovider.ErrVolumeInUse) || errors.Is(err, ebsprovider.ErrAlreadyExists)
}

// cleanupContext keeps the caller's values but not its cancellation, bounded
// so a stuck provider cannot hang the caller.
func (p *Preparer) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	limit := p.cleanupTimeout
	if limit <= 0 {
		limit = defaultCleanupTimeout
	}
	return context.WithTimeout(context.WithoutCancel(ctx), limit)
}

// buildImage writes a sparse raw file of the volume's size and formats it.
func buildImage(ctx context.Context, run func(context.Context, string, string) error, mkfs, path string, sizeGiB int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("storevolume: create image: %w", err)
	}
	if err := f.Truncate(sizeGiB << 30); err != nil {
		_ = f.Close()
		return fmt.Errorf("storevolume: size image: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("storevolume: close image: %w", err)
	}
	if err := run(ctx, mkfs, path); err != nil {
		return fmt.Errorf("storevolume: format image: %w", err)
	}
	return nil
}

func runMkfsExt4(ctx context.Context, mkfs, path string) error {
	out, err := exec.CommandContext(ctx, mkfs, "-q", "-F", "-L", "sqsstore", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", mkfs, err, out)
	}
	return nil
}

func (p *Preparer) findTool(name string) (string, error) {
	look := p.lookPath
	if look == nil {
		look = exec.LookPath
	}
	if path, err := look(name); err == nil {
		return path, nil
	}
	dirs := p.toolPaths
	if dirs == nil {
		dirs = sbinDirs
	}
	for _, dir := range dirs {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("storevolume: %w: %s", ErrToolMissing, name)
}

func (p *Preparer) mkfsRunner() func(context.Context, string, string) error {
	if p.runMkfs != nil {
		return p.runMkfs
	}
	return runMkfsExt4
}
