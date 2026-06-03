package host

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

const (
	// imdsNetnsAddr is the host/listener side of the IMDS /30, assigned to the
	// veth host end inside the per-VPC netns. .254 is MetaDataServerIP; the /30
	// makes .253 (the LRP) directly connected so the reply path has a next-hop.
	imdsNetnsAddr = "169.254.169.254/30"
	// imdsNetnsGateway is the IMDS LRP (.253) — the netns default gateway that
	// routes guest replies back into OVN. Mirrors external.imdsLRPNetwork's .253.
	imdsNetnsGateway = "169.254.169.253"
)

// imdsPortLSPName is the OVN logical-switch-port the OVS-side veth binds to via
// external_ids:iface-id. It mirrors topology.IMDSPort but is duplicated here to
// avoid an import cycle (host ← handlers/imds ← topology).
func imdsPortLSPName(vpcID string) string { return "imds-port-" + vpcID }

// EnsureIMDSVeth idempotently creates the per-VPC IMDS veth pair, moves the host end
// into a dedicated netns with 169.254.169.254/30 + a default route via the .253 LRP,
// and attaches the OVS end to br-int so ovn-controller binds the localport here. The
// netns gives the host-served reply a real L3 next-hop (SO_BINDTODEVICE alone cannot
// route the SYN-ACK) and isolates overlapping VPC CIDRs into separate routing domains.
// Returns the netns name and host-end name the listener enters and binds.
func EnsureIMDSVeth(ctx context.Context, vpcID string) (netnsName, hostEndName string, err error) {
	ovsEnd := IMDSOVSPortName(vpcID)
	hostEnd := IMDSHostVethName(vpcID)
	netns := IMDSNetnsName(vpcID)

	// Idempotency probe: the full plumbing (veth pair, netns, host-end address +
	// route) exists from a prior boot only when the OVS end is a port on br-int
	// AND the netns is enterable. A live OVS port over a stale netns (name
	// present but setns(2) fails EINVAL — e.g. a crash between umount and unlink,
	// or a tmpfs remount across reboot) leaves the listener permanently
	// unbindable, so that case must NOT short-circuit: tear the inconsistent
	// plumbing down and rebuild it below.
	if imdsOVSPortOnBrInt(ovsEnd) {
		if netnsEnterable(netns) {
			slog.Debug("IMDS veth already present", "vpc", vpcID, "ovs_end", ovsEnd, "host_end", hostEnd, "netns", netns)
			return netns, hostEnd, nil
		}
		slog.Warn("IMDS netns unenterable behind a live OVS port (stale handle), rebuilding plumbing",
			"vpc", vpcID, "netns", netns, "ovs_end", ovsEnd)
		if err := removeIMDSPlumbing(ovsEnd, hostEnd, netns); err != nil {
			slog.Warn("Failed to tear down stale IMDS plumbing before rebuild", "vpc", vpcID, "err", err)
		}
	}

	if err := ensureNetns(netns); err != nil {
		return "", "", err
	}

	if out, err := utils.SudoCommand("ip", "link", "add", ovsEnd, "type", "veth", "peer", "name", hostEnd).CombinedOutput(); err != nil {
		if cleanErr := removeIMDSPlumbing(ovsEnd, hostEnd, netns); cleanErr != nil {
			slog.Warn("Failed to clean up IMDS plumbing after veth-create failure", "vpc", vpcID, "err", cleanErr)
		}
		return "", "", fmt.Errorf("create IMDS veth pair %s/%s: %s: %w", ovsEnd, hostEnd, strings.TrimSpace(string(out)), err)
	}

	if err := configureIMDSNetns(netns, hostEnd, ovsEnd); err != nil {
		if cleanErr := removeIMDSPlumbing(ovsEnd, hostEnd, netns); cleanErr != nil {
			slog.Warn("Failed to clean up IMDS plumbing after netns config failure", "vpc", vpcID, "err", cleanErr)
		}
		return "", "", err
	}

	ifaceID := imdsPortLSPName(vpcID)
	if out, err := utils.SudoCommand("ovs-vsctl", "add-port", "br-int", ovsEnd,
		"--", "set", "Interface", ovsEnd, "external_ids:iface-id="+ifaceID).CombinedOutput(); err != nil {
		if cleanErr := removeIMDSPlumbing(ovsEnd, hostEnd, netns); cleanErr != nil {
			slog.Warn("Failed to clean up IMDS plumbing after OVS failure", "vpc", vpcID, "err", cleanErr)
		}
		return "", "", fmt.Errorf("add IMDS veth %s to br-int: %s: %w", ovsEnd, strings.TrimSpace(string(out)), err)
	}

	slog.Info("IMDS veth plumbing complete", "vpc", vpcID, "ovs_end", ovsEnd, "host_end", hostEnd, "netns", netns, "iface_id", ifaceID)
	return netns, hostEnd, nil
}

