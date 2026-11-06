package hostlink

import (
	"errors"
	"fmt"
	"io"
	"os"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"golang.org/x/sys/unix"
)

// On a VMM without fw_cfg the executor attaches the same entries as a tar
// archive on a second disk, which the runtime image's init extracts here in
// the fw_cfg sysfs layout, so the fw_cfg readers work unchanged.
const (
	ConfigDiskRoot   = "/spinifex-cfg"
	ConfigDiskDir    = ConfigDiskRoot + "/by_name/" + lambdav1.FwCfgPrefix
	ConfigDiskDevice = "/dev/vdb"
)

// zeroChunk bounds each write while erasing the configuration disk.
const zeroChunk = 64 << 10

// unmount is a var so tests without privilege can stand in for the tmpfs.
var unmount = unix.Unmount

// SealConfigDisk overwrites the whole configuration disk with zeros, then
// removes the extracted tree. The tree stays if the disk cannot be erased, so
// SealFwCfg's check on root fails closed.
func SealConfigDisk(device, root string) error {
	if err := zeroDevice(device); err != nil {
		return fmt.Errorf("hostlink: erase configuration disk: %w", err)
	}
	if err := unmount(root, 0); err != nil && !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("hostlink: unmount %s: %w", root, err)
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("hostlink: remove %s: %w", root, err)
	}
	return nil
}

func zeroDevice(device string) error {
	f, err := os.OpenFile(device, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = f.Close()
		return err
	}
	zeros := make([]byte, zeroChunk)
	for left := size; left > 0; {
		n := min(left, int64(len(zeros)))
		if _, err := f.Write(zeros[:n]); err != nil {
			_ = f.Close()
			return err
		}
		left -= n
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
