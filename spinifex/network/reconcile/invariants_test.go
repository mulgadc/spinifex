package reconcile

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"

	imds "github.com/mulgadc/spinifex/spinifex/handlers/imds"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// IMDS-datapath invariants (imds-v1 plan, Step 9). A guest LSP is created by two
// independent paths — the live topology manager (EnsurePort) and the reconciler
// (applyPorts) — and the per-subnet DHCPOptions row likewise. Both paths are
// exercised here so neither can drift away from the contract the IMDS handler
// trusts. These live in the reconcile package because it is the only network
// package that can import both topology (the live path) and itself (the
// reconciler path) without a cycle.

// TestI1_GuestLSPMustHavePortSecurity asserts that every guest-attached LSP,
// created by either path, carries port_security equal to its addresses
// ("<MAC> <IP>"). This is the load-bearing security boundary: ovn-controller
// drops any frame whose eth.src/ip4.src doesn't match port_security at the
// ingress LSP, so a compromised guest cannot forge a peer's source IP. Without
// it the IMDS (VPC-ID, source-IP) → ENI mapping is forgeable.
func TestI1_GuestLSPMustHavePortSecurity(t *testing.T) {
	ctx := context.Background()

	// Live path: topology.EnsurePort.
	t.Run("live", func(t *testing.T) {
		m := mock.New()
		mgr := topology.NewLiveManager(m)
		vpc := topology.VPCSpec{VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.0.0/16")}
		sub := topology.SubnetSpec{SubnetID: "subnet-a", VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.1.0/24")}
		mac, _ := net.ParseMAC("02:00:00:00:00:01")
		port := topology.PortSpec{
			PortID: "eni-a", SubnetID: "subnet-a", VPCID: "vpc-a",
			PrivateIP: netip.MustParseAddr("10.0.1.5"), MAC: mac,
		}
		if err := mgr.EnsureVPC(ctx, vpc); err != nil {
			t.Fatalf("EnsureVPC: %v", err)
		}
		if err := mgr.EnsureSubnet(ctx, sub); err != nil {
			t.Fatalf("EnsureSubnet: %v", err)
		}
		if err := mgr.EnsurePort(ctx, port); err != nil {
			t.Fatalf("EnsurePort: %v", err)
		}
		assertPortSecurity(t, m, topology.Port(port.PortID), "02:00:00:00:00:01 10.0.1.5")
	})

	// Reconciler path: applyPorts.
	t.Run("reconciler", func(t *testing.T) {
		rec, m := newTestReconciler(t)
		intent := freshIntent(t)
		if err := rec.Reconcile(ctx, intent); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		port := intent.Ports["eni-a"]
		want := port.MAC.String() + " " + port.PrivateIP.String()
		assertPortSecurity(t, m, topology.Port(port.PortID), want)
	})
}

// assertPortSecurity fails with the ADR clause when the LSP lacks port_security
// equal to its addresses.
func assertPortSecurity(t *testing.T, m *mock.Client, portName, want string) {
	t.Helper()
	lsp, err := m.GetLogicalSwitchPort(context.Background(), portName)
	if err != nil {
		t.Fatalf("guest LSP %s missing: %v", portName, err)
	}
	if len(lsp.PortSecurity) != 1 || lsp.PortSecurity[0] != want {
		t.Errorf("guest LSP %s PortSecurity = %v, want [%q]: guest LSPs without "+
			"port_security allow source-IP spoofing, breaking the IMDS "+
			"(VPC-ID, source-IP) → ENI mapping", portName, lsp.PortSecurity, want)
	}
	// port_security must mirror addresses exactly, or the enforced identity and
	// the advertised identity diverge.
	if len(lsp.Addresses) != 1 || lsp.Addresses[0] != lsp.PortSecurity[0] {
		t.Errorf("guest LSP %s Addresses %v != PortSecurity %v: enforced and advertised "+
			"identity must match", portName, lsp.Addresses, lsp.PortSecurity)
	}
}

// TestI5_SubnetDHCPOptionsContainsIMDSRoute asserts the per-subnet DHCPOptions
// row carries classless_static_route (RFC 3442 option 121) with both the
// default route and the IMDS /32, via the subnet gateway. Both the helper and
// both emission paths (live EnsureSubnet, reconciler applySubnets) are checked
// so the two paths cannot diverge again. Without option 121, guests with an
// auto-installed 169.254.0.0/16 link-local route never reach IMDS.
func TestI5_SubnetDHCPOptionsContainsIMDSRoute(t *testing.T) {
	ctx := context.Background()
	const cidr = "10.0.1.0/24"

	t.Run("helper", func(t *testing.T) {
		opts := topology.BuildSubnetDHCPOptions("10.0.1.1", "02:00:00:00:00:01", "{8.8.8.8}")
		assertClasslessStaticRoute(t, opts, "10.0.1.1")
	})

	t.Run("live", func(t *testing.T) {
		m := mock.New()
		mgr := topology.NewLiveManager(m)
		if err := mgr.EnsureVPC(ctx, topology.VPCSpec{VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.0.0/16")}); err != nil {
			t.Fatalf("EnsureVPC: %v", err)
		}
		if err := mgr.EnsureSubnet(ctx, topology.SubnetSpec{SubnetID: "subnet-a", VPCID: "vpc-a", CIDR: netip.MustParsePrefix(cidr)}); err != nil {
			t.Fatalf("EnsureSubnet: %v", err)
		}
		opts := dhcpOptionsFor(t, m, cidr)
		assertClasslessStaticRoute(t, opts, "10.0.1.1")
	})

	t.Run("reconciler", func(t *testing.T) {
		rec, m := newTestReconciler(t)
		if err := rec.Reconcile(ctx, freshIntent(t)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		opts := dhcpOptionsFor(t, m, cidr)
		assertClasslessStaticRoute(t, opts, "10.0.1.1")
	})
}

// dhcpOptionsFor returns the emitted Options map for a subnet CIDR.
func dhcpOptionsFor(t *testing.T, m *mock.Client, cidr string) map[string]string {
	t.Helper()
	row, err := m.FindDHCPOptionsByCIDR(context.Background(), cidr)
	if err != nil {
		t.Fatalf("DHCPOptions for %s missing: %v", cidr, err)
	}
	return row.Options
}

// assertClasslessStaticRoute fails with the ADR clause unless option 121 carries
// both the default route and the IMDS /32 via gwIP.
func assertClasslessStaticRoute(t *testing.T, opts map[string]string, gwIP string) {
	t.Helper()
	got := opts["classless_static_route"]
	wantDefault := "0.0.0.0/0," + gwIP
	wantIMDS := imds.MetaDataServerIP + "/32," + gwIP
	if !strings.Contains(got, wantDefault) || !strings.Contains(got, wantIMDS) {
		t.Errorf("classless_static_route = %q, must contain both %q and %q: subnets "+
			"without DHCP option 121 for %s break IMDS reachability on guests with "+
			"auto-installed link-local routes (NetworkManager, RHEL/Fedora, Ubuntu desktop)",
			got, wantDefault, wantIMDS, imds.MetaDataServerIP)
	}
}
