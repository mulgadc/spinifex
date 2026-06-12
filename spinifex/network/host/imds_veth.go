package host

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// imdsNetnsAddr is the IMDS address assigned to the veth host end inside the netns.
// Reply path resolves the guest via the subnet CIDR added on-link.
const imdsNetnsAddr = "169.254.169.254/30"

// imdsPortLSPName returns the LSP name the OVS-side veth binds to.
// Duplicated from topology.IMDSPort to avoid a host←handlers/imds←topology cycle.
func imdsPortLSPName(subnetID string) string { return "imds-port-" + subnetID }

// imdsLSPMAC returns the logical MAC OVN uses for 169.254.169.254. The host-end veth
// must carry this MAC exactly — OVN delivers frames with dl_dst set to it.
// Duplicated from external.IMDSSpecForSubnet to avoid the import cycle.
func imdsLSPMAC(subnetID string) string { return utils.HashMAC("imds-" + subnetID) }

// EnsureIMDSVeth idempotently creates the per-subnet IMDS veth pair: moves the host
// end into a dedicated netns with 169.254.169.254/30 + subnet CIDR on-link, and
// attaches the OVS end to br-int. Returns netns and host-end names.
func EnsureIMDSVeth(ctx context.Context, subnetID string, cidr netip.Prefix) (netnsName, hostEndName string, err error) {
	if !cidr.IsValid() {
		return "", "", fmt.Errorf("EnsureIMDSVeth: subnet %s requires a valid CIDR", subnetID)
	}
	ovsEnd := IMDSOVSPortName(subnetID)
	hostEnd := IMDSHostVethName(subnetID)
	netns := IMDSNetnsName(subnetID)

	// Idempotency: plumbing is complete only when both the OVS port is on br-int
	// AND the netns is enterable. A live OVS port over an unenterable netns
	// (stale /run/netns handle, setns EINVAL) must not short-circuit — tear and rebuild.
	if imdsOVSPortOnBrInt(ovsEnd) {
		if netnsEnterable(netns) {
			slog.Debug("IMDS veth already present", "subnet", subnetID, "ovs_end", ovsEnd, "host_end", hostEnd, "netns", netns)
			return netns, hostEnd, nil
		}
		slog.Warn("IMDS netns unenterable behind a live OVS port (stale handle), rebuilding plumbing",
			"subnet", subnetID, "netns", netns, "ovs_end", ovsEnd)
		if err := removeIMDSPlumbing(ovsEnd, hostEnd, netns); err != nil {
			slog.Warn("Failed to tear down stale IMDS plumbing before rebuild", "subnet", subnetID, "err", err)
		}
	}

	if err := ensureNetns(netns); err != nil {
		return "", "", err
	}

	if out, err := utils.SudoCommand("ip", "link", "add", ovsEnd, "type", "veth", "peer", "name", hostEnd).CombinedOutput(); err != nil {
		if cleanErr := removeIMDSPlumbing(ovsEnd, hostEnd, netns); cleanErr != nil {
			slog.Warn("Failed to clean up IMDS plumbing after veth-create failure", "subnet", subnetID, "err", cleanErr)
		}
		return "", "", fmt.Errorf("create IMDS veth pair %s/%s: %s: %w", ovsEnd, hostEnd, strings.TrimSpace(string(out)), err)
	}

	if err := configureIMDSNetns(netns, hostEnd, ovsEnd, imdsLSPMAC(subnetID), cidr); err != nil {
		if cleanErr := removeIMDSPlumbing(ovsEnd, hostEnd, netns); cleanErr != nil {
			slog.Warn("Failed to clean up IMDS plumbing after netns config failure", "subnet", subnetID, "err", cleanErr)
		}
		return "", "", err
	}

	ifaceID := imdsPortLSPName(subnetID)
	if out, err := utils.SudoCommand("ovs-vsctl", "add-port", "br-int", ovsEnd,
		"--", "set", "Interface", ovsEnd, "external_ids:iface-id="+ifaceID).CombinedOutput(); err != nil {
		if cleanErr := removeIMDSPlumbing(ovsEnd, hostEnd, netns); cleanErr != nil {
			slog.Warn("Failed to clean up IMDS plumbing after OVS failure", "subnet", subnetID, "err", cleanErr)
		}
		return "", "", fmt.Errorf("add IMDS veth %s to br-int: %s: %w", ovsEnd, strings.TrimSpace(string(out)), err)
	}

	slog.Info("IMDS veth plumbing complete", "subnet", subnetID, "ovs_end", ovsEnd, "host_end", hostEnd, "netns", netns, "iface_id", ifaceID)
	return netns, hostEnd, nil
}