// configureIMDSNetns moves the host end into the netns, brings it (and lo) up,
// and assigns the IMDS address + default route so the host-served reply has a
// real next-hop. The OVS end stays in the root netns for ovn-controller to bind.
// The addr/route adds tolerate "File exists" so a re-run after a partial failure
// converges rather than wedging.
func configureIMDSNetns(netns, hostEnd, ovsEnd string) error {
	if out, err := utils.SudoCommand("ip", "link", "set", ovsEnd, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("bring up IMDS OVS end %s: %s: %w", ovsEnd, strings.TrimSpace(string(out)), err)
	}
	if out, err := utils.SudoCommand("ip", "link", "set", hostEnd, "netns", netns).CombinedOutput(); err != nil {
		return fmt.Errorf("move IMDS host end %s into netns %s: %s: %w", hostEnd, netns, strings.TrimSpace(string(out)), err)
	}
	for _, dev := range []string{"lo", hostEnd} {
		if out, err := utils.SudoCommand("ip", "-n", netns, "link", "set", dev, "up").CombinedOutput(); err != nil {
			return fmt.Errorf("bring up %s in netns %s: %s: %w", dev, netns, strings.TrimSpace(string(out)), err)
		}
	}
	if err := ipNetnsTolerate(netns, "File exists", "addr", "add", imdsNetnsAddr, "dev", hostEnd); err != nil {
		return err
	}
	return ipNetnsTolerate(netns, "File exists", "route", "add", "default", "via", imdsNetnsGateway)
}

// ensureNetns creates the netns, treating "already exists" as success — but
// only when the pre-existing handle is actually enterable. A stale
// /run/netns/<ns> bind-mount (name resolves yet setns(2) fails EINVAL) returns
// "File exists" from `ip netns add` while being unusable, which would leave the
// IMDS listener permanently unbindable; that handle is torn down and recreated.
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

// imdsOVSPortOnBrInt reports whether the IMDS OVS-end veth is currently a port
// on br-int — the cheap signal that prior plumbing exists.
func imdsOVSPortOnBrInt(ovsEnd string) bool {
	out, err := utils.SudoCommand("ovs-vsctl", "port-to-br", ovsEnd).CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) == "br-int"
}

// netnsEnterable reports whether netns can be entered via setns(2). A stale
// bind-mount resolves by name but fails EINVAL; `ip -n <netns> link show lo`
// performs the setns and surfaces it. A truly-absent netns also reports false,
// which is the correct signal for both callers (recreate / rebuild).
func netnsEnterable(netns string) bool {
	return utils.SudoCommand("ip", "-n", netns, "link", "show", "lo").Run() == nil
}

// ipNetnsTolerate runs `ip -n <netns> <args...>`, treating output containing
// tolerate as success (for idempotent re-runs of addr/route adds).
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

// RemoveIMDSVeth detaches the OVS port, deletes the netns (which destroys the
// host end and thus the veth pair), and clears any root-netns remnant. Idempotent:
// safe to call for a VPC that never had a veth on this chassis.
func RemoveIMDSVeth(ctx context.Context, vpcID string) error {
	return removeIMDSPlumbing(IMDSOVSPortName(vpcID), IMDSHostVethName(vpcID), IMDSNetnsName(vpcID))
}

// removeIMDSPlumbing tears down the OVS port, the netns, and any leftover veth.
// Deleting the netns destroys the host end (and its peer); the trailing link del
// covers the partial state where the host end was never moved into the netns.
func removeIMDSPlumbing(ovsEnd, hostEnd, netns string) error {
	if out, err := utils.SudoCommand("ovs-vsctl", "--if-exists", "del-port", ovsEnd).CombinedOutput(); err != nil {
		slog.Warn("Failed to remove IMDS veth from OVS", "ovs_end", ovsEnd, "err", err, "out", strings.TrimSpace(string(out)))
	}

	if out, err := utils.SudoCommand("ip", "netns", "del", netns).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		// "No such file or directory" — already gone. Anything else is logged
		// but not fatal: the link del below still runs.
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
