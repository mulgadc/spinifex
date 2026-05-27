package host

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
)

// Physical implements Wiring for the "WAN NIC enslaved to br-ext" model
// (former vpcd BridgeModeDirect). The gateway LRP uses the NIC's MAC and
// the OS-assigned IP on the bridge; NAT can be distributed because each
// chassis owns its own uplink.
type Physical struct {
	// ExternalInterface is the WAN NIC name (e.g. "enp0s3") that must be an
	// OVS port on UplinkBridge. Required; an empty value is a misconfig.
	ExternalInterface string

	// UplinkBridge is the OVS bridge that holds the WAN NIC and carries the
	// IPv4 uplink address. Conventionally "br-ext"; configurable for legacy
	// deployments where the bridge holding the IP differs from the OVN
	// external bridge.
	UplinkBridge string

	// Runner executes ovs-vsctl / ip; nil falls back to NewExecRunner.
	Runner Runner

	// Reader observes kernel link state; nil falls back to NewKernelReader.
	Reader InterfaceReader
}

var _ Wiring = (*Physical)(nil)

func (p *Physical) runner() Runner {
	if p.Runner != nil {
		return p.Runner
	}
	return NewExecRunner()
}

func (p *Physical) reader() InterfaceReader {
	if p.Reader != nil {
		return p.Reader
	}
	return NewKernelReader()
}

// EnsureBridges creates br-int and br-ext idempotently with the OVS settings
// ovn-controller expects. setup-ovn.sh historically does this; the daemon
// routes through here so the responsibility is in one place.
func (p *Physical) EnsureBridges(ctx context.Context) error {
	return ensureBridges(ctx, p.runner(), p.UplinkBridge)
}

// EnsureUplinkPort verifies the WAN NIC is an OVS port on UplinkBridge and
// returns its MAC. The MAC is the L2 identity the gateway LRP must share so
// upstream switch CAM stays consistent across LRP MAC writes.
func (p *Physical) EnsureUplinkPort(ctx context.Context) (net.HardwareAddr, error) {
	if p.ExternalInterface == "" {
		return nil, fmt.Errorf("host.Physical: ExternalInterface required")
	}
	if p.UplinkBridge == "" {
		return nil, fmt.Errorf("host.Physical: UplinkBridge required")
	}

	out, err := p.runner().Run(ctx, "ovs-vsctl", "port-to-br", p.ExternalInterface)
	if err != nil {
		return nil, fmt.Errorf("port-to-br %q: %w", p.ExternalInterface, err)
	}
	br := strings.TrimSpace(string(out))
	if br != p.UplinkBridge {
		return nil, fmt.Errorf("host.Physical: %q on bridge %q, expected %q", p.ExternalInterface, br, p.UplinkBridge)
	}

	mac, err := p.reader().LinkMAC(p.ExternalInterface)
	if err != nil {
		return nil, fmt.Errorf("read MAC for %q: %w", p.ExternalInterface, err)
	}
	slog.Info("host.Physical: uplink verified", "iface", p.ExternalInterface, "bridge", br, "mac", mac.String())
	return mac, nil
}

// UplinkMode returns UplinkModePhysical (distributed NAT eligible).
func (p *Physical) UplinkMode() UplinkMode { return UplinkModePhysical }

// ExternalCIDR returns the IPv4 prefix on UplinkBridge. The OS network stack
// assigns it (netplan static or systemd-networkd DHCP) before vpcd starts in
// steady state; ErrNoUplinkAddr means the boot race is still active and the
// caller should retry.
func (p *Physical) ExternalCIDR(_ context.Context) (netip.Prefix, error) {
	if p.UplinkBridge == "" {
		return netip.Prefix{}, fmt.Errorf("host.Physical: UplinkBridge required")
	}
	return p.reader().BridgeCIDR(p.UplinkBridge)
}
