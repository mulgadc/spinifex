package workerboot

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// SystemDisk checks with e2fsck and mounts with the mount syscall.
type SystemDisk struct {
	E2fsck string
}

var _ Disk = SystemDisk{}

// Check runs a preen check: 0 is clean and 1 corrected; anything higher,
// including a disk that is not ext4, refuses. Nothing formats.
func (d SystemDisk) Check(dev string) error {
	out, err := exec.Command(d.E2fsck, "-p", dev).CombinedOutput()
	if err == nil {
		return nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
		return nil
	}
	return fmt.Errorf("e2fsck refused: %w: %s", err, strings.Join(strings.Fields(string(out)), " "))
}

// Mount mounts the disk ext4 with no device nodes, setuid or execution.
func (SystemDisk) Mount(dev, dir string) error {
	return syscall.Mount(dev, dir, "ext4", syscall.MS_NODEV|syscall.MS_NOSUID|syscall.MS_NOEXEC, "")
}

// IsBlockDevice reports whether path is a block (not character) device node.
func IsBlockDevice(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	m := fi.Mode()
	return m&os.ModeDevice != 0 && m&os.ModeCharDevice == 0
}
