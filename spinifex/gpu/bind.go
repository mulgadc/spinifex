package gpu

import (
	"fmt"
	"os"
	"path/filepath"
)

// BindVFIO unbinds addr from its current driver and binds it to vfio-pci
// using driver_override. Returns the original driver name (empty if unbound).
// Idempotent: returns ("vfio-pci", nil) if already bound to vfio-pci.
func BindVFIO(addr string) (string, error) {
	return bindVFIO("/sys", addr)
}

// UnbindVFIO unbinds addr from vfio-pci, clears driver_override, and rebinds
// to originalDriver. If originalDriver is empty the device is left unbound.
func UnbindVFIO(addr, originalDriver string) error {
	return unbindVFIO("/sys", addr, originalDriver)
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
		if err := writeSysfs(unbindPath, addr); err != nil {
			return "", fmt.Errorf("unbind %s from %s: %w", addr, current, err)
		}
	}

	if err := writeSysfs(filepath.Join(devPath, "driver_override"), "vfio-pci"); err != nil {
		return "", fmt.Errorf("set driver_override for %s: %w", addr, err)
	}

	if err := writeSysfs(filepath.Join(sysfsRoot, "bus/pci/drivers/vfio-pci/bind"), addr); err != nil {
		return "", fmt.Errorf("bind %s to vfio-pci: %w", addr, err)
	}

	return current, nil
}

func unbindVFIO(sysfsRoot, addr, originalDriver string) error {
	devPath := filepath.Join(sysfsRoot, "bus/pci/devices", addr)

	current, err := readDriver(devPath)
	if err != nil {
		return fmt.Errorf("read driver for %s: %w", addr, err)
	}
	if current != "vfio-pci" {
		// Already unbound from vfio-pci (e.g. released by QEMU before Release was called).
		return nil
	}

	if err := writeSysfs(filepath.Join(sysfsRoot, "bus/pci/drivers/vfio-pci/unbind"), addr); err != nil {
		return fmt.Errorf("unbind %s from vfio-pci: %w", addr, err)
	}

	if err := writeSysfs(filepath.Join(devPath, "driver_override"), ""); err != nil {
		return fmt.Errorf("clear driver_override for %s: %w", addr, err)
	}

	if originalDriver != "" {
		rebindPath := filepath.Join(sysfsRoot, "bus/pci/drivers", originalDriver, "bind")
		if err := writeSysfs(rebindPath, addr); err != nil {
			return fmt.Errorf("rebind %s to %s: %w", addr, originalDriver, err)
		}
	}

	return nil
}

// writeSysfs writes value to a sysfs attribute path.
// Uses O_WRONLY|O_CREATE|O_TRUNC so it works against both real sysfs
// (file exists, kernel handles the write) and test temp dirs (file created).
func writeSysfs(path, value string) error {
	return os.WriteFile(path, []byte(value), 0o600)
}
