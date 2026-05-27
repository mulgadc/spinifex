package host

import (
	"context"
	"fmt"
)

// ensureBridges idempotently creates br-int and uplinkBridge with OVN settings.
// br-int needs fail-mode=secure + disable-in-band (canonical OVN integration bridge).
// Shared by Physical and Veth — only uplink port wiring differs.
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
