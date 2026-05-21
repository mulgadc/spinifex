package host

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
)

// Default veth pair endpoint names. veth-wan-ovs is the OVS-side endpoint
// (attached to UplinkBridge / br-ext); veth-wan-br is the Linux-side
// endpoint enslaved to the Linux bridge that holds the WAN IP.
const (
	VethOVSEnd   = "veth-wan-ovs"
	VethLinuxEnd = "veth-wan-br"
)

// Veth implements Wiring for the "Linux bridge bridged into OVS via veth"
// model (former vpcd BridgeModeVeth). The OS-managed Linux bridge holds the
// IPv4 uplink; OVS sees a veth peer. NAT is centralised because only one
// chassis owns the bridge IP.
type Veth struct {
	// LinuxBridge is the Linux bridge holding the OS-assigned WAN IPv4
	// (e.g. "br-wan"). veth-wan-br must be enslaved to it.
	LinuxBridge string

	// UplinkBridge is the OVS bridge that ovn-bridge-mappings targets and
	// that veth-wan-ovs attaches to. Conventionally "br-ext".
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

// EnsureBridges ensures br-int and UplinkBridge exist. The veth pair itself
// is provisioned by spinifex-veth-wan.service (a oneshot systemd unit); this
// method only verifies the OVS bridges that the veth peers will attach to.
func (v *Veth) EnsureBridges(ctx context.Context) error {
	return ensureBridges(ctx, v.runner(), v.UplinkBridge)
}

// EnsureUplinkPort verifies that the veth pair is wired as expected
// (veth-wan-ovs → UplinkBridge, veth-wan-br → LinuxBridge) and returns the
// MAC of the OVS-side endpoint. The gateway LRP uses this MAC so that frames
// egressing OVS carry the same source-L2 the upstream router learned.
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

// ExternalCIDR returns the IPv4 prefix on LinuxBridge — the OS network
// stack (netplan or networkd) owns this address; Spinifex never assigns it.
func (v *Veth) ExternalCIDR(_ context.Context) (netip.Prefix, error) {
	if v.LinuxBridge == "" {
		return netip.Prefix{}, fmt.Errorf("host.Veth: LinuxBridge required")
	}
	return v.reader().BridgeCIDR(v.LinuxBridge)
}
