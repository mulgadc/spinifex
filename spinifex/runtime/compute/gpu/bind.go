package gpu

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// BindVFIO unbinds addr from its current driver and binds it to vfio-pci
// using driver_override. Returns the original driver name (empty if unbound).
// Idempotent: returns ("vfio-pci", nil) if already bound to vfio-pci.
func BindVFIO(addr string) (string, error) {
	return bindVFIO("/sys", addr)
}

// CurrentDriver returns the name of the driver currently bound to addr,
// or "" if the device is unbound.
func CurrentDriver(addr string) (string, error) {
	return readDriver(filepath.Join("/sys/bus/pci/devices", addr))
}

func bindVFIO(sysfsRoot, addr string) (string, error) {
	devPath := filepath.Join(sysfsRoot, "bus/pci/devices", addr)

	current, err := readDriver(devPath)
	if err != nil {
		return "", fmt.Errorf("read driver for %s: %w", addr, err)
	}
	if current == "vfio-pci" {
		return "vfio-pci", nil
	}

	if current != "" {
		unbindPath := filepath.Join(sysfsRoot, "bus/pci/drivers", current, "unbind")
		if err := sysfsWrite(unbindPath, addr); err != nil {
			return "", fmt.Errorf("unbind %s from %s: %w", addr, current, err)
		}
	}

	if err := sysfsWrite(filepath.Join(devPath, "driver_override"), "vfio-pci"); err != nil {
		return "", fmt.Errorf("set driver_override for %s: %w", addr, err)
	}

	if err := sysfsWrite(filepath.Join(sysfsRoot, "bus/pci/drivers/vfio-pci/bind"), addr); err != nil {
		return "", fmt.Errorf("bind %s to vfio-pci: %w", addr, err)
	}

	return current, nil
}

func unbindVFIO(sysfsRoot, addr, originalDriver string) error {
	devPath := filepath.Join(sysfsRoot, "bus/pci/devices", addr)

	if _, err := os.Stat(devPath); err != nil {
		return fmt.Errorf("stat device %s: %w", addr, err)
	}
	current, err := readDriver(devPath)
	if err != nil {
		return fmt.Errorf("read driver for %s: %w", addr, err)
	}
	if current != "vfio-pci" {
		return checkRestored(devPath, addr, current, originalDriver)
	}

	if err := sysfsWrite(filepath.Join(sysfsRoot, "bus/pci/drivers/vfio-pci/unbind"), addr); err != nil {
		return fmt.Errorf("unbind %s from vfio-pci: %w", addr, err)
	}

	// AMD CDNA/RDNA GPUs are left in a corrupted hardware state after QEMU exits
	// without a proper reset. Issue a PCI reset now so the original driver sees
	// a clean device. The linux-vendor-reset module (gnif/vendor-reset), if
	// loaded, intercepts this write and applies the AMD-specific reset sequence.
	if err := resetPCI(sysfsRoot, addr); err != nil {
		slog.Warn("PCI reset failed — GPU may require manual recovery",
			"pci", addr, "err", err)
	}

	// A zero-length write never reaches the kernel's store; a newline clears it.
	if err := sysfsWrite(filepath.Join(devPath, "driver_override"), "\n"); err != nil {
		return fmt.Errorf("clear driver_override for %s: %w", addr, err)
	}

	if originalDriver != "" {
		rebindPath := filepath.Join(sysfsRoot, "bus/pci/drivers", originalDriver, "bind")
		if err := sysfsWrite(rebindPath, addr); err != nil {
			return fmt.Errorf("rebind %s to %s: %w", addr, originalDriver, err)
		}
	}

	return nil
}

// checkRestored accepts a function something else already moved off vfio-pci
// only if it is back on its original driver with no override left behind.
func checkRestored(devPath, addr, current, originalDriver string) error {
	if current != originalDriver {
		return fmt.Errorf("%s is bound to %q, want original driver %q", addr, current, originalDriver)
	}
	b, err := os.ReadFile(filepath.Join(devPath, "driver_override"))
	if err != nil {
		return fmt.Errorf("read driver_override for %s: %w", addr, err)
	}
	if v := strings.TrimSpace(string(b)); v != "" && v != "(null)" {
		return fmt.Errorf("%s still has driver_override %q", addr, v)
	}
	return nil
}

// sysfsWrite performs every bind and override write; tests swap in a model of
// kernfs, whose semantics a regular file does not share.
var sysfsWrite = writeSysfs

// writeSysfs writes value to a sysfs attribute path. Works against both real
// sysfs and test temp dirs (file created if absent).
func writeSysfs(path, value string) error {
	return os.WriteFile(path, []byte(value), 0o600)
}
