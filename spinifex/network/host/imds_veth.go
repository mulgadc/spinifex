package host

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// imdsPortLSPName is the OVN logical-switch-port the OVS-side veth binds to via
// external_ids:iface-id. It mirrors topology.IMDSPort but is duplicated here to
// avoid an import cycle (host ← handlers/imds ← topology).
func imdsPortLSPName(vpcID string) string { return "imds-port-" + vpcID }

// EnsureIMDSVeth idempotently creates the per-VPC IMDS veth pair and attaches the OVS
// end to br-int with external_ids:iface-id set to the imds-port LSP, so ovn-controller
// binds the localport here. Returns the host-end name the listener SO_BINDTODEVICEs to.
func EnsureIMDSVeth(ctx context.Context, vpcID string) (hostEndName string, err error) {
	ovsEnd := IMDSOVSPortName(vpcID)
	hostEnd := IMDSHostVethName(vpcID)

	// Idempotency probe: if the OVS end is already a port on br-int, both veth
	// ends and the OVS attachment exist from a prior boot — nothing to do.
	if out, probeErr := utils.SudoCommand("ovs-vsctl", "port-to-br", ovsEnd).CombinedOutput(); probeErr == nil {
		if strings.TrimSpace(string(out)) == "br-int" {
			slog.Debug("IMDS veth already present", "vpc", vpcID, "ovs_end", ovsEnd, "host_end", hostEnd)
			return hostEnd, nil
		}
	}

	if out, err := utils.SudoCommand("ip", "link", "add", ovsEnd, "type", "veth", "peer", "name", hostEnd).CombinedOutput(); err != nil {
		return "", fmt.Errorf("create IMDS veth pair %s/%s: %s: %w", ovsEnd, hostEnd, strings.TrimSpace(string(out)), err)
	}

	for _, dev := range []string{ovsEnd, hostEnd} {
		if out, err := utils.SudoCommand("ip", "link", "set", dev, "up").CombinedOutput(); err != nil {
			if cleanErr := removeIMDSVethPair(ovsEnd, hostEnd); cleanErr != nil {
				slog.Warn("Failed to clean up IMDS veth after bring-up failure", "vpc", vpcID, "err", cleanErr)
			}
			return "", fmt.Errorf("bring up IMDS veth %s: %s: %w", dev, strings.TrimSpace(string(out)), err)
		}
	}

	ifaceID := imdsPortLSPName(vpcID)
	if out, err := utils.SudoCommand("ovs-vsctl", "add-port", "br-int", ovsEnd,
		"--", "set", "Interface", ovsEnd, "external_ids:iface-id="+ifaceID).CombinedOutput(); err != nil {
		if cleanErr := removeIMDSVethPair(ovsEnd, hostEnd); cleanErr != nil {
			slog.Warn("Failed to clean up IMDS veth after OVS failure", "vpc", vpcID, "err", cleanErr)
		}
		return "", fmt.Errorf("add IMDS veth %s to br-int: %s: %w", ovsEnd, strings.TrimSpace(string(out)), err)
	}

	slog.Info("IMDS veth plumbing complete", "vpc", vpcID, "ovs_end", ovsEnd, "host_end", hostEnd, "iface_id", ifaceID)
	return hostEnd, nil
}

// RemoveIMDSVeth detaches the OVS port and deletes the veth pair. Idempotent:
// safe to call for a VPC that never had a veth on this chassis.
func RemoveIMDSVeth(ctx context.Context, vpcID string) error {
	return removeIMDSVethPair(IMDSOVSPortName(vpcID), IMDSHostVethName(vpcID))
}

// removeIMDSVethPair removes the OVS port then deletes the veth pair. Deleting
// either end removes the pair; the host end is the canonical one to delete.
func removeIMDSVethPair(ovsEnd, hostEnd string) error {
	if out, err := utils.SudoCommand("ovs-vsctl", "--if-exists", "del-port", ovsEnd).CombinedOutput(); err != nil {
		slog.Warn("Failed to remove IMDS veth from OVS", "ovs_end", ovsEnd, "err", err, "out", strings.TrimSpace(string(out)))
	}

	if out, err := utils.SudoCommand("ip", "link", "del", hostEnd).CombinedOutput(); err != nil {
		// "Cannot find device" — already gone. Treat as success (idempotent).
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "Cannot find device") {
			slog.Debug("IMDS veth already absent", "host_end", hostEnd)
			return nil
		}
		return fmt.Errorf("delete IMDS veth %s: %s: %w", hostEnd, msg, err)
	}

	slog.Info("IMDS veth removed", "ovs_end", ovsEnd, "host_end", hostEnd)
	return nil
}
