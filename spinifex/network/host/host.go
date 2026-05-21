// Package host is L0 of the spinifex network stack: kernel/OVS host wiring.
// It owns the host's OVS bridges (br-int, br-ext), the uplink port that
// attaches br-ext to the WAN, and the external CIDR readable off the WAN
// bridge. Bridge mode (physical vs veth) is resolved here and is invisible
// above L0; layers L1+ receive UplinkMode as an init-time enum.
//
// See docs/development/feature/spinifex-network-redesign.md §5 and
// docs/development/proposals/az-local-architecture/0006-spinifex-network-layer-contract.md
// (S2, S3) for the full contract.
package host

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

// UplinkMode names the physical wiring strategy that attaches br-ext to the
// WAN. The two supported values map to former vpcd bridge modes "direct" and
// "veth"; macvlan was removed in Phase 0.
type UplinkMode int

const (
	// UplinkModeUnknown is the zero value; never returned by a configured
	// Wiring implementation.
	UplinkModeUnknown UplinkMode = iota

	// UplinkModePhysical enslaves the WAN NIC directly to br-ext. Distributed
	// NAT is available — each compute node's gateway LRP can SNAT locally
	// using its own uplink MAC/IP.
	UplinkModePhysical

	// UplinkModeVeth bridges a Linux bridge (e.g. br-wan, holding the OS-
	// assigned WAN IP) to br-ext via a veth pair. Centralised NAT is used —
	// only one chassis owns the gateway LRP, and that chassis owns the
	// uplink IP.
	UplinkModeVeth
)

// String returns the canonical name used in logs and config diagnostics.
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

// Wiring owns the host's L0 network state. Implementations are constructed
// once at vpcd startup (Phase 2) and consulted by L2/L5 for uplink CIDR and
// by L3 for NAT distribution mode (via UplinkMode at init time).
//
// Every method is idempotent: a second call with the same host state is a
// no-op. EnsureBridges and EnsureUplinkPort may be called before any other
// method and may be safely re-invoked on reconcile.
type Wiring interface {
	// EnsureBridges ensures br-int and br-ext exist with the OVS settings
	// required by ovn-controller (br-int: fail-mode=secure, disable-in-band).
	// br-int is the OVN integration bridge; br-ext is the external bridge
	// that ovn-bridge-mappings targets and that the uplink port attaches to.
	EnsureBridges(ctx context.Context) error

	// EnsureUplinkPort ensures the uplink port (physical NIC or veth-wan-ovs)
	// is attached to br-ext and returns its MAC address. The MAC is used by
	// L2 when programming the gateway LRP so that the LRP shares L2 identity
	// with the uplink, satisfying upstream switch MAC learning.
	EnsureUplinkPort(ctx context.Context) (net.HardwareAddr, error)

	// UplinkMode returns the configured mode. L3 reads this once at startup
	// to decide NAT distribution (distributed for physical, centralised for
	// veth); it never re-reads at runtime (ADR-0006 S3).
	UplinkMode() UplinkMode

	// ExternalCIDR returns the IPv4 prefix assigned to this node's uplink
	// bridge by the OS network stack (netplan static config or
	// systemd-networkd DHCP). Only valid after EnsureUplinkPort has returned
	// successfully — that call blocks the boot race where vpcd starts before
	// the WAN address is assigned.
	ExternalCIDR(ctx context.Context) (netip.Prefix, error)
}

// Runner executes a privileged host command (ovs-vsctl, ip) and returns
// combined stdout/stderr. It exists so tests can substitute a stub without
// invoking sudo or mutating the host's OVS conf.db. The production
// implementation is execRunner in run.go.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// InterfaceReader observes kernel network state (interface IP, link master).
// Split from Runner because the production reader uses net.InterfaceByName
// and /sys/class/net rather than shelling out; testability is the same but
// the implementations are unrelated.
type InterfaceReader interface {
	// BridgeCIDR returns the first IPv4 prefix assigned to the named link,
	// or an error when the link has no IPv4 address yet (boot race).
	BridgeCIDR(name string) (netip.Prefix, error)

	// LinkMAC returns the hardware address of the named link.
	LinkMAC(name string) (net.HardwareAddr, error)

	// LinkMaster returns the master link name (the bridge a port is
	// enslaved to), or "" when the link has no master.
	LinkMaster(name string) (string, error)
}

// ErrNoUplinkAddr is returned by ExternalCIDR when the WAN bridge has no
// IPv4 address yet. Callers (Phase 2's startup path) retry with a bounded
// timeout to absorb the boot race between vpcd start and OS DHCP/netplan
// completion.
var ErrNoUplinkAddr = fmt.Errorf("uplink bridge has no IPv4 address")
