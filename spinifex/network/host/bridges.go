package host

import (
	"context"
	"fmt"
)

// ensureBridges idempotently creates br-int and uplinkBridge with the OVS
// settings ovn-controller requires:
//   - br-int: fail-mode=secure (preserve flows across ovn-controller restart)
//     and other-config:disable-in-band=true (no OVS-installed flows). These
//     are the same settings daemon.SetupComputeNode applies today; the
//     responsibility moves here in Phase 2.
//   - uplinkBridge: created if missing. ovn-bridge-mappings (managed by
//     setup-ovn.sh) targets it; the localnet port LSPs will reference it
//     once L5 lands.
//
// Shared between Physical and Veth implementations because both modes
// require identical OVS bridge state — only the uplink port wiring differs.
func ensureBridges(ctx context.Context, r Runner, uplinkBridge string) error {
	if uplinkBridge == "" {
		return fmt.Errorf("host: ensureBridges UplinkBridge required")
	}
	if _, err := r.Run(ctx, "ovs-vsctl", "--may-exist", "add-br", "br-int"); err != nil {
		return fmt.Errorf("create br-int: %w", err)
	}
	if _, err := r.Run(ctx, "ovs-vsctl", "set", "Bridge", "br-int", "fail-mode=secure"); err != nil {
		return fmt.Errorf("set br-int fail-mode: %w", err)
	}
	if _, err := r.Run(ctx, "ovs-vsctl", "set", "Bridge", "br-int", "other-config:disable-in-band=true"); err != nil {
		return fmt.Errorf("set br-int disable-in-band: %w", err)
	}
	if _, err := r.Run(ctx, "ip", "link", "set", "br-int", "up"); err != nil {
		return fmt.Errorf("bring up br-int: %w", err)
	}
	if _, err := r.Run(ctx, "ovs-vsctl", "--may-exist", "add-br", uplinkBridge); err != nil {
		return fmt.Errorf("create %s: %w", uplinkBridge, err)
	}
	if _, err := r.Run(ctx, "ip", "link", "set", uplinkBridge, "up"); err != nil {
		return fmt.Errorf("bring up %s: %w", uplinkBridge, err)
	}
	return nil
}
