// Package host is L0 of the spinifex network stack: OVS bridges (br-int,
// br-ext), uplink port, and external CIDR. Bridge mode resolved here
// and invisible above L0 (ADR-0006 S2, S3).
package host

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

// UplinkMode names the physical wiring strategy attaching br-ext to the WAN.
type UplinkMode int

const (
	UplinkModeUnknown UplinkMode = iota

	// UplinkModePhysical enslaves the WAN NIC directly to br-ext (distributed NAT).
	UplinkModePhysical

	// UplinkModeVeth bridges a Linux bridge to br-ext via a veth pair (centralised NAT).
	UplinkModeVeth
)

func (m UplinkMode) String() string {
	switch m {
	case UplinkModePhysical:
		return "physical"
	case UplinkModeVeth:
		return "veth"
	default:
		return "unknown"
	}
}

// Wiring owns the host's L0 network state. Methods are idempotent.
type Wiring interface {
	// EnsureBridges ensures br-int and br-ext exist with the OVS settings
	// ovn-controller requires (br-int: fail-mode=secure, disable-in-band).
	EnsureBridges(ctx context.Context) error

	// EnsureUplinkPort attaches the uplink port to br-ext and returns its MAC.
	// L2 programs the gateway LRP with this MAC so the LRP shares L2 identity
	// with the uplink (upstream switch MAC learning).
	EnsureUplinkPort(ctx context.Context) (net.HardwareAddr, error)

	// UplinkMode returns the configured mode. L3 reads once at startup
	// (ADR-0006 S3); never re-reads at runtime.
	UplinkMode() UplinkMode

	// ExternalCIDR returns the IPv4 prefix on this node's uplink bridge.
	// Only valid after EnsureUplinkPort succeeds (absorbs boot-race vs DHCP).
	ExternalCIDR(ctx context.Context) (netip.Prefix, error)
}

// Runner executes a privileged host command and returns combined stdout/stderr.
// Stubbable for tests; production impl is execRunner in run.go.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// InterfaceReader observes kernel network state via net.InterfaceByName / /sys/class/net.
type InterfaceReader interface {
	// BridgeCIDR returns the first IPv4 prefix on the named link, or error if none.
	BridgeCIDR(name string) (netip.Prefix, error)

	// LinkMAC returns the hardware address of the named link.
	LinkMAC(name string) (net.HardwareAddr, error)

	// LinkMaster returns the master link name, or "" when none.
	LinkMaster(name string) (string, error)
}

// ErrNoUplinkAddr is returned by ExternalCIDR when the WAN bridge has no IPv4 yet.
var ErrNoUplinkAddr = fmt.Errorf("uplink bridge has no IPv4 address")