// configureIMDSNetns moves the host end into the netns, sets its MAC (OVN delivers
// frames with dl_dst = localport MAC, so the veth must own it), brings both lo and
// hostEnd up, and adds the IMDS addr + subnet CIDR on-link. Tolerates "File exists".
func configureIMDSNetns(netns, hostEnd, ovsEnd, hostEndMAC string, cidr netip.Prefix) error {
	if out, err := utils.SudoCommand("ip", "link", "set", ovsEnd, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("bring up IMDS OVS end %s: %s: %w", ovsEnd, strings.TrimSpace(string(out)), err)
	}
	if out, err := utils.SudoCommand("ip", "link", "set", hostEnd, "netns", netns).CombinedOutput(); err != nil {
		return fmt.Errorf("move IMDS host end %s into netns %s: %s: %w", hostEnd, netns, strings.TrimSpace(string(out)), err)
	}
	if out, err := utils.SudoCommand("ip", "-n", netns, "link", "set", hostEnd, "address", hostEndMAC).CombinedOutput(); err != nil {
		return fmt.Errorf("set IMDS host end %s MAC %s in netns %s: %s: %w", hostEnd, hostEndMAC, netns, strings.TrimSpace(string(out)), err)
	}
	for _, dev := range []string{"lo", hostEnd} {
		if out, err := utils.SudoCommand("ip", "-n", netns, "link", "set", dev, "up").CombinedOutput(); err != nil {
			return fmt.Errorf("bring up %s in netns %s: %s: %w", dev, netns, strings.TrimSpace(string(out)), err)
		}
	}
	if err := ipNetnsTolerate(netns, "File exists", "addr", "add", imdsNetnsAddr, "dev", hostEnd); err != nil {
		return err
	}
	return ipNetnsTolerate(netns, "File exists", "route", "add", cidr.String(), "dev", hostEnd)
}

// ensureNetns creates the netns, treating "already exists" as success only if the
// handle is enterable. A stale bind-mount (setns EINVAL) is torn down and recreated.
func ensureNetns(netns string) error {
	out, err := utils.SudoCommand("ip", "netns", "add", netns).CombinedOutput()
	if err == nil {
		return nil
	}
	if !strings.Contains(string(out), "File exists") {
		return fmt.Errorf("create IMDS netns %s: %s: %w", netns, strings.TrimSpace(string(out)), err)
	}
	if netnsEnterable(netns) {
		return nil
	}
	slog.Warn("IMDS netns present but unenterable (stale handle), recreating", "netns", netns)
	if out, err := utils.SudoCommand("ip", "netns", "del", netns).CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); !strings.Contains(msg, "No such file") {
			return fmt.Errorf("delete stale IMDS netns %s: %s: %w", netns, msg, err)
		}
	}
	if out, err := utils.SudoCommand("ip", "netns", "add", netns).CombinedOutput(); err != nil {
		return fmt.Errorf("recreate IMDS netns %s: %s: %w", netns, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// imdsOVSPortOnBrInt reports whether the OVS-end veth is on br-int.
func imdsOVSPortOnBrInt(ovsEnd string) bool {
	out, err := utils.SudoCommand("ovs-vsctl", "port-to-br", ovsEnd).CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) == "br-int"
}

// netnsEnterable reports whether netns can be entered via setns(2).
// A stale bind-mount or absent netns both return false.
func netnsEnterable(netns string) bool {
	return utils.SudoCommand("ip", "-n", netns, "link", "show", "lo").Run() == nil
}

// ipNetnsTolerate runs `ip -n <netns> <args...>`, treating output containing
// tolerate as success (idempotent addr/route adds).
func ipNetnsTolerate(netns, tolerate string, args ...string) error {
	full := append([]string{"-n", netns}, args...)
	if out, err := utils.SudoCommand("ip", full...).CombinedOutput(); err != nil {
		if strings.Contains(string(out), tolerate) {
			return nil
		}
		return fmt.Errorf("ip %s: %s: %w", strings.Join(full, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

// RemoveIMDSVeth detaches the OVS port, deletes the netns, and clears any leftover
// veth. Idempotent.
func RemoveIMDSVeth(ctx context.Context, subnetID string) error {
	return removeIMDSPlumbing(IMDSOVSPortName(subnetID), IMDSHostVethName(subnetID), IMDSNetnsName(subnetID))
}

// removeIMDSPlumbing removes the OVS port, netns, and any leftover veth.
// The trailing link del covers the case where the host end was never moved to the netns.
func removeIMDSPlumbing(ovsEnd, hostEnd, netns string) error {
	if out, err := utils.SudoCommand("ovs-vsctl", "--if-exists", "del-port", ovsEnd).CombinedOutput(); err != nil {
		slog.Warn("Failed to remove IMDS veth from OVS", "ovs_end", ovsEnd, "err", err, "out", strings.TrimSpace(string(out)))
	}

	if out, err := utils.SudoCommand("ip", "netns", "del", netns).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		// "No such file or directory" — already gone; other errors logged but not fatal.
		if !strings.Contains(msg, "No such file") {
			slog.Warn("Failed to delete IMDS netns", "netns", netns, "err", err, "out", msg)
		}
	}

	if out, err := utils.SudoCommand("ip", "link", "del", ovsEnd).CombinedOutput(); err != nil {
		// "Cannot find device" — the pair was already destroyed with the netns.
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "Cannot find device") {
			slog.Debug("IMDS veth already absent", "ovs_end", ovsEnd)
			return nil
		}
		return fmt.Errorf("delete IMDS veth %s: %s: %w", ovsEnd, msg, err)
	}

	slog.Info("IMDS veth removed", "ovs_end", ovsEnd, "host_end", hostEnd, "netns", netns)
	return nil
}
