package reconcile

import (
	"context"
	"net"
	"net/netip"
	"testing"

	imds "github.com/mulgadc/spinifex/spinifex/handlers/imds"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// IMDS-datapath invariants. A guest LSP is created by two
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

// TestI5_SubnetRouterPortProxiesIMDSARP asserts the subnet router LSP carries
// options:arp_proxy = 169.254.169.254, so the subnet LRP answers ARP/ND for the
// IMDS address link-local. Both emission paths (live EnsureSubnet, reconciler
// applySubnets) are checked so they cannot diverge. This replaces the former
// DHCP option-121 mechanism: proxy-ARP reaches DHCP and fully static guests
// identically (AWS Nitro parity), whereas option 121 never reached a guest with
// no DHCP client.
func TestI5_SubnetRouterPortProxiesIMDSARP(t *testing.T) {
	ctx := context.Background()
	const cidr = "10.0.1.0/24"

	t.Run("live", func(t *testing.T) {
		m := mock.New()
		mgr := topology.NewLiveManager(m)
		if err := mgr.EnsureVPC(ctx, topology.VPCSpec{VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.0.0/16")}); err != nil {
			t.Fatalf("EnsureVPC: %v", err)
		}
		if err := mgr.EnsureSubnet(ctx, topology.SubnetSpec{SubnetID: "subnet-a", VPCID: "vpc-a", CIDR: netip.MustParsePrefix(cidr)}); err != nil {
			t.Fatalf("EnsureSubnet: %v", err)
		}
		assertARPProxy(t, m, topology.SubnetSwitchRouterPort("subnet-a"))
	})

	t.Run("reconciler", func(t *testing.T) {
		rec, m := newTestReconciler(t)
		if err := rec.Reconcile(ctx, freshIntent(t)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertARPProxy(t, m, topology.SubnetSwitchRouterPort("subnet-a"))
	})
}

// assertARPProxy fails with the ADR clause unless the subnet router LSP carries
// options:arp_proxy for the IMDS address.
func assertARPProxy(t *testing.T, m *mock.Client, portName string) {
	t.Helper()
	lsp, err := m.GetLogicalSwitchPort(context.Background(), portName)
	if err != nil {
		t.Fatalf("subnet router LSP %s missing: %v", portName, err)
	}
	if lsp.Options["arp_proxy"] != imds.MetaDataServerIP {
		t.Errorf("subnet router LSP %s options:arp_proxy = %q, want %q: subnet router "+
			"ports without options:arp_proxy for %s leave IMDS unreachable for any guest "+
			"that treats the address as link-local (the common static-IP and "+
			"NetworkManager/RHEL/Ubuntu case)",
			portName, lsp.Options["arp_proxy"], imds.MetaDataServerIP, imds.MetaDataServerIP)
	}
}
