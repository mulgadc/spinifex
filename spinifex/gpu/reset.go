package gpu

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// resetPCI issues a PCI Function-Level Reset for addr via sysfs. It is a no-op
// for non-AMD devices: NVIDIA and Intel GPUs handle the unbind/rebind cycle
// without requiring an explicit reset.
//
// AMD Instinct (CDNA) data-centre GPUs expose a reset sysfs node and support
// FLR correctly — writing "1" to it clears the hardware state left behind by
// QEMU, allowing the next Claim to bind a clean device. Consumer AMD GPUs
// (Polaris/Vega/Navi) have a firmware reset bug that FLR does not fully fix;
// those architectures are not the target here.
//
// Returns nil when the reset attribute is absent so callers treat this as
// best-effort.
func resetPCI(sysfsRoot, addr string) error {
	vendorBytes, _ := os.ReadFile(filepath.Join(sysfsRoot, "bus/pci/devices", addr, "vendor"))
	if strings.TrimSpace(string(vendorBytes)) != "0x1002" {
		return nil // not AMD (or unreadable vendor); unbind/rebind is sufficient
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

	slog.Info("PCI reset issued", "pci", addr)
	return nil
}
