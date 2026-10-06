package host

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
)

// Physical implements Wiring for the "WAN NIC enslaved to br-ext" model.
// Each chassis owns its uplink; NAT can be distributed.
type Physical struct {
	// ExternalInterface is the WAN NIC (e.g. "enp0s3") on UplinkBridge.
	ExternalInterface string

	// UplinkBridge holds the WAN NIC and IPv4 uplink (conventionally "br-ext").
	UplinkBridge string

	Runner Runner
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

// EnsureBridges creates br-int and br-ext idempotently with OVN settings.
func (p *Physical) EnsureBridges(ctx context.Context) error {
	return ensureBridges(ctx, p.runner(), p.UplinkBridge)
}

// EnsureUplinkPort verifies the WAN NIC is on UplinkBridge and returns its MAC.
// The gateway LRP shares this MAC so upstream switch CAM stays consistent.
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

// ExternalCIDR returns the IPv4 prefix on UplinkBridge.
// ErrNoUplinkAddr means the OS hasn't assigned an IP yet — caller should retry.
func (p *Physical) ExternalCIDR(_ context.Context) (netip.Prefix, error) {
	if p.UplinkBridge == "" {
		return netip.Prefix{}, fmt.Errorf("host.Physical: UplinkBridge required")
	}
	return p.reader().BridgeCIDR(p.UplinkBridge)
}
