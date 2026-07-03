package host

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
)

// Transit segment for routed-NAT mode. RFC 6598 CGN space so it cannot
// collide with user VPC CIDRs (RFC 1918) or the operator's LAN.
const (
	NATTransitCIDR        = "100.127.0.0/24"
	NATTransitGatewayIP   = "100.127.0.1"
	NATTransitGatewayCIDR = "100.127.0.1/24"

	// NATTransitHostEnd carries the transit gateway IP in the host stack.
	NATTransitHostEnd = "spx-nat-host"

	// NATTransitOVSEnd is the veth peer attached to the OVS uplink bridge.
	NATTransitOVSEnd = "spx-nat-ovs"
)

// Routed implements Wiring for the routed-NAT model: no WAN NIC is bridged;
// br-ext reaches the host via the spx-nat veth pair and the host kernel
// forwards + masquerades the transit /24 out whatever uplink it has.
type Routed struct {
	// UplinkBridge is the OVS bridge ovn-bridge-mappings targets (conventionally "br-ext").
	UplinkBridge string

	Runner Runner
	Reader InterfaceReader
}

var _ Wiring = (*Routed)(nil)

func (r *Routed) runner() Runner {
	if r.Runner != nil {
		return r.Runner
	}
	return NewExecRunner()
}

func (r *Routed) reader() InterfaceReader {
	if r.Reader != nil {
		return r.Reader
	}
	return NewKernelReader()
}

// EnsureBridges ensures br-int and UplinkBridge exist. The transit veth pair
// is provisioned by setup-ovn.sh --nat-uplink.
func (r *Routed) EnsureBridges(ctx context.Context) error {
	return ensureBridges(ctx, r.runner(), r.UplinkBridge)
}

// EnsureUplinkPort verifies the transit veth wiring — OVS end on UplinkBridge,
// host end carrying the transit gateway IP — and returns the OVS-side MAC.
func (r *Routed) EnsureUplinkPort(ctx context.Context) (net.HardwareAddr, error) {
	if r.UplinkBridge == "" {
		return nil, fmt.Errorf("host.Routed: UplinkBridge required")
	}

	out, err := r.runner().Run(ctx, "ovs-vsctl", "port-to-br", NATTransitOVSEnd)
	if err != nil {
		return nil, fmt.Errorf("port-to-br %q: not on OVS — run setup-ovn.sh --nat-uplink: %w", NATTransitOVSEnd, err)
	}
	br := strings.TrimSpace(string(out))
	if br != r.UplinkBridge {
		return nil, fmt.Errorf("host.Routed: %q on bridge %q, expected %q", NATTransitOVSEnd, br, r.UplinkBridge)
	}

	cidr, err := r.reader().BridgeCIDR(NATTransitHostEnd)
	if err != nil {
		return nil, fmt.Errorf("host.Routed: read transit CIDR on %q: %w", NATTransitHostEnd, err)
	}
	want := netip.MustParsePrefix(NATTransitGatewayCIDR)
	if cidr != want {
		return nil, fmt.Errorf("host.Routed: %q has %s, expected %s", NATTransitHostEnd, cidr, want)
	}

	mac, err := r.reader().LinkMAC(NATTransitOVSEnd)
	if err != nil {
		return nil, fmt.Errorf("read MAC for %q: %w", NATTransitOVSEnd, err)
	}
	slog.Info("host.Routed: transit uplink verified",
		"ovs_end", NATTransitOVSEnd, "host_end", NATTransitHostEnd,
		"uplink_bridge", r.UplinkBridge, "transit", NATTransitGatewayCIDR, "mac", mac.String())
	return mac, nil
}

// UplinkMode returns UplinkModeRouted (routed NAT).
func (r *Routed) UplinkMode() UplinkMode { return UplinkModeRouted }

// ExternalCIDR returns the transit prefix on the host-side veth end.
func (r *Routed) ExternalCIDR(_ context.Context) (netip.Prefix, error) {
	return r.reader().BridgeCIDR(NATTransitHostEnd)
}
