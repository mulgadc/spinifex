package host

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// OVSPlumber implements vm.NetworkPlumber using system commands
// (ip, ovs-vsctl); tests use a mock satisfying the same interface.
type OVSPlumber struct{}

var _ vm.NetworkPlumber = (*OVSPlumber)(nil)

// NewOVSPlumber returns the default plumber wired to utils.SudoCommand.
func NewOVSPlumber() *OVSPlumber { return &OVSPlumber{} }

// SetupTap creates the kernel tap, brings it up, and attaches it to spec.Bridge.
// Pre-create del-port is unconditional: OVS conf.db survives reboot but kernel
// taps don't, and --may-exist would silently keep stale external_ids.
func (p *OVSPlumber) SetupTap(spec vm.TapSpec) error {
	if err := utils.SudoCommand("ovs-vsctl", "--if-exists", "del-port", spec.Bridge, spec.Name).Run(); err != nil {
		slog.Warn("Pre-create del-port failed (continuing)", "tap", spec.Name, "bridge", spec.Bridge, "err", err)
	}
	if _, err := os.Stat("/sys/class/net/" + spec.Name); err == nil {
		if err := utils.SudoCommand("ip", "tuntap", "del", "dev", spec.Name, "mode", "tap").Run(); err != nil {
			slog.Warn("Pre-create tap del failed (continuing)", "tap", spec.Name, "err", err)
		}
	}

	// Non-root daemon: QEMU inherits the uid without CAP_NET_ADMIN, so the tap
	// must be owned by the calling euid (kernel TUNSETIFF check). Use numeric
	// uid/gid to avoid NSS lookup failures in hardened systemd units.
	addArgs := []string{"tuntap", "add", "dev", spec.Name, "mode", "tap"}
	if uid := os.Geteuid(); uid != 0 {
		addArgs = append(addArgs, "user", strconv.Itoa(uid), "group", strconv.Itoa(os.Getegid()))
	}
	if out, err := utils.SudoCommand("ip", addArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("create tap %s: %s: %w", spec.Name, strings.TrimSpace(string(out)), err)
	}

	if out, err := utils.SudoCommand("ip", "link", "set", spec.Name, "up").CombinedOutput(); err != nil {
		if cleanErr := utils.SudoCommand("ip", "tuntap", "del", "dev", spec.Name, "mode", "tap").Run(); cleanErr != nil {
			slog.Warn("Failed to clean up tap after bring-up failure", "tap", spec.Name, "err", cleanErr)
		}
		return fmt.Errorf("bring up tap %s: %s: %w", spec.Name, strings.TrimSpace(string(out)), err)
	}

	addPortArgs := []string{"add-port", spec.Bridge, spec.Name}
	if len(spec.ExternalIDs) > 0 {
		addPortArgs = append(addPortArgs, "--", "set", "Interface", spec.Name)
		// Sort keys for deterministic command construction (test assertions, log readability).
		keys := make([]string, 0, len(spec.ExternalIDs))
		for k := range spec.ExternalIDs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			addPortArgs = append(addPortArgs, fmt.Sprintf("external_ids:%s=%s", k, spec.ExternalIDs[k]))
		}
	}
	if out, err := utils.SudoCommand("ovs-vsctl", addPortArgs...).CombinedOutput(); err != nil {
		if cleanErr := utils.SudoCommand("ip", "tuntap", "del", "dev", spec.Name, "mode", "tap").Run(); cleanErr != nil {
			slog.Warn("Failed to clean up tap after OVS failure", "tap", spec.Name, "err", cleanErr)
		}
		return fmt.Errorf("add tap %s to %s: %s: %w", spec.Name, spec.Bridge, strings.TrimSpace(string(out)), err)
	}

	slog.Info("Tap plumbing complete", "tap", spec.Name, "bridge", spec.Bridge, "external_ids", spec.ExternalIDs)
	return nil
}

// CleanupTap removes the named tap from OVS (any bridge) and the kernel.
// Idempotent: callers may invoke for an instance that never reached SetupTap
// (e.g. terminate that races mid-launch).
func (p *OVSPlumber) CleanupTap(name string) error {
	if out, err := utils.SudoCommand("ovs-vsctl", "--if-exists", "del-port", name).CombinedOutput(); err != nil {
		slog.Warn("Failed to remove tap from OVS", "tap", name, "err", err, "out", strings.TrimSpace(string(out)))
	}

	if _, err := os.Stat("/sys/class/net/" + name); os.IsNotExist(err) {
		slog.Info("Tap already absent", "tap", name)
		return nil
	}

	if out, err := utils.SudoCommand("ip", "tuntap", "del", "dev", name, "mode", "tap").CombinedOutput(); err != nil {
		return fmt.Errorf("delete tap %s: %s: %w", name, strings.TrimSpace(string(out)), err)
	}

	slog.Info("Tap cleanup complete", "tap", name)
	return nil
}
