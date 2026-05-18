package gpu

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// resetPCI issues a PCI reset for addr via sysfs. It is a no-op for non-AMD
// devices: NVIDIA and Intel handle FLR correctly through the standard unbind /
// rebind cycle, so vendor-reset is not needed for them.
//
// For AMD (vendor 0x1002), CDNA/RDNA GPUs are left in a corrupted hardware
// state after QEMU exits without a proper reset. If the linux-vendor-reset
// module (github.com/gnif/vendor-reset) is loaded it intercepts the sysfs
// write and applies the AMD-specific reset sequence; otherwise the kernel falls
// back to a standard Function-Level Reset, which may be insufficient.
//
// Returns nil when the reset attribute is absent — the device does not support
// FLR — so callers treat this as best-effort.
func resetPCI(sysfsRoot, addr string) error {
	vendorBytes, err := os.ReadFile(filepath.Join(sysfsRoot, "bus/pci/devices", addr, "vendor"))
	if err != nil || strings.TrimSpace(string(vendorBytes)) != "0x1002" {
		return nil // not AMD; standard unbind/rebind is sufficient
	}

	vendorResetLoaded := false
	if _, err := os.Stat(filepath.Join(sysfsRoot, "module/vendor_reset")); err == nil {
		vendorResetLoaded = true
	}
	if !vendorResetLoaded {
		slog.Warn("vendor-reset module not loaded — AMD GPU reset may be incomplete; install linux-vendor-reset DKMS module",
			"pci", addr)
	}

	resetPath := filepath.Join(sysfsRoot, "bus/pci/devices", addr, "reset")
	if _, err := os.Stat(resetPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("AMD GPU does not expose PCI FLR node — GPU may not reset cleanly",
				"pci", addr)
			return nil
		}
		return fmt.Errorf("stat reset node for %s: %w", addr, err)
	}

	if err := os.WriteFile(resetPath, []byte("1"), 0o600); err != nil {
		return fmt.Errorf("PCI reset for %s: %w", addr, err)
	}

	slog.Info("PCI reset issued", "pci", addr, "vendor_reset", vendorResetLoaded)
	return nil
}
