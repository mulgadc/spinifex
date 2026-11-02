package hostlink

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// FwCfgRoot is the sysfs tree the qemu_fw_cfg module exposes; removing the
// module removes every entry under it, including the guest key.
const FwCfgRoot = "/sys/firmware/qemu_fw_cfg"

// UnloadFwCfg removes the qemu_fw_cfg module from the running kernel.
func UnloadFwCfg() error {
	if err := unix.DeleteModule("qemu_fw_cfg", 0); err != nil {
		return fmt.Errorf("hostlink: unload qemu_fw_cfg: %w", err)
	}
	return nil
}

// SealFwCfg makes fw_cfg unreadable for the rest of the boot. It succeeds only
// once root is gone, so a built-in or busy module fails closed rather than
// leaving the guest key readable by code started later.
func SealFwCfg(root string, unload func() error) error {
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("hostlink: fw_cfg: %w", err)
	}
	unloadErr := unload()
	_, err := os.Stat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if unloadErr == nil {
		unloadErr = errors.New("unload reported success")
	}
	return fmt.Errorf("hostlink: fw_cfg still readable at %s: %w", root, errors.Join(unloadErr, err))
}
