package nbd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
)

// WriteRawImage copies a raw image onto a published export with qemu-img,
// which speaks NBD as a target and is already a runtime dependency. Passing
// the export as the target rather than a file is what keeps the bulk data off
// NATS, whose request size the provider contract caps far below an image.
func WriteRawImage(ctx context.Context, sourcePath, nbdURI string, progress io.Writer) error {
	target, err := qemuTarget(nbdURI)
	if err != nil {
		return err
	}

	// -n keeps qemu-img from trying to create a target that already exists,
	// and --target-is-zero lets it skip the source's holes, so the caller
	// must pass only an export of a volume that has never been written.
	cmd := exec.CommandContext(ctx, "qemu-img", "convert",
		"-f", "raw", "-O", "raw", "-n", "--target-is-zero", "-p",
		sourcePath, target)
	if progress != nil {
		cmd.Stdout = progress
	}
	var stderr []byte
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("qemu-img stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start qemu-img: %w", err)
	}
	stderr, _ = io.ReadAll(stderrPipe)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("qemu-img convert: %w: %s", err, stderr)
	}
	return nil
}

// qemuTarget renders a provider NBD URI in the form qemu-img accepts. The
// contract's unix form is not qemu's, so it is rebuilt rather than passed on.
func qemuTarget(nbdURI string) (string, error) {
	serverType, socketPath, host, port, err := ParseNBDURI(nbdURI)
	if err != nil {
		return "", fmt.Errorf("parse NBD URI %q: %w", nbdURI, err)
	}
	switch serverType {
	case "unix":
		if _, err := os.Stat(socketPath); err != nil {
			return "", fmt.Errorf("NBD socket %s: %w", socketPath, err)
		}
		return "nbd+unix:///?socket=" + socketPath, nil
	case "inet":
		return "nbd://" + net.JoinHostPort(host, strconv.Itoa(port)), nil
	default:
		return "", fmt.Errorf("unsupported NBD server type %q in %q", serverType, nbdURI)
	}
}
