// Package imagewrite copies a raw disk image onto a published EBS volume
// export, whichever kind the provider publishes. The image import and the SQS
// store volume share it, so both write every export kind the same way.
package imagewrite

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/nbd"
)

// QemuImg runs qemu-img with args, sending its standard output to progress
// when progress is not nil.
type QemuImg func(ctx context.Context, progress io.Writer, args ...string) error

// Writer copies a raw image onto a published export. Its zero value writes
// through the host's qemu-img.
type Writer struct {
	// NBD writes to an nbd export. Nil uses nbd.WriteRawImage.
	NBD func(ctx context.Context, sourcePath, nbdURI string, progress io.Writer) error
	// QemuImg runs qemu-img for a block export. Nil runs it on this host.
	QemuImg QemuImg
}

// Write copies the raw image at sourcePath onto export, dispatching on its
// kind. An export that fails validation, or of a kind with no writer, is an
// error and nothing is written.
func (w Writer) Write(ctx context.Context, sourcePath string, export ebsprovider.VolumeExport, progress io.Writer) error {
	if err := export.Validate(); err != nil {
		return fmt.Errorf("image write: %w", err)
	}
	switch export.Kind {
	case ebsprovider.ExportKindNBD:
		write := w.NBD
		if write == nil {
			write = nbd.WriteRawImage
		}
		return write(ctx, sourcePath, export.NBD.NBDURI, progress)
	case ebsprovider.ExportKindBlock:
		return w.writeBlock(ctx, sourcePath, *export.Block, progress)
	default:
		return fmt.Errorf("image write: no writer for a %q export", export.Kind)
	}
}

// writeBlock copies the image onto the export's device. The guest sees only
// SizeBytes of the device, so an image larger than that is refused unwritten.
func (w Writer) writeBlock(ctx context.Context, sourcePath string, export ebsprovider.BlockExport, progress io.Writer) error {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return fmt.Errorf("image write: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("image write: source %s is not a regular file", sourcePath)
	}
	size := info.Size()
	if uint64(size) > export.SizeBytes { //nolint:gosec // G115: a regular file's size is never negative
		return fmt.Errorf("image write: image %s is %d bytes, larger than the %d-byte export at %s",
			sourcePath, size, export.SizeBytes, export.DevicePath)
	}
	run := w.QemuImg
	if run == nil {
		run = runQemuImg
	}
	// -n writes into the existing device rather than creating a target. A
	// device is not known to read as zeros, so holes are written too.
	if err := run(ctx, progress, "convert", "-f", "raw", "-O", "raw", "-n", "-p", sourcePath, export.DevicePath); err != nil {
		return fmt.Errorf("image write to %s: %w", export.DevicePath, err)
	}
	return nil
}

func runQemuImg(ctx context.Context, progress io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "qemu-img", args...)
	if progress != nil {
		cmd.Stdout = progress
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("qemu-img: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}
