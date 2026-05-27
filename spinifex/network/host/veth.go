package host

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
)

// veth-wan-ovs is the OVS-side endpoint; veth-wan-br is the Linux-bridge-side endpoint.
const (
	VethOVSEnd   = "veth-wan-ovs"
	VethLinuxEnd = "veth-wan-br"
)

// Veth implements Wiring for the "Linux bridge bridged into OVS via veth" model.
// OS-managed Linux bridge holds the IPv4 uplink; NAT is centralised (one chassis owns the IP).
type Veth struct {
	// LinuxBridge holds the OS-assigned WAN IPv4 (e.g. "br-wan").
	LinuxBridge string

	// UplinkBridge is the OVS bridge ovn-bridge-mappings targets (conventionally "br-ext").
	UplinkBridge string

	Runner Runner
	Reader InterfaceReader
}

var _ Wiring = (*Veth)(nil)

func (v *Veth) runner() Runner {
	if v.Runner != nil {
		return v.Runner
	}
	return NewExecRunner()
}

func (v *Veth) reader() InterfaceReader {
	if v.Reader != nil {
		return v.Reader
	}
	return NewKernelReader()
}

// EnsureBridges ensures br-int and UplinkBridge exist. The veth pair is
// provisioned by spinifex-veth-wan.service.
func (v *Veth) EnsureBridges(ctx context.Context) error {
	return ensureBridges(ctx, v.runner(), v.UplinkBridge)
}

// EnsureUplinkPort verifies the veth pair wiring and returns the OVS-side MAC.
// The gateway LRP uses this MAC so OVS-egress frames carry the L2 src the
// upstream router learned.
func (v *Veth) EnsureUplinkPort(ctx context.Context) (net.HardwareAddr, error) {
	if v.LinuxBridge == "" {
		return nil, fmt.Errorf("host.Veth: LinuxBridge required")
	}
	if v.UplinkBridge == "" {
		return nil, fmt.Errorf("host.Veth: UplinkBridge required")
	}

	out, err := v.runner().Run(ctx, "ovs-vsctl", "port-to-br", VethOVSEnd)
	if err != nil {
		return nil, fmt.Errorf("port-to-br %q: not on OVS — is spinifex-veth-wan.service running? %w", VethOVSEnd, err)
	}
	br := strings.TrimSpace(string(out))
	if br != v.UplinkBridge {
		return nil, fmt.Errorf("host.Veth: %q on bridge %q, expected %q", VethOVSEnd, br, v.UplinkBridge)
	}

	master, err := v.reader().LinkMaster(VethLinuxEnd)
	if err != nil {
		return nil, fmt.Errorf("host.Veth: read master for %q: %w", VethLinuxEnd, err)
	}
	if master != v.LinuxBridge {
		return nil, fmt.Errorf("host.Veth: %q master is %q, expected %q", VethLinuxEnd, master, v.LinuxBridge)
	}

	mac, err := v.reader().LinkMAC(VethOVSEnd)
	if err != nil {
		return nil, fmt.Errorf("read MAC for %q: %w", VethOVSEnd, err)
	}
	slog.Info("host.Veth: uplink verified",
		"ovs_end", VethOVSEnd, "linux_end", VethLinuxEnd,
		"linux_bridge", v.LinuxBridge, "uplink_bridge", v.UplinkBridge, "mac", mac.String())
	return mac, nil
}

// UplinkMode returns UplinkModeVeth (centralised NAT).
func (v *Veth) UplinkMode() UplinkMode { return UplinkModeVeth }

// ExternalCIDR returns the IPv4 prefix on LinuxBridge (OS-assigned).
func (v *Veth) ExternalCIDR(_ context.Context) (netip.Prefix, error) {
	if v.LinuxBridge == "" {
		return netip.Prefix{}, fmt.Errorf("host.Veth: LinuxBridge required")
	}
	return v.reader().BridgeCIDR(v.LinuxBridge)
}
